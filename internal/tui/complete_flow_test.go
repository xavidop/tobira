package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

var keyCtrlSpace = tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl}

// catalogDriver is the Fake with a Completer over a fixed catalog and an
// Invalidate counter, like the SQL drivers' catalog cache.
type catalogDriver struct {
	driver.Driver
	cat *catalog
}

type catalog struct {
	names       []string
	latency     time.Duration
	completes   atomic.Int32
	invalidated atomic.Int32
	// gate, when set, holds every Complete until it is closed, ignoring
	// the context, like a driver slow to unwind.
	gate chan struct{}
}

func (d catalogDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	b, _ := c.(driver.Browser)
	return &catalogConn{Conn: c, Browser: b, cat: d.cat}, nil
}

type catalogConn struct {
	driver.Conn
	driver.Browser
	cat *catalog
}

// Complete returns every catalog name starting with the word before the
// cursor, after the injected latency.
func (c *catalogConn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	c.cat.completes.Add(1)
	if c.cat.gate != nil {
		<-c.cat.gate
	}
	if c.cat.latency > 0 {
		select {
		case <-time.After(c.cat.latency):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	start := req.Cursor
	for start > 0 && isIdentRune(rune(req.Text[start-1])) {
		start--
	}
	prefix := req.Text[start:req.Cursor]
	var out []driver.Completion
	for _, n := range c.cat.names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, driver.Completion{Label: n, Kind: "collection"})
		}
	}
	return out, nil
}

func (c *catalogConn) Invalidate() { c.cat.invalidated.Add(1) }

