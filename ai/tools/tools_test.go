package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

var ctx = context.Background()

func TestToolNames(t *testing.T) {
	e := newEnv(t)
	var names []string
	for _, r := range e.ts.Read() {
		names = append(names, r.Name())
	}
	want := []string{"list_connections", "list_namespaces", "list_collections", "describe", "sample", "query", "explain"}
	if !slices.Equal(names, want) {
		t.Fatalf("Read() = %v, want %v", names, want)
	}
	if got := e.ts.Execute().Name(); got != "execute" {
		t.Fatalf("Execute() = %q", got)
	}
}

func TestListConnectionsGolden(t *testing.T) {
	e := newEnv(t)
	if _, err := e.mgr.Open(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	got, err := e.ts.ListConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := mustJSON(t, got)
	noSecret(t, "list_connections", b)
	golden(t, "list_connections", b)
}

func TestSchemaToolsGolden(t *testing.T) {
	e := newEnv(t)
	ns, err := e.ts.ListNamespaces(ctx, ListNamespacesInput{Connection: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "list_namespaces", mustJSON(t, ns))
	cols, err := e.ts.ListCollections(ctx, ListCollectionsInput{Connection: "dev", Namespace: "main"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "list_collections", mustJSON(t, cols))
	d, err := e.ts.Describe(ctx, DescribeInput{Connection: "dev", Namespace: "main", Collection: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	b := mustJSON(t, d)
	noSecret(t, "describe", b)
	if strings.Contains(string(b), "CREATE TABLE") {
		t.Errorf("describe output carries DDL:\n%s", b)
	}
	golden(t, "describe", b)
}

func TestDataToolsGolden(t *testing.T) {
	e := newEnv(t, withMaxRows(2))
	q, err := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from tobira_people order by id"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "query", mustJSON(t, q))
	s, err := e.ts.Sample(ctx, SampleInput{Connection: "dev", Namespace: "main", Collection: "orders", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "sample", mustJSON(t, s))
	x, err := e.ts.Explain(ctx, ExplainInput{Connection: "dev", Query: "select * from orders"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "explain", mustJSON(t, x))
}

// Every read tool passes ReadOnly, the row limit and a deadline.
func TestReadToolsAreReadOnlyAndLimited(t *testing.T) {
	e := newEnv(t, withMaxRows(3))
	if _, err := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from tobira_people", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ts.Explain(ctx, ExplainInput{Connection: "dev", Query: "select * from tobira_people"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.fake.Calls() {
		switch c.Op {
		case "query":
			if !c.Query.ReadOnly || c.Query.Limit != 3 {
				t.Errorf("query call = %+v, want ReadOnly and Limit 3", c.Query)
			}
		case "explain":
			if !c.Query.ReadOnly {
				t.Errorf("explain call = %+v, want ReadOnly", c.Query)
			}
		}
	}
}

func TestMaxRowsDefaultsAndCap(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 50}, {-1, 50}, {7, 7}, {200, 200}, {500, 200}} {
		if got := NewToolset(ToolDeps{MaxRows: tc.in}).maxRows; got != tc.want {
			t.Errorf("MaxRows %d -> %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestWriteViaQueryIsRefused(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{"delete from tobira_people where id = 1", "select * from orders; delete from tobira_people where id = 2"} {
		out, err := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.Error, "read-only") {
			t.Errorf("write %q via query: error = %q, want a read-only refusal", q, out.Error)
		}
	}
	// Count People through the Fake: the tool caps rows at MaxRows.
	c, err := e.mgr.Open(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := c.Query(ctx, driver.Query{Text: "select * from tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != 57 {
		t.Fatalf("People rows = %d, %v; want 57 after refused writes", len(rows), err)
	}
}

// A connection whose driver does not declare CapReadOnly gets the schema
// tools only: query, sample and explain refuse before touching the driver.
func TestCapReadOnlyGate(t *testing.T) {
	e := newEnv(t, withFakeOptions(drivertest.WithoutCapabilities(driver.CapReadOnly)))
	q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	s, _ := e.ts.Sample(ctx, SampleInput{Connection: "dev", Collection: "orders"})
	x, _ := e.ts.Explain(ctx, ExplainInput{Connection: "dev", Query: "select * from orders"})
	for name, msg := range map[string]string{"query": q.Error, "sample": s.Error, "explain": x.Error} {
		if !strings.Contains(msg, "schema tools") {
			t.Errorf("%s on a connection without readonly: error = %q", name, msg)
		}
	}
	for _, op := range e.fakeOps() {
		if op == "query" || op == "browse" || op == "explain" {
			t.Errorf("the driver saw a %s call", op)
		}
	}
	d, err := e.ts.Describe(ctx, DescribeInput{Connection: "dev", Collection: "orders"})
	if err != nil || d.Error != "" || len(d.Fields) != 3 {
		t.Fatalf("describe without readonly = %+v, %v", d, err)
	}
}

func TestRowPolicyNever(t *testing.T) {
	e := newEnv(t, withPolicy(RowsNever))
	asked := false
	e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		asked = true
		return assistant.ConsentOnce, nil
	}})
	q, err := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	if err != nil {
		t.Fatal(err)
	}
	if q.Rows != nil || q.RowsWithheld != 2 || q.Note != "rows withheld by policy (ai.send_rows: never)" || len(q.Columns) != 3 {
		t.Fatalf("never: %+v", q)
	}
	x, _ := e.ts.Explain(ctx, ExplainInput{Connection: "dev", Query: "select * from orders"})
	if x.Plan != "" || x.RowsWithheld != 1 || x.Note == "" {
		t.Fatalf("never explain: %+v", x)
	}
	if asked {
		t.Fatal("never asked for consent")
	}
	golden(t, "query_never", mustJSON(t, q))
}

func TestRowPolicyAlways(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAlways))
	q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	if q.Rows == nil || q.RowsWithheld != 0 || q.Note != "" {
		t.Fatalf("always: %+v", q)
	}
}

func TestRowPolicyAsk(t *testing.T) {
	type ask struct {
		conn string
		rows int
	}
	setup := func(t *testing.T, answer assistant.Consent) (*env, *[]ask) {
		e := newEnv(t, withPolicy(RowsAsk))
		var asks []ask
		e.setEvents(assistant.Events{ConsentRows: func(_ context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
			asks = append(asks, ask{req.Conn, req.Rows})
			return answer, nil
		}})
		return e, &asks
	}
	query := func(e *env, conn string) RowsOutput {
		q, err := e.ts.Query(ctx, QueryInput{Connection: conn, Query: "select * from orders"})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	t.Run("once", func(t *testing.T) {
		e, asks := setup(t, assistant.ConsentOnce)
		if q := query(e, "dev"); q.Rows == nil {
			t.Fatalf("once: rows not sent: %+v", q)
		}
		query(e, "dev")
		if !slices.Equal(*asks, []ask{{"dev", 2}, {"dev", 2}}) {
			t.Fatalf("once asks = %v, want two", *asks)
		}
	})
	t.Run("session", func(t *testing.T) {
		e, asks := setup(t, assistant.ConsentSession)
		query(e, "dev")
		if q := query(e, "dev"); q.Rows == nil {
			t.Fatalf("session: second call withheld: %+v", q)
		}
		if s, _ := e.ts.Sample(ctx, SampleInput{Connection: "dev", Collection: "orders"}); s.Rows == nil {
			t.Fatalf("session: sample withheld: %+v", s)
		}
		query(e, "dev2") // another connection asks again
		if !slices.Equal(*asks, []ask{{"dev", 2}, {"dev2", 2}}) {
			t.Fatalf("session asks = %v", *asks)
		}
		// A new toolset (a new panel session) forgets.
		other := NewToolset(e.ts.deps)
		other.fallbackEvents = e.ts.fallbackEvents
		if q, _ := other.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"}); q.Rows == nil {
			t.Fatalf("new toolset: %+v", q)
		}
		if len(*asks) != 3 {
			t.Fatalf("a new toolset did not ask again: %v", *asks)
		}
	})
	t.Run("no", func(t *testing.T) {
		e, asks := setup(t, assistant.ConsentNo)
		q := query(e, "dev")
		if q.Rows != nil || q.RowsWithheld != 2 || !strings.Contains(q.Note, "declined") {
			t.Fatalf("no: %+v", q)
		}
		query(e, "dev")
		if len(*asks) != 2 {
			t.Fatalf("a refusal must not be remembered: %v", *asks)
		}
	})
	t.Run("nil callback", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAsk))
		q := query(e, "dev")
		if q.Rows != nil || q.RowsWithheld != 2 {
			t.Fatalf("nil ConsentRows must withhold: %+v", q)
		}
	})
	t.Run("zero rows are not asked about", func(t *testing.T) {
		e, asks := setup(t, assistant.ConsentNo)
		q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders where id = 99"})
		if len(*asks) != 0 || q.RowsWithheld != 0 || string(q.Rows) != "[]" {
			t.Fatalf("zero rows: asks %v, out %+v", *asks, q)
		}
	})
	t.Run("default policy is ask", func(t *testing.T) {
		if NewToolset(ToolDeps{}).policy != RowsAsk {
			t.Fatal("empty Policy must mean ask")
		}
	})
}

