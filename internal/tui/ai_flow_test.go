package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/assistant/assistanttest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/palette"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

const proposed = "select * from tobira_people"

// aiSideW is the narrowest screen with the side panel.
const aiSideW = 120

// answerAI answers the open AI prompt with key once its grace period is
// over.
func (h *harness) answerAI(k string) {
	h.t.Helper()
	if h.m.ask == nil {
		h.t.Fatalf("no prompt to answer:\n%s", h.text())
	}
	h.m.ask.opened = h.m.ask.opened.Add(-time.Second)
	h.keys(k)
}

// askFake is a Fake whose Ask proposes the People query.
func askFake() *assistanttest.Fake {
	return &assistanttest.Fake{
		ModelName: "fake/model",
		AskFn: func(_ context.Context, req assistant.AskRequest, _ assistant.Events) (assistant.AskResult, error) {
			return assistant.AskResult{
				Query:       proposed,
				Explanation: "Lists **every** person.",
				Usage:       assistant.Usage{Model: "fake/model", InputTokens: 1234, OutputTokens: 340, Latency: 2100 * time.Millisecond},
			}, nil
		},
	}
}

// newAIHarness is newQueryHarness with f as the assistant (nil: none).
func newAIHarness(t *testing.T, f *assistanttest.Fake, hist HistoryStore, opts ...drivertest.FakeOption) *harness {
	t.Helper()
	h := newQueryHarness(t, hist, opts...)
	if f != nil {
		h.m.asst = f
	}
	return h
}

// openAI opens the panel with a and checks that it has the focus.
func (h *harness) openAI() {
	h.t.Helper()
	h.keys("a")
	if !h.m.ai.open || h.m.focus != focusAI {
		h.t.Fatalf("a: panel open %v focus %v\n%s", h.m.ai.open, h.m.focus, h.text())
	}
}

// askAI types q into the focused panel and sends it, then waits for the
// turn to end.
func (h *harness) askAI(q string) {
	h.t.Helper()
	h.typ(q)
	h.keys("enter")
	h.pump(func() bool { return h.m.ai.turn == nil })
}

func queryOps(f *drivertest.Fake) []drivertest.Call {
	var out []drivertest.Call
	for _, c := range f.Calls() {
		if c.Op == "query" {
			out = append(out, c)
		}
	}
	return out
}

func TestAIPanelOpenFocusClose(t *testing.T) {
	h := newAIHarness(t, askFake(), nil)
	h.send(tea.WindowSizeMsg{Width: aiSideW, Height: winH})
	h.openAI()
	if !strings.Contains(h.text(), "AI · fake/model · local · ask") {
		t.Fatalf("no header:\n%s", h.text())
	}
	if l := h.m.lay; l.aiOverlay || l.ai.w < 40 || l.ai.x+l.ai.w != aiSideW || !l.showSidebar || !l.showGrid {
		t.Fatalf("at %d columns the panel is not on the right of both panes: %+v", aiSideW, l)
	}
	checkFrame(t, h, aiSideW, winH)
	h.keys("esc")
	if h.m.focus != focusGrid || !h.m.ai.open {
		t.Fatalf("esc: focus %v open %v", h.m.focus, h.m.ai.open)
	}
	h.keys("a") // visible and unfocused: a closes it
	if h.m.ai.open || strings.Contains(h.text(), "AI · fake/model") {
		t.Fatalf("a again did not close the panel:\n%s", h.text())
	}
	// The placeholders are gone.
	h.keys("a")
	if strings.Contains(h.text(), "arrives in") {
		t.Fatalf("placeholder still shown:\n%s", h.text())
	}
	// With a collection open the header names it.
	h.keys("esc", "a")
	h.openColl("local", "tobira_people")
	h.keys("a")
	h.send(tea.WindowSizeMsg{Width: 44, Height: winH})
	if !strings.Contains(h.text(), "… · ask") {
		t.Fatalf("a narrow header lost its mode:\n%s", h.text())
	}
	h.send(tea.WindowSizeMsg{Width: 140, Height: winH}) // room for the whole header
	if !strings.Contains(h.text(), "AI · fake/model · local/main/tobira_people · ask") {
		t.Fatalf("header without the collection:\n%s", h.text())
	}
}

func TestAIPanelOverlayBelow120Columns(t *testing.T) {
	h := newAIHarness(t, askFake(), nil)
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	h.openAI()
	l := h.m.lay
	if !l.aiOverlay || l.ai.x != 0 || l.ai.w != 80 {
		t.Fatalf("below 120 columns the panel is not a full-pane overlay: %+v", l.ai)
	}
	h.send(tea.WindowSizeMsg{Width: aiSideW - 1, Height: 24})
	if l := h.m.lay; !l.aiOverlay || l.ai.w != aiSideW-1 {
		t.Fatalf("at %d columns the panel is not an overlay: %+v", aiSideW-1, l.ai)
	}
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	if strings.Contains(h.text(), "connections") {
		t.Fatalf("the sidebar shows through the overlay:\n%s", h.text())
	}
	checkFrame(t, h, 80, 24)
	h.keys("esc") // the overlay hides with the focus
	if !strings.Contains(h.text(), "connections") {
		t.Fatalf("esc did not reveal the panes:\n%s", h.text())
	}
	h.keys("a") // hidden: a brings it back
	if h.m.focus != focusAI || !h.m.lay.aiOverlay {
		t.Fatal("a did not refocus the hidden overlay")
	}
	for _, sz := range [][2]int{{20, 5}, {1, 1}, {99, 10}, {119, 8}, {120, 8}, {140, 40}} {
		h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkFrame(t, h, sz[0], sz[1])
	}
}

func TestAIAskProposalLoadRunCopy(t *testing.T) {
	hist := &fakeHistory{}
	f := askFake()
	h := newAIHarness(t, f, hist)
	h.openAI()
	h.askAI("who is here?")
	calls := f.Calls()
	if len(calls) != 1 || calls[0].Method != "Ask" {
		t.Fatalf("calls = %+v", calls)
	}
	req := calls[0].Req.(assistant.AskRequest)
	if req.Question != "who is here?" || req.Scope.Conn != "local" || calls[0].Session == 0 {
		t.Fatalf("request = %+v (session %d)", req, calls[0].Session)
	}
	txt := h.text()
	for _, want := range []string{"who is here?", proposed, "every person", "ctrl+o load · f5 run · ctrl+y copy", "fake/model · 1.2k in / 340 out · 2.1s"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("missing %q:\n%s", want, txt)
		}
	}
	if len(queryOps(h.fake)) != 0 {
		t.Fatal("a proposal ran without a keypress")
	}
	// enter and y on the empty input do nothing: a double enter after a
	// question never replaces the buffer, and a question may start with y.
	h.keys("enter")
	if h.m.edOpen || h.m.ed.Value() != "" || h.m.focus != focusAI {
		t.Fatalf("enter on the empty input acted: open %v value %q focus %v", h.m.edOpen, h.m.ed.Value(), h.m.focus)
	}
	h.keys("y")
	h.drain(20 * time.Millisecond)
	if len(h.clip) != 0 || h.m.ai.in.Value() != "y" {
		t.Fatalf("y copied (%q) instead of typing (%q)", h.clip, h.m.ai.in.Value())
	}
	h.m.ai.in.SetValue("")
	// ctrl+o loads it into the editor, which opens.
	h.send(ctrl('o'))
	if !h.m.edOpen || h.m.ed.Value() != proposed || h.m.focus != focusEditor {
		t.Fatalf("ctrl+o: open %v value %q focus %v", h.m.edOpen, h.m.ed.Value(), h.m.focus)
	}
	h.drain(30 * time.Millisecond)
	if len(queryOps(h.fake)) != 0 {
		t.Fatal("ctrl+o ran the proposal")
	}
	// F5 loads and runs it through the editor path. The buffer was never
	// run, so replacing it with other text asks first.
	h.m.ed.SetValue("something else")
	h.send(palette.AskCmd{})
	if h.m.focus != focusAI {
		t.Fatal(":ask did not focus the panel")
	}
	h.send(keyF5)
	if h.m.ask == nil || !strings.Contains(h.text(), "replace the editor buffer? y/N") {
		t.Fatalf("F5 replaced unsaved text without asking:\n%s", h.text())
	}
	h.keys("y")
	h.pump(func() bool { return h.m.running == nil && len(h.m.tabs) == 1 })
	ops := queryOps(h.fake)
	if len(ops) != 1 || ops[0].Query.Text != proposed || ops[0].Query.ReadOnly {
		t.Fatalf("run = %+v", ops)
	}
	h.pump(func() bool { return len(hist.all()) == 1 })
	if e := hist.all()[0]; e.Source != history.SourceTUI || e.Query != proposed {
		t.Fatalf("history = %+v", e)
	}
	if h.m.ed.Value() != proposed {
		t.Fatalf("F5 did not load the buffer: %q", h.m.ed.Value())
	}
	// ctrl+y copies it.
	h.send(palette.AskCmd{})
	h.send(ctrl('y'))
	h.drain(20 * time.Millisecond)
	if len(h.clip) == 0 || h.clip[len(h.clip)-1] != proposed {
		t.Fatalf("clipboard = %q", h.clip)
	}
	// With text in the input, enter sends it instead.
	h.typ("again")
	h.keys("enter")
	h.pump(func() bool { return h.m.ai.turn == nil && len(f.Calls()) == 2 })
	if got := f.Calls()[1].Req.(assistant.AskRequest); len(got.History) != 2 || got.History[0].Role != "user" || got.History[1].Role != "model" {
		t.Fatalf("history sent = %+v", got.History)
	}
	if f.Calls()[1].Session != f.Calls()[0].Session {
		t.Fatal("the second turn used another session")
	}
}

