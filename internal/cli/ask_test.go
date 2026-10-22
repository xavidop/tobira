package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/assistant/assistanttest"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

const oneConn = "connections:\n  - name: main\n    driver: fake\n    dsn: fake://x\n"

// askHarness is a harness with one connection, an ai: section and the fake
// assistant wired as the factory.
func askHarness(t *testing.T, aiSection string) (*harness, *assistanttest.Fake) {
	t.Helper()
	h := newHarness(t)
	if aiSection == "" {
		aiSection = "ai:\n  provider: ollama\n"
	}
	if err := os.WriteFile(h.cfg, []byte(oneConn+aiSection), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &assistanttest.Fake{ModelName: "fake/m"}
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) { return fake, nil }
	return h, fake
}

func scriptAsk(f *assistanttest.Fake, r assistant.AskResult) {
	f.AskFn = func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
		return r, nil
	}
}

var usage120 = assistant.Usage{Model: "fake/m", InputTokens: 1234, OutputTokens: 340, Latency: 2100 * time.Millisecond}

func TestAskTextPrintsQueryExplanationFooter(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: "select count(*) from orders", Explanation: "Counts the orders.", Dialect: "sqlite", Usage: usage120})
	out, errOut, code := h.exec(t, context.Background(), "ask", "how many orders?")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	want := "```sqlite\nselect count(*) from orders\n```\n\nCounts the orders.\n\nfake/m · 1.2k in / 340 out · 2.1s\n"
	if out != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", out, want)
	}
	calls := f.Calls()
	if len(calls) != 1 || calls[0].Method != "Ask" {
		t.Fatalf("calls = %+v", calls)
	}
	req := calls[0].Req.(assistant.AskRequest)
	if req.Question != "how many orders?" || req.Scope != (assistant.Scope{Conn: "main"}) {
		t.Errorf("request = %+v", req)
	}
}

func TestAskJSON(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: "select 1", Explanation: "One.", Dialect: "sqlite", Usage: usage120})
	out, _, code := h.exec(t, context.Background(), "ask", "q", "--format", "json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	want := map[string]any{
		"mode": "ask", "query": "select 1", "explanation": "One.",
		"usage": map[string]any{"model": "fake/m", "input_tokens": 1234., "output_tokens": 340., "latency_ms": 2100.},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("json = %v\nwant %v", got, want)
	}
}

func scriptAnswer(f *assistanttest.Fake, r assistant.AnswerResult) {
	f.AnswerFn = func(context.Context, assistant.AnswerRequest, assistant.Events) (assistant.AnswerResult, error) {
		return r, nil
	}
}

var answered = assistant.AnswerResult{
	Markdown: "There are **3** orders.",
	Ran:      []assistant.RanQuery{{Conn: "main", Query: "select count(*) from orders", Rows: 1}, {Conn: "main", Query: "select bad", Err: "no such column"}},
	Usage:    usage120,
}

func TestAnswerText(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAnswer(f, answered)
	out, errOut, code := h.exec(t, context.Background(), "ask", "--answer", "how many?")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	want := "There are **3** orders.\n\nQueries run:\n  select count(*) from orders (1 row)\n  select bad (error: no such column)\n\nfake/m · 1.2k in / 340 out · 2.1s\n"
	if out != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", out, want)
	}
	req := f.Calls()[0].Req.(assistant.AnswerRequest)
	if req.AllowExecute {
		t.Error("the CLI must never offer execute")
	}
}

