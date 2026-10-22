package sidebar

import (
	"errors"
	"flag"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

var update = flag.Bool("update", false, "update golden files")

// harness drives a Model the way Bubble Tea would, running commands inline
// and recording the messages the sidebar emits to its parent.
type harness struct {
	t    *testing.T
	m    Model
	fake *drivertest.Fake
	mgr  *session.Manager
	out  []tea.Msg
}

func newHarness(t *testing.T, km keymap.Map) *harness {
	t.Helper()
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "a", Driver: "fake", DSN: "fake://a", Color: "green"},
		{Name: "b", Driver: "fake", DSN: "fake://b", Color: "nonsense"},
	}}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	h := &harness{t: t, fake: f, mgr: mgr}
	h.m = New(mgr, theme.Dark(), km).Resize(40, 12)
	return h
}

// exec runs cmd (and every command it spawns) to completion.
func (h *harness) exec(cmd tea.Cmd) {
	h.t.Helper()
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("command did not finish")
	}
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			h.exec(c)
		}
		return
	}
	if msg == nil {
		return
	}
	h.send(msg)
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	var cmd tea.Cmd
	h.m, cmd = h.m.Update(msg)
	switch msg.(type) {
	case StatusMsg, OpenCollectionMsg:
		h.out = append(h.out, msg)
	}
	h.exec(cmd)
}

func (h *harness) key(s string) {
	h.t.Helper()
	var k tea.KeyPressMsg
	switch s {
	case "enter":
		k = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		k = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		k = tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		k = tea.KeyPressMsg{Code: tea.KeyUp}
	case "left":
		k = tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		k = tea.KeyPressMsg{Code: tea.KeyRight}
	default:
		r := []rune(s)[0]
		k = tea.KeyPressMsg{Code: r, Text: s}
	}
	h.send(k)
}

func (h *harness) typ(s string) {
	for _, r := range s {
		h.key(string(r))
	}
}

func (h *harness) view(w, ht int) string {
	return ansi.Strip(h.m.View(w, ht, true))
}

func (h *harness) statuses() []StatusMsg {
	var out []StatusMsg
	for _, m := range h.out {
		if s, ok := m.(StatusMsg); ok {
			out = append(out, s)
		}
	}
	return out
}

func (h *harness) calls(op string) int {
	n := 0
	for _, c := range h.fake.Calls() {
		if c.Op == op {
			n++
		}
	}
	return n
}

// expandAll opens connection a, its first namespace, and leaves the cursor
// on that namespace.
func (h *harness) expandAll() {
	h.t.Helper()
	h.exec(h.m.Expand("a"))
	h.key("down") // first namespace
	h.key("enter")
}

func TestExpandLoadsNamespacesLazily(t *testing.T) {
	h := newHarness(t, keymap.Default())
	if h.calls("open") != 0 {
		t.Fatal("nothing should open before expansion")
	}
	h.exec(h.m.Expand("a"))
	if h.calls("open") != 1 || h.calls("namespaces") != 1 {
		t.Fatalf("after expanding a: open=%d namespaces=%d, want 1 and 1", h.calls("open"), h.calls("namespaces"))
	}
	if h.calls("collections") != 0 {
		t.Fatal("collections must not load until a namespace is expanded")
	}
	v := h.view(40, 12)
	if strings.Contains(v, "tobira_people") {
		t.Fatalf("collections visible before namespace expand:\n%s", v)
	}
	h.key("down")
	h.key("enter")
	if h.calls("collections") != 1 {
		t.Fatalf("collections calls = %d, want 1", h.calls("collections"))
	}
	if v := h.view(40, 12); !strings.Contains(v, "tobira_people") {
		t.Fatalf("collection not shown after namespace expand:\n%s", v)
	}
	// Collapse and re-expand: cached, no new call.
	h.key("enter")
	h.key("enter")
	if h.calls("collections") != 1 {
		t.Fatalf("re-expand reloaded: collections calls = %d", h.calls("collections"))
	}
}

