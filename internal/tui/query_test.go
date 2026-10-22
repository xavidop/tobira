package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// fakeHistory records entries in memory and lists them like the store:
// newest first, by connection and case-insensitive substring.
type fakeHistory struct {
	mu      sync.Mutex
	entries []history.Entry
	lists   []history.Filter // every List call
	latency time.Duration    // delays List, honouring ctx
	err     error            // when set, List fails with it
}

func (f *fakeHistory) List(ctx context.Context, flt history.Filter) ([]history.Entry, error) {
	f.mu.Lock()
	f.lists = append(f.lists, flt)
	lat := f.latency
	f.mu.Unlock()
	if lat > 0 {
		select {
		case <-time.After(lat):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []history.Entry
	for i := len(f.entries) - 1; i >= 0 && (flt.Limit <= 0 || len(out) < flt.Limit); i-- {
		e := f.entries[i]
		if flt.Conn != "" && e.Conn != flt.Conn {
			continue
		}
		if (flt.Source != "" && e.Source != flt.Source) || slices.Contains(flt.ExcludeSources, e.Source) {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Query), strings.ToLower(flt.Search)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeHistory) listCalls() []history.Filter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]history.Filter(nil), f.lists...)
}

func (f *fakeHistory) Record(_ context.Context, e history.Entry, _ driver.ConnConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeHistory) all() []history.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]history.Entry(nil), f.entries...)
}

// newQueryHarness is newHarness with a history recorder and optional Fake
// options.
func newQueryHarness(t *testing.T, hist HistoryStore, opts ...drivertest.FakeOption) *harness {
	t.Helper()
	f := drivertest.NewFake(append([]drivertest.FakeOption{drivertest.WithPeople()}, opts...)...)
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "local", Driver: "fake", DSN: "fake://local"},
		{Name: "ro", Driver: "fake", DSN: "fake://ro", ReadOnly: true},
	}}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark", History: hist})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.run(h.m.Init())
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	return h
}

func ctrl(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

var (
	keyF5        = tea.KeyPressMsg{Code: tea.KeyF5}
	keyShiftF5   = tea.KeyPressMsg{Code: tea.KeyF5, Mod: tea.ModShift}
	keyCtrlEnter = tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl}
	keyAltEnter  = tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}
)

// openEditor opens People and the editor, focused, with text in it.
func (h *harness) openEditor(text string) {
	h.t.Helper()
	h.openPeople()
	h.send(ctrl('e'))
	if h.m.focus != focusEditor || !h.m.edOpen {
		h.t.Fatalf("ctrl+e: focus %v open %v", h.m.focus, h.m.edOpen)
	}
	h.send(tea.PasteMsg{Content: text})
}

// runUntilDone sends k and waits for the run to finish.
func (h *harness) runUntilDone(k tea.KeyPressMsg) {
	h.t.Helper()
	h.send(k)
	h.pump(func() bool { return h.m.running == nil })
}

func TestEditorRunStatementOpensTab(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people")
	h.runUntilDone(keyF5)
	if len(h.m.tabs) != 1 || h.m.active != 1 {
		t.Fatalf("tabs = %d active = %d", len(h.m.tabs), h.m.active)
	}
	out := h.text()
	if !strings.Contains(out, "1:select * fro") || !strings.Contains(out, "Ada Lovelace") {
		t.Fatalf("result tab missing:\n%s", out)
	}
	if want := "of " + itoa(len(drivertest.People.Rows)); !strings.Contains(h.statusLine(), want) {
		t.Fatalf("status bar has no row count %q: %q", want, h.statusLine())
	}
	checkFrame(t, h, winW, winH)

	// ctrl+enter runs the statement under the cursor: the second one.
	h.send(tea.PasteMsg{Content: ";\nselect * from tobira_people where id = 2"})
	h.runUntilDone(keyCtrlEnter)
	if len(h.m.tabs) != 2 || h.m.active != 2 {
		t.Fatalf("tabs = %d active = %d", len(h.m.tabs), h.m.active)
	}
	if got := h.m.tabs[1].stmt; got != "select * from tobira_people where id = 2" {
		t.Fatalf("ran %q", got)
	}
	if h.m.tabs[1].rows != 1 {
		t.Fatalf("rows = %d, want 1", h.m.tabs[1].rows)
	}
	// The run used the effective read-only flag of the connection.
	calls := h.fake.Calls()
	if q := calls[len(calls)-1].Query; q.ReadOnly || q.Limit != 1000 {
		t.Fatalf("query options = %+v", q)
	}
}