// newCatalogHarness is newQueryHarness on a catalogDriver.
func newCatalogHarness(t *testing.T, cat *catalog, hist HistoryStore) *harness {
	t.Helper()
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(catalogDriver{Driver: f.Driver(), cat: cat}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}
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

func (h *harness) popupLabels() []string {
	var out []string
	for _, it := range h.m.cp.items {
		out = append(out, it.Label)
	}
	return out
}

func (h *harness) waitPopup() {
	h.t.Helper()
	h.pump(func() bool { return h.m.popupShown() })
}

func TestCompletionCtrlSpaceAndTab(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tob")
	if h.m.cp.open {
		t.Fatal("a paste opened the popup")
	}
	h.send(keyCtrlSpace)
	h.waitPopup()
	if got := h.popupLabels(); len(got) != 1 || got[0] != "tobira_people" {
		t.Fatalf("popup = %v", got)
	}
	if !strings.Contains(h.text(), "tobira_people  collection · main") {
		t.Fatalf("popup not drawn:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	h.keys("tab")
	if got := h.m.ed.Value(); got != "select * from tobira_people" {
		t.Fatalf("tab accepted into %q", got)
	}
	if h.m.cp.open {
		t.Fatal("the popup stayed open after accepting")
	}
	// With the popup closed, tab moves on to the next pane instead of
	// touching the text.
	h.keys("tab")
	if got := h.m.ed.Value(); got != "select * from tobira_people" {
		t.Fatalf("tab without the popup changed the text to %q", got)
	}
	if h.m.focus == focusEditor {
		t.Fatal("tab without the popup left the focus in the editor")
	}
}

func TestCompletionAutoOpens(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("")
	h.typ("s")
	h.drain(150 * time.Millisecond)
	if h.m.cp.open {
		t.Fatal("one character opened the popup")
	}
	h.typ("e")
	h.waitPopup()
	if got := h.popupLabels(); len(got) == 0 || got[0] != "select" {
		t.Fatalf("popup after two characters = %v", got)
	}
	h.keys("enter") // accepts, no newline
	if got := h.m.ed.Value(); got != "select" {
		t.Fatalf("enter accepted into %q", got)
	}
	// After "." the open collection's fields, from the schema cache.
	h.typ(" tobira_people.")
	// A slow machine may show a lookup for part of the name first.
	h.pump(func() bool { return h.m.popupShown() && h.m.cp.items[0].Label == "id" })
	got := h.popupLabels()
	if len(got) != 8 || got[0] != "id" || got[1] != "name" {
		t.Fatalf("fields after the dot = %v", got)
	}
	h.keys("down", "down")
	h.send(ctrl('p'))
	h.send(ctrl('n'))
	h.send(ctrl('n'))
	h.keys("enter")
	if v := h.m.ed.Value(); v != "select tobira_people.age" {
		t.Fatalf("accepted %q", v)
	}
	// With the popup closed, enter is a newline again.
	h.keys("enter")
	if v := h.m.ed.Value(); v != "select tobira_people.age\n" {
		t.Fatalf("enter without the popup = %q", v)
	}
}

func TestCompletionClosing(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tob")
	h.send(keyCtrlSpace)
	h.waitPopup()
	// esc closes the popup only; a second esc leaves the editor.
	h.keys("esc")
	if h.m.cp.open || h.m.focus != focusEditor {
		t.Fatalf("esc: open %v focus %v", h.m.cp.open, h.m.focus)
	}
	h.keys("esc")
	if h.m.focus != focusGrid {
		t.Fatal("the second esc did not return to the grid")
	}
	h.send(ctrl('e'))
	// Moving off the token closes it.
	h.send(keyCtrlSpace)
	h.waitPopup()
	h.send(tea.KeyPressMsg{Code: tea.KeyHome})
	if h.m.cp.open {
		t.Fatal("moving off the token left the popup open")
	}
	// An empty answer closes it.
	h.m.ed.SetValue("select * from tob")
	h.send(keyCtrlSpace)
	h.waitPopup()
	h.typ("zz")
	h.pump(func() bool { return !h.m.cp.open })
	// A lookup inside a string does nothing.
	h.m.ed.SetValue("select 'tob")
	h.send(keyCtrlSpace)
	h.drain(100 * time.Millisecond)
	if h.m.cp.open {
		t.Fatal("completion inside a string")
	}
}

func TestCompletionWithoutCompleterUsesTheSchemaCache(t *testing.T) {
	h := newQueryHarness(t, nil, drivertest.WithoutCapabilities(driver.CapCompleter))
	h.openEditor("select * from TOB")
	h.send(keyCtrlSpace)
	h.waitPopup()
	if got := h.popupLabels(); len(got) != 1 || got[0] != "tobira_people" {
		t.Fatalf("popup = %v", got)
	}
	for _, c := range h.fake.Calls() {
		if c.Op == "complete" {
			t.Fatal("the Completer was called on a driver without it")
		}
	}
}

func TestCompletionEscapesLabels(t *testing.T) {
	h := newCatalogHarness(t, &catalog{names: []string{"ev\x1b]52;c;aGk=\x07il", "ev\u202eok"}}, nil)
	h.openPeople()
	h.send(ctrl('e'))
	h.typ("ev")
	h.waitPopup()
	checkFrame(t, h, winW, winH)
	if !strings.Contains(h.text(), `ev\x1b]52;c;aGk=\x07il`) || !strings.Contains(h.text(), `ev\u{202E}ok`) {
		t.Fatalf("label not escaped:\n%s", h.text())
	}
}

// TestCompletionResponsiveness types 20 characters against a 100k-name
// catalog that answers in 200 ms. Command results are fed back between
// keystrokes, as Bubble Tea does, and some pauses are longer than the
// debounce, so lookups start and are still running while later keys
// arrive. Every Update, keystroke or result, must stay within the budget:
// a lookup that ran on the Update goroutine would blow it by 200 ms.
func TestCompletionResponsiveness(t *testing.T) {
	// The timing is retried: one attempt can lose to a loaded machine (a GC
	// pause, a descheduled test while `make test` runs in parallel), but a
	// lookup on the Update goroutine blocks for the catalog's 200ms latency
	// in every attempt, so it still fails all of them.
	retryTiming(t, timingAttempts, func(t *testing.T) string { return completionTiming(t) })
}

// timingAttempts is how many times a timing measurement runs before its
// test fails.
const timingAttempts = 3

// retryTiming runs measure up to attempts times. measure fails t itself for
// a functional fault and returns "" when the timing held or why it did not;
// t fails only when every attempt missed the timing.
func retryTiming(t *testing.T, attempts int, measure func(*testing.T) string) {
	t.Helper()
	var misses []string
	for i := range attempts {
		why := measure(t)
		if t.Failed() {
			return
		}
		if why == "" {
			if i > 0 {
				t.Logf("timing held on attempt %d after: %s", i+1, strings.Join(misses, "; "))
			}
			return
		}
		misses = append(misses, why)
	}
	t.Errorf("timing missed in all %d attempts: %s", attempts, strings.Join(misses, "; "))
}

// completionTiming types a query against a slow, large catalog and times
// every Update. It returns "" when the typical Update kept within
// keystrokeBudget and none came near the lookup latency, else why not.
func completionTiming(t *testing.T) string {
	cat := &catalog{latency: 200 * time.Millisecond}
	for i := range 100_000 {
		cat.names = append(cat.names, fmt.Sprintf("col_%05d", i))
	}
	h := newCatalogHarness(t, cat, nil)
	h.openPeople()
	h.send(ctrl('e'))
	var took []time.Duration
	timed := func(msg tea.Msg) {
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				h.run(c)
			}
			return
		}
		start := time.Now()
		_, cmd := h.m.Update(msg)
		took = append(took, time.Since(start))
		h.run(cmd)
	}
	// feed delivers command results for d, timing each Update.
	feed := func(d time.Duration) {
		end := time.After(d)
		for {
			select {
			case msg := <-h.ch:
				timed(msg)
			case <-end:
				return
			}
		}
	}
	gaps := []time.Duration{30, 120, 30, 180, 90} // ms; 120, 180 and 90 outlast the debounce
	for i, r := range "select * from col_00" {
		timed(tea.KeyPressMsg{Code: r, Text: string(r)})
		feed(gaps[i%len(gaps)] * time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	ready := func() bool {
		return h.m.popupShown() && len(h.m.cp.items) > 0 && strings.HasPrefix(h.m.cp.items[0].Label, "col_00")
	}
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("no popup for col_00:\n%s", h.text())
		}
		feed(10 * time.Millisecond)
	}
	if n := cat.completes.Load(); n < 2 {
		t.Fatalf("Completer calls = %d, want lookups to overlap typing (>= 2)", n)
	}
	if n := len(h.m.cp.items); n != maxCompletions {
		t.Fatalf("popup holds %d items, want %d", n, maxCompletions)
	}
	for _, it := range h.m.cp.items {
		if !strings.HasPrefix(it.Label, "col_00") {
			t.Fatalf("a stale answer was shown: %q", it.Label)
		}
	}
	checkFrame(t, h, winW, winH)
	// One slow Update (a GC pause, a descheduled test) is noise; a lookup
	// on the Update goroutine blocks for the catalog's 200ms latency every
	// time. So: the typical Update within the budget, and none anywhere
	// near the latency.
	slices.Sort(took)
	p95, slowest := took[(len(took)*95)/100], took[len(took)-1]
	t.Logf("%d updates, p95 %v, slowest %v, %d Completer calls", len(took), p95, slowest, cat.completes.Load())
	var why []string
	if p95 > keystrokeBudget {
		why = append(why, fmt.Sprintf("p95 Update took %v, budget %v", p95, keystrokeBudget))
	}
	if slowest > keystrokeCeiling {
		why = append(why, fmt.Sprintf("slowest Update took %v, ceiling %v", slowest, keystrokeCeiling))
	}
	return strings.Join(why, ", ")
}

// TestCompletionStaleResultDropped answers a lookup after the buffer has
// changed: the answer must not be shown.
func TestCompletionStaleResultDropped(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tob")
	cmd := h.m.lookup()
	msg := cmd() // the answer for "tob"
	h.typ("i")
	h.send(msg)
	if h.m.cp.open {
		t.Fatal("an answer for an older buffer was shown")
	}
	// The same answer is dropped after a cursor move too.
	cmd = h.m.lookup()
	msg = cmd()
	h.send(tea.KeyPressMsg{Code: tea.KeyLeft})
	h.send(msg)
	if h.m.cp.open {
		t.Fatal("an answer for another cursor was shown")
	}
}

func TestReloadInvalidatesCompletion(t *testing.T) {
	cat := &catalog{names: []string{"alpha"}}
	h := newCatalogHarness(t, cat, nil)
	h.openPeople()
	h.send(ctrl('e'))
	h.send(keyCtrlSpace)
	h.drain(50 * time.Millisecond)
	if h.m.schema["local"] == nil {
		t.Fatal("no schema cache after a lookup")
	}
	h.keys("esc", "esc", ":") // the first esc closes the popup
	h.typ("reload")
	h.keys("enter")
	h.pump(func() bool { return cat.invalidated.Load() == 1 })
	if h.m.schema["local"] != nil {
		t.Fatal(":reload left the TUI schema cache")
	}
	// A statement with no result columns (DDL or DML) invalidates too.
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "insert into tobira_people values (1)"})
	h.runUntilDone(keyF5)
	h.pump(func() bool { return cat.invalidated.Load() == 2 })
	// A query with columns does not.
	h.m.ed.SetValue("select * from tobira_people")
	h.runUntilDone(keyF5)
	h.drain(50 * time.Millisecond)
	if n := cat.invalidated.Load(); n != 2 {
		t.Fatalf("Invalidate calls = %d, want 2", n)
	}
}