func TestExpandingTwoNodesDoesNotCancelEither(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.fake.SetLatency(30 * time.Millisecond)
	cmdA := h.m.Expand("a")
	cmdB := h.m.Expand("b")
	h.exec(tea.Batch(cmdA, cmdB))
	if v := h.view(40, 12); !strings.Contains(v, "▾") {
		t.Fatalf("expected an expanded node:\n%s", v)
	}
	if len(h.statuses()) != 0 {
		t.Fatalf("unexpected errors: %+v", h.statuses())
	}
}

func TestReexpandSupersedesPreviousRequest(t *testing.T) {
	h := newHarness(t, keymap.Default())
	stale := h.m.Expand("a")
	fresh := h.m.Expand("a")
	staleMsg, freshMsg := stale(), fresh()
	h.send(staleMsg)
	if strings.Contains(h.view(40, 12), "main") {
		t.Fatal("result of the superseded request was applied")
	}
	h.send(freshMsg)
	if !strings.Contains(h.view(40, 12), "main") {
		t.Fatalf("current request result not applied:\n%s", h.view(40, 12))
	}
}

func TestStatusDots(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.fake.FailNext("open", fmt.Errorf("%w: password=hunter2 rejected", driver.ErrAuth))
	h.exec(h.m.Expand("b"))
	h.exec(h.m.Expand("a"))

	sts := h.statuses()
	if len(sts) != 1 || !sts[0].Err {
		t.Fatalf("want one error status, got %+v", sts)
	}
	if strings.Contains(sts[0].Text, "hunter2") || !strings.Contains(sts[0].Text, "b") {
		t.Fatalf("status must name b and be redacted: %q", sts[0].Text)
	}
	v := h.view(40, 12)
	if !strings.Contains(v, "(error)") {
		t.Fatalf("failed node should say (error):\n%s", v)
	}
	if strings.Contains(v, "hunter2") {
		t.Fatalf("secret leaked into view:\n%s", v)
	}
	if got := h.mgr.State("b").Status; got != session.Failed {
		t.Fatalf("b status = %v", got)
	}
	// a stays usable: its namespaces are listed.
	if h.calls("namespaces") != 1 {
		t.Fatalf("a should have loaded namespaces, calls=%d", h.calls("namespaces"))
	}

	th := theme.Dark()
	for st, want := range map[session.Status]color.Color{
		session.Idle: th.Muted, session.Connecting: th.Warn, session.Ready: th.OK, session.Failed: th.Danger,
	} {
		if got := h.m.statusColor(st); got != want {
			t.Errorf("statusColor(%v) = %v, want %v", st, got, want)
		}
	}
	if got := h.m.nameColor("a"); got != th.OK {
		t.Errorf("green connection tint = %v, want OK", got)
	}
	if got := h.m.nameColor("b"); got != th.Fg {
		t.Errorf("unknown colour should fall back to Fg, got %v", got)
	}
}

func TestEnterOnCollectionEmitsOpen(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.expandAll()
	h.key("down") // first collection
	conn, ref := h.m.Selected()
	if conn != "a" || ref == nil {
		t.Fatalf("Selected = %q, %v", conn, ref)
	}
	h.key("enter")
	var got *OpenCollectionMsg
	for _, m := range h.out {
		if o, ok := m.(OpenCollectionMsg); ok {
			got = &o
		}
	}
	if got == nil {
		t.Fatal("no OpenCollectionMsg emitted")
	}
	if got.Conn != "a" || got.Ref != *ref {
		t.Fatalf("OpenCollectionMsg = %+v, want conn a ref %+v", got, *ref)
	}
}

func TestSelectedOnNonCollection(t *testing.T) {
	h := newHarness(t, keymap.Default())
	if conn, ref := h.m.Selected(); conn != "a" || ref != nil {
		t.Fatalf("Selected = %q, %v", conn, ref)
	}
}