func TestAIProposalRunHonoursReadOnly(t *testing.T) {
	f := askFake()
	h := newAIHarness(t, f, nil)
	h.openColl("ro", "tobira_people")
	h.openAI()
	h.askAI("who?")
	h.send(keyF5)
	h.pump(func() bool { return h.m.running == nil && len(h.m.tabs) == 1 })
	if ops := queryOps(h.fake); len(ops) != 1 || !ops[0].Query.ReadOnly {
		t.Fatalf("run on ro = %+v", ops)
	}
}

func TestAIAnswerModeShowsToolCalls(t *testing.T) {
	f := &assistanttest.Fake{
		ModelName: "fake/model",
		AnswerFn: func(_ context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			ev.OnToolCall(assistant.ToolCall{Name: "query", Input: "select count(*) from tobira_people", Rows: 1})
			ev.OnToolCall(assistant.ToolCall{Name: "describe", Input: "nope", Rows: -1, Err: "boom"})
			return assistant.AnswerResult{Markdown: "There are **5** people.", Usage: assistant.Usage{Model: "fake/model"}}, nil
		},
	}
	h := newAIHarness(t, f, nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH}) // whole tool lines
	h.openAI()
	h.keys("tab")
	if !strings.Contains(h.text(), "· answer") {
		t.Fatalf("tab did not switch to answer:\n%s", h.text())
	}
	h.askAI("how many?")
	txt := h.text()
	for _, want := range []string{"▸ query on local: select count(*) from tobira_people → 1 row", "▸ describe on local: nope → error: boom", "There are 5 people."} {
		if !strings.Contains(txt, want) {
			t.Fatalf("missing %q:\n%s", want, txt)
		}
	}
	if req := f.Calls()[0].Req.(assistant.AnswerRequest); req.AllowExecute {
		t.Fatal("execute offered by default")
	}
	// In a narrow panel the line wraps whole: the input and the outcome
	// both stay readable, nothing is cut.
	h.send(tea.WindowSizeMsg{Width: 50, Height: winH})
	txt = h.text()
	for _, want := range []string{"select count(*) from", "tobira_people → 1 row", "error: boom"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("narrow tool line lost %q:\n%s", want, txt)
		}
	}
	for _, l := range strings.Split(txt, "\n") {
		if strings.HasPrefix(l, "▸ ") && strings.Contains(l, "…") {
			t.Fatalf("narrow tool line was cut:\n%s", txt)
		}
	}
	// :ask! sends in answer mode from anywhere.
	h.keys("tab", "esc") // back to ask, then leave
	h.send(palette.AskCmd{Question: "and now?", Answer: true})
	h.pump(func() bool { return h.m.ai.turn == nil && len(f.Calls()) == 2 })
	if f.Calls()[1].Method != "Answer" || f.Calls()[1].Req.(assistant.AnswerRequest).Question != "and now?" {
		t.Fatalf("calls = %+v", f.Calls())
	}
	if h.m.focus != focusAI {
		t.Fatal(":ask! did not focus the panel")
	}
}

// consentFake asks for consent for 3 rows in every Answer and reports the
// answer as its markdown.
func consentFake() *assistanttest.Fake {
	return &assistanttest.Fake{
		ModelName: "fake/model",
		AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			c, err := ev.AskConsent(ctx, assistant.ConsentRequest{Conn: req.Scope.Conn, Rows: 3, What: "query: select * from people"})
			if err != nil {
				return assistant.AnswerResult{}, err
			}
			return assistant.AnswerResult{Markdown: fmt.Sprintf("consent=%d", c)}, nil
		},
	}
}

func TestAIConsentPrompt(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want assistant.Consent
	}{{"y", assistant.ConsentOnce}, {"a", assistant.ConsentSession}, {"n", assistant.ConsentNo}, {"esc", assistant.ConsentNo}} {
		t.Run(tc.key, func(t *testing.T) {
			h := newAIHarness(t, consentFake(), nil)
			h.openAI()
			h.keys("tab")
			h.typ("rows?")
			h.keys("enter")
			h.until("send 3 rows from local to fake/model?")
			if !strings.Contains(h.text(), "query: select * from people") {
				t.Fatalf("the prompt does not say what produced the rows:\n%s", h.text())
			}
			if !strings.Contains(h.text(), "[y]es once · [a]lways this session · [n]o") {
				t.Fatalf("prompt:\n%s", h.text())
			}
			checkFrame(t, h, winW, winH)
			h.answerAI(tc.key)
			h.pump(func() bool { return h.m.ai.turn == nil })
			if want := fmt.Sprintf("consent=%d", tc.want); !strings.Contains(h.text(), want) {
				t.Fatalf("want %s:\n%s", want, h.text())
			}
		})
	}
	t.Run("cancel answers no", func(t *testing.T) {
		got := make(chan error, 1)
		f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			c, err := ev.AskConsent(ctx, assistant.ConsentRequest{Conn: "local", Rows: 1})
			if c != assistant.ConsentNo {
				err = errors.New("consent given")
			}
			got <- err
			return assistant.AnswerResult{}, err
		}}
		h := newAIHarness(t, f, nil)
		h.openAI()
		h.keys("tab")
		h.typ("rows?")
		h.keys("enter")
		h.until("send 1 row from local")
		h.m.cancelTurn("cancelled")
		if h.m.ask != nil {
			t.Fatal("the prompt of a cancelled turn stayed open")
		}
		select {
		case err := <-got:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("callback returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the consent callback still blocks after cancel")
		}
	})
}

