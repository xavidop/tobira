package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
)

// A driver error can carry row values ("invalid input syntax: "<value>"").
// Unless rows may be sent for this call, it is scrubbed before it reaches the
// model; the kind and the rest of the message stay.
func TestDriverErrorsFollowTheRowPolicy(t *testing.T) {
	leak := func() error {
		return driver.Wrap(driver.ErrSyntax, "fake", "query", errors.New(`invalid input syntax for type integer: "secret-row-value" at row 98765`))
	}
	scrubbed := func(t *testing.T, what, msg string) {
		t.Helper()
		if msg != "invalid query: syntax error" {
			t.Errorf("%s: error = %q, want it reduced to kind", what, msg)
		}
	}
	full := func(t *testing.T, what, msg string) {
		t.Helper()
		if !strings.Contains(msg, "secret-row-value") {
			t.Errorf("%s: error = %q, want the full redacted error", what, msg)
		}
	}
	t.Run("never", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsNever))
		for _, op := range []string{"query", "browse", "explain"} {
			e.fake.FailNext(op, leak())
		}
		q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
		scrubbed(t, "query", q.Error)
		s, _ := e.ts.Sample(ctx, SampleInput{Connection: "dev", Collection: "orders"})
		scrubbed(t, "sample", s.Error)
		x, _ := e.ts.Explain(ctx, ExplainInput{Connection: "dev", Query: "select * from orders"})
		scrubbed(t, "explain", x.Error)
		e.setEvents(assistant.Events{ConfirmExecute: func(context.Context, string, string) (bool, error) { return true, nil }})
		e.fake.FailNext("query", leak())
		xe, _ := e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "dev", Statement: "delete from orders"})
		scrubbed(t, "execute", xe.Error)
		for _, tc := range e.toolCalls() {
			if strings.Contains(tc.Err, "secret-row-value") {
				t.Errorf("ToolCall event leaks: %+v", tc)
			}
		}
		// Errors that are not driver errors keep their quoted names.
		u, _ := e.ts.Query(ctx, QueryInput{Connection: "nope", Query: "select 1"})
		if !strings.Contains(u.Error, `"nope"`) {
			t.Errorf("unknown connection error was scrubbed: %q", u.Error)
		}
	})
	t.Run("ask without consent does not prompt", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAsk))
		asked := 0
		e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
			asked++
			return assistant.ConsentOnce, nil
		}})
		e.fake.FailNext("query", leak())
		q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
		scrubbed(t, "ask", q.Error)
		if asked != 0 {
			t.Fatalf("an error opened a consent prompt (%d)", asked)
		}
	})
	t.Run("ask with session consent", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAsk))
		e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
			return assistant.ConsentSession, nil
		}})
		_, _ = e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
		e.fake.FailNext("query", leak())
		q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
		full(t, "session consent", q.Error)
	})
	t.Run("always", func(t *testing.T) {
		e := newEnv(t, withPolicy(RowsAlways))
		e.fake.FailNext("query", leak())
		q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
		full(t, "always", q.Error)
	})
}

// Redaction runs before the cut: a password that straddles the cut point is
// masked, never left as a prefix.
func TestRedactBeforeTruncate(t *testing.T) {
	e := newEnv(t)
	for _, at := range []int{MaxValueRunes - 3, MaxValueRunes - 1} {
		long := strings.Repeat("c", at) + dsnPassword + strings.Repeat("d", 40)
		e.fake.OnDescribe("main", "orders", func(s *driver.CollectionSchema) {
			s.Comment = long
			s.Fields[2].Default = long
		})
		d, _ := e.ts.Describe(ctx, DescribeInput{Connection: "dev", Collection: "orders"})
		if d.Error != "" {
			t.Fatal(d.Error)
		}
		for _, s := range []string{d.Comment, d.Fields[2].Default} {
			if strings.Contains(s, dsnPassword[:4]) {
				t.Errorf("cut at %d leaves a password prefix: %q", at, s[len(s)-20:])
			}
		}
		e.fake.AddCollection("main", "long", []driver.Column{{Name: "v", Type: driver.TypeString}}, []driver.Row{{driver.String(long)}})
		q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from long"})
		if strings.Contains(string(q.Rows), dsnPassword[:4]) {
			t.Errorf("value cut at %d leaves a password prefix: %s", at, q.Rows)
		}
	}
}