func TestArrowNavigation(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.key("right") // expand a
	h.exec(nil)
	h.key("down")
	if conn, _ := h.m.Selected(); conn != "a" {
		t.Fatalf("conn = %q", conn)
	}
	h.key("left") // namespace is not expanded: move to parent
	if h.m.cursor != 0 {
		t.Fatalf("cursor = %d, want parent row 0", h.m.cursor)
	}
	h.key("left") // collapse a
	if len(h.m.rows()) != 2 {
		t.Fatalf("a should be collapsed, rows = %d", len(h.m.rows()))
	}
}

func TestVimKeys(t *testing.T) {
	h := newHarness(t, keymap.Vim())
	h.key("j")
	if h.m.cursor != 1 {
		t.Fatalf("j: cursor = %d", h.m.cursor)
	}
	h.key("k")
	if h.m.cursor != 0 {
		t.Fatalf("k: cursor = %d", h.m.cursor)
	}
	h.key("G")
	if h.m.cursor != 1 {
		t.Fatalf("G: cursor = %d", h.m.cursor)
	}
	h.key("g")
	if h.m.cursor != 1 {
		t.Fatalf("a single g moved the cursor to %d", h.m.cursor)
	}
	h.key("g")
	if h.m.cursor != 0 {
		t.Fatalf("gg: cursor = %d", h.m.cursor)
	}
}

func TestFilter(t *testing.T) {
	h := newHarness(t, keymap.Vim())
	h.expandAll()
	full := h.view(40, 12)
	if !strings.Contains(full, "tobira_people") {
		t.Fatalf("setup:\n%s", full)
	}

	h.key("/")
	// While the input is focused, vim letters must not move the cursor or act.
	h.typ("peo")
	v := h.view(40, 12)
	if !strings.Contains(v, "/ peo") {
		t.Fatalf("filter line missing:\n%s", v)
	}
	if !strings.Contains(v, "tobira_people") || !strings.Contains(v, "a") {
		t.Fatalf("match and its ancestors must stay:\n%s", v)
	}
	if strings.Contains(v, "b\n") || strings.Contains(v, "● b") {
		t.Fatalf("non-matching connection b still visible:\n%s", v)
	}
	for _, l := range strings.Split(v, "\n")[1:] {
		if s := strings.TrimSpace(l); s != "" && !strings.ContainsAny(s, "ap") {
			t.Fatalf("unexpected row %q", s)
		}
	}

	h.key("enter") // keeps the filter, drops focus
	if !strings.Contains(h.view(40, 12), "/ peo") || strings.Contains(h.view(40, 12), "● b") {
		t.Fatalf("enter should keep the filter:\n%s", h.view(40, 12))
	}

	h.key("/")
	h.key("esc") // clears
	v = h.view(40, 12)
	if strings.Contains(v, "/ ") || !strings.Contains(v, "● b") {
		t.Fatalf("esc should clear the filter:\n%s", v)
	}
}

func TestFilterCaseInsensitive(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.expandAll()
	h.key("/")
	h.typ("PEOPLE")
	if !strings.Contains(h.view(40, 12), "tobira_people") {
		t.Fatal("filter should be case-insensitive")
	}
}

func TestFilterSwallowsActionKeys(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.key("/")
	h.typ("qyY?s") // all bare-letter actions in the default keymap
	if got := h.m.filter.Value(); got != "qyY?s" {
		t.Fatalf("input = %q", got)
	}
	if len(h.out) != 0 {
		t.Fatalf("no message should be emitted: %+v", h.out)
	}
}

func TestMouseClickSelects(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.exec(h.m.Expand("a"))
	// rows: 0 a, 1 namespace, 2 b
	h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
	if conn, _ := h.m.Selected(); conn != "b" {
		t.Fatalf("click on row 2 selected %q, want b", conn)
	}
	// Clicking the selected node acts like enter (expand -> load).
	before := h.calls("open")
	h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
	if h.calls("open") != before+1 {
		t.Fatalf("second click should expand b; opens %d -> %d", before, h.calls("open"))
	}
	// Clicks past the last row and with other buttons are ignored.
	h.send(tea.MouseClickMsg{X: 3, Y: 9, Button: tea.MouseLeft})
	h.send(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseRight})
	if h.m.cursor != 2 {
		t.Fatalf("cursor moved to %d", h.m.cursor)
	}
}