func TestAIConsentMemorySpansTheSessionUntilReload(t *testing.T) {
	// Consent memory lives in the assistant's Session; the panel's part is
	// to keep one Session per conversation, and to drop it only when its
	// connection is repointed or removed.
	f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(context.Context, assistant.AnswerRequest, assistant.Events) (assistant.AnswerResult, error) {
		return assistant.AnswerResult{Markdown: "ok"}, nil
	}}
	h, path := newConnHarness(t)
	h.m.asst = f
	h.openAI()
	h.keys("tab")
	h.askAI("one")
	h.askAI("two")
	// A connection write that leaves local alone keeps its session.
	h.m.focus = focusSidebar
	h.m.sb = h.m.sb.Select("ro")
	h.step("E")
	h.typ("2") // rename ro
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	h.pump(func() bool { return !h.m.saving && h.m.form == nil })
	if names := h.names(); len(names) != 2 || names[0] != "local" || names[1] != "ro2" {
		t.Fatalf("names after the rename = %v", names)
	}
	h.m.sb = h.m.sb.Select("local") // the write selected ro2
	h.send(palette.AskCmd{})
	h.askAI("three")
	h.send(palette.ReloadCmd{}) // the file is unchanged, but :reload drops every session
	h.pump(func() bool { return !h.m.saving })
	h.send(palette.AskCmd{})
	h.askAI("four")
	if err := os.WriteFile(path, []byte(strings.Replace(fileText(t, path), "fake://local", "fake://moved", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	h.send(palette.ReloadCmd{}) // local is repointed
	h.pump(func() bool { return !h.m.saving })
	h.send(palette.AskCmd{})
	h.askAI("five")
	calls := f.Calls()
	if len(calls) != 5 {
		t.Fatalf("calls = %d", len(calls))
	}
	if s := calls; s[0].Session != s[1].Session || s[1].Session != s[2].Session || s[2].Session == s[3].Session || s[3].Session == s[4].Session {
		t.Fatalf("sessions = %d %d %d %d %d: want one across turns and an unrelated write, a new one after each :reload", s[0].Session, s[1].Session, s[2].Session, s[3].Session, s[4].Session)
	}
}

func TestAIExecute(t *testing.T) {
	answers := make(chan bool, 4)
	f := &assistanttest.Fake{
		ModelName: "fake/model",
		AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			if !req.AllowExecute {
				return assistant.AnswerResult{Markdown: "no execute"}, nil
			}
			ok, err := ev.ConfirmExec(ctx, req.Scope.Conn, "delete from tobira_people where id = 1")
			answers <- ok
			return assistant.AnswerResult{Markdown: fmt.Sprintf("executed=%v", ok)}, err
		},
	}
	h := newAIHarness(t, f, nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH}) // the whole header
	h.openAI()
	h.keys("tab")
	h.askAI("delete ada")
	if !strings.Contains(h.text(), "no execute") {
		t.Fatalf("execute on by default:\n%s", h.text())
	}
	h.send(palette.AIExecuteCmd{On: true})
	if !strings.Contains(h.text(), "· answer · execute") {
		t.Fatalf("header without execute:\n%s", h.text())
	}
	for _, tc := range []struct {
		key  string
		want bool
	}{{"y", true}, {"n", false}} {
		h.send(palette.AskCmd{})
		h.typ("delete ada")
		h.keys("enter")
		h.until("run this statement on local? it may change data. y/N")
		if !strings.Contains(h.text(), "delete from tobira_people where id = 1") {
			t.Fatalf("statement not shown:\n%s", h.text())
		}
		h.answerAI(tc.key)
		h.pump(func() bool { return h.m.ai.turn == nil })
		if got := <-answers; got != tc.want {
			t.Fatalf("%s: confirm = %v", tc.key, got)
		}
	}
	h.send(palette.AIExecuteCmd{On: false})
	h.send(palette.AskCmd{})
	h.askAI("delete ada")
	if req := f.Calls()[len(f.Calls())-1].Req.(assistant.AnswerRequest); req.AllowExecute {
		t.Fatal(":ai execute off left execute on")
	}
}

func TestAIExecuteRefusedOnReadOnly(t *testing.T) {
	f := consentFake()
	h := newAIHarness(t, f, nil)
	h.openColl("ro", "tobira_people")
	h.send(palette.AIExecuteCmd{On: true})
	if fl := h.flashText(); !strings.Contains(fl, "ro is read-only") {
		t.Fatalf("flash = %q", fl)
	}
	h.openAI()
	if strings.Contains(h.text(), "· execute") {
		t.Fatalf("execute on for a read-only connection:\n%s", h.text())
	}
}

