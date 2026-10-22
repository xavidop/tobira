package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
)

// Events set on the turn's context with WithEvents win over
// ToolDeps.Events, so one Toolset (a panel session) serves concurrent turns
// that each report to their own callbacks.
func TestWithEventsOverridesDeps(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAsk))
	var deps, mine []string
	e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		deps = append(deps, "consent")
		return assistant.ConsentNo, nil
	}})
	turn := WithEvents(ctx, assistant.Events{
		ToolCall: func(tc assistant.ToolCall) { mine = append(mine, "call:"+tc.Name) },
		ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
			mine = append(mine, "consent")
			return assistant.ConsentOnce, nil
		},
	})
	q, err := e.ts.Query(turn, QueryInput{Connection: "dev", Query: "select * from orders"})
	if err != nil || q.Rows == nil {
		t.Fatalf("query = %+v, %v", q, err)
	}
	if len(deps) != 0 || len(e.toolCalls()) != 0 {
		t.Fatalf("ToolDeps.Events used: %v %v", deps, e.toolCalls())
	}
	if strings.Join(mine, ",") != "consent,call:query" {
		t.Fatalf("turn events = %v", mine)
	}
}

// ResultRows applies the row policy and the tool encoding to rows the
// user already has (a result tab): rows are redacted and cut, capped at
// MaxRows, and withheld without consent.
func TestResultRows(t *testing.T) {
	cols := []driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "secret", NativeType: "text", Type: driver.TypeString}}
	rows := []driver.Row{
		{driver.Int(1), driver.String("pw=" + dsnPassword)},
		{driver.Int(2), driver.String(strings.Repeat("x", 300))},
		{driver.Int(3), driver.Null()},
	}
	t.Run("always", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAlways), withMaxRows(2))
		out, err := e.ts.ResultRows(ctx, "dev", "select * from secrets", cols, rows, false)
		if err != nil {
			t.Fatal(err)
		}
		b := mustJSON(t, out)
		noSecret(t, "result rows", b)
		if !out.Truncated || len(out.Columns) != 2 || out.Columns[1].Type != "text" || !strings.Contains(string(out.Rows), "…") {
			t.Fatalf("always: %s", b)
		}
		var got [][]any
		if err := json.Unmarshal(out.Rows, &got); err != nil || len(got) != 2 {
			t.Fatalf("rows not capped at MaxRows (%v): %s", err, out.Rows)
		}
		if len(e.toolCalls()) != 0 {
			t.Fatalf("ResultRows is not a tool call: %v", e.toolCalls())
		}
	})
	t.Run("ask declined", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAsk))
		var asked int
		turn := WithEvents(ctx, assistant.Events{ConsentRows: func(_ context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
			asked = req.Rows
			return assistant.ConsentNo, nil
		}})
		out, err := e.ts.ResultRows(turn, "dev", "select * from secrets", cols, rows, true)
		if err != nil {
			t.Fatal(err)
		}
		if asked != 3 || out.Rows != nil || out.RowsWithheld != 3 || !strings.Contains(out.Note, "declined") || !out.Truncated {
			t.Fatalf("declined: asked %d, %+v", asked, out)
		}
	})
	t.Run("never", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsNever))
		out, _ := e.ts.ResultRows(ctx, "dev", "select * from secrets", cols, rows, false)
		if out.Rows != nil || out.RowsWithheld != 3 || len(out.Columns) != 2 {
			t.Fatalf("never: %+v", out)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAsk))
		c, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.ts.ResultRows(c, "dev", "select * from secrets", cols, rows, false); err == nil {
			t.Fatal("a cancelled turn must be an error")
		}
	})
}