func TestMouseWheelScrolls(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.exec(h.m.Expand("a"))
	h.m = h.m.Resize(40, 2) // rows: a, main, b
	first := func() string { return strings.TrimSpace(strings.Split(h.view(40, 2), "\n")[0]) }
	if !strings.HasSuffix(first(), "a") {
		t.Fatalf("first row = %q", first())
	}
	h.send(tea.MouseWheelMsg{X: 1, Y: 1, Button: tea.MouseWheelDown})
	if !strings.HasSuffix(first(), "main") {
		t.Fatalf("wheel down should scroll the view; first row = %q", first())
	}
	h.send(tea.MouseWheelMsg{X: 1, Y: 1, Button: tea.MouseWheelUp})
	if !strings.HasSuffix(first(), "a") {
		t.Fatalf("wheel up should scroll back; first row = %q", first())
	}
}

func TestDriverNamesAreEscaped(t *testing.T) {
	f := drivertest.NewFake()
	evil := "a\x1b[31mb\nc"
	f.AddCollection(evil, "t\x1b]0;x\x07", []driver.Column{{Name: "id"}}, nil)
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "a", Driver: "fake", DSN: "fake://a"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	h := &harness{t: t, fake: f, mgr: mgr}
	h.m = New(mgr, theme.Dark(), keymap.Default()).Resize(40, 6)
	h.expandAll()
	rawView := h.m.View(40, 6, true)
	tuitest.NoInjectedControls(t, rawView)
	raw := ansi.Strip(rawView)
	if !strings.Contains(raw, `a\x1b[31mb c`) {
		t.Fatalf("want escaped namespace in view: %q", raw)
	}
	if n := len(strings.Split(raw, "\n")); n != 6 {
		t.Fatalf("view has %d lines, want 6: %q", n, raw)
	}
	// The raw name is still what refs carry.
	h.key("down")
	if _, ref := h.m.Selected(); ref == nil || ref.Namespace != evil {
		t.Fatalf("ref = %+v, want raw namespace", ref)
	}
}

func TestFilterMatchesTheEscapedDisplayText(t *testing.T) {
	f := drivertest.NewFake()
	f.AddCollection("a\x1b[31mb\nc", "t\x1b]0;x\x07", []driver.Column{{Name: "id"}}, nil)
	f.AddCollection("plain", "other", []driver.Column{{Name: "id"}}, nil)
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "a", Driver: "fake", DSN: "fake://a"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	h := &harness{t: t, fake: f, mgr: mgr}
	h.m = New(mgr, theme.Dark(), keymap.Default()).Resize(40, 8)
	h.expandAll()
	for _, typed := range []string{`x1b[31`, `mb c`, `x1b]0;x`, `\x07`} {
		h.key("/")
		h.typ(typed)
		h.key("enter")
		v := h.view(40, 8)
		if !strings.Contains(v, "a") || strings.Contains(v, "plain") {
			t.Fatalf("filter %q should match the escaped name and hide the rest:\n%s", typed, v)
		}
		h.key("/")
		h.m.clearFilter()
	}
}

func TestClosedConnectionReloads(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.exec(h.m.Expand("a"))
	if err := h.mgr.Close("a"); err != nil {
		t.Fatal(err)
	}
	h.m = h.m.Sync() // the parent's connecting tick
	if got := len(h.m.rows()); got != 2 {
		t.Fatalf("closed connection should collapse; rows = %d", got)
	}
	h.exec(h.m.Expand("a"))
	if h.calls("open") != 2 || h.calls("namespaces") != 2 {
		t.Fatalf("re-expanding should reopen and reload: open=%d namespaces=%d", h.calls("open"), h.calls("namespaces"))
	}
}

func TestFilterDrawsMatchedAncestorsOpen(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.expandAll()
	h.key("down")
	h.key("/")
	h.typ("people")
	h.key("enter")
	v := h.view(40, 6)
	if !strings.Contains(v, "▾ ● a") || !strings.Contains(v, "▾ main") {
		t.Fatalf("matched ancestors should be drawn open:\n%s", v)
	}
	// enter on an ancestor drawn open must not hide the match.
	h.key("up")
	h.key("enter")
	if !strings.Contains(h.view(40, 6), "tobira_people") {
		t.Fatal("enter on a filter-opened ancestor hid its match")
	}
}