func TestEditorRunSelection(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people where id = 3; -- c\nselect 1")
	h.m.ed.SetCursor(0)
	// The selection takes the semicolon and the comment: only the statement
	// Split found is sent.
	for range len("select * from tobira_people where id = 3; -- c") {
		h.send(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	}
	h.runUntilDone(keyF5)
	if got := h.m.tabs[0].stmt; got != "select * from tobira_people where id = 3" {
		t.Fatalf("ran %q", got)
	}
	calls := h.fake.Calls()
	if q := calls[len(calls)-1].Query.Text; q != "select * from tobira_people where id = 3" {
		t.Fatalf("driver got %q", q)
	}
}

func TestEditorRunBufferRefusesSeveralStatements(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people; select * from tobira_people")
	h.send(keyAltEnter)
	h.send(tea.WindowSizeMsg{Width: 220, Height: winH}) // room for the whole flash
	if !strings.Contains(h.statusLine(), "the buffer holds 2 statements; put the cursor in one and press ctrl+enter") {
		t.Fatalf("no refusal: %q", h.statusLine())
	}
	if len(h.m.tabs) != 0 || h.m.running != nil {
		t.Fatal("a refused buffer run started")
	}
	// One statement runs.
	h.m.ed.SetValue("select * from tobira_people")
	h.runUntilDone(keyShiftF5)
	if len(h.m.tabs) != 1 {
		t.Fatalf("tabs = %d after a single-statement buffer run", len(h.m.tabs))
	}
}

func TestEditorCtrlCCancelsARun(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people")
	h.fake.SetLatency(5 * time.Second)
	h.send(keyF5)
	h.drain(30 * time.Millisecond)
	if h.m.running == nil {
		t.Fatal("run not in flight")
	}
	h.keys("ctrl+c")
	if h.quitted() {
		t.Fatal("ctrl+c quit while a run was in flight")
	}
	if h.m.running != nil || h.m.tabs[0].state != tabCancelled {
		t.Fatalf("run not cancelled: running=%v state=%v", h.m.running, h.m.tabs[0].state)
	}
	if !strings.Contains(h.text(), "cancelled") {
		t.Fatalf("tab not marked cancelled:\n%s", h.text())
	}
	h.drain(50 * time.Millisecond) // the cancelled result is dropped
	if h.m.tabs[0].state != tabCancelled {
		t.Fatal("a late result overwrote the cancelled tab")
	}
	h.keys("ctrl+c")
	if !h.quitted() {
		t.Fatal("ctrl+c with nothing running did not quit")
	}
}

func TestEditorTabsSwitchAndClose(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people where id = 1")
	h.runUntilDone(keyF5)
	h.runUntilDone(keyF5)
	h.keys("[") // typed into the editor, not a tab switch
	if h.m.active != 2 || !strings.HasSuffix(h.m.ed.Value(), "[") {
		t.Fatalf("[ in the editor: active %d value %q", h.m.active, h.m.ed.Value())
	}
	h.send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	h.keys("esc")
	if h.m.focus != focusGrid || !h.m.edOpen {
		t.Fatalf("esc: focus %v open %v", h.m.focus, h.m.edOpen)
	}
	h.keys("[")
	if h.m.active != 1 {
		t.Fatalf("[ -> active %d, want 1", h.m.active)
	}
	h.keys("[")
	if h.m.active != 0 || !strings.Contains(h.text(), "Ada Lovelace") {
		t.Fatalf("[ -> active %d, want the browse tab", h.m.active)
	}
	h.keys("x")
	if len(h.m.tabs) != 2 || !strings.Contains(h.statusLine(), "the browse tab cannot be closed") {
		t.Fatalf("x on the browse tab: tabs %d, status %q", len(h.m.tabs), h.statusLine())
	}
	h.keys("[") // wraps to the last tab
	if h.m.active != 2 {
		t.Fatalf("[ wrap -> active %d", h.m.active)
	}
	h.keys("]") // wraps to the browse tab
	if h.m.active != 0 {
		t.Fatalf("] wrap -> active %d", h.m.active)
	}
	h.keys("]", "x")
	if len(h.m.tabs) != 1 || h.m.active != 1 {
		t.Fatalf("x: tabs %d active %d", len(h.m.tabs), h.m.active)
	}
	checkFrame(t, h, winW, winH)
}

func TestEditorTenthRunClosesTheOldestTab(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people where id = 1")
	for range 9 {
		h.runUntilDone(keyF5)
	}
	first := h.m.tabs[0]
	h.runUntilDone(keyF5)
	if len(h.m.tabs) != 9 || h.m.tabs[0] == first || h.m.active != 9 {
		t.Fatalf("tabs %d active %d (oldest kept: %v)", len(h.m.tabs), h.m.active, h.m.tabs[0] == first)
	}
	if !strings.Contains(h.text(), "9:select") || strings.Contains(h.text(), "10:") {
		t.Fatalf("tab labels:\n%s", h.text())
	}
}

func TestEditorSetLimit(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people")
	h.keys("esc", ":")
	h.typ("set limit 2")
	h.keys("enter")
	h.pump(func() bool { return h.m.limit == 2 })
	if h.m.limit != 2 || !strings.Contains(h.statusLine(), "row limit set to 2") {
		t.Fatalf("limit %d, status %q", h.m.limit, h.statusLine())
	}
	h.runUntilDone(keyF5)
	tb := h.m.tabs[0]
	if tb.rows != 2 || !tb.truncated {
		t.Fatalf("rows %d truncated %v", tb.rows, tb.truncated)
	}
	if !strings.Contains(h.text(), "Truncated") {
		t.Fatalf("Truncated not shown:\n%s", h.text())
	}
	h.keys(":")
	h.typ("set limit 0")
	h.keys("enter")
	if h.m.limit != 2 || !strings.Contains(h.text(), "limit must be a whole number from 1 to 100000") {
		t.Fatalf("bad limit accepted: %d\n%s", h.m.limit, h.text())
	}
}

func TestEditorExplain(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people")
	h.keys("esc", ":")
	h.typ("explain")
	h.keys("enter")
	h.pump(func() bool { return h.m.running == nil && len(h.m.tabs) == 1 })
	tb := h.m.tabs[0]
	if !tb.isText || !strings.Contains(h.text(), "FAKE") {
		t.Fatalf("explain tab:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
}

func TestEditorExplainNotSupported(t *testing.T) {
	h := newQueryHarness(t, nil, drivertest.WithoutCapabilities(driver.CapExplainer))
	h.openEditor("select * from tobira_people")
	h.keys("esc", ":")
	h.typ("explain")
	h.keys("enter")
	h.pump(func() bool { return h.m.running == nil && len(h.m.tabs) == 1 })
	if !strings.Contains(h.text(), "explain is not supported by fake") {
		t.Fatalf("no unsupported message:\n%s", h.text())
	}
}

func TestEditorReadOnlyRefusal(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.keys("down", "enter") // expand ro
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.keys("down", "enter")
	h.until("Ada Lovelace")
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "delete from tobira_people where id = 1"})
	h.runUntilDone(keyF5)
	calls := h.fake.Calls()
	if q := calls[len(calls)-1].Query; !q.ReadOnly {
		t.Fatalf("query on a read-only connection without ReadOnly: %+v", q)
	}
	if !strings.Contains(h.text(), "🔒 ro is read-only; the statement was refused") {
		t.Fatalf("no read-only message:\n%s", h.text())
	}
	h.keys("esc", ":")
	h.typ("explain")
	h.keys("enter")
	h.pump(func() bool { return len(h.m.tabs) == 2 && h.m.running == nil })
	calls = h.fake.Calls()
	if c := calls[len(calls)-1]; c.Op != "explain" || !c.Query.ReadOnly {
		t.Fatalf("explain on a read-only connection: %+v", c)
	}
}

func TestEditorRunsAreRecordedInHistory(t *testing.T) {
	hist := &fakeHistory{}
	h := newQueryHarness(t, hist)
	h.openEditor("select * from tobira_people")
	h.runUntilDone(keyF5)
	// Each record is stored by its own command, off the Update goroutine,
	// so two runs in flight may land in either order: wait for the run's
	// entry before starting the explain.
	h.pump(func() bool { return len(hist.all()) == 1 })
	if got := hist.all()[0].Rows; got != int64(len(drivertest.People.Rows)) {
		t.Fatalf("rows recorded = %d", got)
	}
	h.keys("esc", ":")
	h.typ("explain")
	h.keys("enter")
	h.pump(func() bool { return h.m.running == nil && len(h.m.tabs) == 2 })
	h.pump(func() bool { return len(hist.all()) == 2 })
	for i, e := range hist.all() {
		if e.Source != history.SourceTUI || e.Conn != "local" || e.Query != "select * from tobira_people" || !e.OK {
			t.Fatalf("entry %d = %+v", i, e)
		}
	}
}

func TestEditorErrorIsRedactedAndEscaped(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.fake.OnQuery("boom", func(driver.Query) (driver.ResultSet, error) {
		return nil, driver.Wrap(driver.ErrSyntax, "fake", "query",
			&textErr{"failed for fake://u:hunter22@db \x1b[2Jcleared"})
	})
	h.openEditor("boom")
	h.runUntilDone(keyF5)
	raw := h.raw()
	if strings.Contains(raw, "hunter22") || strings.Contains(raw, "\x1b[2J") {
		t.Fatalf("error not redacted/escaped:\n%s", ansi.Strip(raw))
	}
	checkFrame(t, h, winW, winH)
}

type textErr struct{ s string }

func (e *textErr) Error() string { return e.s }

func TestEditorPastedControlsAreEscaped(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select '\x1b]52;c;ZXZpbA==\x07' \x9b\u009b")
	checkFrame(t, h, winW, winH)
	if !strings.Contains(h.text(), `\x1b]52`) {
		t.Fatalf("control not shown as an escape:\n%s", h.text())
	}
}

func TestEditorBufferPerConnection(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.bothExpanded() // local's people open, the sidebar cursor on ro's
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select 'local'"})
	h.keys("esc", "shift+tab", "enter") // open ro's people
	h.pump(func() bool { return h.m.cur.conn == "ro" })
	if v := h.m.ed.Value(); v != "" {
		t.Fatalf("ro's editor shows %q", v)
	}
	if d := h.m.ed.Dialect(); d != "fake" {
		t.Fatalf("dialect = %q", d)
	}
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select 'ro'"})
	h.keys("esc", "shift+tab", "up", "up", "up", "enter") // back to local's people
	h.pump(func() bool { return h.m.cur.conn == "local" })
	if v := h.m.ed.Value(); v != "select 'local'" {
		t.Fatalf("local's buffer = %q", v)
	}
	h.keys("shift+tab", "down", "down", "down", "enter")
	h.pump(func() bool { return h.m.cur.conn == "ro" })
	if v := h.m.ed.Value(); v != "select 'ro'" {
		t.Fatalf("ro's buffer = %q", v)
	}
}

func TestEditorToggleAndShortTerminal(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openPeople()
	h.keys(":")
	h.typ("editor")
	h.keys("enter")
	h.pump(func() bool { return h.m.edOpen })
	if !h.m.edOpen || h.m.focus != focusEditor {
		t.Fatal(":editor did not open the editor")
	}
	l := h.m.lay
	if l.editor.h < minEditorH || l.editor.h+1 < l.col.h*2/5 {
		t.Fatalf("editor rect %+v in column %+v", l.editor, l.col)
	}
	checkFrame(t, h, winW, winH)
	h.send(tea.WindowSizeMsg{Width: 80, Height: 12})
	if h.m.lay.grid.h != 0 || h.m.lay.edTitle.y != h.m.lay.col.y {
		t.Fatalf("short terminal: grid %+v editor title %+v", h.m.lay.grid, h.m.lay.edTitle)
	}
	checkFrame(t, h, 80, 12)
	h.send(ctrl('e'))
	if h.m.edOpen || h.m.focus != focusGrid {
		t.Fatalf("ctrl+e in the editor: open %v focus %v", h.m.edOpen, h.m.focus)
	}
	checkFrame(t, h, 80, 12)
}

func TestEditorMouse(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people")
	h.runUntilDone(keyF5)
	h.keys("esc")
	l := h.m.lay
	// Click the browse tab label at the left of the tab bar.
	h.send(tea.MouseClickMsg{X: l.tabs.x + 1, Y: l.tabs.y, Button: tea.MouseLeft})
	if h.m.active != 0 {
		t.Fatalf("click on the browse tab: active %d", h.m.active)
	}
	h.send(tea.MouseClickMsg{X: l.editor.x + 6, Y: l.editor.y, Button: tea.MouseLeft})
	if h.m.focus != focusEditor {
		t.Fatalf("click in the editor: focus %v", h.m.focus)
	}
	if c := h.m.ed.Cursor(); c == len(h.m.ed.Value()) {
		t.Fatalf("click did not move the cursor (%d)", c)
	}
}

func TestEditorGolden(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people\nwhere id = 1")
	h.runUntilDone(keyF5)
	checkFrame(t, h, winW, winH)
	golden(t, "editor_result_tab", h.text())
}

func TestNonBrowserPointsAtTheEditor(t *testing.T) {
	h := newQueryHarness(t, nil, drivertest.WithoutCapabilities(driver.CapBrowser))
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.keys("down", "enter")
	h.until("browsing not supported by fake; open the editor with ctrl+e")
}

// TestProgramEditorFlow runs the real program under teatest: open the
// editor, type, run with F5 and see the result tab.
func TestProgramEditorFlow(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(winW, winH))
	waitFor := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return strings.Contains(ansi.Strip(string(b)), s) }, teatest.WithDuration(5*time.Second))
	}
	waitFor("connections")
	tm.Send(key("enter"))
	waitFor("main")
	tm.Send(ctrl('e'))
	waitFor("editor · local")
	for _, r := range "select * from tobira_people" {
		tm.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	tm.Send(keyF5)
	waitFor("Ada Lovelace")
	tm.Send(key("esc"))
	tm.Send(key("q"))
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if len(final.tabs) != 1 || final.tabs[0].rows != len(drivertest.People.Rows) {
		t.Fatalf("final tabs: %d", len(final.tabs))
	}
}

