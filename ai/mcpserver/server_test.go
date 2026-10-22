package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivers/sqlite"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

func TestMain(m *testing.M) {
	// Genkit logs through slog's default; the binary discards it (main.go).
	slog.SetDefault(slog.New(slog.DiscardHandler))
	for _, k := range []string{"GENKIT_ENV", "GENKIT_TELEMETRY_SERVER", "GENKIT_REFLECTION_V2_SERVER"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// ddlSecret is in a comment of the orders table's DDL: a schema resource
// must never carry it.
const ddlSecret = "ddl-token-9f2c"

// readTools are the tools every server offers.
var readTools = []string{
	tools.NameListConnections, tools.NameListNamespaces, tools.NameListCollections, tools.NameDescribe,
	tools.NameSample, tools.NameQuery, tools.NameExplain,
}

// env is a session manager over a temp SQLite database, seen through the
// connections lite, lite2, ro (read-only) and hidden, plus boom, a fake
// whose describe panics, and a history store.
type env struct {
	mgr  *session.Manager
	hist *history.Store
	db   string
	// unavailable is Options.Unavailable.
	unavailable map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	c, err := sqlite.New().Open(ctx, driver.ConnConfig{DSN: "sqlite:" + db})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(drivertest.SQLSeed("sqlite"),
		"create table orders (id integer primary key, -- "+ddlSecret+"\n status text not null default 'open')",
		"insert into orders (status) values ('open'), ('paid')",
		// Collections whose names the URI template cannot carry raw.
		`create table "sp ace" (id integer primary key)`,
		`create table "do$lar" (id integer primary key)`) {
		rs, err := c.Query(ctx, driver.Query{Text: s})
		if err != nil {
			t.Fatalf("seed %.40s: %v", s, err)
		}
		_ = rs.Close()
	}
	_ = c.Close()

	boom := drivertest.NewFake(drivertest.WithPeople())
	boom.OnDescribe("main", "tobira_people", func(*driver.CollectionSchema) { panic("driver bug") })
	reg := driver.NewRegistry()
	for _, d := range []driver.Driver{sqlite.New(), boom.Driver()} {
		if err := reg.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	dsn := "sqlite:" + db
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "lite", Driver: "sqlite", DSN: dsn},
		{Name: "lite2", Driver: "sqlite", DSN: dsn},
		{Name: "ro", Driver: "sqlite", DSN: dsn, ReadOnly: true},
		{Name: "hidden", Driver: "sqlite", DSN: dsn},
		{Name: "boom", Driver: "fake", DSN: "fake://x"},
	}}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	hreg := driver.NewRegistry()
	if err := hreg.Add(sqlite.New()); err != nil {
		t.Fatal(err)
	}
	hist, err := history.Open(ctx, hreg, filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hist.Close() })
	return &env{mgr: mgr, hist: hist, db: db}
}

// lockedBuffer is a bytes.Buffer safe for concurrent use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// serve runs Serve over pipes and returns an initialised client named
// clientName. At cleanup the client hangs up, Serve must return nil, and
// everything Serve wrote must be JSON-RPC frames.
func (e *env) serve(t *testing.T, pol config.MCP, clientName string) *client.Client {
	t.Helper()
	return e.serveWith(t, pol, clientName, nil)
}

// serveWith is serve, with prep applied to the server before it listens
// (nil runs Serve itself).
func (e *env) serveWith(t *testing.T, pol config.MCP, clientName string, prep func(*srv)) *client.Client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	raw := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		o := Options{Manager: e.mgr, History: e.hist, Policy: pol, Version: "test", Stdin: inR, Stdout: outW, Unavailable: e.unavailable}
		var err error
		if prep == nil {
			err = Serve(context.Background(), o)
		} else {
			var s *srv
			if s, err = newServer(context.Background(), o); err == nil {
				prep(s)
				err = s.listen(context.Background())
			}
		}
		_ = outW.Close()
		done <- err
	}()
	c := client.NewClient(transport.NewIO(io.TeeReader(outR, raw), inW, io.NopCloser(strings.NewReader(""))))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: clientName, Version: "1.0.0"}
	res, err := c.Initialize(ctx, init)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.ServerInfo.Name != "tobira" || res.ServerInfo.Version != "test" {
		t.Errorf("server info = %+v", res.ServerInfo)
	}
	t.Cleanup(func() {
		_ = c.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve after the client hung up: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after the client hung up")
		}
		assertFrames(t, raw.String())
	})
	return c
}