func TestTruncationAndBytes(t *testing.T) {
	long := strings.Repeat("é", 300)
	cases := []struct {
		v    driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.String("short"), "short"},
		{driver.String(long), strings.Repeat("é", 256) + "…"},
		{driver.Bytes(make([]byte, 10)), "<bytes 10 B>"},
		{driver.Bytes(make([]byte, 1229)), "<bytes 1.2 KiB>"},
		{driver.Bytes(make([]byte, 3<<20)), "<bytes 3.0 MiB>"},
		{driver.JSON([]byte(`{"a": [1, 2]}`)), `{"a":[1,2]}`},
		{driver.ObjectValue(driver.Member{Key: "k", Value: driver.String(long)}), `{"k":"` + strings.Repeat("é", 250) + "…"},
		{driver.List(driver.Int(1), driver.Bytes([]byte{1})), `[1,"AQ=="]`},
		{driver.Time(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)), "2026-10-20T09:00:00Z"},
	}
	ts := NewToolset(ToolDeps{})
	for _, tc := range cases {
		got := ts.encodeValue(tc.v, func(s string) string { return s })
		if got != tc.want {
			t.Errorf("encodeValue(%v) = %#v, want %#v", tc.v, got, tc.want)
		}
	}
	// Numbers stay numbers.
	for _, v := range []driver.Value{driver.Int(42), driver.Float(1.5), driver.Decimal("12.50"), driver.Bool(true)} {
		got := mustJSON(t, ts.encodeValue(v, func(s string) string { return s }))
		want := mustJSON(t, v)
		if string(got) != string(want) {
			t.Errorf("encodeValue(%v) = %s, want %s", v, got, want)
		}
	}
}