func TestAnswerJSON(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAnswer(f, answered)
	out, _, code := h.exec(t, context.Background(), "ask", "--answer", "q", "--format", "json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var got struct {
		Mode     string `json:"mode"`
		Markdown string `json:"markdown"`
		Ran      []struct {
			Conn, Query string
			Rows        int
			Err         string `json:"error"`
		} `json:"ran"`
		Usage struct {
			Model     string `json:"model"`
			In        int    `json:"input_tokens"`
			Out       int    `json:"output_tokens"`
			LatencyMS int    `json:"latency_ms"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Mode != "answer" || got.Markdown != answered.Markdown || len(got.Ran) != 2 || got.Ran[1].Err != "no such column" ||
		got.Usage.Model != "fake/m" || got.Usage.In != 1234 || got.Usage.Out != 340 || got.Usage.LatencyMS != 2100 {
		t.Errorf("json = %+v", got)
	}
	if strings.Contains(out, `"explanation"`) {
		t.Errorf("answer json carries ask fields: %s", out)
	}
}

// Model output is untrusted: control sequences are escaped, newlines kept.
func TestAskEscapesModelOutput(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: "select 1\x1b[2J", Explanation: "line one\nline \x1b]0;pwn\x07two", Usage: usage120})
	out, _, code := h.exec(t, context.Background(), "ask", "q")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("raw control bytes in %q", out)
	}
	if !strings.Contains(out, `select 1\x1b[2J`) || !strings.Contains(out, "line one\nline ") {
		t.Errorf("escaping lost text or newlines: %q", out)
	}
	scriptAnswer(f, assistant.AnswerResult{Markdown: "a\x1b[31mred\nb", Ran: []assistant.RanQuery{{Query: "q\x1b[0m", Err: "e\x07"}}, Usage: usage120})
	out, _, _ = h.exec(t, context.Background(), "ask", "--answer", "q")
	if strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, "red\nb") {
		t.Errorf("answer output = %q", out)
	}
}

func TestAskStdinQuestion(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
	h.in = "what is\nin stdin?\n"
	if _, _, code := h.exec(t, context.Background(), "ask", "-"); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if q := f.Calls()[0].Req.(assistant.AskRequest).Question; q != "what is\nin stdin?" {
		t.Errorf("question = %q", q)
	}
}

func TestAskUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		args []string
		want string
	}{
		{"no question", "", []string{"ask"}, ""},
		{"empty", "", []string{"ask", "  "}, "question"},
		{"empty stdin", "\n", []string{"ask", "-"}, "question"},
		{"three arguments", "", []string{"ask", "a", "b", "c"}, "1 or 2 arguments"},
		{"positional connection and --conn", "", []string{"ask", "main", "q", "--conn", "main"}, "not both"},
		{"--run with --answer", "", []string{"ask", "q", "--run", "--answer"}, "--answer"},
		{"--limit without --run", "", []string{"ask", "q", "--limit", "5"}, "--run"},
		{"--timeout without --run", "", []string{"ask", "q", "--timeout", "5s"}, "--run"},
		{"--run with a text format", "", []string{"ask", "q", "--run", "--format", "text"}, "text"},
		{"--run with a negative limit", "", []string{"ask", "q", "--run", "--limit", "-1"}, "--limit"},
		{"bad format", "", []string{"ask", "q", "--format", "xml"}, "xml"},
		{"bad collection", "", []string{"ask", "q", "--collection", "a.b.c"}, "collection"},
		{"unterminated quote", "", []string{"ask", "q", "--collection", `"a.b`}, "collection"},
		{"unknown flag", "", []string{"ask", "q", "--allow-execute"}, "allow-execute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, f := askHarness(t, "")
			h.in = c.in
			_, errOut, code := h.exec(t, context.Background(), c.args...)
			if code != 1 || !strings.Contains(errOut, c.want) || !strings.HasPrefix(errOut, "error: ") {
				t.Errorf("code=%d stderr=%q, want 1 mentioning %q", code, errOut, c.want)
			}
			if len(f.Calls()) != 0 {
				t.Errorf("the assistant was called: %+v", f.Calls())
			}
		})
	}
}

func TestAskCollectionScope(t *testing.T) {
	cases := []struct {
		arg  string
		want assistant.Scope
	}{
		{"main.orders", assistant.Scope{Conn: "main", Namespace: "main", Collection: "orders"}},
		{"sales.orders", assistant.Scope{Conn: "main", Namespace: "sales", Collection: "orders"}},
		{"orders", assistant.Scope{Conn: "main", Namespace: "main", Collection: "orders"}}, // the fake's default namespace is "main"
		{`"a.b"."c.d"`, assistant.Scope{Conn: "main", Namespace: "a.b", Collection: "c.d"}},
		{`sales."odd.name"`, assistant.Scope{Conn: "main", Namespace: "sales", Collection: "odd.name"}},
		{`"say ""hi"""`, assistant.Scope{Conn: "main", Namespace: "main", Collection: `say "hi"`}},
	}
	for _, c := range cases {
		t.Run(c.arg, func(t *testing.T) {
			h, f := askHarness(t, "")
			scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
			if _, errOut, code := h.exec(t, context.Background(), "ask", "q", "--collection", c.arg); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if got := f.Calls()[0].Req.(assistant.AskRequest).Scope; got != c.want {
				t.Errorf("scope = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestAskConnectionSelection(t *testing.T) {
	t.Run("none configured", func(t *testing.T) {
		h, f := askHarness(t, "")
		_ = os.WriteFile(h.cfg, []byte("ai:\n  provider: ollama\n"), 0o600)
		_, errOut, code := h.exec(t, context.Background(), "ask", "q")
		if code != 1 || !strings.Contains(errOut, "--conn") || len(f.Calls()) != 0 {
			t.Errorf("code=%d stderr=%q", code, errOut)
		}
	})
	t.Run("several configured", func(t *testing.T) {
		h, f := askHarness(t, "")
		_ = os.WriteFile(h.cfg, []byte(testConfig+"ai:\n  provider: ollama\n"), 0o600)
		_, errOut, code := h.exec(t, context.Background(), "ask", "q")
		if code != 1 || !strings.Contains(errOut, "local-pg") || !strings.Contains(errOut, "ro") || len(f.Calls()) != 0 {
			t.Errorf("code=%d stderr=%q, want exit 1 listing the names", code, errOut)
		}
	})
	t.Run("several, one chosen", func(t *testing.T) {
		h, f := askHarness(t, "")
		_ = os.WriteFile(h.cfg, []byte(testConfig+"ai:\n  provider: ollama\n"), 0o600)
		scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
		if _, errOut, code := h.exec(t, context.Background(), "ask", "q", "--conn", "ro"); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
		if got := f.Calls()[0].Req.(assistant.AskRequest).Scope.Conn; got != "ro" {
			t.Errorf("conn = %q", got)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		h, _ := askHarness(t, "")
		_, errOut, code := h.exec(t, context.Background(), "ask", "q", "--conn", "mian")
		if code != 1 || !strings.Contains(errOut, `"main"`) {
			t.Errorf("code=%d stderr=%q, want exit 1 with a suggestion", code, errOut)
		}
	})
	t.Run("dsn", func(t *testing.T) {
		h, f := askHarness(t, "")
		scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
		if _, errOut, code := h.exec(t, context.Background(), "ask", "q", "--conn", "fake://u:hunter2pw@h/db"); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
		got := f.Calls()[0].Req.(assistant.AskRequest).Scope.Conn
		if strings.Contains(got, "hunter2pw") || !strings.HasPrefix(got, "fake://") {
			t.Errorf("scope conn = %q, want the redacted DSN", got)
		}
	})
}

// consentSeen runs one answer turn that asks for row consent once.
func consentSeen(t *testing.T, aiSection string, args ...string) (assistant.Consent, int, string) {
	t.Helper()
	h, f := askHarness(t, aiSection)
	var got assistant.Consent
	f.AnswerFn = func(ctx context.Context, _ assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
		var err error
		got, err = ev.AskConsent(ctx, assistant.ConsentRequest{Conn: "main", Rows: 3})
		return assistant.AnswerResult{Markdown: "ok", Usage: usage120}, err
	}
	_, errOut, code := h.exec(t, context.Background(), append([]string{"ask", "--answer", "q"}, args...)...)
	return got, code, errOut
}

func TestAskRowConsent(t *testing.T) {
	if got, code, _ := consentSeen(t, ""); code != 0 || got != assistant.ConsentNo {
		t.Errorf("ask policy, no flag: consent=%v code=%d, want ConsentNo", got, code)
	}
	if got, code, _ := consentSeen(t, "", "--send-rows"); code != 0 || got != assistant.ConsentSession {
		t.Errorf("ask policy, --send-rows: consent=%v code=%d, want ConsentSession", got, code)
	}
	if got, code, _ := consentSeen(t, "ai:\n  provider: ollama\n  send_rows: always\n", "--send-rows"); code != 0 || got != assistant.ConsentSession {
		t.Errorf("always policy, --send-rows: consent=%v code=%d", got, code)
	}
	// Refused by policy: exit 4.
	h, f := askHarness(t, "ai:\n  provider: ollama\n  send_rows: never\n")
	_, errOut, code := h.exec(t, context.Background(), "ask", "q", "--send-rows")
	if code != 4 || !strings.Contains(errOut, "ai.send_rows is never; edit the config to allow rows") || len(f.Calls()) != 0 {
		t.Errorf("never policy: code=%d stderr=%q calls=%d", code, errOut, len(f.Calls()))
	}
	if _, code, _ := consentSeen(t, "ai:\n  provider: ollama\n  send_rows: never\n"); code != 0 {
		t.Errorf("never policy without the flag: code=%d", code)
	}
}

func TestAskExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"scope", fmt.Errorf("%w: connection main: boom", assistant.ErrScope), 1},
		{"not configured", assistant.ErrNotConfigured, 1},
		{"auth", fmt.Errorf("%w: bad key", assistant.ErrAuth), 2},
		{"unavailable", assistant.ErrUnavailable, 2},
		{"rate limit", assistant.ErrRateLimit, 2},
		{"provider", assistant.ErrProvider, 3},
		{"model error", errors.New("model said no"), 3},
		{"turn error auth", &assistant.TurnError{Err: assistant.ErrAuth, Usage: usage120}, 2},
		{"turn error plain", &assistant.TurnError{Err: errors.New("x")}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, f := askHarness(t, "")
			f.AskFn = func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
				return assistant.AskResult{}, c.err
			}
			out, errOut, code := h.exec(t, context.Background(), "ask", "q")
			if code != c.code || !strings.HasPrefix(errOut, "error: ") || out != "" {
				t.Errorf("code=%d stdout=%q stderr=%q, want %d", code, out, errOut, c.code)
			}
		})
	}
}

func TestAskNotConfigured(t *testing.T) {
	h, _ := askHarness(t, "")
	_ = os.WriteFile(h.cfg, []byte(oneConn), 0o600)
	out, errOut, code := h.exec(t, context.Background(), "ask", "q")
	want := "error: ai is not configured: add an ai: section to " + h.cfg + " (see https://xavidop.github.io/tobira/docs/ai/)\n"
	if code != 1 || out != "" || errOut != want {
		t.Errorf("code=%d stderr=%q, want %q", code, errOut, want)
	}
	// An ai: section but a binary with no assistant behaves the same.
	_ = os.WriteFile(h.cfg, []byte(oneConn+"ai:\n  provider: ollama\n"), 0o600)
	h.newAssistant = nil
	if _, errOut, code = h.exec(t, context.Background(), "ask", "q"); code != 1 || errOut != want {
		t.Errorf("nil factory: code=%d stderr=%q", code, errOut)
	}
}

func TestAskInitError(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{fmt.Errorf("%w: key rejected for postgres://u:s3cretpw@h/db", assistant.ErrAuth), 2},
		{fmt.Errorf("%w: boom", assistant.ErrProvider), 3},
		{errors.New("prompt override ask.prompt is invalid"), 1},
	}
	for _, c := range cases {
		h, _ := askHarness(t, "")
		h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) { return nil, c.err }
		out, errOut, code := h.exec(t, context.Background(), "ask", "q")
		if code != c.code || out != "" || !strings.HasPrefix(errOut, "error: ") || strings.Contains(errOut, "s3cretpw") {
			t.Errorf("err %v: code=%d stdout=%q stderr=%q, want %d, redacted", c.err, code, out, errOut, c.code)
		}
	}
}