func TestEditorTinyWindows(t *testing.T) {
	sizes := [][2]int{{20, 5}, {1, 1}, {10, 2}, {59, 10}, {60, 15}, {60, 16}, {5, 40}, {200, 3}, {100, 7}}
	for _, sz := range sizes {
		t.Run(itoa(sz[0])+"x"+itoa(sz[1]), func(t *testing.T) {
			h := newQueryHarness(t, nil)
			h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			h.send(ctrl('e'))
			h.send(tea.PasteMsg{Content: "select * from tobira_people\nwhere id = 1"})
			checkFrame(t, h, sz[0], sz[1])
			h.send(keyF5)
			h.drain(100 * time.Millisecond)
			checkFrame(t, h, sz[0], sz[1])
			h.keys("esc", "[", "]")
			checkFrame(t, h, sz[0], sz[1])
			h.send(tea.MouseClickMsg{X: sz[0] - 1, Y: sz[1] - 2, Button: tea.MouseLeft})
			checkFrame(t, h, sz[0], sz[1])
		})
	}
}

func TestStatementAt(t *testing.T) {
	text := "select 1;  select 2;\n\nselect 3"
	spans := sqlcomplete.Split(sqlcomplete.SQLite(), text)
	for _, tc := range []struct {
		cursor int
		want   string
	}{
		{0, "select 1"}, {8, "select 1"}, {9, "select 1"}, {10, "select 1"},
		{11, "select 2"}, {19, "select 2"}, {21, "select 2"}, {22, "select 3"}, {len(text), "select 3"},
	} {
		s := statementAt(spans, tc.cursor)
		if got := text[s.Start:s.End]; got != tc.want {
			t.Errorf("cursor %d: %q, want %q", tc.cursor, got, tc.want)
		}
	}
	lead := "  \nselect 9"
	if s := statementAt(sqlcomplete.Split(sqlcomplete.SQLite(), lead), 0); lead[s.Start:s.End] != "select 9" {
		t.Errorf("cursor before the first statement: %q", lead[s.Start:s.End])
	}
}