func TestAIFixWithAI(t *testing.T) {
	driverErr := errors.New("no such table: tobira_peple")
	f := &assistanttest.Fake{
		ModelName: "fake/model",
		FixErrorFn: func(_ context.Context, req assistant.FixErrorRequest, _ assistant.Events) (assistant.AskResult, error) {
			return assistant.AskResult{Query: proposed, Explanation: "Typo in the table name."}, nil
		},
	}
	h := newAIHarness(t, f, nil)
	h.fake.OnQuery("select * from tobira_peple", func(driver.Query) (driver.ResultSet, error) { return nil, driverErr })
	h.runInEditor("select * from tobira_peple")
	if h.m.activeTab() == nil || h.m.activeTab().state != tabFailed {
		t.Fatalf("no error tab:\n%s", h.text())
	}
	h.keys("f")
	h.pump(func() bool { return len(f.Calls()) == 1 && h.m.ai.turn == nil })
	if !h.m.ai.open || h.m.focus != focusAI {
		t.Fatal("f did not open the panel")
	}
	req := f.Calls()[0].Req.(assistant.FixErrorRequest)
	if req.Query != "select * from tobira_peple" || !errors.Is(req.Err, driverErr) || !strings.Contains(req.Error, "no such table") || req.Scope.Conn != "local" {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(h.text(), "Typo in the table name.") || !strings.Contains(h.text(), "ctrl+o load") {
		t.Fatalf("no proposal:\n%s", h.text())
	}
	// f on the browse tab still filters.
	h.keys("esc")
	h.openColl("local", "tobira_people")
	h.m.active = 0
	h.keys("f")
	if !h.m.fl.Focused() {
		t.Fatal("f on the browse tab no longer filters")
	}
}

func TestAIExplainResults(t *testing.T) {
	f := &assistanttest.Fake{
		ModelName: "fake/model",
		ExplainResultsFn: func(_ context.Context, req assistant.ExplainResultsRequest, _ assistant.Events) (assistant.AnswerResult, error) {
			return assistant.AnswerResult{Markdown: fmt.Sprintf("%d columns, %d rows", len(req.Columns), len(req.Rows))}, nil
		},
	}
	h := newAIHarness(t, f, nil)
	h.send(palette.ExplainResultsCmd{})
	if fl := h.flashText(); !strings.Contains(fl, "result tab") {
		t.Fatalf("no result tab: flash %q", fl)
	}
	h.send(palette.SetLimitCmd{N: 2})
	h.runInEditor("select * from tobira_people")
	h.send(palette.ExplainResultsCmd{})
	h.pump(func() bool { return len(f.Calls()) == 1 && h.m.ai.turn == nil })
	req := f.Calls()[0].Req.(assistant.ExplainResultsRequest)
	if req.Query != "select * from tobira_people" || len(req.Rows) != 2 || !req.Truncated || len(req.Columns) != len(drivertest.People.Columns) {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(h.text(), fmt.Sprintf("%d columns, 2 rows", len(drivertest.People.Columns))) {
		t.Fatalf("no explanation:\n%s", h.text())
	}
}

// blockingFake's Ask waits for its ctx; returned is closed once it has.
func blockingFake(entered chan<- struct{}, returned *atomic.Bool, linger time.Duration) *assistanttest.Fake {
	return &assistanttest.Fake{
		ModelName: "fake/model",
		AskFn: func(ctx context.Context, _ assistant.AskRequest, _ assistant.Events) (assistant.AskResult, error) {
			entered <- struct{}{}
			<-ctx.Done()
			time.Sleep(linger)
			returned.Store(true)
			return assistant.AskResult{}, ctx.Err()
		},
	}
}

func TestAICancelWithCtrlC(t *testing.T) {
	entered := make(chan struct{}, 1)
	var returned atomic.Bool
	h := newAIHarness(t, blockingFake(entered, &returned, 0), nil)
	h.openAI()
	h.typ("slow?")
	h.keys("enter")
	h.awaitEntered(entered)
	if !strings.Contains(h.text(), "thinking") {
		t.Fatalf("no progress line:\n%s", h.text())
	}
	h.keys("ctrl+c")
	if h.m.ai.turn != nil || h.quitted() {
		t.Fatal("ctrl+c did not cancel the turn (or quit)")
	}
	h.pump(returned.Load)
	h.drain(30 * time.Millisecond)
	if !strings.Contains(h.text(), "cancelled") {
		t.Fatalf("no cancel note:\n%s", h.text())
	}
}

func TestAIStaleResultIsDropped(t *testing.T) {
	t.Run("old events after a newer turn started", func(t *testing.T) {
		// Delivered straight to Update, so the check cannot lean on the
		// cancelled context dropping them first.
		release := make(chan struct{})
		f := &assistanttest.Fake{ModelName: "fake/model", AskFn: func(ctx context.Context, _ assistant.AskRequest, _ assistant.Events) (assistant.AskResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return assistant.AskResult{Query: "select 'NEW'", Explanation: "NEW"}, ctx.Err()
		}}
		h := newAIHarness(t, f, nil)
		h.openAI()
		h.typ("first")
		h.keys("enter")
		h.pump(func() bool { return len(f.Calls()) == 1 })
		old := h.m.ai.turn.id
		h.typ("second")
		h.keys("enter")
		h.pump(func() bool { return len(f.Calls()) == 2 })
		closed := make(chan aiEvent)
		close(closed)
		reply := make(chan assistant.Consent, 1)
		for _, ev := range []aiEvent{
			aiToolEvent{id: old, tc: assistant.ToolCall{Name: "OLDTOOL", Rows: -1}},
			aiConsentEvent{id: old, req: assistant.ConsentRequest{Conn: "local", Rows: 9}, reply: reply},
			aiDoneEvent{id: old, res: aiResult{question: "first", markdown: "OLD", query: "select 'OLD'"}},
		} {
			h.send(aiEventMsg{ev: ev, ch: closed})
		}
		if h.m.ai.turn == nil || h.m.ask != nil || strings.Contains(h.text(), "OLD") {
			t.Fatalf("old events were applied:\n%s", h.text())
		}
		if c := <-reply; c != assistant.ConsentNo {
			t.Fatalf("a stale consent got %d", c)
		}
		close(release)
		h.pump(func() bool { return h.m.ai.turn == nil })
		if strings.Contains(h.text(), "OLD") || !strings.Contains(h.text(), "NEW") {
			t.Fatalf("stale result shown:\n%s", h.text())
		}
	})
	t.Run("newer turn", func(t *testing.T) {
		releaseOld, releaseNew := make(chan struct{}), make(chan struct{})
		oldDone := make(chan struct{})
		var n atomic.Int32
		f := &assistanttest.Fake{ModelName: "fake/model", AskFn: func(ctx context.Context, _ assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
			if n.Add(1) == 1 {
				<-releaseOld // ignores the cancel: its late result must still be dropped
				ev.OnToolCall(assistant.ToolCall{Name: "OLDTOOL", Rows: -1})
				defer close(oldDone)
				return assistant.AskResult{Query: "select 'OLD'", Explanation: "OLD"}, nil
			}
			<-releaseNew
			return assistant.AskResult{Query: "select 'NEW'", Explanation: "NEW"}, nil
		}}
		h := newAIHarness(t, f, nil)
		h.openAI()
		h.typ("first")
		h.keys("enter")
		h.pump(func() bool { return n.Load() == 1 })
		h.typ("second")
		h.keys("enter")
		h.pump(func() bool { return n.Load() == 2 })
		close(releaseOld) // the old turn ends while the new one runs
		<-oldDone
		h.drain(50 * time.Millisecond)
		if h.m.ai.turn == nil || strings.Contains(h.text(), "OLD") {
			t.Fatalf("stale result shown, or it ended the new turn:\n%s", h.text())
		}
		close(releaseNew)
		h.pump(func() bool { return h.m.ai.turn == nil })
		if strings.Contains(h.text(), "OLD") || !strings.Contains(h.text(), "NEW") {
			t.Fatalf("stale result shown:\n%s", h.text())
		}
	})
	t.Run("connection switch", func(t *testing.T) {
		release := make(chan struct{})
		f := &assistanttest.Fake{ModelName: "fake/model", AskFn: func(ctx context.Context, _ assistant.AskRequest, _ assistant.Events) (assistant.AskResult, error) {
			<-release
			return assistant.AskResult{Query: "select 'OLD'", Explanation: "OLD"}, nil
		}}
		h := newAIHarness(t, f, nil)
		h.openAI()
		h.typ("first")
		h.keys("enter")
		h.pump(func() bool { return len(f.Calls()) == 1 })
		// With no collection open, moving the sidebar cursor is browsing,
		// not a switch: the turn goes on.
		h.m.sb = h.m.sb.Select("ro")
		h.send(tickMsg{})
		h.m.sb = h.m.sb.Select("local")
		h.send(tickMsg{})
		if h.m.ai.turn == nil {
			t.Fatal("moving the sidebar cursor cancelled the turn")
		}
		// Opening a collection of another connection switches the scope.
		h.openColl("ro", "tobira_people")
		close(release)
		h.drain(50 * time.Millisecond)
		if h.m.ai.turn != nil {
			t.Fatal("the turn survived a connection switch")
		}
		h.openColl("local", "tobira_people")
		h.send(palette.AskCmd{})
		if strings.Contains(h.text(), "OLD") {
			t.Fatalf("result of a switched connection shown:\n%s", h.text())
		}
	})
	t.Run("same connection", func(t *testing.T) {
		release := make(chan struct{})
		f := &assistanttest.Fake{ModelName: "fake/model", AskFn: func(ctx context.Context, _ assistant.AskRequest, _ assistant.Events) (assistant.AskResult, error) {
			<-release
			return assistant.AskResult{Explanation: "KEPT"}, nil
		}}
		h := newAIHarness(t, f, nil)
		h.openColl("local", "tobira_people")
		h.openAI()
		h.typ("first")
		h.keys("enter")
		h.pump(func() bool { return len(f.Calls()) == 1 })
		h.m.sb = h.m.sb.Select("ro") // the scope stays local's collection
		h.send(tickMsg{})
		close(release)
		h.pump(func() bool { return h.m.ai.turn == nil })
		if !strings.Contains(h.text(), "KEPT") {
			t.Fatalf("result dropped without a switch:\n%s", h.text())
		}
	})
}

func TestAITurnCancelledAndWaitedForOnConnectionEdit(t *testing.T) {
	entered := make(chan struct{}, 1)
	var returned atomic.Bool
	h, _ := newConnHarness(t)
	h.m.asst = blockingFake(entered, &returned, 150*time.Millisecond)
	h.openAI()
	h.typ("slow?")
	h.keys("enter")
	h.awaitEntered(entered)
	h.keys("esc")
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("E")
	h.typ("2") // a rename closes the connection
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	h.sendSaved() // the SavedMsg: the write starts
	if h.m.ai.turn != nil {
		t.Fatal("turn not cancelled before the write")
	}
	h.pump(func() bool { return !h.m.saving })
	if !returned.Load() {
		t.Fatal("the write did not wait for the cancelled turn")
	}
}

func TestAIQuitCancelsTheTurnWithoutAsking(t *testing.T) {
	entered := make(chan struct{}, 1)
	var returned atomic.Bool
	h := newAIHarness(t, blockingFake(entered, &returned, 0), nil)
	h.openAI()
	h.typ("slow?")
	h.keys("enter")
	h.awaitEntered(entered)
	h.keys("esc", "q")
	if !h.quitted() {
		t.Fatalf("q with a running turn did not quit:\n%s", h.text())
	}
	h.pump(returned.Load)
}

func TestAINotConfiguredAndInitError(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		h := newAIHarness(t, nil, nil)
		h.send(tea.WindowSizeMsg{Width: 200, Height: winH}) // unwrapped lines
		h.openAI()
		if !strings.Contains(h.text(), "AI is not configured — add an ai: section (see docs/ai)") {
			t.Fatalf("no configure message:\n%s", h.text())
		}
		h.typ("hello")
		h.keys("enter")
		h.drain(20 * time.Millisecond)
		if h.m.ai.turn != nil {
			t.Fatal("a turn started without an assistant")
		}
	})
	t.Run("init error", func(t *testing.T) {
		h := newAIHarness(t, nil, nil)
		h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
		h.m.aiErr = fmt.Errorf("%w: invalid key ****", assistant.ErrAuth)
		h.openAI()
		txt := h.text()
		if !strings.Contains(txt, "invalid key ****") || !strings.Contains(txt, "api_key") {
			t.Fatalf("no init error:\n%s", txt)
		}
	})
	t.Run("turn errors", func(t *testing.T) {
		var next error
		f := &assistanttest.Fake{ModelName: "fake/model", AskFn: func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
			return assistant.AskResult{}, next
		}}
		h := newAIHarness(t, f, nil)
		h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
		h.openAI()
		for _, tc := range []struct {
			err  error
			want string
		}{
			{fmt.Errorf("%w: 401", assistant.ErrAuth), "check ai.api_key"},
			{fmt.Errorf("%w: 429", assistant.ErrRateLimit), "rate limit"},
			{assistant.ErrNotConfigured, "add an ai: section"},
			{&assistant.TurnError{Err: fmt.Errorf("%w: bad answer", assistant.ErrProvider), Usage: assistant.Usage{Model: "fake/model", InputTokens: 10, OutputTokens: 2}}, "fake/model · 10 in / 2 out"},
		} {
			next = tc.err
			h.askAI("q")
			if !strings.Contains(h.text(), tc.want) {
				t.Fatalf("%v: want %q:\n%s", tc.err, tc.want, h.text())
			}
		}
	})
}