func TestAskCancel(t *testing.T) {
	h, f := askHarness(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	f.AskFn = func(ctx context.Context, _ assistant.AskRequest, _ assistant.Events) (assistant.AskResult, error) {
		cancel()
		<-ctx.Done()
		return assistant.AskResult{}, ctx.Err()
	}
	out, errOut, code := h.exec(t, ctx, "ask", "q")
	if code != 3 || out != "" || !strings.Contains(errOut, "canceled") {
		t.Errorf("code=%d stdout=%q stderr=%q, want 3 and canceled", code, out, errOut)
	}
}

func TestAskProgress(t *testing.T) {
	script := func(f *assistanttest.Fake) {
		f.AnswerFn = func(_ context.Context, _ assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			ev.OnToolCall(assistant.ToolCall{Name: "query", Input: "main: select 1\x1b[2J", Rows: 1})
			ev.OnToolCall(assistant.ToolCall{Name: "describe", Input: "main.orders", Rows: -1})
			ev.OnToolCall(assistant.ToolCall{Name: "query", Input: "main: bad", Rows: -1, Err: "no such table"})
			return assistant.AnswerResult{Markdown: "ok", Usage: usage120}, nil
		}
	}
	t.Run("tty", func(t *testing.T) {
		h, f := askHarness(t, "")
		h.tty = true
		script(f)
		out, errOut, code := h.exec(t, context.Background(), "ask", "--answer", "q")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		for _, w := range []string{"thinking… (fake/m)", "▸ query main: select 1\\x1b[2J: 1 row", "▸ describe main.orders", "▸ query main: bad: error: no such table"} {
			if !strings.Contains(errOut, w) {
				t.Errorf("stderr missing %q:\n%q", w, errOut)
			}
		}
		if strings.Contains(errOut, "\x1b[2J") {
			t.Errorf("tool input not escaped: %q", errOut)
		}
		if !strings.Contains(out, "\x1b[2m") {
			t.Errorf("a terminal gets a dim footer: %q", out)
		}
	})
	t.Run("not a tty", func(t *testing.T) {
		h, f := askHarness(t, "")
		script(f)
		out, errOut, code := h.exec(t, context.Background(), "ask", "--answer", "q")
		if code != 0 || errOut != "" || strings.Contains(out, "\x1b") {
			t.Errorf("code=%d stderr=%q stdout=%q, want silence and plain text", code, errOut, out)
		}
	})
}

// The ask command does not record itself in the history.
func TestAskDoesNotRecordItself(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
	if _, _, code := h.exec(t, context.Background(), "ask", "q"); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out, _, _ := h.exec(t, context.Background(), "history", "--format", "json")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("history = %s", out)
	}
}