// assertFrames fails unless out is only newline-terminated JSON-RPC 2.0
// messages.
func assertFrames(t *testing.T, out string) {
	t.Helper()
	if out != "" && !strings.HasSuffix(out, "\n") {
		t.Errorf("stdout does not end with a complete frame: %q", out)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" && out == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("stdout line %d is not JSON: %q", i+1, line)
			continue
		}
		if string(m["jsonrpc"]) != `"2.0"` {
			t.Errorf("stdout line %d is not JSON-RPC 2.0: %q", i+1, line)
		}
		_, id := m["id"]
		_, method := m["method"]
		if !id && !method {
			t.Errorf("stdout line %d is neither a response nor a message: %q", i+1, line)
		}
	}
}

func call(t *testing.T, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = name
	req.Params.Arguments = args
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case mcp.TextContent:
			sb.WriteString(v.Text)
		case *mcp.TextContent:
			sb.WriteString(v.Text)
		default:
			t.Fatalf("content %T, want text", c)
		}
	}
	return sb.String()
}

// okJSON decodes a successful tool result.
func okJSON(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(t, res))
	}
	if err := json.Unmarshal([]byte(text(t, res)), v); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text(t, res))
	}
}

func toolNames(t *testing.T, c *client.Client) []string {
	t.Helper()
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func TestToolListWithoutWrites(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	if got := toolNames(t, c); !slices.Equal(got, sorted(readTools)) {
		t.Errorf("tools = %v, want %v", got, sorted(readTools))
	}
}

func TestToolListWithWrites(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite", "lite2"}, AllowWrites: []string{"lite"}}, "unit")
	want := sorted(append(slices.Clone(readTools), tools.NameExecute))
	if got := toolNames(t, c); !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// The tools carry the Toolset's input schemas, so clients see the
// arguments and which are required.
func TestToolsCarryTheirInputSchema(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name != tools.NameQuery {
			continue
		}
		b, _ := json.Marshal(tl)
		var got struct {
			InputSchema struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"inputSchema"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		s := got.InputSchema
		if s.Type != "object" || s.Properties["connection"] == nil || s.Properties["query"] == nil || s.Properties["limit"] == nil ||
			!slices.Contains(s.Required, "connection") || !slices.Contains(s.Required, "query") {
			t.Errorf("query input schema = %s", b)
		}
		return
	}
	t.Fatal("no query tool")
}

func TestListConnectionsShowsOnlyExposed(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite", "ro"}}, "unit")
	var conns []tools.Connection
	okJSON(t, call(t, c, tools.NameListConnections, nil), &conns)
	var names []string
	for _, cn := range conns {
		names = append(names, cn.Name)
	}
	if !slices.Equal(names, []string{"lite", "ro"}) {
		t.Errorf("connections = %v, want [lite ro]", names)
	}
}

// A configured but non-exposed connection reads exactly like a name that
// does not exist, in every tool.
func TestNonExposedIsIndistinguishableFromUnknown(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}, AllowWrites: []string{"lite"}}, "unit")
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{tools.NameListNamespaces, map[string]any{}},
		{tools.NameListCollections, map[string]any{}},
		{tools.NameDescribe, map[string]any{"collection": "orders"}},
		{tools.NameSample, map[string]any{"collection": "orders"}},
		{tools.NameQuery, map[string]any{"query": "select 1"}},
		{tools.NameExplain, map[string]any{"query": "select 1"}},
		{tools.NameExecute, map[string]any{"statement": "delete from orders"}},
	} {
		got := map[string]string{}
		for _, name := range []string{"hidden", "nosuch"} {
			args := map[string]any{"connection": name}
			for k, v := range tc.args {
				args[k] = v
			}
			res := call(t, c, tc.tool, args)
			if !res.IsError {
				t.Fatalf("%s on %s: not an error: %s", tc.tool, name, text(t, res))
			}
			got[name] = strings.ReplaceAll(text(t, res), name, "NAME")
		}
		if got["hidden"] != got["nosuch"] {
			t.Errorf("%s: non-exposed %q differs from unknown %q", tc.tool, got["hidden"], got["nosuch"])
		}
		if !strings.Contains(got["hidden"], "unknown connection") {
			t.Errorf("%s: %q, want unknown connection", tc.tool, got["hidden"])
		}
	}
}

func TestQueryReturnsRows(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	var out struct {
		Rows [][]any `json:"rows"`
	}
	okJSON(t, call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "select status from orders order by id"}), &out)
	if len(out.Rows) != 2 || out.Rows[0][0] != "open" || out.Rows[1][0] != "paid" {
		t.Errorf("rows = %v", out.Rows)
	}
}