// queryCall returns the last query call the Fake recorded.
func (h *harness) lastQuery() drivertest.Call {
	h.t.Helper()
	calls := h.fake.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Op == "query" {
			return calls[i]
		}
	}
	h.t.Fatal("no query call")
	return drivertest.Call{}
}

// startSlowRun starts a run that waits on the Fake's latency.
func (h *harness) startSlowRun() {
	h.t.Helper()
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select * from tobira_people"})
	h.fake.SetLatency(5 * time.Second)
	h.send(keyF5)
	h.pump(func() bool {
		calls := h.fake.Calls()
		return len(calls) > 0 && calls[len(calls)-1].Op == "query"
	})
	if h.m.running == nil {
		h.t.Fatal("run not in flight")
	}
}

// runCancelledBeforeClose waits for the slow run's driver call to end with
// a cancellation, which proves its context was cancelled before the
// connection was closed (a closed Fake connection answers ErrConnection, a
// finished call nil).
func (h *harness) runCancelledBeforeClose() {
	h.t.Helper()
	h.pump(func() bool { return h.lastQuery().Err != nil })
	if err := h.lastQuery().Err; !errors.Is(err, context.Canceled) {
		h.t.Fatalf("run call ended with %v, want a cancellation", err)
	}
}

func TestEditorRunCancelledBeforeConnectionEdit(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.startSlowRun()
	h.keys("esc")
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("E")
	h.typ("2") // a rename closes the connection
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	h.send(<-h.ch) // the SavedMsg: the write starts
	if h.m.running != nil || h.m.tabs[0].state != tabCancelled {
		t.Fatalf("run not cancelled before the write: running=%v", h.m.running != nil)
	}
	h.pump(func() bool { return !h.m.saving })
	h.runCancelledBeforeClose()
	h.drain(50 * time.Millisecond)
	if h.m.tabs[0].state != tabCancelled {
		t.Fatal("a late result landed in the cancelled tab")
	}
}