func TestTruncationThroughQuery(t *testing.T) {
	e := newEnv(t)
	e.fake.AddCollection("main", "blobs", []driver.Column{{Name: "b", Type: driver.TypeBytes}, {Name: "s", Type: driver.TypeString}},
		[]driver.Row{{driver.Bytes(make([]byte, 2048)), driver.String(strings.Repeat("x", 1000))}})
	q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from blobs"})
	want := `[["<bytes 2.0 KiB>","` + strings.Repeat("x", 256) + `…"]]`
	if string(q.Rows) != want {
		t.Fatalf("rows = %s", q.Rows)
	}
}

// Secrets never leave a tool: a driver error that echoes the DSN is masked
// in the tool output, the ToolCall event and the history; so is a value.
func TestRedaction(t *testing.T) {
	e := newEnv(t)
	e.fake.FailNext("query", driver.Wrap(driver.ErrSyntax, "fake", "query",
		errors.New("bad query on fake://admin:"+dsnPassword+"@db.local/app near "+dsnPassword)))
	q, err := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	if err != nil {
		t.Fatal(err)
	}
	if q.Error == "" {
		t.Fatal("the driver error did not reach the model")
	}
	noSecret(t, "query error", []byte(q.Error))
	e.fake.AddCollection("main", "leaky", []driver.Column{{Name: "v", Type: driver.TypeString}}, []driver.Row{{driver.String("pw is " + dsnPassword)}})
	v, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from leaky"})
	noSecret(t, "query value", mustJSON(t, v))
	noSecret(t, "tool call events", mustJSON(t, e.toolCalls()))
	noSecret(t, "history", mustJSON(t, e.hist.all()))
	calls := e.toolCalls()
	if len(calls) != 2 || calls[0].Name != "query" || calls[0].Err == "" || calls[1].Rows != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
}