func TestAIModelTextIsEscaped(t *testing.T) {
	evil := "click \x1b]8;;http://evil.example\x07here\x1b]8;;\x07 \x1b[2J\x9b31m and [a link](http://example.com) \u202e"
	f := &assistanttest.Fake{
		ModelName: "fake/\x1b[31mmodel",
		AskFn: func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
			return assistant.AskResult{Query: "select '\x1b]0;title\x07'", Explanation: evil, Usage: assistant.Usage{Model: "m\x1b[5m"}}, nil
		},
		AnswerFn: func(_ context.Context, _ assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			ev.OnToolCall(assistant.ToolCall{Name: "q\x1b[1m", Input: "x\x1b]8;;y\x07", Rows: -1, Err: "e\x07"})
			return assistant.AnswerResult{Markdown: "# Title\n\n" + evil + "\n\n```sql\nselect '\x1b[2J'\n```"}, nil
		},
	}
	h := newAIHarness(t, f, nil)
	h.openAI()
	h.askAI("q")
	tuitest.NoInjectedControls(t, h.raw())
	if !strings.Contains(h.text(), `\x1b`) {
		t.Fatalf("escapes not shown visibly:\n%s", h.text())
	}
	h.keys("tab")
	h.askAI("q")
	tuitest.NoInjectedControls(t, h.raw())
	if !strings.Contains(h.text(), "http://example.com") {
		t.Fatalf("link target dropped:\n%s", h.text())
	}
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	tuitest.NoInjectedControls(t, h.raw())
	h.send(palette.ThemeCmd{Name: "tobira-light"})
	h.send(palette.AskCmd{})
	tuitest.NoInjectedControls(t, h.raw())
}

func TestAIPanelGoldens(t *testing.T) {
	t.Run("side 120x30", func(t *testing.T) {
		h := newAIHarness(t, askFake(), nil)
		h.send(tea.WindowSizeMsg{Width: aiSideW, Height: winH})
		h.openAI()
		h.askAI("who is here?")
		checkFrame(t, h, aiSideW, winH)
		golden(t, "ai_panel_proposal", h.text())
	})
	t.Run("overlay 100x30", func(t *testing.T) {
		h := newAIHarness(t, askFake(), nil)
		h.openAI()
		h.askAI("who is here?")
		checkFrame(t, h, winW, winH)
		golden(t, "ai_panel_overlay_100", h.text())
	})
	t.Run("overlay 80x24", func(t *testing.T) {
		h := newAIHarness(t, askFake(), nil)
		h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
		h.openAI()
		h.askAI("who is here?")
		checkFrame(t, h, 80, 24)
		golden(t, "ai_panel_overlay", h.text())
	})
}

// TestProgramAIPanelFlow runs the real program under teatest: open the
// panel, ask, load the proposal and run it.
func TestProgramAIPanelFlow(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark", Assistant: askFake()})
	if err != nil {
		t.Fatal(err)
	}
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(aiSideW, winH))
	var mu sync.Mutex
	var seen strings.Builder
	waitFor := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
			mu.Lock()
			defer mu.Unlock()
			seen.WriteString(ansi.Strip(string(b)))
			return strings.Contains(seen.String(), s)
		}, teatest.WithDuration(5*time.Second))
	}
	waitFor("connections")
	tm.Send(key("a"))
	waitFor("AI · fake/model")
	for _, r := range "who is here?" {
		tm.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	tm.Send(key("enter"))
	waitFor("ctrl+o load · f5 run") // the proposal's footer, not the status bar hint
	tm.Send(keyF5)
	waitFor("Ada Lovelace")
	tm.Send(key("esc"))
	tm.Send(key("q"))
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if len(final.tabs) != 1 || final.tabs[0].rows != len(drivertest.People.Rows) {
		t.Fatalf("final tabs: %d", len(final.tabs))
	}
}

func TestAIVimLettersAreTyped(t *testing.T) {
	h := newAIHarness(t, askFake(), nil)
	h.send(palette.KeymapCmd{Name: "vim"})
	h.openAI()
	h.typ("jk gG")
	if got := h.m.ai.in.Value(); got != "jk gG" {
		t.Fatalf("input = %q", got)
	}
}

