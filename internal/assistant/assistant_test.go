package assistant_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xavidop/tobira/internal/assistant"
)

func TestErrorsAreDistinctAndWrap(t *testing.T) {
	all := []error{assistant.ErrNotConfigured, assistant.ErrAuth, assistant.ErrProvider, assistant.ErrRateLimit, assistant.ErrUnavailable, assistant.ErrScope}
	for i, a := range all {
		for j, b := range all {
			if (i == j) != errors.Is(a, b) {
				t.Fatalf("errors.Is(%v, %v) = %v", a, b, errors.Is(a, b))
			}
		}
		if w := fmt.Errorf("context: %w", a); !errors.Is(w, a) {
			t.Fatalf("%v does not survive %%w wrapping", a)
		}
	}
	if assistant.ErrNotConfigured.Error() != "ai is not configured" {
		t.Fatalf("ErrNotConfigured = %q", assistant.ErrNotConfigured)
	}
}

func TestEventsNilCallbacksAreSafe(t *testing.T) {
	var ev assistant.Events
	ev.OnToolCall(assistant.ToolCall{Name: "query", Rows: -1})
	ev.OnUsage(assistant.Usage{Model: "m"})
	c, err := ev.AskConsent(context.Background(), assistant.ConsentRequest{Conn: "pg", Rows: 10})
	if err != nil || c != assistant.ConsentNo {
		t.Fatalf("AskConsent with no callback = %v, %v; want ConsentNo, nil", c, err)
	}
}

func TestEventsForwardToCallbacks(t *testing.T) {
	var gotTool assistant.ToolCall
	var gotUsage assistant.Usage
	var gotConn string
	var gotRows int
	ev := assistant.Events{
		ToolCall: func(tc assistant.ToolCall) { gotTool = tc },
		Usage:    func(u assistant.Usage) { gotUsage = u },
		ConsentRows: func(_ context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
			gotConn, gotRows = req.Conn, req.Rows
			return assistant.ConsentSession, nil
		},
	}
	ev.OnToolCall(assistant.ToolCall{Name: "describe", Rows: -1})
	ev.OnUsage(assistant.Usage{Model: "googleai/gemini-3.8-flash", InputTokens: 3})
	c, err := ev.AskConsent(context.Background(), assistant.ConsentRequest{Conn: "pg", Rows: 7})
	if err != nil || c != assistant.ConsentSession {
		t.Fatalf("AskConsent = %v, %v", c, err)
	}
	if gotTool.Name != "describe" || gotUsage.InputTokens != 3 || gotConn != "pg" || gotRows != 7 {
		t.Fatalf("callbacks got %+v %+v %q %d", gotTool, gotUsage, gotConn, gotRows)
	}
}

// A cancelled context wins over the callback: no consent is asked for.
func TestAskConsentHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	ev := assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		called = true
		return assistant.ConsentOnce, nil
	}}
	c, err := ev.AskConsent(ctx, assistant.ConsentRequest{Conn: "pg", Rows: 1})
	if !errors.Is(err, context.Canceled) || c != assistant.ConsentNo || called {
		t.Fatalf("AskConsent on cancelled ctx = %v, %v (called %v)", c, err, called)
	}
}

// ConfirmExec refuses without a callback and on a cancelled context, and
// forwards otherwise.
func TestConfirmExec(t *testing.T) {
	ok, err := assistant.Events{}.ConfirmExec(context.Background(), "pg", "delete from t")
	if ok || err != nil {
		t.Fatalf("ConfirmExec with no callback = %v, %v; want false, nil", ok, err)
	}
	var gotConn, gotStmt string
	ev := assistant.Events{ConfirmExecute: func(_ context.Context, conn, stmt string) (bool, error) {
		gotConn, gotStmt = conn, stmt
		return true, nil
	}}
	ok, err = ev.ConfirmExec(context.Background(), "pg", "delete from t")
	if !ok || err != nil || gotConn != "pg" || gotStmt != "delete from t" {
		t.Fatalf("ConfirmExec = %v, %v (got %q %q)", ok, err, gotConn, gotStmt)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gotConn = ""
	ok, err = ev.ConfirmExec(ctx, "pg", "x")
	if ok || !errors.Is(err, context.Canceled) || gotConn != "" {
		t.Fatalf("ConfirmExec on cancelled ctx = %v, %v (asked %v)", ok, err, gotConn != "")
	}
}

// A TurnError carries the usage a failed turn spent and unwraps to its
// cause, so sentinel checks keep working.
func TestTurnError(t *testing.T) {
	u := assistant.Usage{Model: "p/m", InputTokens: 10, OutputTokens: 2}
	var err error = &assistant.TurnError{Err: fmt.Errorf("%w: bad answer", assistant.ErrProvider), Usage: u}
	if !errors.Is(err, assistant.ErrProvider) || err.Error() != "ai provider error: bad answer" {
		t.Fatalf("err = %v", err)
	}
	var te *assistant.TurnError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &te) || te.Usage != u {
		t.Fatalf("usage not recoverable: %+v", te)
	}
}

// ConsentWhat is one escaped line of at most MaxConsentWhatRunes runes.
func TestConsentWhat(t *testing.T) {
	if got := assistant.ConsentWhat("query", "select *\n\tfrom t"); got != "query: select * from t" {
		t.Fatalf("got %q", got)
	}
	if got := assistant.ConsentWhat("sample", ""); got != "sample" {
		t.Fatalf("got %q", got)
	}
	got := assistant.ConsentWhat("query", "select '\x1b[31m\u202e' from t")
	if strings.ContainsAny(got, "\x1b\u202e") || !strings.Contains(got, `\u{001B}`) || !strings.Contains(got, `\u{202E}`) {
		t.Fatalf("controls not escaped: %q", got)
	}
	long := assistant.ConsentWhat("query", strings.Repeat("é", 500))
	if n := utf8.RuneCountInString(long); n != assistant.MaxConsentWhatRunes || !strings.HasSuffix(long, "…") {
		t.Fatalf("cut to %d runes: %q", n, long)
	}
}

func TestRowsText(t *testing.T) {
	for n, want := range map[int]string{0: "0 rows", 1: "1 row", 2: "2 rows"} {
		if got := assistant.RowsText(n); got != want {
			t.Errorf("RowsText(%d) = %q, want %q", n, got, want)
		}
	}
}