// The extra Redact hook (the provider key) applies after the connection's.
func TestExtraRedact(t *testing.T) {
	e := newEnv(t)
	deps := e.ts.deps
	deps.Redact = func(s string) string { return strings.ReplaceAll(s, "open", "[masked]") }
	ts := NewToolset(deps)
	q, _ := ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	if strings.Contains(string(q.Rows), "open") || !strings.Contains(string(q.Rows), "[masked]") {
		t.Fatalf("extra redact not applied: %s", q.Rows)
	}
}

func TestToolCallEvents(t *testing.T) {
	e := newEnv(t)
	_, _ = e.ts.Describe(ctx, DescribeInput{Connection: "dev", Collection: "orders"})
	_, _ = e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	_, _ = e.ts.Query(ctx, QueryInput{Connection: "nope", Query: "select 1"})
	calls := e.toolCalls()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Name != "describe" || calls[0].Rows != -1 || calls[0].Err != "" || !strings.Contains(calls[0].Input, `"collection":"orders"`) {
		t.Errorf("describe event = %+v", calls[0])
	}
	if calls[1].Name != "query" || calls[1].Rows != 2 || !strings.Contains(calls[1].Input, "select * from orders") {
		t.Errorf("query event = %+v", calls[1])
	}
	if calls[2].Err == "" {
		t.Errorf("failed call event = %+v", calls[2])
	}
}

func TestHistoryEntries(t *testing.T) {
	e := newEnv(t)
	_, _ = e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	_, _ = e.ts.Sample(ctx, SampleInput{Connection: "dev", Namespace: "main", Collection: "orders", Limit: 1})
	_, _ = e.ts.Explain(ctx, ExplainInput{Connection: "dev", Query: "select * from orders"})
	_, _ = e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from missing"})
	_, _ = e.ts.Describe(ctx, DescribeInput{Connection: "dev", Collection: "orders"}) // not recorded
	got := e.hist.all()
	type row struct {
		query string
		ok    bool
		rows  int64
	}
	var rows []row
	for _, h := range got {
		if h.Source != history.SourceAI || h.Conn != "dev" {
			t.Errorf("entry %+v: want source ai, conn dev", h)
		}
		rows = append(rows, row{h.Query, h.OK, h.Rows})
	}
	want := []row{
		{"select * from orders", true, 2},
		{"sample main.orders limit 1", true, 1},
		{"explain select * from orders", true, 1},
		{"select * from missing", false, 0},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("history = %+v\nwant %+v", rows, want)
	}
}

