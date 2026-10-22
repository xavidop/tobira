package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// rawStdout records every byte the server writes to stdout.
type rawStdout struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (r *rawStdout) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.Write(p)
}

func (r *rawStdout) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.String()
}

func mcpCall(t *testing.T, c *client.Client, name string, args map[string]any) (string, bool) {
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
	var sb strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// TestMCPEndToEndStdio runs `tobira mcp --expose lite` as a subprocess and
// drives it with mcp-go's stdio client over a real SQLite database: every
// call works, writes through query are refused, the calls are in
// `tobira history --mcp` with the client's name, and stdout carried only
// JSON-RPC frames.
func TestMCPEndToEndStdio(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := newSQLiteFile(t, bin, env)
	if out, code := run(t, bin, env, "query", "sqlite:"+path, "insert into t (id) values (1), (2)"); code != 0 {
		t.Fatalf("seed: exit %d (%q)", code, out)
	}
	env = writeConfig(t, env, "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+path+"\n  - name: other\n    driver: sqlite\n    dsn: sqlite:"+path+"\n")

	cmd := exec.Command(bin, "mcp", "--expose", "lite")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Requests stop waiting as soon as the server exits. Every response has
	// been read by then (the client reads stdout continuously), so Wait
	// closing the pipe loses nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
		cancel()
	}()
	wait := func() error {
		select {
		case err := <-exited:
			return err
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			return errors.New("tobira mcp did not exit after stdin closed")
		}
	}
	defer func() {
		if t.Failed() {
			t.Logf("stderr:\n%s", stderr.String())
		}
	}()

	raw := &rawStdout{}
	c := client.NewClient(transport.NewIO(io.TeeReader(stdout, raw), stdin, io.NopCloser(strings.NewReader(""))))
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "tobira-e2e", Version: "0.0.1"}
	ir, err := c.Initialize(ctx, init)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if ir.ServerInfo.Name != "tobira" || ir.ServerInfo.Version != "dev" {
		t.Errorf("server info = %+v", ir.ServerInfo)
	}

	lt, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range lt.Tools {
		names = append(names, tl.Name)
	}
	for _, want := range []string{"list_connections", "describe", "query", "sample", "explain"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v lack %s", names, want)
		}
	}
	if slices.Contains(names, "execute") {
		t.Errorf("execute offered without allow_writes: %v", names)
	}

	out, isErr := mcpCall(t, c, "describe", map[string]any{"connection": "lite", "collection": "t"})
	if isErr || !strings.Contains(out, `"collection":"t"`) || !strings.Contains(out, `"primary_key":["id"]`) {
		t.Errorf("describe = %s (error %v)", out, isErr)
	}
	out, isErr = mcpCall(t, c, "query", map[string]any{"connection": "lite", "query": "select id from t order by id"})
	if isErr || !strings.Contains(out, `"rows":[[1],[2]]`) {
		t.Errorf("query = %s (error %v)", out, isErr)
	}
	out, isErr = mcpCall(t, c, "query", map[string]any{"connection": "lite", "query": "delete from t"})
	if !isErr {
		t.Errorf("a write through query was not refused: %s", out)
	}
	out, isErr = mcpCall(t, c, "query", map[string]any{"connection": "other", "query": "select 1"})
	if !isErr || !strings.Contains(out, "unknown connection") {
		t.Errorf("non-exposed connection = %s (error %v), want unknown", out, isErr)
	}

	var rr mcp.ReadResourceRequest
	rr.Params.URI = "tobira://lite/main/t/schema"
	res, err := c.ReadResource(ctx, rr)
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("resource contents = %d", len(res.Contents))
	}
	trc, ok := res.Contents[0].(mcp.TextResourceContents)
	if !ok || trc.MIMEType != "application/json" || !strings.Contains(trc.Text, `"collection":"t"`) || strings.Contains(strings.ToLower(trc.Text), "create table") {
		t.Errorf("schema resource = %#v", res.Contents[0])
	}

	if err := c.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("tobira mcp exit: %v", err)
	}

	// Stdout carried only JSON-RPC frames.
	frames := raw.String()
	if !strings.HasSuffix(frames, "\n") {
		t.Errorf("stdout does not end with a complete frame: %q", frames)
	}
	lines := strings.Split(strings.TrimSuffix(frames, "\n"), "\n")
	if len(lines) < 7 {
		t.Errorf("stdout has %d frames, want one per request", len(lines))
	}
	for i, line := range lines {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil || string(m["jsonrpc"]) != `"2.0"` {
			t.Errorf("stdout line %d is not a JSON-RPC frame: %q", i+1, line)
		}
	}

	// History has one mcp entry per tool call, with the client's name.
	hout, code := run(t, bin, env, "history", "--mcp", "--format", "json")
	if code != 0 {
		t.Fatalf("history --mcp: exit %d (%q)", code, hout)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(hout), &rows); err != nil {
		var doc struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal([]byte(hout), &doc); err != nil {
			t.Fatalf("history is not JSON: %v\n%s", err, hout)
		}
		rows = doc.Rows
	}
	if len(rows) != 5 {
		t.Fatalf("history --mcp has %d entries, want 5 (one per tool call and resource read):\n%s", len(rows), hout)
	}
	var queries []string
	for _, r := range rows {
		if r["source"] != "mcp" || r["client"] != "tobira-e2e/0.0.1" {
			t.Errorf("entry %v, want source mcp and client tobira-e2e/0.0.1", r)
		}
		q, _ := r["query"].(string)
		queries = append(queries, q)
	}
	all := strings.Join(queries, "\n")
	for _, want := range []string{"describe", "select id from t order by id", "delete from t", "resource tobira://lite/main/t/schema"} {
		if !strings.Contains(all, want) {
			t.Errorf("history queries %q lack %q", queries, want)
		}
	}
}

// Nothing exposed: exit 1 with the message on stderr and nothing on stdout.
func TestMCPEndToEndNothingExposed(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	stdout, stderr, code := runBoth(t, bin, env, "mcp")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing is exposed: pass --expose or set mcp.expose") {
		t.Errorf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// A signal while serving is a clean shutdown: exit 0.
func TestMCPEndToEndSignalExits0(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := newSQLiteFile(t, bin, env)
	env = writeConfig(t, env, "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+path+"\nmcp:\n  expose: [lite]\n")
	cmd := exec.Command(bin, "mcp")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	raw := &rawStdout{}
	c := client.NewClient(transport.NewIO(io.TeeReader(stdout, raw), stdin, io.NopCloser(strings.NewReader(""))))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "tobira-e2e"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("exit after SIGINT: %v, want 0", err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("tobira mcp did not exit after SIGINT")
	}
	// Shutdown wrote nothing but frames: no banner, no log line.
	assertJSONRPCOnly(t, raw.String())
}

// assertJSONRPCOnly fails unless out is newline-terminated JSON-RPC 2.0
// frames.
func assertJSONRPCOnly(t *testing.T, out string) {
	t.Helper()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("stdout does not end with a complete frame: %q", out)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil || string(m["jsonrpc"]) != `"2.0"` {
			t.Errorf("stdout line %d is not a JSON-RPC frame: %q", i+1, line)
		}
	}
}