func TestCompletionGolden(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tobira_people\nwhere tobira_people.")
	h.send(keyCtrlSpace)
	h.waitPopup()
	checkFrame(t, h, winW, winH)
	golden(t, "editor_completion", h.text())
}

// ---- history ---------------------------------------------------------------------

func seededHistory(qs ...string) *fakeHistory {
	f := &fakeHistory{}
	for _, q := range qs {
		f.entries = append(f.entries, history.Entry{Conn: "local", Source: history.SourceTUI, Query: q, OK: true})
	}
	f.entries = append(f.entries, history.Entry{Conn: "ro", Query: "select 'other connection'"})
	return f
}

func TestHistoryWalk(t *testing.T) {
	hist := seededHistory("select 1", "select 2", "select 2", "select\n3")
	h := newQueryHarness(t, hist)
	h.openEditor("draft")
	h.keys("up")
	h.pump(func() bool { return h.m.ed.Value() == "select\n3" })
	if got := hist.listCalls(); len(got) != 1 || !reflect.DeepEqual(got[0], history.Filter{Conn: "local", ExcludeSources: recallExcluded, Limit: walkLimit}) {
		t.Fatalf("List calls = %+v", got)
	}
	h.keys("up") // the cursor is on the last line of a two-line entry
	if v := h.m.ed.Value(); v != "select\n3" {
		t.Fatalf("up inside an entry walked: %q", v)
	}
	h.keys("up")
	if v := h.m.ed.Value(); v != "select 2" {
		t.Fatalf("second entry = %q (consecutive duplicates once)", v)
	}
	h.keys("up")
	if v := h.m.ed.Value(); v != "select 1" {
		t.Fatalf("third entry = %q", v)
	}
	h.keys("up") // the oldest stays
	if v := h.m.ed.Value(); v != "select 1" {
		t.Fatalf("past the oldest = %q", v)
	}
	h.keys("down", "down")
	if v := h.m.ed.Value(); v != "select\n3" {
		t.Fatalf("down twice = %q", v)
	}
	h.keys("down")
	if v := h.m.ed.Value(); v != "draft" || h.m.walk.active {
		t.Fatalf("the stash was not restored: %q active %v", v, h.m.walk.active)
	}
	// An edit ends the walk and keeps the text.
	h.keys("up")
	h.pump(func() bool { return h.m.ed.Value() == "select\n3" })
	h.typ("x")
	if h.m.walk.active || h.m.ed.Value() != "select\n3x" {
		t.Fatalf("edit: active %v value %q", h.m.walk.active, h.m.ed.Value())
	}
	h.keys("down")
	if h.m.ed.Value() != "select\n3x" {
		t.Fatal("down after an edit walked the history")
	}
}