func TestExecute(t *testing.T) {
	const stmt = "delete from orders where id = 1"
	confirmWith := func(e *env, answer bool) *int {
		n := 0
		e.setEvents(assistant.Events{ConfirmExecute: func(_ context.Context, conn, s string) (bool, error) {
			n++
			if s != stmt {
				t.Errorf("confirm got %q", s)
			}
			return answer, nil
		}})
		return &n
	}
	ran := func(e *env) bool { return slices.Contains(e.fakeOps(), "query") }

	t.Run("not allowed", func(t *testing.T) {
		e := newEnv(t)
		asked := confirmWith(e, true)
		out, err := e.ts.Exec(ctx, ExecuteInput{Connection: "dev", Statement: stmt})
		if err != nil || !strings.Contains(out.Error, "not enabled") || ran(e) || *asked != 0 {
			t.Fatalf("not allowed: %+v %v (asked %d)", out, err, *asked)
		}
	})
	t.Run("no confirm callback", func(t *testing.T) {
		e := newEnv(t)
		out, _ := e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "dev", Statement: stmt})
		if !strings.Contains(out.Error, "declined") || ran(e) {
			t.Fatalf("nil confirm: %+v", out)
		}
	})
	t.Run("declined", func(t *testing.T) {
		e := newEnv(t)
		asked := confirmWith(e, false)
		out, _ := e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "dev", Statement: stmt})
		if !strings.Contains(out.Error, "declined") || ran(e) || *asked != 1 {
			t.Fatalf("declined: %+v", out)
		}
	})
	t.Run("read-only connection", func(t *testing.T) {
		e := newEnv(t)
		asked := confirmWith(e, true)
		out, _ := e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "ro", Statement: stmt})
		if !strings.Contains(out.Error, "read-only") || ran(e) || *asked != 0 {
			t.Fatalf("read-only: %+v (asked %d)", out, *asked)
		}
		if err := e.mgr.SetReadOnly("dev", true); err != nil {
			t.Fatal(err)
		}
		out, _ = e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "dev", Statement: stmt})
		if !strings.Contains(out.Error, "read-only") || ran(e) {
			t.Fatalf("runtime read-only: %+v", out)
		}
	})
	t.Run("confirmed", func(t *testing.T) {
		e := newEnv(t)
		asked := confirmWith(e, true)
		out, err := e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "dev", Statement: stmt})
		if err != nil || out.Error != "" || out.RowsAffected == nil || *out.RowsAffected != 1 || *asked != 1 {
			t.Fatalf("confirmed: %+v %v", out, err)
		}
		for _, c := range e.fake.Calls() {
			if c.Op == "query" && c.Query.ReadOnly {
				t.Error("execute ran with ReadOnly")
			}
		}
		h := e.hist.all()
		if len(h) != 1 || h[0].Query != stmt || h[0].Source != history.SourceAI || !h[0].OK {
			t.Fatalf("history = %+v", h)
		}
	})
}

func TestToolErrors(t *testing.T) {
	e := newEnv(t)
	cases := map[string]func() string{
		"unknown connection": func() string {
			o, _ := e.ts.ListNamespaces(ctx, ListNamespacesInput{Connection: "nope"})
			return o.Error
		},
		"broken connection": func() string {
			o, _ := e.ts.Describe(ctx, DescribeInput{Connection: "bad", Collection: "x"})
			return o.Error
		},
		"unknown namespace": func() string {
			o, _ := e.ts.ListCollections(ctx, ListCollectionsInput{Connection: "dev", Namespace: "nope"})
			return o.Error
		},
		"unknown collection": func() string {
			o, _ := e.ts.Describe(ctx, DescribeInput{Connection: "dev", Collection: "nope"})
			return o.Error
		},
		"unknown collection sample": func() string {
			o, _ := e.ts.Sample(ctx, SampleInput{Connection: "dev", Collection: "nope"})
			return o.Error
		},
		"empty query": func() string {
			o, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "  "})
			return o.Error
		},
		"missing connection": func() string {
			o, _ := e.ts.Query(ctx, QueryInput{Query: "select 1"})
			return o.Error
		},
	}
	for name, fn := range cases {
		if msg := fn(); msg == "" {
			t.Errorf("%s: no tool error", name)
		}
	}
	o, _ := e.ts.ListNamespaces(ctx, ListNamespacesInput{Connection: "nope"})
	if !strings.Contains(o.Error, `unknown connection "nope"`) {
		t.Errorf("unknown connection error = %q", o.Error)
	}
}