// ErrorText sends a statement error the user already has (fix_error) as the
// row policy allows: the full redacted text, or the scrubbed form a tool
// error would get. Under ask it asks for one row's consent.
func TestErrorText(t *testing.T) {
	de := &driver.Error{Kind: driver.ErrConstraint, Driver: "postgres", Op: "query", Code: "23505",
		Err: errors.New("Key (email)=(alice@example.com) already exists; login " + dsnPassword)}
	const scrubbed = "constraint violation: query failed (SQLSTATE 23505)"
	plain := `duplicate key value "alice@example.com" for 12345678`
	consent := func(c assistant.Consent, asked *int) context.Context {
		return WithEvents(ctx, assistant.Events{ConsentRows: func(_ context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
			*asked = req.Rows
			return c, nil
		}})
	}
	cases := []struct {
		name    string
		policy  RowPolicy
		consent *assistant.Consent // nil: no callback
		err     error
		want    string // exact, or "full" for the redacted full text
		asks    int
	}{
		{"always", RowsAlways, nil, de, "full", 0},
		{"never", RowsNever, nil, de, scrubbed, 0},
		{"ask granted", RowsAsk, ptr(assistant.ConsentOnce), de, "full", 1},
		{"ask declined", RowsAsk, ptr(assistant.ConsentNo), de, scrubbed, 1},
		{"ask without callback", RowsAsk, nil, de, scrubbed, 0},
		{"never, plain text", RowsNever, nil, nil, "duplicate key value …", 0},
		{"ask declined, plain text", RowsAsk, ptr(assistant.ConsentNo), nil, "duplicate key value …", 1},
		{"always, plain text", RowsAlways, nil, nil, plain, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, withPolicy(c.policy))
			asked := 0
			turn := ctx
			if c.consent != nil {
				turn = consent(*c.consent, &asked)
			}
			got, err := e.ts.ErrorText(turn, "dev", c.err, plain)
			if err != nil {
				t.Fatal(err)
			}
			noSecret(t, c.name, []byte(got))
			want := c.want
			if want == "full" {
				want = "Key (email)=(alice@example.com) already exists; login xxxxx"
				if !strings.Contains(got, want) {
					t.Fatalf("got %q, want the full redacted error", got)
				}
			} else if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if asked != c.asks {
				t.Fatalf("asked for %d rows, want %d", asked, c.asks)
			}
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAsk))
		c, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.ts.ErrorText(c, "dev", de, plain); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func ptr[T any](v T) *T { return &v }