// Large integers survive the trip through Genkit's tool runner untouched.
func TestQueryKeepsLargeIntegers(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	res := call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "select 9007199254740993 as n"})
	if !strings.Contains(text(t, res), "9007199254740993") {
		t.Errorf("result %s lost the integer", text(t, res))
	}
}

func TestMaxRowsAboveTheAssistantCap(t *testing.T) {
	const q = "with recursive c(x) as (select 1 union all select x + 1 from c where x < 250) select x from c"
	for _, tc := range []struct {
		max       int
		rows      int
		truncated bool
	}{{300, 250, false}, {5, 5, true}} {
		c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}, MaxRows: tc.max}, "unit")
		var out struct {
			Rows      [][]any `json:"rows"`
			Truncated bool    `json:"truncated"`
		}
		okJSON(t, call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": q, "limit": 1000}), &out)
		if len(out.Rows) != tc.rows || out.Truncated != tc.truncated {
			t.Errorf("max_rows %d: %d rows, truncated %v; want %d, %v", tc.max, len(out.Rows), out.Truncated, tc.rows, tc.truncated)
		}
	}
}

func TestSendRowsNever(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}, SendRows: "never"}, "unit")
	res := call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "select status from orders"})
	var out map[string]any
	okJSON(t, res, &out)
	if _, ok := out["rows"]; ok || out["rows_withheld"] != float64(2) || !strings.Contains(text(t, res), "withheld") {
		t.Errorf("result = %s, want rows withheld", text(t, res))
	}
	if strings.Contains(text(t, res), "paid") {
		t.Errorf("row data leaked: %s", text(t, res))
	}
	// Statement errors are reduced as in withheld mode.
	res = call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "select * from orders where status = 'secret-value' and nosuchcol = 1"})
	if !res.IsError || strings.Contains(text(t, res), "secret-value") {
		t.Errorf("error = %q, want a reduced error", text(t, res))
	}
}

func countOrders(t *testing.T, c *client.Client) float64 {
	t.Helper()
	var out struct {
		Rows [][]float64 `json:"rows"`
	}
	okJSON(t, call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "select count(*) from orders"}), &out)
	return out.Rows[0][0]
}

func TestExecuteRefusedOutsideAllowWrites(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite", "lite2"}, AllowWrites: []string{"lite"}}, "unit")
	res := call(t, c, tools.NameExecute, map[string]any{"connection": "lite2", "statement": "delete from orders"})
	if !res.IsError || !strings.Contains(text(t, res), "refused by policy") {
		t.Errorf("execute on lite2 = %q (error %v), want refused by policy", text(t, res), res.IsError)
	}
	if n := countOrders(t, c); n != 2 {
		t.Errorf("orders = %v after a refused delete, want 2", n)
	}
}

func TestExecuteRefusedOnReadOnly(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite", "ro"}, AllowWrites: []string{"ro"}}, "unit")
	res := call(t, c, tools.NameExecute, map[string]any{"connection": "ro", "statement": "delete from orders"})
	if !res.IsError || !strings.Contains(text(t, res), "refused by policy") {
		t.Errorf("execute on ro = %q (error %v), want refused by policy", text(t, res), res.IsError)
	}
	if n := countOrders(t, c); n != 2 {
		t.Errorf("orders = %v after a refused delete, want 2", n)
	}
}

func TestExecuteAllowed(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}, AllowWrites: []string{"lite"}}, "unit")
	var out struct {
		RowsAffected *int64 `json:"rows_affected"`
	}
	okJSON(t, call(t, c, tools.NameExecute, map[string]any{"connection": "lite", "statement": "insert into orders (status) values ('new')"}), &out)
	if out.RowsAffected == nil || *out.RowsAffected != 1 {
		t.Errorf("rows_affected = %v, want 1", out.RowsAffected)
	}
	if n := countOrders(t, c); n != 3 {
		t.Errorf("orders = %v, want 3", n)
	}
}

// The query tool stays read-only even on an allow-listed connection.
func TestQueryRefusesWrites(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}, AllowWrites: []string{"lite"}}, "unit")
	res := call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "delete from orders"})
	if !res.IsError {
		t.Errorf("a write through query succeeded: %s", text(t, res))
	}
	if n := countOrders(t, c); n != 2 {
		t.Errorf("orders = %v, want 2", n)
	}
}

func readResource(c *client.Client, uri string) (*mcp.ReadResourceResult, error) {
	var req mcp.ReadResourceRequest
	req.Params.URI = uri
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.ReadResource(ctx, req)
}