func TestEditorRunCancelledBeforeConnectionDelete(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.startSlowRun()
	h.keys("esc")
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("D")
	h.typ("local")
	h.keys("enter")
	if h.m.running != nil || h.m.tabs[0].state != tabCancelled {
		t.Fatal("run not cancelled before the delete")
	}
	h.pump(func() bool { return !h.m.saving })
	h.runCancelledBeforeClose()
}

func TestEditorRunCancelledBeforeReload(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.startSlowRun()
	h.keys("esc", ":")
	h.typ("reload")
	h.keys("enter")
	h.pump(func() bool { return h.m.saving || h.m.running == nil })
	if h.m.running != nil || h.m.tabs[0].state != tabCancelled {
		t.Fatal("run not cancelled before the reload")
	}
	h.pump(func() bool { return !h.m.saving })
	h.runCancelledBeforeClose()
}

func TestEditorCtrlCDuringARunCancelsOnlyTheRun(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people")
	h.fake.SetLatency(5 * time.Second)
	h.keys("esc", "s") // a browse page load starts
	if !h.m.gr.Loading() {
		t.Fatal("no browse load")
	}
	h.send(ctrl('e'))
	h.send(keyF5)
	h.keys("ctrl+c")
	if h.m.running != nil || !h.m.gr.Loading() {
		t.Fatalf("ctrl+c: running=%v browse loading=%v", h.m.running != nil, h.m.gr.Loading())
	}
	h.keys("ctrl+c") // now the browse load
	if h.quitted() || h.m.gr.Loading() {
		t.Fatal("second ctrl+c did not cancel the browse load")
	}
}