func TestTinyViewsDoNotPanic(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.expandAll()
	h.key("/")
	for w := -1; w <= 6; w++ {
		for ht := -1; ht <= 4; ht++ {
			for _, focused := range []bool{true, false} {
				v := h.m.View(w, ht, focused)
				if w < 4 || ht < 2 {
					if strings.TrimSpace(ansi.Strip(v)) != "" {
						t.Fatalf("View(%d,%d) = %q, want blank", w, ht, v)
					}
				}
			}
		}
	}
}

func TestViewFitsSize(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.expandAll()
	for _, w := range []int{4, 5, 8, 20, 40} {
		v := h.view(w, 6)
		lines := strings.Split(v, "\n")
		if len(lines) != 6 {
			t.Fatalf("w=%d: %d lines, want 6", w, len(lines))
		}
		for _, l := range lines {
			if ansi.StringWidth(l) != w {
				t.Fatalf("w=%d: line %q has width %d", w, l, ansi.StringWidth(l))
			}
		}
	}
}

func TestLongNameTruncated(t *testing.T) {
	f := drivertest.NewFake()
	f.AddCollection("public", "a_really_long_collection_name_indeed", []driver.Column{{Name: "id"}}, nil)
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "a", Driver: "fake", DSN: "fake://a"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	h := &harness{t: t, fake: f, mgr: mgr}
	h.m = New(mgr, theme.Dark(), keymap.Default()).Resize(24, 8)
	h.expandAll()
	v := h.view(24, 8)
	if !strings.Contains(v, "…") {
		t.Fatalf("long name should be truncated with an ellipsis:\n%s", v)
	}
}

func TestSidebarExpandedGolden(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.expandAll()
	h.key("down") // cursor on the collection
	golden(t, "sidebar_expanded.golden", h.view(32, 8))
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func rowNames(m Model) []string {
	var out []string
	for _, r := range m.rows() {
		out = append(out, strings.Repeat(" ", r.depth)+r.n.name)
	}
	return out
}

func TestRefreshKeepsExpansionOfUnchanged(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.key("enter") // expand a
	before := rowNames(h.m)
	if len(before) < 3 {
		t.Fatalf("rows = %v", before)
	}
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "a", Driver: "fake", DSN: "fake://a", Color: "green"},
		{Name: "c", Driver: "fake", DSN: "fake://c"},
	}}
	added, changed, removed := h.mgr.Reload(cfg)
	if fmt.Sprint(added, changed, removed) != "[c] [] [b]" {
		t.Fatalf("reload = %v %v %v", added, changed, removed)
	}
	h.m = h.m.Refresh()
	got := rowNames(h.m)
	want := append(append([]string{}, before[:len(before)-1]...), "c") // a and its children, b gone, c new
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if conn, _ := h.m.Selected(); conn != "a" {
		t.Fatalf("cursor moved to %q", conn)
	}
}

func TestRefreshChangedConnectionCollapses(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.key("enter") // expand a
	changed := config.Connection{Name: "a", Driver: "fake", DSN: "fake://a2", Color: "green"}
	cfg := &config.Config{Connections: []config.Connection{changed, {Name: "b", Driver: "fake", DSN: "fake://b", Color: "nonsense"}}}
	if _, c, _ := h.mgr.Reload(cfg); fmt.Sprint(c) != "[a]" {
		t.Fatalf("changed = %v", c)
	}
	h.m = h.m.Refresh()
	if got := rowNames(h.m); fmt.Sprint(got) != "[a b]" {
		t.Fatalf("rows = %v, want a collapsed", got)
	}
	// It expands again, reopening lazily.
	h.key("enter")
	if got := rowNames(h.m); len(got) < 3 {
		t.Fatalf("a did not re-expand: %v", got)
	}
}