func TestNamespaceLabelIsRedacted(t *testing.T) {
	e := newEnv(t)
	deps := e.ts.deps
	deps.Redact = func(s string) string { return strings.ReplaceAll(s, "namespace", "[label]") }
	ns, _ := NewToolset(deps).ListNamespaces(ctx, ListNamespacesInput{Connection: "dev"})
	if len(ns.Namespaces) != 1 || ns.Namespaces[0].Label != "[label]" {
		t.Fatalf("label not redacted: %+v", ns)
	}
}

// The read-only switch flipped while the confirm dialog is open wins.
func TestExecuteRechecksReadOnlyAfterConfirm(t *testing.T) {
	e := newEnv(t)
	e.setEvents(assistant.Events{ConfirmExecute: func(context.Context, string, string) (bool, error) {
		if err := e.mgr.SetReadOnly("dev", true); err != nil {
			t.Error(err)
		}
		return true, nil
	}})
	out, _ := e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "dev", Statement: "delete from orders where id = 1"})
	if !strings.Contains(out.Error, "read-only") {
		t.Fatalf("execute after a read-only flip: %+v", out)
	}
	for _, op := range e.fakeOps() {
		if op == "query" {
			t.Fatal("the statement ran")
		}
	}
}

// Concurrent calls on one connection share one consent prompt.
func TestConsentSingleFlight(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAsk))
	var prompts atomic.Int32
	release := make(chan struct{})
	e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		prompts.Add(1)
		<-release
		return assistant.ConsentOnce, nil
	}})
	var wg sync.WaitGroup
	outs := make([]RowsOutput, 2)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], _ = e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.ts.consentWaiters("dev") < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if prompts.Load() != 1 {
		t.Fatalf("prompts = %d, want 1", prompts.Load())
	}
	for i, o := range outs {
		if o.Rows == nil {
			t.Errorf("call %d did not get the shared answer: %+v", i, o)
		}
	}
}

// Session consent belongs to the connection's configuration: a reload that
// repoints the name asks again, an unchanged one does not.
func TestSessionConsentFollowsTheConfig(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAsk))
	asked := 0
	e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		asked++
		return assistant.ConsentSession, nil
	}})
	q := func() { _, _ = e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"}) }
	conns := []config.Connection{{Name: "dev", Driver: "fake", DSN: "fake://admin:" + dsnPassword + "@db.local/app"}}
	q()
	e.mgr.Reload(&config.Config{Connections: conns})
	q()
	if asked != 1 {
		t.Fatalf("an unchanged reload asked again (%d)", asked)
	}
	conns[0].DSN = "fake://admin:" + dsnPassword + "@other.local/app"
	e.mgr.Reload(&config.Config{Connections: conns})
	q()
	if asked != 2 {
		t.Fatalf("a repointed connection kept its consent (%d)", asked)
	}
}

func TestAskErrorWithholds(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAsk))
	e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		return assistant.ConsentOnce, errors.New("the panel went away")
	}})
	q, err := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	if err != nil || q.Rows != nil || q.RowsWithheld != 2 || q.Note != noteNoAnswer {
		t.Fatalf("ask error: %+v %v", q, err)
	}
}

func TestAskOnExplain(t *testing.T) {
	for _, tc := range []struct {
		answer assistant.Consent
		sent   bool
	}{{assistant.ConsentOnce, true}, {assistant.ConsentNo, false}} {
		e := newEnv(t, withPolicy(RowsAsk))
		var gotRows int
		e.setEvents(assistant.Events{ConsentRows: func(_ context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
			gotRows = req.Rows
			return tc.answer, nil
		}})
		x, _ := e.ts.Explain(ctx, ExplainInput{Connection: "dev", Query: "select * from orders"})
		if gotRows != 1 || (x.Plan != "") != tc.sent || (x.RowsWithheld == 1) == tc.sent {
			t.Errorf("explain with %v: %+v (asked for %d rows)", tc.answer, x, gotRows)
		}
	}
}

func TestRowPolicyOnExecute(t *testing.T) {
	e := newEnv(t, withPolicy(RowsNever))
	e.setEvents(assistant.Events{ConfirmExecute: func(context.Context, string, string) (bool, error) { return true, nil }})
	out, _ := e.ts.Exec(WithExecute(ctx), ExecuteInput{Connection: "dev", Statement: "select * from orders"})
	if out.Error != "" || out.Rows != nil || out.RowsWithheld != 2 || out.Note != noteNever {
		t.Fatalf("execute under never: %+v", out)
	}
}