func TestEditorClosingARunningTabRecordsAndDropsTheResult(t *testing.T) {
	hist := &fakeHistory{}
	h := newQueryHarness(t, hist)
	h.openEditor("select * from tobira_people")
	h.fake.SetLatency(300 * time.Millisecond)
	h.send(keyF5)
	h.keys("esc", "x")
	if len(h.m.tabs) != 0 || h.m.running != nil {
		t.Fatalf("tabs %d running %v", len(h.m.tabs), h.m.running != nil)
	}
	h.pump(func() bool { return len(hist.all()) == 1 })
	if e := hist.all()[0]; e.OK || e.Source != history.SourceTUI || e.Query != "select * from tobira_people" {
		t.Fatalf("entry = %+v", e)
	}
	h.drain(400 * time.Millisecond)
	if len(h.m.tabs) != 0 || len(hist.all()) != 1 {
		t.Fatalf("the closed run came back: tabs %d entries %d", len(h.m.tabs), len(hist.all()))
	}
}

type failingHistory struct{ calls int }

func (f *failingHistory) Record(context.Context, history.Entry, driver.ConnConfig) error {
	f.calls++
	return errors.New("disk full")
}

func (f *failingHistory) List(context.Context, history.Filter) ([]history.Entry, error) {
	return nil, errors.New("disk full")
}