func resourceText(t *testing.T, res *mcp.ReadResourceResult) (string, string) {
	t.Helper()
	if len(res.Contents) != 1 {
		t.Fatalf("contents = %d, want 1", len(res.Contents))
	}
	switch v := res.Contents[0].(type) {
	case mcp.TextResourceContents:
		return v.Text, v.MIMEType
	case *mcp.TextResourceContents:
		return v.Text, v.MIMEType
	}
	t.Fatalf("content %T, want text", res.Contents[0])
	return "", ""
}

func TestSchemaResourceTemplateIsListed(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	res, err := c.ListResourceTemplates(context.Background(), mcp.ListResourceTemplatesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ResourceTemplates) != 1 {
		t.Fatalf("templates = %d, want 1", len(res.ResourceTemplates))
	}
	b, _ := json.Marshal(res.ResourceTemplates[0])
	if !strings.Contains(string(b), `"uriTemplate":"tobira://{connection}/{namespace}/{collection}/schema"`) {
		t.Errorf("template = %s", b)
	}
}

func TestSchemaResourceIsDescribeWithoutDDL(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	const uri = "tobira://lite/main/orders/schema"
	res, err := readResource(c, uri)
	if err != nil {
		t.Fatal(err)
	}
	body, mime := resourceText(t, res)
	if mime != "application/json" {
		t.Errorf("mime = %q", mime)
	}
	var got tools.DescribeOutput
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if got.Collection != "orders" || len(got.Fields) != 2 || got.Fields[1].Name != "status" || !slices.Equal(got.PrimaryKey, []string{"id"}) {
		t.Errorf("describe = %s", body)
	}
	low := strings.ToLower(body)
	if strings.Contains(body, ddlSecret) || strings.Contains(low, "create table") || strings.Contains(low, `"ddl"`) {
		t.Errorf("resource carries DDL: %s", body)
	}
	if v, ok := res.Contents[0].(mcp.TextResourceContents); ok && v.URI != uri {
		t.Errorf("uri = %q", v.URI)
	}
}

// A schema URI naming a non-exposed connection fails exactly like one
// naming a connection that does not exist.
func TestSchemaResourceNonExposedIsNotFound(t *testing.T) {
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	got := map[string]string{}
	for _, name := range []string{"hidden", "nosuch"} {
		_, err := readResource(c, "tobira://"+name+"/main/orders/schema")
		if err == nil {
			t.Fatalf("%s: read succeeded", name)
		}
		got[name] = strings.ReplaceAll(err.Error(), name, "NAME")
	}
	if got["hidden"] != got["nosuch"] {
		t.Errorf("non-exposed %q differs from unknown %q", got["hidden"], got["nosuch"])
	}
	if !strings.Contains(got["hidden"], "not found") {
		t.Errorf("error = %q, want not found", got["hidden"])
	}
	for _, uri := range []string{"tobira://lite/main/nosuch/schema", "tobira://lite/main/orders", "tobira://lite/main/orders/ddl", "tobira://lite/a/b/c/schema"} {
		if _, err := readResource(c, uri); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: err = %v, want not found", uri, err)
		}
	}
}

func historyMCP(t *testing.T, e *env) []history.Entry {
	t.Helper()
	got, err := e.hist.List(context.Background(), history.Filter{Source: history.SourceMCP, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(got) // oldest first
	return got
}

// Every tool call is recorded once, with source mcp and the client name
// from initialize.
func TestHistoryRecordsEveryCallWithTheClient(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite", "lite2"}, AllowWrites: []string{"lite"}}, "claude-code")
	call(t, c, tools.NameListConnections, nil)
	call(t, c, tools.NameDescribe, map[string]any{"connection": "lite", "collection": "orders"})
	call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "select 1"})
	call(t, c, tools.NameQuery, map[string]any{"connection": "hidden", "query": "select 2"})
	call(t, c, tools.NameExecute, map[string]any{"connection": "lite2", "statement": "delete from orders"})
	call(t, c, tools.NameExecute, map[string]any{"connection": "lite", "statement": "update orders set status = 'x' where id = 99"})

	got := historyMCP(t, e)
	if len(got) != 6 {
		for _, h := range got {
			t.Logf("%+v", h)
		}
		t.Fatalf("history has %d mcp entries, want 6 (one per call)", len(got))
	}
	for _, h := range got {
		if h.Source != history.SourceMCP || h.Client != "claude-code/1.0.0" {
			t.Errorf("entry %+v, want source mcp and client claude-code/1.0.0", h)
		}
	}
	type want struct {
		conn, query string
		ok          bool
	}
	for i, w := range []want{
		{"", "list_connections", true},
		{"lite", "describe", true},
		{"lite", "select 1", true},
		{"", "hidden", false}, // a refused name is never recorded as the connection
		{"lite2", "refused by policy: delete from orders", false},
		{"lite", "update orders set status = 'x' where id = 99", true},
	} {
		h := got[i]
		if h.Conn != w.conn || !strings.Contains(h.Query, w.query) || h.OK != w.ok {
			t.Errorf("entry %d = conn %q query %q ok %v; want conn %q, query containing %q, ok %v", i, h.Conn, h.Query, h.OK, w.conn, w.query, w.ok)
		}
	}
}