func TestHistoryWalkWithoutHistory(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("a\nb")
	h.keys("up", "up")
	if _, col, _ := h.m.ed.CursorLine(); !h.m.ed.FirstLine() || col != 0 {
		t.Fatal("without a history store up should just move")
	}
	h2 := newQueryHarness(t, &fakeHistory{})
	h2.openEditor("draft")
	h2.keys("up")
	h2.until("no history for local yet")
	if h2.m.ed.Value() != "draft" || h2.m.walk.active {
		t.Fatal("an empty history changed the buffer")
	}
}

func TestHistorySearch(t *testing.T) {
	hist := seededHistory("select * from tobira_people", "select 2", "select name\nfrom tobira_people", "select 4")
	h := newQueryHarness(t, hist)
	h.openEditor("draft")
	h.send(ctrl('r'))
	if !strings.Contains(h.text(), "(reverse-i-search) '': ") {
		t.Fatalf("no prompt:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	h.typ("PEO")
	want := history.Filter{Conn: "local", ExcludeSources: recallExcluded, Search: "PEO", Limit: searchLimit}
	h.pump(func() bool {
		// The reads run concurrently, so their order of arrival varies.
		return slices.ContainsFunc(hist.listCalls(), func(f history.Filter) bool { return reflect.DeepEqual(f, want) }) && h.m.ed.Value() == "select name\nfrom tobira_people"
	})
	if !strings.Contains(h.text(), "(reverse-i-search) 'PEO': select name⏎from tobira_people") {
		t.Fatalf("prompt:\n%s", h.text())
	}
	h.send(ctrl('r')) // next older match
	if v := h.m.ed.Value(); v != "select * from tobira_people" {
		t.Fatalf("ctrl+r again = %q", v)
	}
	h.keys("enter")
	if h.m.hs.active || h.m.ed.Value() != "select * from tobira_people" {
		t.Fatalf("enter: active %v value %q", h.m.hs.active, h.m.ed.Value())
	}
	if strings.Contains(h.text(), "reverse-i-search") {
		t.Fatal("the prompt stayed after enter")
	}

	// esc restores the original buffer; so does ctrl+g.
	for _, cancel := range []tea.KeyPressMsg{key("esc"), ctrl('g')} {
		h.m.ed.SetValue("original")
		h.send(ctrl('r'))
		h.typ("4")
		h.pump(func() bool { return h.m.ed.Value() == "select 4" })
		h.send(cancel)
		if h.m.hs.active || h.m.ed.Value() != "original" || h.m.focus != focusEditor {
			t.Fatalf("%s: active %v value %q focus %v", cancel.String(), h.m.hs.active, h.m.ed.Value(), h.m.focus)
		}
	}

	// No match says so.
	h.send(ctrl('r'))
	h.typ("zzz")
	h.until("(failed reverse-i-search) 'zzz': ")
	h.keys("esc")
}

func TestHistorySearchEscapesAndDropsStale(t *testing.T) {
	hist := seededHistory("select '\x1b[2J\x1b]52;c;aGk=\x07'", "select 'b'")
	hist.latency = 100 * time.Millisecond
	h := newQueryHarness(t, hist)
	h.openEditor("")
	h.send(ctrl('r'))
	h.typ("se")
	h.typ("l")
	// "se" and "sel" both match; only the answer for "sel" may land, and
	// the prompt shows the term it is for.
	h.pump(func() bool { return h.m.hs.idx < len(h.m.hs.matches) })
	if !strings.Contains(h.text(), "'sel': select 'b'") {
		t.Fatalf("prompt:\n%s", h.text())
	}
	h.send(ctrl('r'))
	tuitest.NoInjectedControls(t, h.raw())
	if !strings.Contains(h.text(), `select '\x1b[2J\x1b]52;c;aGk=\x07'`) {
		t.Fatalf("match not escaped:\n%s", h.text())
	}
}

func TestHistorySearchGolden(t *testing.T) {
	hist := seededHistory("select * from tobira_people where id = 1", "select name\nfrom tobira_people")
	h := newQueryHarness(t, hist)
	h.openEditor("draft")
	h.send(ctrl('r'))
	h.typ("from")
	h.pump(func() bool { return strings.Contains(h.m.ed.Value(), "select name") })
	checkFrame(t, h, winW, winH)
	golden(t, "editor_history_search", h.text())
}

// TestProgramCompletionAndHistory runs the real program under teatest:
// ctrl+space then tab completes, and ctrl+r finds a past query.
func TestProgramCompletionAndHistory(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	hist := seededHistory("select * from tobira_people where id = 7")
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark", History: hist})
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
	for _, r := range "select * from tobira_pe" {
		tm.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	waitFor("collection · main")
	tm.Send(key("tab"))
	tm.Send(ctrl('r'))
	waitFor("(reverse-i-search)")
	for _, r := range "id = 7" {
		tm.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	// Every prefix of the term matches the one entry of local, and the
	// renderer only redraws what changed, so wait for the match itself.
	waitFor("tobira_people where id = 7")
	tm.Send(key("enter"))
	tm.Send(key("esc"))
	tm.Send(key("q"))
	// The buffer holds a query that was never run: quitting asks first.
	waitFor("discard the unsaved query? y/N")
	tm.Send(key("y"))
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if got := final.ed.Value(); got != "select * from tobira_people where id = 7" {
		t.Fatalf("final buffer = %q", got)
	}
}

func TestCompletionAndSearchTinyWindows(t *testing.T) {
	sizes := [][2]int{{20, 5}, {1, 1}, {10, 2}, {59, 10}, {60, 16}, {5, 40}, {200, 3}, {100, 7}, {30, 12}}
	for _, sz := range sizes {
		t.Run(itoa(sz[0])+"x"+itoa(sz[1]), func(t *testing.T) {
			h := newQueryHarness(t, seededHistory("select 1\nfrom x"))
			h.openPeople()
			h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			h.send(ctrl('e'))
			h.send(tea.PasteMsg{Content: "select\n\n\n\n\n\n\n\n\nfrom tobira_people where "})
			h.send(keyCtrlSpace)
			h.drain(60 * time.Millisecond)
			checkFrame(t, h, sz[0], sz[1])
			h.send(ctrl('r'))
			h.typ("1")
			h.drain(60 * time.Millisecond)
			checkFrame(t, h, sz[0], sz[1])
		})
	}
}

// ---- popup, lookups and recall --------------------------------------------------

// newDialectHarness is newQueryHarness with the Fake reporting dialect.
func newDialectHarness(t *testing.T, dialect string) *harness {
	t.Helper()
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(dialectDriver{Driver: f.Driver(), name: "fake", dialect: dialect}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.run(h.m.Init())
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	return h
}

func TestCompletionFiltersWhileTyping(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tob")
	h.send(keyCtrlSpace)
	h.waitPopup()
	// A character that still matches keeps the item, at once.
	h.typ("i")
	if got := h.popupLabels(); !h.m.popupShown() || len(got) != 1 || got[0] != "tobira_people" {
		t.Fatalf("after typing i: shown %v items %v", h.m.popupShown(), got)
	}
	// One that matches nothing closes the popup before any lookup answers,
	// and enter is a newline, not the old item.
	h.typ("x")
	if h.m.cp.open {
		t.Fatalf("the popup kept stale items: %v", h.popupLabels())
	}
	h.keys("enter")
	if v := h.m.ed.Value(); v != "select * from tobix\n" {
		t.Fatalf("enter after a non-matching edit = %q", v)
	}
	// Enter and tab never accept an item that does not match the prefix.
	h.m.ed.SetValue("select * from tob")
	h.send(keyCtrlSpace)
	h.waitPopup()
	h.m.cp.items = []driver.Completion{{Label: "users"}}
	h.keys("tab")
	if v := h.m.ed.Value(); v != "select * from tob" {
		t.Fatalf("tab accepted a non-matching item: %q", v)
	}
}

func TestCompletionKeepsTheQuote(t *testing.T) {
	h := newQueryHarness(t, nil, drivertest.WithoutCapabilities(driver.CapCompleter))
	h.openEditor(`select * from "tob`)
	h.send(keyCtrlSpace)
	h.waitPopup()
	h.keys("tab")
	if v := h.m.ed.Value(); v != `select * from "tobira_people"` {
		t.Fatalf("accepted into %q", v)
	}
	// A closing quote already typed is replaced, not doubled.
	h.m.ed.SetValue(`select * from "tob" x`)
	h.m.ed.SetCursor(len(`select * from "tob`))
	h.send(keyCtrlSpace)
	h.waitPopup()
	h.keys("enter")
	if v := h.m.ed.Value(); v != `select * from "tobira_people" x` {
		t.Fatalf("accepted into %q", v)
	}
}

func TestCompletionNotAutoInsideLiterals(t *testing.T) {
	// A string opened on an earlier line.
	h := newQueryHarness(t, nil)
	h.openEditor("select 'abc\nto")
	h.typ("b")
	h.raw() // a frame lexes the new text, as Bubble Tea draws after each Update
	h.drain(200 * time.Millisecond)
	if h.m.cp.open {
		t.Fatal("the popup opened inside a multi-line string")
	}
	// A mysql # comment, which the line scan does not know.
	h = newDialectHarness(t, "mysql")
	h.openPeople()
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select 1 # to"})
	h.typ("b")
	h.raw()
	h.drain(200 * time.Millisecond)
	if h.m.cp.open {
		t.Fatal("the popup opened inside a mysql comment")
	}
	// ctrl+space still completes there.
	h.send(keyCtrlSpace)
	h.waitPopup()
}

func TestRunClosesThePopup(t *testing.T) {
	h := newQueryHarness(t, nil)
	h.openEditor("select * from tob")
	h.send(keyCtrlSpace)
	h.waitPopup()
	h.runUntilDone(keyF5)
	if h.m.cp.open {
		t.Fatal("the popup stayed open over the run")
	}
}

// startSlowLookup types until an automatic lookup is in flight on a Fake
// that answers in 5 s, then leaves the editor.
func (h *harness) startSlowLookup() {
	h.t.Helper()
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select * from to"})
	h.fake.SetLatency(5 * time.Second)
	h.typ("b")
	h.pump(func() bool {
		calls := h.fake.Calls()
		return len(calls) > 0 && calls[len(calls)-1].Op == "complete"
	})
	h.keys("esc")
}

func (h *harness) lookupCancelledBeforeClose() {
	h.t.Helper()
	last := func() drivertest.Call {
		calls := h.fake.Calls()
		for i := len(calls) - 1; i >= 0; i-- {
			if calls[i].Op == "complete" {
				return calls[i]
			}
		}
		return drivertest.Call{}
	}
	h.pump(func() bool { return last().Err != nil })
	if err := last().Err; !errors.Is(err, context.Canceled) {
		h.t.Fatalf("the lookup ended with %v, want a cancellation", err)
	}
}

func TestCompletionLookupCancelledBeforeReload(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.startSlowLookup()
	h.keys(":")
	h.typ("reload")
	h.keys("enter")
	h.pump(func() bool { return !h.m.saving })
	h.lookupCancelledBeforeClose()
}

func TestCompletionLookupCancelledBeforeConnectionDelete(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.startSlowLookup()
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("D")
	h.typ("local")
	h.keys("enter")
	h.pump(func() bool { return !h.m.saving })
	h.lookupCancelledBeforeClose()
}

func TestCompletionLookupCancelledBeforeConnectionEdit(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.startSlowLookup()
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("E")
	h.typ("2") // a rename closes the connection
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	h.pump(func() bool { return h.m.form == nil && !h.m.saving }) // the write ran
	h.lookupCancelledBeforeClose()
}

func TestHistoryWalkSkipsTheDraft(t *testing.T) {
	hist := seededHistory("select 1", "select 2")
	h := newQueryHarness(t, hist)
	h.openEditor("select 2") // just run: the newest entry is the buffer
	h.keys("up")
	h.pump(func() bool { return h.m.ed.Value() == "select 1" })
	h.keys("down")
	if v := h.m.ed.Value(); v != "select 2" || h.m.walk.active {
		t.Fatalf("down = %q active %v", v, h.m.walk.active)
	}
	h2 := newQueryHarness(t, seededHistory("draft"))
	h2.openEditor("draft")
	h2.keys("up")
	h2.until("no other queries in the history of local")
	if h2.m.walk.active || h2.m.ed.Value() != "draft" {
		t.Fatal("a history holding only the draft changed the buffer")
	}
}

func TestHistorySearchFailureClearsTheMatch(t *testing.T) {
	hist := seededHistory("select 12")
	h := newQueryHarness(t, hist)
	h.openEditor("draft")
	h.send(ctrl('r'))
	h.typ("1")
	h.pump(func() bool { return h.m.ed.Value() == "select 12" })
	hist.mu.Lock()
	hist.err = errors.New("disk I/O error")
	hist.mu.Unlock()
	h.typ("2")
	h.until("(reverse-i-search) '12': search failed")
	if len(h.m.hs.matches) != 0 {
		t.Fatalf("stale matches kept: %v", h.m.hs.matches)
	}
}

func TestHistorySearchRefusedInATinyEditor(t *testing.T) {
	h := newQueryHarness(t, seededHistory("select 1"))
	h.openEditor("draft")
	h.send(tea.WindowSizeMsg{Width: winW, Height: 4})
	if h.m.lay.editor.h >= 2 {
		t.Fatalf("editor is %d lines; the test needs fewer than 2", h.m.lay.editor.h)
	}
	h.send(ctrl('r'))
	if h.m.hs.active {
		t.Fatal("the search started with no room for its prompt")
	}
	if !strings.Contains(h.statusLine(), "enlarge the editor to search history") {
		t.Fatalf("no refusal: %q", h.statusLine())
	}
	checkFrame(t, h, winW, 4)
}

func TestCtrlCAbortsTheSearch(t *testing.T) {
	h := newQueryHarness(t, seededHistory("select 1"))
	h.openEditor("draft")
	h.send(ctrl('r'))
	h.typ("1")
	h.pump(func() bool { return h.m.ed.Value() == "select 1" })
	h.keys("ctrl+c")
	if h.quitted() {
		t.Fatal("ctrl+c during a search quit")
	}
	if h.m.hs.active || h.m.ed.Value() != "draft" {
		t.Fatalf("ctrl+c: active %v value %q", h.m.hs.active, h.m.ed.Value())
	}
}

// A config write waits for every completion lookup still unwinding on
// the connection, not only the latest one.
func TestStopLookupsWaitsForEveryUnwindingLookup(t *testing.T) {
	cat := &catalog{names: []string{"tobira_people"}, gate: make(chan struct{})}
	h := newCatalogHarness(t, cat, nil)
	h.openPeople()
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select * from tob"})
	var dones []chan struct{}
	for range 2 { // the second lookup cancels the first, which keeps running
		cmd := h.m.lookup()
		done := make(chan struct{})
		dones = append(dones, done)
		go func() { cmd(); close(done) }()
	}
	for cat.completes.Load() < 2 {
		time.Sleep(time.Millisecond) // both lookups inside Complete
	}
	h.m.stopLookups("local")
	if n := len(stillRunning(h.m.stopping)); n != 2 {
		t.Fatalf("the write waits for %d lookups, want both unwinding ones", n)
	}
	close(cat.gate)
	for _, d := range dones {
		<-d
	}
	if n := len(stillRunning(h.m.stopping)); n != 0 {
		t.Fatalf("%d lookups still counted as running after they returned", n)
	}
}

// Recall (the walk and ctrl+r) offers only what the user ran (cli and tui),
// never the tool calls of MCP clients or the AI assistant.
func TestHistoryRecallSkipsMCPAndAI(t *testing.T) {
	hist := &fakeHistory{}
	for _, e := range []history.Entry{
		{Source: history.SourceCLI, Query: "select 'cli'"},
		{Source: history.SourceMCP, Query: `describe {"collection":"t","connection":"local"}`},
		{Source: history.SourceTUI, Query: "select 'tui'"},
		{Source: history.SourceAI, Query: "select 'ai'"},
		{Source: history.SourceMCP, Query: "resource tobira://local/main/t/schema"},
	} {
		e.Conn, e.OK = "local", true
		hist.entries = append(hist.entries, e)
	}
	h := newQueryHarness(t, hist)
	h.openEditor("draft")
	h.keys("up")
	h.pump(func() bool { return h.m.ed.Value() == "select 'tui'" })
	h.keys("up")
	if v := h.m.ed.Value(); v != "select 'cli'" {
		t.Fatalf("second entry = %q, want the cli query (mcp and ai skipped)", v)
	}
	h.keys("up")
	if v := h.m.ed.Value(); v != "select 'cli'" {
		t.Fatalf("past the oldest = %q", v)
	}
	h.keys("down", "down")
	h.m.ed.SetValue("orig")
	h.send(ctrl('r'))
	h.typ("resource")
	h.until("(failed reverse-i-search) 'resource': ")
	h.keys("esc")
	h.send(ctrl('r'))
	h.typ("'ai'")
	h.until("(failed reverse-i-search) ''ai'': ")
	h.keys("esc")
}