// The 30 s per-call timeout is a tool error the model can react to; a
// cancelled turn is an error that ends the turn.
func TestTimeoutAndCancel(t *testing.T) {
	e := newEnv(t)
	e.ts.timeout = 50 * time.Millisecond
	out, err := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "sleep 10s"})
	if err != nil || !strings.Contains(out.Error, "timed out") {
		t.Fatalf("timeout: %+v %v", out, err)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.ts.Query(c, QueryInput{Connection: "dev", Query: "select * from orders"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn: err = %v", err)
	}
	if NewToolset(ToolDeps{}).timeout != 30*time.Second {
		t.Fatal("the default timeout must be 30s")
	}
}

// Through Genkit: the tool's output reaches the model as JSON and a tool
// error is an output, not a failed call.
func TestGenkitTools(t *testing.T) {
	e := newEnv(t)
	tools := e.ts.Read()
	q := tools[5].(ai.Tool)
	got, err := q.RunRaw(ctx, map[string]any{"connection": "dev", "query": "select * from orders"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["truncated"] != false || len(m["rows"].([]any)) != 2 {
		t.Fatalf("query via genkit = %#v", got)
	}
	got, err = q.RunRaw(ctx, map[string]any{"connection": "nope", "query": "select 1"})
	if err != nil {
		t.Fatalf("a tool error must be an output: %v", err)
	}
	if m := got.(map[string]any); m["error"] == nil {
		t.Fatalf("no error field: %#v", got)
	}
	lc := tools[0].(ai.Tool)
	got, err = lc.RunRaw(ctx, map[string]any{})
	if err != nil || len(got.([]any)) != 4 {
		t.Fatalf("list_connections via genkit = %#v, %v", got, err)
	}
	def := tools[3].(ai.Tool).Definition()
	if req := fmt.Sprint(def.InputSchema["required"]); def.Name != "describe" || def.Description == "" || req != "[connection collection]" {
		t.Fatalf("describe definition = %+v", def)
	}
}

// browseSpy records the options of every Browse.
type browseSpy struct {
	driver.Driver
	mu   sync.Mutex
	opts []driver.BrowseOptions
}

func (d *browseSpy) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return spyConn{Conn: c, d: d}, nil
}

type spyConn struct {
	driver.Conn
	d *browseSpy
}

func (c spyConn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	c.d.mu.Lock()
	c.d.opts = append(c.d.opts, opts)
	c.d.mu.Unlock()
	return c.Conn.(driver.Browser).Browse(ctx, ref, opts)
}

func TestSampleIsReadOnlyAndLimited(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	spy := &browseSpy{Driver: f.Driver()}
	reg := driver.NewRegistry()
	if err := reg.Add(spy); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "dev", Driver: "fake", DSN: "fake://x"}}}, reg, secrets.Resolver{}, false)
	defer func() { _ = mgr.CloseAll() }()
	ts := NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways, MaxRows: 4})
	out, err := ts.Sample(ctx, SampleInput{Connection: "dev", Collection: "tobira_people", Limit: 99})
	if err != nil || out.Error != "" || out.count != 4 || !out.Truncated {
		t.Fatalf("sample = %+v, %v", out, err)
	}
	if len(spy.opts) != 1 || !spy.opts[0].ReadOnly || spy.opts[0].Limit != 4 {
		t.Fatalf("browse options = %+v, want ReadOnly and Limit 4", spy.opts)
	}
}

// describe carries a wide-column table's partition and clustering keys,
// and leaves them out for other collections.
func TestDescribeWideColumnKeys(t *testing.T) {
	e := newEnv(t, withFakeOptions(drivertest.WithWideColumns()))
	d, err := e.ts.Describe(ctx, DescribeInput{Connection: "dev", Namespace: "main", Collection: drivertest.WideColumnCollection})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.PartitionKey, []string{"tenant"}) {
		t.Errorf("partition_key = %v", d.PartitionKey)
	}
	if want := []KeyColumn{{Name: "ts", Desc: true}, {Name: "seq"}}; !slices.Equal(d.ClusteringKey, want) {
		t.Errorf("clustering_key = %+v, want %+v", d.ClusteringKey, want)
	}
	b := mustJSON(t, d)
	if !strings.Contains(string(b), `"clustering_key": [`) || !strings.Contains(string(b), `"desc": true`) {
		t.Errorf("JSON lacks the clustering key:\n%s", b)
	}
	o, err := e.ts.Describe(ctx, DescribeInput{Connection: "dev", Namespace: "main", Collection: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if b := mustJSON(t, o); strings.Contains(string(b), "partition_key") || strings.Contains(string(b), "clustering_key") {
		t.Errorf("orders carries wide-column keys:\n%s", b)
	}
}