// The client name is stored as history stores any client: escaped, its
// first 64 runes.
func TestHistoryClientNameIsEscapedAndCapped(t *testing.T) {
	e := newEnv(t)
	name := "evil\x1b[31m\u202e" + strings.Repeat("x", 100)
	c := e.serve(t, config.MCP{Expose: []string{"lite"}}, name)
	call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": "select 1"})
	got := historyMCP(t, e)
	if len(got) != 1 {
		t.Fatalf("entries = %d", len(got))
	}
	if err := e.hist.Record(context.Background(), history.Entry{Conn: "ref", Source: history.SourceCLI, Query: "ref", Client: name + "/1.0.0"}, driver.ConnConfig{}); err != nil {
		t.Fatal(err)
	}
	ref, err := e.hist.List(context.Background(), history.Filter{Conn: "ref"})
	if err != nil || len(ref) != 1 {
		t.Fatalf("ref: %v %v", ref, err)
	}
	if got[0].Client != ref[0].Client || strings.ContainsAny(got[0].Client, "\x1b\u202e") {
		t.Errorf("client = %q, want %q", got[0].Client, ref[0].Client)
	}
}

// A panicking driver becomes a tool error; the server keeps serving.
func TestPanicIsRecoveredAsToolError(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite", "boom"}}, "unit")
	res := call(t, c, tools.NameDescribe, map[string]any{"connection": "boom", "collection": "tobira_people"})
	if !res.IsError || !strings.Contains(text(t, res), "internal error") {
		t.Errorf("result = %q (error %v), want an internal error", text(t, res), res.IsError)
	}
	if strings.Contains(text(t, res), "driver bug") {
		t.Errorf("panic value leaked: %q", text(t, res))
	}
	if _, err := readResource(c, "tobira://boom/main/tobira_people/schema"); err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Errorf("resource err = %v, want internal error", err)
	}
	if n := countOrders(t, c); n != 2 {
		t.Errorf("after the panic: orders = %v", n)
	}
}