func TestRefreshSelectAndRemovedCursor(t *testing.T) {
	h := newHarness(t, keymap.Default())
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "a", Driver: "fake", DSN: "fake://a", Color: "green"},
		{Name: "b", Driver: "fake", DSN: "fake://b", Color: "nonsense"},
		{Name: "z", Driver: "fake", DSN: "fake://z"},
	}}
	h.mgr.Reload(cfg)
	h.m = h.m.Refresh().Select("z")
	if conn, _ := h.m.Selected(); conn != "z" {
		t.Fatalf("selected %q", conn)
	}
	// Removing the selected connection leaves the cursor in range.
	h.mgr.Reload(&config.Config{Connections: cfg.Connections[:1]})
	h.m = h.m.Refresh()
	if conn, _ := h.m.Selected(); conn != "a" {
		t.Fatalf("selected %q after removal", conn)
	}
	h.mgr.Reload(&config.Config{})
	h.m = h.m.Refresh()
	if conn, _ := h.m.Selected(); conn != "" {
		t.Fatalf("selected %q with no connections", conn)
	}
	_ = h.m.View(40, 12, true) // empty tree renders
}

func TestRefreshDropsLateResultOfRemoved(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.fake.SetLatency(30 * time.Millisecond)
	var cmd tea.Cmd
	h.m, cmd = h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // expand a: load in flight
	if cmd == nil || !h.m.Loading() {
		t.Fatal("expected a load in flight")
	}
	h.mgr.Reload(&config.Config{})
	h.m = h.m.Refresh()
	if h.m.Loading() {
		t.Fatal("removed connection still loading")
	}
	h.exec(cmd) // the late result must be ignored, not panic
	if got := rowNames(h.m); len(got) != 0 {
		t.Fatalf("rows = %v", got)
	}
	if len(h.out) != 0 {
		t.Fatalf("stale error surfaced: %v", h.out)
	}
}

func TestPlainStatusDot(t *testing.T) {
	h := newHarness(t, keymap.Default())
	if v := h.view(40, 12); !strings.Contains(v, "●") || strings.Contains(v, "*") {
		t.Fatalf("default dot:\n%s", v)
	}
	h.m = h.m.SetPlain(true)
	if v := h.view(40, 12); strings.Contains(v, "●") || !strings.Contains(v, "*") {
		t.Fatalf("plain dot:\n%s", v)
	}
}

func TestEnterRetriesFailedConnection(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.fake.FailNext("open", errors.New("dial: connection refused"))
	h.exec(h.m.Expand("a"))
	if h.mgr.State("a").Status != session.Failed {
		t.Fatal("a did not fail")
	}
	if conn, ok := h.m.SelectedConn(); !ok || conn != "a" {
		t.Fatalf("SelectedConn = %q, %v", conn, ok)
	}
	h.key("enter")
	if h.mgr.State("a").Status != session.Ready || !strings.Contains(h.view(40, 12), "main") {
		t.Fatalf("enter did not retry:\n%s", h.view(40, 12))
	}
}

func TestBrokenEntryIsListedButNotExpandable(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Connections: []config.Connection{{Name: "a", Driver: "fake", DSN: "fake://a"}},
		Broken:      []config.BrokenConnection{{Name: "bad", Line: 4, Err: "name is required"}, {Line: 9, Err: "x"}},
	}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	h := &harness{t: t, fake: f, mgr: mgr}
	h.m = New(mgr, theme.Dark(), keymap.Default()).Resize(40, 12)
	v := h.view(40, 12)
	for _, want := range []string{"a", "bad", "(entry at line 9)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q:\n%s", want, v)
		}
	}
	h.key("down")
	h.key("enter")
	h.key("right")
	if conn, _ := h.m.Selected(); conn != "bad" {
		t.Fatalf("cursor on %q", conn)
	}
	if rows := h.m.rows(); len(rows) != 3 || rows[1].n.expanded || rows[1].n.loading || h.calls("open") != 0 {
		t.Fatalf("broken entry expanded or opened: %d rows", len(rows))
	}
	for _, line := range strings.Split(h.view(40, 12), "\n") {
		if strings.Contains(line, "bad") && (strings.Contains(line, collapsedMarker) || strings.Contains(line, expandedMarker)) {
			t.Fatalf("broken entry drawn expandable: %q", line)
		}
	}
}