func TestAITranscriptClearedWhenReloadRepoints(t *testing.T) {
	h, path := newConnHarness(t)
	f := askFake()
	h.m.asst = f
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.openAI()
	h.askAI("who is here?")
	if !strings.Contains(h.text(), "who is here?") {
		t.Fatalf("no transcript:\n%s", h.text())
	}
	// Repoint local behind tobira's back, then :reload.
	if err := os.WriteFile(path, []byte(strings.Replace(connSeed, "fake://local", "fake://elsewhere", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	h.send(palette.ReloadCmd{})
	h.pump(func() bool { return !h.m.saving })
	txt := h.text()
	if strings.Contains(txt, "who is here?") || !strings.Contains(txt, "transcript cleared") {
		t.Fatalf("transcript kept after a repoint:\n%s", txt)
	}
	h.send(palette.AskCmd{})
	h.askAI("again")
	calls := f.Calls()
	if len(calls) != 2 || calls[0].Session == calls[1].Session || len(calls[1].Req.(assistant.AskRequest).History) != 0 {
		t.Fatalf("calls after the repoint = %+v", calls)
	}
}

func TestAIPromptNeverReplacesAnother(t *testing.T) {
	h := newAIHarness(t, consentFake(), nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.openAI()
	h.keys("tab")
	h.m.askYN("discard the unsaved query? y/N", "", nil, "")
	h.send(palette.AskCmd{Question: "rows?", Answer: true})
	h.pump(func() bool { return h.m.ai.queued != nil })
	if h.m.ask == nil || h.m.ask.consent != nil || h.m.ai.turn == nil {
		t.Fatalf("the open prompt was replaced or the turn ended:\n%s", h.text())
	}
	h.keys("n") // answers the y/N; the queued AI prompt opens
	if h.m.ask == nil || h.m.ask.consent == nil || h.m.ai.queued != nil {
		t.Fatalf("the queued AI prompt did not open:\n%s", h.text())
	}
	// A y/N that replaces an AI prompt queues it again.
	h.m.askYN("quit? y/N", "", nil, "")
	if h.m.ai.queued == nil || h.m.ask.consent != nil {
		t.Fatal("the replaced AI prompt was not queued")
	}
	h.keys("n")
	h.answerAI("y")
	h.pump(func() bool { return h.m.ai.turn == nil })
	if !strings.Contains(h.text(), "consent=1") {
		t.Fatalf("the AI prompt was not answered by the user:\n%s", h.text())
	}
}

func TestAIExecuteConfirmRefusedWhenReadOnly(t *testing.T) {
	got := make(chan bool, 1)
	f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
		ok, err := ev.ConfirmExec(ctx, req.Scope.Conn, "delete from tobira_people") // asked even though not offered
		got <- ok
		return assistant.AnswerResult{Markdown: "done"}, err
	}}
	h := newAIHarness(t, f, nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.openColl("ro", "tobira_people")
	h.openAI()
	h.keys("tab")
	h.askAI("delete everyone")
	if ok := <-got; ok || h.m.ask != nil {
		t.Fatalf("execute confirmed on a read-only connection (prompt %v)", h.m.ask != nil)
	}
	if !strings.Contains(h.text(), "execute refused: ro is read-only") {
		t.Fatalf("no refusal note:\n%s", h.text())
	}
}

func TestAIExecuteBeforeThePanelOpens(t *testing.T) {
	h := newAIHarness(t, askFake(), nil)
	h.send(palette.AIExecuteCmd{On: true})
	h.openAI()
	if !strings.Contains(h.text(), "> ask about this connection") {
		t.Fatalf("the input was not set up:\n%s", h.text())
	}
	h.typ("hi")
	if h.m.ai.in.Value() != "hi" {
		t.Fatalf("input = %q", h.m.ai.in.Value())
	}
}

func TestAIGlamourCharacterReferencesAreEscaped(t *testing.T) {
	md := "&#x202E;rev &#x9d;8;;http://e &#x90;dcs &#155;2J &#27;[2J &#x200B;zw and [a link](http://link.example)\n\n" +
		"![img](http://img.example)"
	f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(context.Context, assistant.AnswerRequest, assistant.Events) (assistant.AnswerResult, error) {
		return assistant.AnswerResult{Markdown: md}, nil
	}}
	h := newAIHarness(t, f, nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.openAI()
	h.keys("tab")
	h.askAI("q")
	raw := h.raw()
	tuitest.NoInjectedControls(t, raw)
	for _, r := range []rune{0x202E, 0x9d, 0x90, 0x9b, 0x200B} {
		if strings.ContainsRune(raw, r) {
			t.Fatalf("%U reached the screen", r)
		}
	}
	// Entities never reach goldmark: they show as typed.
	for _, want := range []string{"&#x202E;rev", "&#x9d;8;;", "&#x90;dcs", "http://link.example"} {
		if !strings.Contains(h.text(), want) {
			t.Fatalf("missing %q:\n%s", want, h.text())
		}
	}
	// The same text straight through the renderer and the filter.
	out, err := renderMarkdown(md, h.m.th, 60)
	if err != nil {
		t.Fatal(err)
	}
	clean := sgrOnly(out)
	tuitest.NoInjectedControls(t, clean)
	if strings.ContainsRune(clean, 0x202E) || strings.ContainsRune(clean, 0x9d) || strings.Contains(clean, "]8;") {
		t.Fatalf("sgrOnly left a control: %q", clean)
	}
}

// gatedConsent asks for consent once go is closed.
func gatedConsent(start <-chan struct{}) *assistanttest.Fake {
	return &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
		select {
		case <-start:
		case <-ctx.Done():
			return assistant.AnswerResult{}, ctx.Err()
		}
		c, err := ev.AskConsent(ctx, assistant.ConsentRequest{Conn: req.Scope.Conn, Rows: 3})
		if err != nil {
			return assistant.AnswerResult{}, err
		}
		return assistant.AnswerResult{Markdown: fmt.Sprintf("consent=%d", c)}, nil
	}}
}

func TestAIPromptIgnoresTypeAhead(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := newAIHarness(t, consentFake(), nil)
	h.m.now = func() time.Time { return now }
	h.openAI()
	h.keys("tab")
	h.typ("rows?")
	h.keys("enter")
	h.until("send 3 rows from local")
	if h.m.focus == focusAI {
		t.Fatal("the focus stayed in the panel input while the prompt is open")
	}
	h.typ("abc") // typed ahead, meant for the input
	h.keys("y")
	h.drain(20 * time.Millisecond)
	if h.m.ask == nil || h.m.ai.turn == nil || h.m.ai.in.Value() != "" {
		t.Fatalf("type-ahead answered the prompt or reached the input (%q)", h.m.ai.in.Value())
	}
	now = now.Add(500 * time.Millisecond)
	for _, k := range []string{"x", "enter", "Y", "tab", "q"} {
		h.keys(k)
		if h.m.ask == nil {
			t.Fatalf("%q dismissed the prompt", k)
		}
	}
	h.keys("y")
	h.pump(func() bool { return h.m.ai.turn == nil })
	if !strings.Contains(h.text(), "consent=1") || h.m.focus != focusAI {
		t.Fatalf("y after the grace: focus %v\n%s", h.m.focus, h.text())
	}
}

func TestAIPromptWaitsForTheCellEditor(t *testing.T) {
	start := make(chan struct{})
	h := newAIHarness(t, gatedConsent(start), nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.openColl("local", "tobira_people")
	h.send(palette.AskCmd{Question: "rows?", Answer: true})
	h.pump(func() bool { return len(h.m.asst.(*assistanttest.Fake).Calls()) == 1 })
	h.keys("esc") // to the grid
	h.keys("e")
	if h.m.cell == nil {
		t.Fatal("no cell editor")
	}
	h.typ("draft")
	close(start)
	h.pump(func() bool { return h.m.ai.queued != nil })
	if h.m.cell == nil || h.m.ask != nil {
		t.Fatalf("the AI prompt replaced the cell edit (cell %v)", h.m.cell != nil)
	}
	h.keys("esc") // close the cell editor
	if h.m.cell != nil {
		t.Fatal("esc kept the cell editor")
	}
	if h.m.ask == nil || h.m.ask.consent == nil || h.m.ai.queued != nil {
		t.Fatalf("the queued prompt did not open:\n%s", h.text())
	}
	h.answerAI("n")
	h.pump(func() bool { return h.m.ai.turn == nil })
	if !strings.Contains(h.text(), "consent=0") {
		t.Fatalf("no answer:\n%s", h.text())
	}
}

func TestAIQueuedPromptCancelled(t *testing.T) {
	start := make(chan struct{})
	h := newAIHarness(t, gatedConsent(start), nil)
	h.openAI()
	h.keys("tab")
	h.m.askYN("quit? y/N", "", nil, "")
	h.send(palette.AskCmd{Question: "rows?", Answer: true})
	close(start)
	h.pump(func() bool { return h.m.ai.queued != nil })
	h.m.cancelTurn("cancelled")
	if h.m.ai.queued != nil {
		t.Fatal("the cancelled turn's prompt stayed queued")
	}
	h.keys("n")
	if h.m.ask != nil {
		t.Fatal("a cancelled turn's prompt opened")
	}
}

func TestAIExecuteSwitchReadAtConfirm(t *testing.T) {
	start := make(chan struct{})
	got := make(chan bool, 1)
	f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
		<-start
		ok, err := ev.ConfirmExec(ctx, req.Scope.Conn, "delete from tobira_people")
		got <- ok
		return assistant.AnswerResult{Markdown: "done"}, err
	}}
	h := newAIHarness(t, f, nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.send(palette.AIExecuteCmd{On: true})
	h.openAI()
	h.keys("tab")
	h.typ("delete")
	h.keys("enter")
	h.pump(func() bool { return len(f.Calls()) == 1 })
	h.send(palette.AIExecuteCmd{On: false})
	close(start)
	h.pump(func() bool { return h.m.ai.turn == nil })
	if ok := <-got; ok || h.m.ask != nil {
		t.Fatal("execute confirmed after :ai execute off")
	}
	if !strings.Contains(h.text(), "execute refused: AI execute is off") {
		t.Fatalf("no refusal note:\n%s", h.text())
	}
}

func TestAIEmptyAnswerIsNotHistory(t *testing.T) {
	f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(context.Context, assistant.AnswerRequest, assistant.Events) (assistant.AnswerResult, error) {
		return assistant.AnswerResult{}, nil
	}}
	h := newAIHarness(t, f, nil)
	h.openAI()
	h.keys("tab")
	h.askAI("one")
	h.askAI("two")
	if hist := f.Calls()[1].Req.(assistant.AnswerRequest).History; len(hist) != 0 {
		t.Fatalf("history = %+v", hist)
	}
}

func TestAIViewDoesNotChangeState(t *testing.T) {
	h := newAIHarness(t, askFake(), nil)
	h.openAI()
	h.askAI("who is here?")
	h.m.ai.scroll = 1000
	c := h.m.ai.conv["local"]
	for i := range c.entries {
		c.entries[i].md = nil
	}
	_ = h.m.View()
	if h.m.ai.scroll != 1000 {
		t.Fatal("View clamped the scroll")
	}
	for _, e := range c.entries {
		if e.md != nil {
			t.Fatal("View filled the markdown cache")
		}
	}
	h.send(tickMsg{})
	if h.m.ai.scroll == 1000 {
		t.Fatal("Update did not clamp the scroll")
	}
}