func TestServeRefusesNothingExposed(t *testing.T) {
	e := newEnv(t)
	var out bytes.Buffer
	err := Serve(context.Background(), Options{Manager: e.mgr, Stdin: strings.NewReader(""), Stdout: &out, Policy: config.MCP{MaxRows: 200, SendRows: "always"}})
	if err == nil || !strings.Contains(err.Error(), "nothing is exposed") {
		t.Errorf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestServeReturnsNilOnCancel(t *testing.T) {
	e := newEnv(t)
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{Manager: e.mgr, Stdin: inR, Stdout: io.Discard, Policy: config.MCP{Expose: []string{"lite"}, MaxRows: 200, SendRows: "always"}})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		if err != nil {
			t.Errorf("err = %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

// A line that is not JSON gets a JSON-RPC parse error, never anything else
// on stdout.
func TestGarbageInputGetsAParseError(t *testing.T) {
	e := newEnv(t)
	var out lockedBuffer
	err := Serve(context.Background(), Options{Manager: e.mgr, Stdin: strings.NewReader("hello\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n"), Stdout: &out,
		Policy: config.MCP{Expose: []string{"lite"}, MaxRows: 200, SendRows: "always"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFrames(t, out.String())
	if !strings.Contains(out.String(), `"code":-32700`) {
		t.Errorf("stdout = %q, want a parse error", out.String())
	}
}

// A policy refusal is in history as failed, its query the statement
// prefixed "refused by policy: ", so `tobira history --mcp` shows it.
func TestHistoryMarksRefusals(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite", "ro", "lite2"}, AllowWrites: []string{"lite", "ro"}}, "unit")
	call(t, c, tools.NameExecute, map[string]any{"connection": "ro", "statement": "delete from orders"})
	call(t, c, tools.NameExecute, map[string]any{"connection": "lite2", "statement": "drop table orders"})
	got := historyMCP(t, e)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	for i, want := range []struct{ conn, query string }{{"ro", "refused by policy: delete from orders"}, {"lite2", "refused by policy: drop table orders"}} {
		if got[i].Conn != want.conn || got[i].Query != want.query || got[i].OK {
			t.Errorf("entry %d = %+v, want conn %q query %q ok false", i, got[i], want.conn, want.query)
		}
	}
}

// Resource reads are recorded as "resource <uri>", ok or failed; a
// non-exposed connection is never stored as the connection.
func TestHistoryRecordsResourceReads(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	if _, err := readResource(c, "tobira://lite/main/orders/schema"); err != nil {
		t.Fatal(err)
	}
	if _, err := readResource(c, "tobira://hidden/main/orders/schema"); err == nil {
		t.Fatal("hidden read succeeded")
	}
	got := historyMCP(t, e)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	want := []history.Entry{
		{Conn: "lite", Query: "resource tobira://lite/main/orders/schema", OK: true},
		{Conn: "", Query: "resource tobira://hidden/main/orders/schema", OK: false},
	}
	for i, w := range want {
		h := got[i]
		if h.Conn != w.Conn || h.Query != w.Query || h.OK != w.OK || h.Client != "unit/1.0.0" {
			t.Errorf("entry %d = %+v, want %+v with client unit/1.0.0", i, h, w)
		}
	}
}

// Options with MaxRows 0 get the MCP default of 200 rows, not the
// assistant's 50.
func TestServeDefaultsMaxRowsTo200(t *testing.T) {
	const q = "with recursive c(x) as (select 1 union all select x + 1 from c where x < 250) select x from c"
	c := newEnv(t).serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	var out struct {
		Rows      [][]any `json:"rows"`
		Truncated bool    `json:"truncated"`
	}
	okJSON(t, call(t, c, tools.NameQuery, map[string]any{"connection": "lite", "query": q, "limit": 1000}), &out)
	if len(out.Rows) != 200 || !out.Truncated {
		t.Errorf("%d rows, truncated %v; want 200, true", len(out.Rows), out.Truncated)
	}
}

// A tool handler that panics outside tobira's adapter becomes a tool error
// without the panic value, and the server goes on.
func TestPanicInAnyToolHandlerIsAToolError(t *testing.T) {
	e := newEnv(t)
	c := e.serveWith(t, config.MCP{Expose: []string{"lite"}}, "unit", func(s *srv) {
		s.mcp.AddTool(mcp.NewTool("crash"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			panic("raw handler bug")
		})
	})
	res := call(t, c, "crash", nil)
	if !res.IsError || !strings.Contains(text(t, res), "internal error") || strings.Contains(text(t, res), "raw handler bug") {
		t.Errorf("result = %q (error %v), want an internal error without the panic value", text(t, res), res.IsError)
	}
	if n := countOrders(t, c); n != 2 {
		t.Errorf("after the panic: orders = %v", n)
	}
}

// fakeSession is a ClientSession for calling hooks directly.
type fakeSession struct{}

func (fakeSession) Initialize()                                         {}
func (fakeSession) Initialized() bool                                   { return true }
func (fakeSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return nil }
func (fakeSession) SessionID() string                                   { return "fake" }

func initRequest(name, version string) *mcp.InitializeRequest {
	var r mcp.InitializeRequest
	r.Params.ClientInfo = mcp.Implementation{Name: name, Version: version}
	return &r
}

// The client is name/version (name alone without a version), the whole
// value cut to 64 runes.
func TestRememberClientKeepsNameAndVersion(t *testing.T) {
	s := &srv{log: slog.New(slog.DiscardHandler)}
	ctx := server.NewMCPServer("t", "1").WithContext(context.Background(), fakeSession{})
	long := strings.Repeat("é", 70)
	for _, tc := range []struct{ name, version, want string }{
		{"cursor", "1.2", "cursor/1.2"},
		{"cursor", "", "cursor"},
		{long, "9", strings.Repeat("é", 64)},
		{strings.Repeat("x", 60), "1.2.3", strings.Repeat("x", 60) + "/1.2"},
	} {
		s.rememberClient(ctx, nil, initRequest(tc.name, tc.version), nil)
		if got := s.clientName(ctx); got != tc.want {
			t.Errorf("%q %q: client %q, want %q", tc.name, tc.version, got, tc.want)
		}
	}
}

// A panic in a hook is recovered: the session goes on.
func TestHookPanicIsRecovered(t *testing.T) {
	s := &srv{} // no logger: the hook's log call panics
	ctx := server.NewMCPServer("t", "1").WithContext(context.Background(), fakeSession{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("hook panic escaped: %v", r)
		}
	}()
	s.rememberClient(ctx, nil, initRequest("x", "1"), nil)
	s.forgetClient(ctx, fakeSession{})
}

// A history write survives the call's cancellation, as the Toolset's do.
func TestRecordIsDetachedFromTheCall(t *testing.T) {
	e := newEnv(t)
	s, err := newServer(context.Background(), Options{Manager: e.mgr, History: e.hist, Policy: config.MCP{Expose: []string{"lite"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), clientKey{}, "c/1"))
	cancel()
	s.recordCall(ctx, tools.NameDescribe, "lite", json.RawMessage(`{"connection":"lite"}`), true, time.Millisecond)
	got := historyMCP(t, e)
	if len(got) != 1 || got[0].Conn != "lite" || got[0].Client != "c/1" {
		t.Errorf("entries = %+v, want the call recorded despite the cancelled context", got)
	}
}

// mcp-go's stdio server runs tool calls on a worker pool, so calls from one
// client overlap. Under -race: every result is right, the panics are
// recovered, and each call is recorded exactly once with the client name.
func TestStdioConcurrentCalls(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite", "lite2", "boom"}, AllowWrites: []string{"lite"}}, "busy")
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var req mcp.CallToolRequest
			want := ""
			switch i % 4 {
			case 0:
				req.Params.Name, req.Params.Arguments, want = tools.NameQuery, map[string]any{"connection": "lite", "query": fmt.Sprintf("select %d as v", i)}, fmt.Sprintf("[[%d]]", i)
			case 1:
				req.Params.Name, req.Params.Arguments, want = tools.NameDescribe, map[string]any{"connection": "lite2", "collection": "orders"}, `"collection":"orders"`
			case 2:
				req.Params.Name, req.Params.Arguments, want = tools.NameDescribe, map[string]any{"connection": "boom", "collection": "tobira_people"}, "internal error"
			case 3:
				req.Params.Name, req.Params.Arguments, want = tools.NameExecute, map[string]any{"connection": "lite2", "statement": fmt.Sprintf("delete from orders where id = %d", i)}, "refused by policy"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := c.CallTool(ctx, req)
			if err != nil {
				errs <- fmt.Sprintf("call %d: %v", i, err)
				return
			}
			var sb strings.Builder
			for _, ct := range res.Content {
				if tc, ok := ct.(mcp.TextContent); ok {
					sb.WriteString(tc.Text)
				}
			}
			if !strings.Contains(sb.String(), want) {
				errs <- fmt.Sprintf("call %d (%s) = %s, want %q", i, req.Params.Name, sb.String(), want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	got := historyMCP(t, e)
	if len(got) != n {
		t.Fatalf("history has %d entries, want %d (one per call)", len(got), n)
	}
	for _, h := range got {
		if h.Source != history.SourceMCP || h.Client != "busy/1.0.0" {
			t.Errorf("entry %+v, want source mcp and client busy/1.0.0", h)
		}
	}
}

// The premise of TestStdioConcurrentCalls: over stdio, calls do overlap.
func TestStdioCallsOverlap(t *testing.T) {
	e := newEnv(t)
	var inFlight, peak atomic.Int32
	c := e.serveWith(t, config.MCP{Expose: []string{"lite"}}, "unit", func(s *srv) {
		s.mcp.AddTool(mcp.NewTool("slow"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			n := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
			return mcp.NewToolResultText("ok"), nil
		})
	})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var req mcp.CallToolRequest
			req.Params.Name = "slow"
			if _, err := c.CallTool(context.Background(), req); err != nil {
				t.Errorf("slow: %v", err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() < 2 {
		t.Errorf("at most %d call ran at once: stdio calls are sequential, the concurrency test proves nothing", peak.Load())
	}
}

// A schema read that names nothing readable is MCP's resource-not-found
// (-32002), with one text for a hidden and an unknown connection alike; a
// read that fails inside tobira stays an internal error (-32603).
func TestSchemaResourceFailuresUseResourceNotFound(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite", "boom"}}, "unit")
	texts := map[string]string{}
	for _, uri := range []string{
		"tobira://hidden/main/orders/schema", "tobira://nosuch/main/orders/schema",
		"tobira://lite/main/nosuch/schema", "tobira://lite/main/orders/ddl", "tobira://lite//orders/x/schema",
	} {
		_, err := readResource(c, uri)
		if !errors.Is(err, mcp.ErrResourceNotFound) {
			t.Errorf("%s: err = %v, want resource not found (-32002)", uri, err)
			continue
		}
		texts[uri] = err.Error()
	}
	if h, u := texts["tobira://hidden/main/orders/schema"], texts["tobira://nosuch/main/orders/schema"]; h != u {
		t.Errorf("hidden %q differs from unknown %q", h, u)
	}
	_, err := readResource(c, "tobira://boom/main/tobira_people/schema")
	if !errors.Is(err, mcp.ErrInternalError) || errors.Is(err, mcp.ErrResourceNotFound) {
		t.Errorf("panicking read: err = %v, want an internal error (-32603)", err)
	}
	// A good read still works, and each read is recorded once.
	if _, err := readResource(c, "tobira://lite/main/orders/schema"); err != nil {
		t.Fatal(err)
	}
	if got := historyMCP(t, e); len(got) != 7 {
		t.Errorf("history has %d entries, want 7 (one per read)", len(got))
	}
}

// A call to a tool the server does not offer is recorded as failed, its
// query "unknown tool <name>" with the name escaped and cut to 64 runes.
func TestHistoryRecordsUnknownTools(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	name := "drop\x1b[31m" + strings.Repeat("x", 100)
	for _, n := range []string{"execute", name} {
		var req mcp.CallToolRequest
		req.Params.Name = n
		if _, err := c.CallTool(context.Background(), req); err == nil {
			t.Fatalf("%q: the call succeeded", n)
		}
	}
	got := historyMCP(t, e)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].Query != "unknown tool execute" || got[0].OK || got[0].Client != "unit/1.0.0" || got[0].Conn != "" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	want := "unknown tool " + query.EscapeControls(name[:64])
	if got[1].Query != want || got[1].OK || strings.Contains(got[1].Query, "\x1b") {
		t.Errorf("entry 1 query = %q, want %q", got[1].Query, want)
	}
}

// A URI the template rejects (a raw space or "$") is not found, even when
// a collection of that name exists: the read is recorded once, as failed,
// and never as a success.
func TestSchemaURIRejectedByTheTemplateIsNotFound(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	uris := []string{"tobira://lite/main/sp ace/schema", "tobira://lite/main/do$lar/schema"}
	for _, uri := range uris {
		if _, err := readResource(c, uri); !errors.Is(err, mcp.ErrResourceNotFound) {
			t.Errorf("%s: err = %v, want resource not found", uri, err)
		}
	}
	got := historyMCP(t, e)
	if len(got) != len(uris) {
		t.Fatalf("history has %d rows, want %d", len(got), len(uris))
	}
	for i, h := range got {
		if h.OK || h.Query != "resource "+uris[i] {
			t.Errorf("row %d = %+v, want a failed read of %s", i, h, uris[i])
		}
	}
	// The escaped form names the same collections and works.
	if _, err := readResource(c, "tobira://lite/main/sp%20ace/schema"); err != nil {
		t.Errorf("escaped space: %v", err)
	}
}

// parseSchemaURI accepts exactly what the template matches.
func TestParseSchemaURIAgreesWithTheTemplate(t *testing.T) {
	re := mcp.NewResourceTemplate(schemaTemplate, "x").URITemplate.Regexp()
	for _, uri := range []string{
		"tobira://lite/main/orders/schema", "tobira://lite//orders/schema", "tobira://lite/main/a%20b/schema",
		"tobira://lite/main/a b/schema", "tobira://lite/main/a$b/schema", "tobira://lite/main/a/b/schema",
		"tobira://lite/main/orders", "tobira:///main/orders/schema", "tobira://lite/main//schema", "tobira://l%zz/main/t/schema",
	} {
		_, ok := parseSchemaURI(uri)
		if ok && !re.MatchString(uri) {
			t.Errorf("%q: parsed, but the template rejects it", uri)
		}
	}
}

// Concurrent reads of one URI in one session are each served and each
// recorded once.
func TestConcurrentReadsOfOneURI(t *testing.T) {
	e := newEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"lite"}}, "unit")
	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := readResource(c, "tobira://lite/main/orders/schema")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("read: %v", err)
		}
	}
	got := historyMCP(t, e)
	ok := 0
	for _, h := range got {
		if h.OK {
			ok++
		}
	}
	if len(got) != n || ok != n {
		t.Errorf("history has %d rows (%d ok), want %d ok rows", len(got), ok, n)
	}
}