func TestAskStdinIsCapped(t *testing.T) {
	h, f := askHarness(t, "")
	h.in = strings.Repeat("x", 64*1024+1)
	_, errOut, code := h.exec(t, context.Background(), "ask", "-")
	if code != 1 || !strings.Contains(errOut, "64 KiB") || len(f.Calls()) != 0 {
		t.Errorf("code=%d stderr=%q calls=%d", code, errOut, len(f.Calls()))
	}
	scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
	h.in = strings.Repeat("x", 64*1024)
	if _, errOut, code = h.exec(t, context.Background(), "ask", "-"); code != 0 {
		t.Errorf("exactly 64 KiB: code=%d stderr=%q", code, errOut)
	}
}

func TestAskStdinTTYHint(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
	h.tty = true
	h.in = "q\n"
	_, errOut, _ := h.exec(t, context.Background(), "ask", "-")
	if !strings.Contains(errOut, "reading the question from stdin; end with Ctrl-D") {
		t.Errorf("stderr = %q", errOut)
	}
	h.tty = false
	if _, errOut, _ = h.exec(t, context.Background(), "ask", "-"); errOut != "" {
		t.Errorf("no hint off a terminal, got %q", errOut)
	}
}

// Runtime errors go through the connection's redactor too, which knows
// passwords a generic pass cannot.
func TestAskRedactsRuntimeErrors(t *testing.T) {
	h, f := askHarness(t, "")
	_ = os.WriteFile(h.cfg, []byte("connections:\n  - name: main\n    driver: fake\n    dsn: fake://u:hunter2pw@h/db\nai:\n  provider: ollama\n"), 0o600)
	f.AskFn = func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
		return assistant.AskResult{}, fmt.Errorf("%w: login failed with hunter2pw", assistant.ErrScope)
	}
	_, errOut, code := h.exec(t, context.Background(), "ask", "q")
	if code != 1 || strings.Contains(errOut, "hunter2pw") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestAskFenceOutgrowsBackticks(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: "select '```' , '`'", Dialect: "sqlite", Usage: usage120})
	out, _, _ := h.exec(t, context.Background(), "ask", "q")
	if !strings.HasPrefix(out, "````sqlite\n") || !strings.Contains(out, "\n````\n") {
		t.Errorf("fence not widened: %q", out)
	}
}