// memAssistant is an Assistant whose sessions remember a ConsentSession,
// like the real one: each Answer asks for consent until it is given for
// the session.
type memAssistant struct{}

type memSession struct{ remembered bool }

func (memAssistant) Model() string                 { return "fake/model" }
func (memAssistant) NewSession() assistant.Session { return &memSession{} }
func (m memAssistant) Ask(ctx context.Context, r assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
	return (&memSession{}).Ask(ctx, r, ev)
}
func (m memAssistant) Answer(ctx context.Context, r assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return (&memSession{}).Answer(ctx, r, ev)
}
func (m memAssistant) ExplainResults(ctx context.Context, r assistant.ExplainResultsRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return (&memSession{}).ExplainResults(ctx, r, ev)
}
func (m memAssistant) FixError(ctx context.Context, r assistant.FixErrorRequest, ev assistant.Events) (assistant.AskResult, error) {
	return (&memSession{}).FixError(ctx, r, ev)
}
func (s *memSession) Ask(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
	return assistant.AskResult{}, nil
}
func (s *memSession) Answer(ctx context.Context, r assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	if !s.remembered {
		c, err := ev.AskConsent(ctx, assistant.ConsentRequest{Conn: r.Scope.Conn, Rows: 2})
		if err != nil {
			return assistant.AnswerResult{}, err
		}
		s.remembered = c == assistant.ConsentSession
	}
	return assistant.AnswerResult{Markdown: "answered"}, nil
}
func (s *memSession) ExplainResults(context.Context, assistant.ExplainResultsRequest, assistant.Events) (assistant.AnswerResult, error) {
	return assistant.AnswerResult{}, nil
}
func (s *memSession) FixError(context.Context, assistant.FixErrorRequest, assistant.Events) (assistant.AskResult, error) {
	return assistant.AskResult{}, nil
}

func TestAIUnchangedReloadAsksForConsentAgain(t *testing.T) {
	h, _ := newConnHarness(t)
	h.m.asst = memAssistant{}
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.openAI()
	h.keys("tab")
	h.typ("one")
	h.keys("enter")
	h.until("send 2 rows from local")
	h.answerAI("a") // always, this session
	h.pump(func() bool { return h.m.ai.turn == nil })
	h.askAI("two") // remembered: no prompt
	if h.m.ask != nil {
		t.Fatal("the session forgot its consent")
	}
	h.send(palette.ReloadCmd{})
	h.pump(func() bool { return !h.m.saving })
	h.send(palette.AskCmd{})
	h.typ("three")
	h.keys("enter")
	h.until("send 2 rows from local")
	h.answerAI("n")
	h.pump(func() bool { return h.m.ai.turn == nil })
}

func TestAIEntitiesRenderLiterally(t *testing.T) {
	md := "fish & chips; x&#27;&#91;8mHID &#x1b;[31mRED &#27;Pdcs &#27;]0;title&#7; a&nbsp;b\n\n`a && b`"
	f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(context.Context, assistant.AnswerRequest, assistant.Events) (assistant.AnswerResult, error) {
		return assistant.AnswerResult{Markdown: md}, nil
	}}
	h := newAIHarness(t, f, nil)
	h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
	h.openAI()
	h.keys("tab")
	h.askAI("q")
	raw := h.raw()
	tuitest.NoInjectedControls(t, raw)
	for _, bad := range []string{"\x1b[8m", "\x1b[31m"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("model-chosen SGR %q reached the screen", bad)
		}
	}
	for _, want := range []string{"x&#27;&#91;8mHID", "&#x1b;[31mRED", "&#27;Pdcs", "&#27;]0;title&#7;", "a&nbsp;b", "fish & chips", "a && b"} {
		if !strings.Contains(h.text(), want) {
			t.Fatalf("missing %q:\n%s", want, h.text())
		}
	}
}

func TestSGROnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"hello", "hello"},
		{"a\x1b[1mb\x1b[0mc", "a\x1b[1mb\x1b[0mc"},
		{"\x1b[1mbold\x1b[0m tail", "\x1b[1mbold\x1b[0m tail"},
		{"x\x1b]8;;http://e\x1b\\link\x1b]8;;\x1b\\ y", "xlink y"},
		{"x\x1b]0;t\x07y", "xy"},
		{"x\x1b[2Jy", "xy"},
		{"x\x1bPdcs text", "xdcs text"},
		{"x\x1b]osc text", "xosc text"},
		{"x\x1bXsos", "xsos"},
		{"x\x1b^pm", "xpm"},
		{"x\x1b_apc", "xapc"},
		{"\x1b\x1b[1mX\x1b[0m", "\x1b[1mX\x1b[0m"},
		{"tail\x1b", "tail"},
		{"x\x1b[12", "x[12"},
		{"r\u202eb\nc\u009d", "r\\u{202E}b\nc\\u009d"},
	} {
		if got := sgrOnly(tc.in); got != tc.want {
			t.Errorf("sgrOnly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAIPromptWaitsForTextInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(h *harness)
		busy func(h *harness) bool
	}{
		{"palette", func(h *harness) { h.keys(":") }, func(h *harness) bool { return h.m.pal.Focused() }},
		{"filter", func(h *harness) { h.keys("f") }, func(h *harness) bool { return h.m.fl.Focused() }},
		{"sidebar filter", func(h *harness) { h.m.focus = focusSidebar; h.keys("/") }, func(h *harness) bool { return h.m.sb.FilterFocused() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := make(chan struct{})
			h := newAIHarness(t, gatedConsent(start), nil)
			h.send(tea.WindowSizeMsg{Width: 200, Height: winH})
			h.openColl("local", "tobira_people")
			h.send(palette.AskCmd{Question: "rows?", Answer: true})
			h.pump(func() bool { return len(h.m.asst.(*assistanttest.Fake).Calls()) == 1 })
			h.keys("esc")
			tc.open(h)
			if !tc.busy(h) {
				t.Fatal("the input did not open")
			}
			close(start)
			h.pump(func() bool { return h.m.ai.queued != nil })
			h.m.ai.queued.opened = time.Time{} // no grace: only the queue protects
			h.typ("yay")
			if h.m.ask != nil || !tc.busy(h) || h.m.ai.turn == nil {
				t.Fatalf("typing in the %s reached the AI prompt", tc.name)
			}
			h.keys("esc")
			if h.m.ask == nil || h.m.ask.consent == nil {
				t.Fatalf("the queued prompt did not open after the %s closed", tc.name)
			}
			h.answerAI("n")
			h.pump(func() bool { return h.m.ai.turn == nil })
		})
	}
}

// A turn is stopped before a write to any connection it touched, not only
// its scope's: a tool call that reports another connection adds it.
func TestAIStopOnTouchedConnection(t *testing.T) {
	f := &assistanttest.Fake{ModelName: "fake/model", AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
		ev.OnToolCall(assistant.ToolCall{Name: "query", Conn: "other", Input: "select 1", Rows: 1})
		<-ctx.Done()
		return assistant.AnswerResult{}, ctx.Err()
	}}
	h := newAIHarness(t, f, nil)
	h.openAI()
	h.keys("tab")
	h.typ("rows?")
	h.keys("enter")
	h.pump(func() bool { return h.m.ai.turn != nil && h.m.ai.turn.conns["other"] })
	h.m.stopAIOn("elsewhere")
	if h.m.ai.turn == nil {
		t.Fatal("a write to an untouched connection stopped the turn")
	}
	h.m.stopAIOn("other")
	if h.m.ai.turn != nil {
		t.Fatal("a write to a connection the turn touched did not stop it")
	}
}