func TestSessionsSyncOnlyWhenStatusCanChange(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.exec(h.m.Expand("a"))
	if err := h.mgr.Close("a"); err != nil {
		t.Fatal(err)
	}
	// Keys, mouse and unrelated messages do not look at the session.
	h.key("down")
	h.send(tea.WindowSizeMsg{})
	h.send(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if got := len(h.m.rows()); got != 3 {
		t.Fatalf("rows = %d, want the stale tree kept (3) until a status change is announced", got)
	}
	// A load result is such a moment.
	h.exec(h.m.Expand("b"))
	if h.m.index["a"].loaded {
		t.Fatal("a load result should have re-synced the closed connection")
	}

	h = newHarness(t, keymap.Default())
	h.exec(h.m.Expand("a"))
	_ = h.mgr.Close("a")
	h.m = h.m.Sync()
	if h.m.index["a"].loaded {
		t.Fatal("Sync (the connecting tick) should drop the closed connection's children")
	}
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func TestDoubleClickWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		gap  time.Duration
		want bool
	}{
		{"inside the window", 399 * time.Millisecond, true},
		{"too slow", 401 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, keymap.Default())
			c := &testClock{t: time.Unix(1000, 0)}
			h.m = h.m.WithClock(c.now)
			h.exec(h.m.Expand("a"))
			h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft}) // select b
			c.t = c.t.Add(tc.gap)
			before := h.calls("open")
			h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
			if got := h.calls("open") == before+1; got != tc.want {
				t.Fatalf("second click activated = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDoubleClickNeedsTheSameNode(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.exec(h.m.Expand("a"))
	h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft}) // b
	h.send(tea.MouseClickMsg{X: 3, Y: 1, Button: tea.MouseLeft}) // namespace
	before := h.calls("open")
	h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft}) // b again, but not twice in a row
	if h.calls("open") != before {
		t.Fatal("clicks on different nodes in between are not a double-click")
	}
}

func TestClickOnKeyboardCursorDoesNotActivate(t *testing.T) {
	h := newHarness(t, keymap.Default())
	h.exec(h.m.Expand("a"))
	h.key("down")
	h.key("down") // on b without a click
	before := h.calls("open")
	h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
	if h.calls("open") != before {
		t.Fatal("a single click must not activate the node under a keyboard cursor")
	}
}

func TestDeliveredLoadReleasesItsRequest(t *testing.T) {
	h := newHarness(t, keymap.Default())
	cmd := h.m.Expand("a")
	tr := h.m.tracker("a")
	if !tr.Current(1) {
		t.Fatal("the load should be the live request while it runs")
	}
	h.exec(cmd)
	if tr.Current(1) {
		t.Fatal("a delivered result should release its request")
	}
	if !h.m.index["a"].loaded {
		t.Fatal("the result should still have been applied")
	}
}

func TestLoadedNames(t *testing.T) {
	h := newHarness(t, keymap.Default())
	if nss, cols := h.m.Loaded("a"); len(nss) != 0 || len(cols) != 0 {
		t.Fatalf("nothing loaded yet: %v %v", nss, cols)
	}
	h.exec(h.m.Expand("a"))
	nss, cols := h.m.Loaded("a")
	if len(nss) != 1 || nss[0] != "main" || len(cols) != 0 {
		t.Fatalf("after expanding a: %v %v", nss, cols)
	}
	h.key("down")
	h.key("enter")
	_, cols = h.m.Loaded("a")
	if len(cols) != 1 || cols[0] != (driver.CollectionRef{Namespace: "main", Name: "tobira_people"}) {
		t.Fatalf("collections = %v", cols)
	}
	if nss, cols := h.m.Loaded("b"); len(nss) != 0 || len(cols) != 0 {
		t.Fatalf("b was never expanded: %v %v", nss, cols)
	}
}