func TestAskBareCollectionOnUnreachableConnectionExits2(t *testing.T) {
	h, f := askHarness(t, "")
	_ = os.WriteFile(h.cfg, []byte("connections:\n  - name: nodrv\n    driver: nope\n    dsn: nope://x\nai:\n  provider: ollama\n"), 0o600)
	_, errOut, code := h.exec(t, context.Background(), "ask", "q", "--collection", "orders")
	if code != 2 || len(f.Calls()) != 0 {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

// The connection may be the first argument; --conn stays as an alias.
func TestAskPositionalConnection(t *testing.T) {
	h, f := askHarness(t, "")
	_ = os.WriteFile(h.cfg, []byte(testConfig+"ai:\n  provider: ollama\n"), 0o600)
	scriptAsk(f, assistant.AskResult{Query: "q", Usage: usage120})
	if _, errOut, code := h.exec(t, context.Background(), "ask", "ro", "who is here?"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	req := f.Calls()[0].Req.(assistant.AskRequest)
	if req.Scope.Conn != "ro" || req.Question != "who is here?" {
		t.Fatalf("request = %+v", req)
	}
	if _, errOut, code := h.exec(t, context.Background(), "ask", "mian", "q"); code != 1 || !strings.Contains(errOut, `unknown connection "mian"`) || len(f.Calls()) != 1 {
		t.Fatalf("unknown positional connection: code=%d stderr=%q", code, errOut)
	}
}

// --run runs the proposal read-only through the query runner: the result
// on stdout in the query formats, the proposal on stderr, the run in the
// history as a CLI run, and the query command's exit codes.
func TestAskRunRunsTheProposalReadOnly(t *testing.T) {
	h, f := askHarness(t, "")
	h.env = map[string]string{"XDG_STATE_HOME": t.TempDir()}
	store := &historySQLite{}
	h.extra = []driver.Driver{store}
	scriptAsk(f, assistant.AskResult{Query: selectPeople, Explanation: "Three people.", Dialect: "fake", Usage: usage120})
	out, errOut, code := h.exec(t, context.Background(), "ask", "main", "who is here?", "--run", "--limit", "2")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Rows) != 2 {
		t.Fatalf("stdout is not the 2-row JSON result (%v):\n%s", err, out)
	}
	if !strings.Contains(errOut, selectPeople) || !strings.Contains(errOut, "Three people.") {
		t.Fatalf("the proposal is not on stderr: %q", errOut)
	}
	var ran []drivertest.Call
	for _, c := range h.fake.Calls() {
		if c.Op == "query" {
			ran = append(ran, c)
		}
	}
	if len(ran) != 1 || ran[0].Query.Text != selectPeople || !ran[0].Query.ReadOnly {
		t.Fatalf("query calls = %+v, want one read-only run of the proposal", ran)
	}
	if rec := store.inserts(); len(rec) != 1 || rec[0][1] != "main" || rec[0][2] != "cli" || rec[0][3] != selectPeople || rec[0][4] != "1" {
		t.Fatalf("history inserts = %q, want one ok CLI run of the proposal on main", rec)
	}
	// --format picks the result format, as for tobira query.
	out, _, code = h.exec(t, context.Background(), "ask", "q", "--run", "--format", "csv")
	if code != 0 || !strings.HasPrefix(out, "id,") {
		t.Fatalf("csv: code=%d stdout=%q", code, out)
	}

	// A proposal that writes is refused by the read-only run: exit 4.
	scriptAsk(f, assistant.AskResult{Query: "delete from tobira_people", Usage: usage120})
	if _, errOut, code := h.exec(t, context.Background(), "ask", "q", "--run"); code != 4 {
		t.Fatalf("write proposal: code=%d stderr=%q, want 4", code, errOut)
	}
}

// The help states the row policy as it is: under always, rows are sent
// without --send-rows.
func TestAskHelpRowPolicy(t *testing.T) {
	h, _ := askHarness(t, "")
	out, _, code := h.exec(t, context.Background(), "ask", "--help")
	flat := strings.Join(strings.Fields(out), " ")
	if code != 0 || !strings.Contains(flat, "with always they are sent without it") || strings.Contains(flat, "unless --send-rows is given (and ai.send_rows is not never)") {
		t.Fatalf("help:\n%s", out)
	}
	if !strings.Contains(out, "ask [connection] <question>") || !strings.Contains(out, "--run") {
		t.Fatalf("help lacks the positional connection or --run:\n%s", out)
	}
}

// historySQLite stands in for the sqlite driver the history store opens and
// keeps the parameters of every history INSERT, as text.
type historySQLite struct {
	stubSQLite
	mu   sync.Mutex
	rows [][]string
}

func (d *historySQLite) Open(context.Context, driver.ConnConfig) (driver.Conn, error) {
	return historyConn{d: d}, nil
}

func (d *historySQLite) inserts() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]string(nil), d.rows...)
}

type historyConn struct {
	stubConn
	d *historySQLite
}

func (c historyConn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if strings.HasPrefix(q.Text, "INSERT INTO history") {
		var row []string
		for _, p := range q.Params {
			row = append(row, p.String())
		}
		c.d.mu.Lock()
		c.d.rows = append(c.d.rows, row)
		c.d.mu.Unlock()
	}
	return c.stubConn.Query(ctx, q)
}

// --run opens its connection read-only at the protocol level too, as
// tobira query --read-only does, not only with read-only calls.
func TestAskRunOpensTheConnectionReadOnly(t *testing.T) {
	h, f := askHarness(t, "")
	scriptAsk(f, assistant.AskResult{Query: selectPeople, Usage: usage120})
	if _, errOut, code := h.exec(t, context.Background(), "ask", "main", "who?", "--run"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(h.opened) == 0 {
		t.Fatal("the run opened no connection")
	}
	for _, cc := range h.opened {
		if !cc.ReadOnly {
			t.Fatalf("driver got %+v, want every Open with ReadOnly", h.opened)
		}
	}
}