// Every consent request names its connection and what produced the rows:
// the tool and its statement, redacted and on one line, or the error text
// for fix_error.
func TestConsentRequestSaysWhat(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAsk))
	deps := e.ts.deps
	deps.Redact = func(s string) string { return strings.ReplaceAll(s, "orders", "[masked]") }
	e.ts = NewToolset(deps)
	var got []assistant.ConsentRequest
	turn := WithEvents(ctx, assistant.Events{ConsentRows: func(_ context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
		got = append(got, req)
		return assistant.ConsentNo, nil
	}})
	_, _ = e.ts.Query(turn, QueryInput{Connection: "dev", Query: "select *\n  from orders"})
	_, _ = e.ts.Sample(turn, SampleInput{Connection: "dev", Namespace: "main", Collection: "orders"})
	_, _ = e.ts.Explain(turn, ExplainInput{Connection: "dev", Query: "select * from orders"})
	_, _ = e.ts.ResultRows(turn, "dev", "select * from orders", []driver.Column{{Name: "id"}}, []driver.Row{{driver.Int(1)}}, false)
	_, _ = e.ts.ErrorText(turn, "dev", errors.New("boom"), "boom")
	want := []assistant.ConsentRequest{
		{Conn: "dev", Rows: 2, What: "query: select * from [masked]"},
		{Conn: "dev", Rows: 2, What: "sample: main.[masked]"},
		{Conn: "dev", Rows: 1, What: "explain: select * from [masked]"},
		{Conn: "dev", Rows: 1, What: "explain_results: select * from [masked]"},
		{Conn: "dev", Rows: 1, What: "the error text of your failed query"},
	}
	if len(got) != len(want) {
		t.Fatalf("consent requests = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A turn bound to one connection fills an empty connection argument with
// it, so the model need not call list_connections first; a turn bound to
// none, or to several, still refuses the empty name.
func TestEmptyConnectionMeansTheScopedOne(t *testing.T) {
	e := newEnv(t)
	o, _ := e.ts.ListNamespaces(WithConnections(ctx, "dev"), ListNamespacesInput{})
	if o.Error != "" || len(o.Namespaces) == 0 {
		t.Fatalf("scoped list_namespaces without a connection = %+v", o)
	}
	q, _ := e.ts.Query(WithConnections(ctx, "dev"), QueryInput{Query: "select * from orders"})
	if q.Error != "" {
		t.Fatalf("scoped query without a connection: %q", q.Error)
	}
	for name, turn := range map[string]context.Context{"unscoped": ctx, "two": WithConnections(ctx, "dev", "dev2")} {
		if o, _ := e.ts.ListNamespaces(turn, ListNamespacesInput{}); !strings.Contains(o.Error, "connection is required") {
			t.Errorf("%s turn without a connection = %q", name, o.Error)
		}
	}
}

// A turn bound to a connection (WithConnections, ToolDeps.Connections) can
// name no other: every tool refuses it with the text of a name that is not
// configured, list_connections leaves it out, execute never asks about it,
// and no policy (always, or a consent of yes) lets its rows out.
func TestToolsScopedToConnections(t *testing.T) {
	unknown := func(name string) string {
		return `unknown connection "` + name + `": call list_connections for the names`
	}
	for _, policy := range []RowPolicy{RowsAlways, RowsAsk} {
		t.Run(string(policy), func(t *testing.T) {
			e := newEnv(t, withPolicy(policy))
			asked, confirmed := 0, 0
			turn := WithConnections(WithExecute(ctx), "dev")
			turn = WithEvents(turn, assistant.Events{
				ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
					asked++
					return assistant.ConsentSession, nil
				},
				ConfirmExecute: func(context.Context, string, string) (bool, error) { confirmed++; return true, nil },
			})
			conns, _ := e.ts.ListConnections(turn)
			if len(conns) != 1 || conns[0].Name != "dev" {
				t.Fatalf("list_connections = %+v, want dev only", conns)
			}
			before := len(e.fake.Calls())
			errs := map[string]string{}
			q, _ := e.ts.Query(turn, QueryInput{Connection: "dev2", Query: "select * from orders"})
			errs["query"] = q.Error
			s, _ := e.ts.Sample(turn, SampleInput{Connection: "dev2", Collection: "orders"})
			errs["sample"] = s.Error
			x, _ := e.ts.Explain(turn, ExplainInput{Connection: "dev2", Query: "select * from orders"})
			errs["explain"] = x.Error
			d, _ := e.ts.Describe(turn, DescribeInput{Connection: "dev2", Collection: "orders"})
			errs["describe"] = d.Error
			ns, _ := e.ts.ListNamespaces(turn, ListNamespacesInput{Connection: "dev2"})
			errs["list_namespaces"] = ns.Error
			cs, _ := e.ts.ListCollections(turn, ListCollectionsInput{Connection: "dev2"})
			errs["list_collections"] = cs.Error
			ex, _ := e.ts.Exec(turn, ExecuteInput{Connection: "dev2", Statement: "delete from orders"})
			errs["execute"] = ex.Error
			ro, _ := e.ts.Query(turn, QueryInput{Connection: "ro", Query: "select * from orders"})
			errs["query ro"] = ro.Error
			for tool, msg := range errs {
				want := unknown("dev2")
				if tool == "query ro" {
					want = unknown("ro")
				}
				if msg != want {
					t.Errorf("%s on another connection: %q, want %q", tool, msg, want)
				}
			}
			if nope, _ := e.ts.Query(turn, QueryInput{Connection: "nope", Query: "select 1"}); nope.Error != unknown("nope") {
				t.Errorf("a name that is not configured: %q", nope.Error)
			}
			if q.Rows != nil || s.Rows != nil || x.Plan != "" || asked != 0 || confirmed != 0 {
				t.Fatalf("another connection's data or prompts: rows %s %s plan %q, asked %d, confirmed %d", q.Rows, s.Rows, x.Plan, asked, confirmed)
			}
			if got := e.fake.Calls()[before:]; len(got) != 0 {
				t.Fatalf("the driver was reached: %+v", got)
			}
			for _, tc := range e.toolCalls() {
				if tc.Conn != "" {
					t.Errorf("a refused call counts as touching %q: %+v", tc.Conn, tc)
				}
			}
			// The bound connection still works, and its calls name it.
			ok, _ := e.ts.Query(turn, QueryInput{Connection: "dev", Query: "select * from orders"})
			if ok.Rows == nil {
				t.Fatalf("dev: %+v", ok)
			}
		})
	}
	t.Run("deps allow set", func(t *testing.T) {
		e := newEnv(t)
		deps := e.ts.deps
		deps.Connections = []string{"dev"}
		ts := NewToolset(deps)
		conns, _ := ts.ListConnections(ctx)
		if len(conns) != 1 || conns[0].Name != "dev" {
			t.Fatalf("list_connections = %+v", conns)
		}
		if q, _ := ts.Query(ctx, QueryInput{Connection: "dev2", Query: "select * from orders"}); q.Error != unknown("dev2") {
			t.Fatalf("dev2: %q", q.Error)
		}
		// A turn narrows the allow set and never widens it.
		if q, _ := ts.Query(WithConnections(ctx, "dev2"), QueryInput{Connection: "dev2", Query: "select * from orders"}); q.Error != unknown("dev2") {
			t.Fatalf("WithConnections widened the allow set: %q", q.Error)
		}
		if q, _ := ts.Query(WithConnections(WithConnections(ctx, "dev"), "dev2"), QueryInput{Connection: "dev", Query: "select * from orders"}); q.Error != unknown("dev") {
			t.Fatalf("a narrower turn kept dev: %q", q.Error)
		}
	})
}

// A tool call reports the connection it used; list_connections none.
func TestToolCallNamesItsConnection(t *testing.T) {
	var calls []assistant.ToolCall
	e := newEnv(t)
	turn := WithEvents(ctx, assistant.Events{ToolCall: func(tc assistant.ToolCall) { calls = append(calls, tc) }})
	_, _ = e.ts.ListConnections(turn)
	_, _ = e.ts.Query(turn, QueryInput{Connection: "dev2", Query: "select * from orders"})
	if len(calls) != 2 || calls[0].Conn != "" || calls[1].Conn != "dev2" {
		t.Fatalf("calls = %+v", calls)
	}
}

// A new turn never joins the consent prompt of a turn that was superseded
// (cancelled while its prompt still unwinds): it asks for itself.
func TestNewTurnDoesNotJoinASupersededPrompt(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAsk))
	opened, release := make(chan struct{}), make(chan struct{})
	old, cancel := context.WithCancel(ctx)
	oldTurn := WithEvents(old, assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		close(opened)
		<-release // a prompt that is slow to unwind after its turn is cancelled
		return assistant.ConsentNo, context.Canceled
	}})
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		_, _ = e.ts.Query(oldTurn, QueryInput{Connection: "dev", Query: "select * from orders"})
	}()
	<-opened
	cancel()
	asked := make(chan struct{}, 1)
	newTurn := WithEvents(ctx, assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		asked <- struct{}{}
		return assistant.ConsentOnce, nil
	}})
	got := make(chan RowsOutput, 1)
	go func() {
		q, _ := e.ts.Query(newTurn, QueryInput{Connection: "dev", Query: "select * from orders"})
		got <- q
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		close(release)
		<-oldDone
		t.Fatalf("the new turn joined the superseded turn's prompt: %+v", <-got)
	}
	if q := <-got; q.Rows == nil {
		t.Fatalf("new turn: rows withheld: %+v", q)
	}
	close(release)
	<-oldDone
}