func TestEditorHistoryFailureIsNotFatal(t *testing.T) {
	hist := &failingHistory{}
	h := newQueryHarness(t, hist)
	h.openEditor("select * from tobira_people")
	h.runUntilDone(keyF5)
	h.drain(50 * time.Millisecond)
	if h.m.tabs[0].state != tabDone || !strings.Contains(h.text(), "Ada Lovelace") {
		t.Fatalf("run failed with history failing:\n%s", h.text())
	}
	if strings.Contains(h.text(), "disk full") {
		t.Fatal("a history failure was shown")
	}
}

func TestEditorOwnsLetterKeys(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("")
	h.typ("qxeDnE?:/fsyY[]")
	if got := h.m.ed.Value(); got != "qxeDnE?:/fsyY[]" {
		t.Fatalf("Value = %q", got)
	}
	if h.quitted() || h.m.form != nil || h.m.confirm != nil || h.m.help || h.m.pal.Focused() || h.m.fl.Focused() {
		t.Fatal("a global binding fired while typing")
	}
}

func TestEditorWheelScrolls(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor(strings.Repeat("select 1;\n", 40))
	h.m.ed.SetCursor(0)
	l := h.m.lay
	h.send(tea.MouseWheelMsg{X: l.editor.x + 3, Y: l.editor.y + 1, Button: tea.MouseWheelDown})
	if h.m.ed.Cursor() == 0 {
		t.Fatal("the wheel did not move the editor")
	}
}

// dialectDriver is the Fake under another name and dialect.
type dialectDriver struct {
	driver.Driver
	name, dialect string
}

func (d dialectDriver) Info() driver.Info {
	i := d.Driver.Info()
	i.Name, i.Dialect, i.Schemes = d.name, d.dialect, []string{d.name}
	return i
}

func TestEditorDialectFollowsAReload(t *testing.T) {
	registerKeyring()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(connSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, stat, err := config.LoadWithStat(path)
	if err != nil {
		t.Fatal(err)
	}
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	_ = reg.Add(dialectDriver{Driver: f.Driver(), name: "pgfake", dialect: "postgresql"})
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark", ConfigPath: path, ConfigStat: stat})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	h.send(ctrl('e'))
	if d := h.m.ed.Dialect(); d != "fake" {
		t.Fatalf("dialect = %q", d)
	}
	edited := strings.Replace(connSeed, "    driver: fake\n    dsn: fake://local", "    driver: pgfake\n    dsn: pgfake://local", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys("esc", ":")
	h.typ("reload")
	h.keys("enter")
	h.until("reloaded the config file")
	if d := h.m.ed.Dialect(); d != "postgresql" {
		t.Fatalf("dialect after the reload = %q, want postgresql", d)
	}
}

// Quitting cancels an editor run in flight, and that run is recorded
// (ok=false) before the program exits, so the history holds every run.
func TestQuitRecordsTheCancelledRun(t *testing.T) {
	hist := &fakeHistory{}
	h := newQueryHarness(t, hist)
	h.openEditor("select * from tobira_people")
	h.fake.SetLatency(5 * time.Second)
	h.send(keyF5)
	h.pump(func() bool { return h.m.running != nil })
	h.keys("esc")
	h.keys("q")
	h.pump(func() bool { return h.didQuit })
	es := hist.all()
	if len(es) != 1 || es[0].Query != "select * from tobira_people" || es[0].OK || es[0].Conn != "local" {
		t.Fatalf("history when the program quit = %+v", es)
	}
}

// A run still in flight when the program stopped another way (its
// context cancelled) is recorded by Run's exit path.
func TestExitRecordsTheCancelledRun(t *testing.T) {
	hist := &fakeHistory{}
	h := newQueryHarness(t, hist)
	h.openEditor("select * from tobira_people")
	h.fake.SetLatency(5 * time.Second)
	h.send(keyF5)
	h.pump(func() bool { return h.m.running != nil })
	h.m.recordRunAtExit() // the program has stopped: no Update races this
	es := hist.all()
	if len(es) != 1 || es[0].OK || h.m.running != nil {
		t.Fatalf("history after the exit = %+v, running %v", es, h.m.running != nil)
	}
}