// AI execute runs outside the editor's transaction, so it is refused while
// one is open on its connection: when the model asks, and when a
// transaction opened while the confirmation waited.
func TestAIExecuteRefusedInTransaction(t *testing.T) {
	const why = "a transaction is open on local; commit or roll back first"
	errs := make(chan error, 4)
	f := &assistanttest.Fake{
		ModelName: "fake/model",
		AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			ok, err := ev.ConfirmExec(ctx, req.Scope.Conn, "delete from tobira_people where id = 1")
			errs <- err
			return assistant.AnswerResult{Markdown: fmt.Sprintf("executed=%v", ok)}, nil
		},
	}
	h := newAIHarness(t, f, nil)
	h.openPeople()
	h.send(palette.AIExecuteCmd{On: true})

	// Open when the model asks: refused without a prompt.
	h.beginTx()
	h.send(palette.AskCmd{Answer: true, Question: "delete ada"})
	h.pump(func() bool { return h.m.ai.turn == nil })
	err := <-errs
	if r, ok := errors.AsType[*assistant.ExecuteRefusal](err); !ok || r.Reason != why {
		t.Fatalf("ConfirmExec err = %v, want the refusal %q", err, why)
	}
	if h.m.ask != nil {
		t.Fatal("a confirmation opened while a transaction is open")
	}
	if !strings.Contains(h.flashText(), why) || !strings.Contains(h.text(), "execute refused: "+why) {
		t.Fatalf("flash %q, transcript:\n%s", h.flashText(), h.text())
	}
	h.keys("esc") // leave the panel for the palette
	h.palette("tx rollback")
	h.pump(func() bool { return h.m.tx == nil })

	// Opened while the confirmation waited (queued behind the palette, where
	// the user begins a transaction): refused at the answer.
	h.keys("esc")
	h.keys(":")
	h.typ("tx begin")
	h.send(palette.AskCmd{Answer: true, Question: "delete ada"})
	h.pump(func() bool { return h.m.ai.queued != nil })
	h.keys("enter")
	h.pump(func() bool { return h.m.tx != nil && h.m.tx.tx != nil && h.m.tx.busy == "" && h.m.ask.isAI() })
	h.answerAI("y")
	h.pump(func() bool { return h.m.ai.turn == nil })
	err = <-errs
	if r, ok := errors.AsType[*assistant.ExecuteRefusal](err); !ok || r.Reason != why {
		t.Fatalf("ConfirmExec err = %v, want the refusal %q", err, why)
	}
	if !strings.Contains(h.flashText(), why) {
		t.Fatalf("flash = %q", h.flashText())
	}
}

// The execute confirmation never hides part of its statement: a statement
// longer than the pane scrolls in a box, a line says how much is left, the
// hint stays on screen, and y is refused until the last line was shown.
func TestAIExecuteConfirmShowsTheWholeStatement(t *testing.T) {
	var b strings.Builder
	for i := range 199 {
		fmt.Fprintf(&b, "select %d; -- harmless\n", i)
	}
	b.WriteString("drop table x")
	stmt := b.String()
	answers := make(chan bool, 1)
	f := &assistanttest.Fake{
		ModelName: "fake/model",
		AnswerFn: func(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
			ok, err := ev.ConfirmExec(ctx, req.Scope.Conn, stmt)
			answers <- ok
			return assistant.AnswerResult{Markdown: fmt.Sprintf("executed=%v", ok)}, err
		},
	}
	h := newAIHarness(t, f, nil)
	h.openPeople()
	h.send(palette.AIExecuteCmd{On: true})
	h.send(palette.AskCmd{Answer: true, Question: "clean up"})
	h.until("run this statement on local? it may change data. y/N")
	screen := h.text()
	if strings.Contains(screen, "drop table x") {
		t.Fatalf("the tail is on the first screen of a 200-line statement:\n%s", screen)
	}
	if !regexp.MustCompile(`… \d+ more lines — ↓/PgDn to read`).MatchString(screen) || !strings.Contains(screen, "y runs it · n or esc refuses") {
		t.Fatalf("no remaining-lines line or no hint:\n%s", screen)
	}
	checkFrame(t, h, winW, winH)
	h.answerAI("y")
	if h.m.ask == nil || len(answers) != 0 {
		t.Fatal("y ran a statement the user had not read to the end")
	}
	if !strings.Contains(h.flashText(), "read the whole statement first") {
		t.Fatalf("flash = %q", h.flashText())
	}
	for i := 0; !strings.Contains(h.text(), "drop table x"); i++ {
		if i > 300 {
			t.Fatalf("PgDn never reached the end:\n%s", h.text())
		}
		h.keys("pgdown")
	}
	screen = h.text()
	if !strings.Contains(screen, "end of the statement") || !strings.Contains(screen, "y runs it · n or esc refuses") {
		t.Fatalf("end of the statement:\n%s", screen)
	}
	checkFrame(t, h, winW, winH)
	h.answerAI("y")
	h.pump(func() bool { return h.m.ai.turn == nil })
	if !<-answers {
		t.Fatal("y after reading the whole statement did not run it")
	}
}

// Loading a proposal over unsaved editor text asks first; n keeps the
// buffer. Text that was just run, or that is the proposal, is replaced
// without asking.
func TestAIProposalAsksBeforeReplacingTheBuffer(t *testing.T) {
	h := newAIHarness(t, askFake(), nil)
	h.openAI()
	h.askAI("who is here?")
	h.m.edConn = "local"
	h.m.ed.SetValue("select my_work")
	h.send(ctrl('o'))
	if h.m.ask == nil || !strings.Contains(h.text(), "replace the editor buffer? y/N") {
		t.Fatalf("ctrl+o replaced unsaved text without asking:\n%s", h.text())
	}
	h.keys("n")
	if h.m.ed.Value() != "select my_work" || !strings.Contains(h.flashText(), "kept the editor buffer") {
		t.Fatalf("n: buffer %q, flash %q", h.m.ed.Value(), h.flashText())
	}
	h.m.runBuf["local"] = "select my_work" // as if it had just run
	h.send(palette.AskCmd{})
	h.send(ctrl('o'))
	if h.m.ask != nil || h.m.ed.Value() != proposed {
		t.Fatalf("a buffer that already ran: ask %v, buffer %q", h.m.ask != nil, h.m.ed.Value())
	}
}

// A pane too short to show the statement never lets y run it.
func TestAIExecuteConfirmRefusesYWhenThePaneIsTooShort(t *testing.T) {
	q := &askState{title: "run this statement on local? it may change data. y/N", reason: "delete from t", seenEnd: true}
	if !execUnread(q, 80, 2) {
		t.Fatal("y allowed in a pane with no room for the statement")
	}
	if execUnread(q, 80, 10) {
		t.Fatal("a short statement that fits counts as unread")
	}
}

// y is refused unless every statement rune can be drawn: a pane narrower
// than the box edge plus one wide rune never draws a wide rune whole.
func TestAIExecuteConfirmRefusesYWhenThePaneIsTooNarrow(t *testing.T) {
	for w := 1; w <= 4; w++ {
		q := &askState{title: "run this statement on local? it may change data. y/N", execConn: "local", reason: "删除表x", seenEnd: true}
		got := execUnread(q, w, 60)
		if want := w < 4; got != want {
			t.Errorf("w=%d: unread = %v, want %v", w, got, want)
		}
	}
}

// The line that names the connection is kept whole in a short pane, other
// title lines go first, and y is refused when even it does not fit.
func TestAIExecuteConfirmKeepsTheConnectionLine(t *testing.T) {
	const conn = "production-eu-west-replica"
	title := "run this statement on " + conn + "? it may change data. y/N"
	q := &askState{title: title, execConn: conn, reason: "delete from t", seenEnd: true}
	// At w=20 the question up to the connection takes three lines and the
	// whole title more; six rows leave three for the title.
	head, _, _, whole := execLayout(q, 20, 6)
	if got := strings.Join(head, ""); !whole || len(head) != 3 || !strings.Contains(got, "replica?") || strings.Contains(got, "y/N") {
		t.Fatalf("w=20 h=6: head %q whole %v: want the lines up to the connection, the rest clipped", head, whole)
	}
	if execUnread(q, 20, 6) {
		t.Fatal("y refused although the connection line and the statement are drawn")
	}
	if execUnread(q, 40, 5) {
		t.Fatal("y refused although the connection line and the statement are drawn")
	}
	// At w=10 the question takes several lines before the connection; four
	// rows leave room for one.
	if !execUnread(q, 10, 4) {
		t.Fatal("y allowed while the connection line is clipped")
	}
}

// :ask opens the panel like a.
func TestAskCommandMatchesTheKey(t *testing.T) {
	h := newHarness(t)
	h.send(palette.AskCmd{})
	if !h.m.ai.open || h.m.focus != focusAI {
		t.Fatalf(":ask: open %v focus %v", h.m.ai.open, h.m.focus)
	}
}
