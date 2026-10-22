package tui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/detail"
	"github.com/xavidop/tobira/internal/tui/grid"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

// updateGolden reports the -update flag. The teatest golden package already
// registers it, so it is looked up rather than redefined.
func updateGolden() bool {
	f := flag.Lookup("update")
	if f == nil {
		return false
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return false
	}
	b, _ := g.Get().(bool)
	return b
}

const (
	winW = 100
	winH = 30
)

// harness drives the root model the way Bubble Tea would: Update on the test
// goroutine, commands on their own goroutines whose results are queued and
// fed back by pump. Commands that never finish in time (flash expiry) are
// simply left running.
type harness struct {
	t       *testing.T
	m       *Model
	fake    *drivertest.Fake
	mgr     *session.Manager
	ch      chan tea.Msg
	didQuit bool
	clip    []string
	msgs    []tea.Msg
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, secrets.Resolver{}) }

func newHarnessWith(t *testing.T, res secrets.Resolver) *harness {
	t.Helper()
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "local", Driver: "fake", DSN: "fake://local", Color: "green"},
		{Name: "ro", Driver: "fake", DSN: "fake://ro", ReadOnly: true},
	}}
	mgr := session.New(cfg, reg, res, false)
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

func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if msg == nil {
			return
		}
		select {
		case h.ch <- msg:
		default:
		}
	}()
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	switch m := msg.(type) {
	case tea.BatchMsg:
		for _, c := range m {
			h.run(c)
		}
		return
	case tea.QuitMsg:
		h.didQuit = true
		return
	}
	if rv := reflect.ValueOf(msg); rv.Kind() == reflect.String && strings.Contains(reflect.TypeOf(msg).String(), "lipboard") {
		h.clip = append(h.clip, rv.String())
	}
	h.msgs = append(h.msgs, msg)
	_, cmd := h.m.Update(msg)
	h.run(cmd)
}

// pump feeds queued command results to the model until cond holds.
func (h *harness) pump(cond func() bool) {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case msg := <-h.ch:
			h.send(msg)
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			h.t.Fatalf("condition not met; screen:\n%s", h.text())
		}
	}
}

func (h *harness) until(substr string) {
	h.t.Helper()
	h.pump(func() bool { return strings.Contains(h.text(), substr) })
}

// drain processes whatever results arrive within d.
func (h *harness) drain(d time.Duration) {
	end := time.After(d)
	for {
		select {
		case msg := <-h.ch:
			h.send(msg)
		case <-end:
			return
		}
	}
}

// quitted reports whether the model asked to quit, after letting queued
// command results arrive.
func (h *harness) quitted() bool {
	h.drain(30 * time.Millisecond)
	return h.didQuit
}

func (h *harness) raw() string  { return h.m.View().Content }
func (h *harness) text() string { return ansi.Strip(h.raw()) }

func (h *harness) keys(ks ...string) {
	h.t.Helper()
	for _, k := range ks {
		h.send(key(k))
	}
}

// typ types text rune by rune.
func (h *harness) typ(s string) {
	h.t.Helper()
	for _, r := range s {
		h.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}
	case "f1":
		return tea.KeyPressMsg{Code: tea.KeyF1}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+home":
		return tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl}
	case "ctrl+end":
		return tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl}
	}
	r := []rune(s)
	if len(r) != 1 {
		panic("key: unknown key " + s)
	}
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

// openPeople expands local and opens its People collection in the grid.
func (h *harness) openPeople() {
	h.t.Helper()
	h.keys("enter") // expand local
	h.until("main")
	h.keys("down", "enter") // expand main
	h.until("tobira_people")
	h.keys("down", "enter") // open the collection
	h.until("Ada Lovelace")
}

var durations = regexp.MustCompile(`<1ms|\d+ms|\d+\.\ds`)

func golden(t *testing.T, name, got string) {
	t.Helper()
	got = durations.ReplaceAllString(got, "T") // load times vary run to run
	path := filepath.Join("testdata", name+".golden")
	if updateGolden() {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden (run with -update to accept):\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// checkFrame asserts the View invariants: exactly h lines, none wider than w,
// and no control sequences beyond styling.
func checkFrame(t *testing.T, h *harness, w, ht int) {
	t.Helper()
	raw := h.raw()
	tuitest.NoInjectedControls(t, raw)
	lines := strings.Split(raw, "\n")
	if w < 1 || ht < 1 {
		return
	}
	if len(lines) != ht {
		t.Fatalf("View has %d lines, want %d:\n%s", len(lines), ht, ansi.Strip(raw))
	}
	for i, l := range lines {
		if got := ansi.StringWidth(l); got > w {
			t.Fatalf("line %d is %d columns wide, max %d: %q", i, got, w, ansi.Strip(l))
		}
	}
}

func TestViewFlags(t *testing.T) {
	h := newHarness(t)
	v := h.m.View()
	if !v.AltScreen || v.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("View = AltScreen %v, MouseMode %v; want alt screen and cell-motion mouse", v.AltScreen, v.MouseMode)
	}
}

func TestAppGoldenStartup(t *testing.T) {
	h := newHarness(t)
	checkFrame(t, h, winW, winH)
	golden(t, "startup", h.text())
}

func TestAppBrowseFlow(t *testing.T) {
	h := newHarness(t)
	h.keys("enter")
	h.until("main")
	golden(t, "flow_1_connection_expanded", h.text())

	h.keys("down", "enter")
	h.until("tobira_people")
	h.keys("down", "enter")
	h.until("Ada Lovelace")
	checkFrame(t, h, winW, winH)
	golden(t, "flow_2_grid", h.text())

	h.keys("enter") // row detail
	h.until("Ōtani Shōhei")
	if !strings.Contains(h.text(), "tags") {
		t.Fatalf("detail overlay missing:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	golden(t, "flow_3_detail", h.text())

	h.keys("esc")
	if strings.Contains(h.text(), "tags │") {
		t.Fatalf("esc did not close the detail:\n%s", h.text())
	}
	h.keys("right", "s") // sort by the name column
	h.until("name ▲")
	h.until("Ada Lovelace") // the re-browse has landed
	checkFrame(t, h, winW, winH)
	golden(t, "flow_4_sorted", h.text())
}

func TestTinyWindow(t *testing.T) {
	sizes := [][2]int{{20, 5}, {1, 1}, {0, 0}, {10, 2}, {59, 10}, {60, 10}, {5, 40}, {200, 3}}
	for _, sz := range sizes {
		t.Run(strings.Join([]string{itoa(sz[0]), itoa(sz[1])}, "x"), func(t *testing.T) {
			h := newHarness(t)
			h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			checkFrame(t, h, sz[0], sz[1])
			h.keys("enter")
			h.drain(50 * time.Millisecond)
			h.keys("down", "enter", "down", "enter", "tab", "f", "esc", ":", "esc", "f1", "esc", "tab")
			h.drain(100 * time.Millisecond)
			checkFrame(t, h, sz[0], sz[1])
			h.send(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
			checkFrame(t, h, sz[0], sz[1])
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestResizeDuringLoad(t *testing.T) {
	h := newHarness(t)
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.fake.SetLatency(200 * time.Millisecond)
	h.keys("down", "enter") // opening takes a while now
	h.send(tea.WindowSizeMsg{Width: 70, Height: 20})
	checkFrame(t, h, 70, 20)
	h.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	h.until("Ada Lovelace")
	checkFrame(t, h, 120, 40)
	l := h.m.lay
	if l.grid.w != 120-sidebarW-1 || l.grid.h != 40-2 {
		t.Fatalf("grid rect after resize = %+v", l.grid)
	}
	if got := strings.Count(h.text(), "\n") + 1; got != 40 {
		t.Fatalf("lines = %d, want 40", got)
	}
}

func TestLayoutMatchesView(t *testing.T) {
	h := newHarness(t)
	h.openPeople()
	l := h.m.lay
	if l.sidebar.w != sidebarW || l.grid.x != sidebarW+1 || l.grid.w != winW-sidebarW-1 {
		t.Fatalf("layout = %+v", l)
	}
	// Opening the filter takes a line from the panes; the sizes handed to the
	// components must follow (View is drawn from the same layout).
	before := l.grid.h
	h.keys("f")
	if got := h.m.lay.grid.h; got != before-1 {
		t.Fatalf("grid height with filter bar = %d, want %d", got, before-1)
	}
	checkFrame(t, h, winW, winH)
}

func TestCtrlCCancelsThenQuits(t *testing.T) {
	h := newHarness(t)
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.fake.SetLatency(3 * time.Second)
	h.keys("down", "enter")
	h.drain(50 * time.Millisecond) // the open is in flight
	h.keys("ctrl+c")
	if h.quitted() {
		t.Fatal("ctrl+c quit while a load was running; it must cancel first")
	}
	if !strings.Contains(h.text(), "cancelled") {
		t.Fatalf("no cancelled flash:\n%s", h.text())
	}
	if h.m.gr.Loading() {
		t.Fatal("load still running after cancel")
	}
	h.keys("ctrl+c")
	if !h.quitted() {
		t.Fatal("second ctrl+c did not quit")
	}
}

func TestCtrlCCancelsPageLoad(t *testing.T) {
	h := newHarness(t)
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.keys("down", "enter")
	h.until("Ada Lovelace")
	h.fake.SetLatency(3 * time.Second)
	h.keys("s") // re-browses with a sort: a page load starts
	if !h.m.gr.Loading() {
		t.Fatal("sort did not start a load")
	}
	h.keys("ctrl+c")
	if h.quitted() || h.m.gr.Loading() {
		t.Fatalf("ctrl+c: quit=%v loading=%v", h.quitted(), h.m.gr.Loading())
	}
}

func TestQuitKeys(t *testing.T) {
	h := newHarness(t)
	h.keys("q")
	if !h.quitted() {
		t.Fatal("q did not quit")
	}
	h = newHarness(t)
	h.keys(":")
	h.typ("q")
	if h.quitted() {
		t.Fatal("typing q in the palette quit")
	}
	h.keys("enter")
	if !h.quitted() {
		// the QuitCmd arrives as a message
		h.pump(h.quitted)
	}
}

func TestQuitSuppressedInOverlay(t *testing.T) {
	h := newHarness(t)
	h.keys("f1")
	h.keys("q")
	if h.quitted() {
		t.Fatal("q quit while help was open")
	}
	h.keys("esc")
	if strings.Contains(h.text(), "Navigation") {
		t.Fatalf("esc did not close help:\n%s", h.text())
	}
}

func TestFirstPaint(t *testing.T) {
	// Measured directly on the model: New + Init + the window-size message +
	// the first View, with the Fake. A teatest run would add program startup
	// and renderer flush time that the model does not control. Retried like
	// every timing test: a loaded machine can miss once, a slow first paint
	// misses every attempt.
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })

	retryTiming(t, timingAttempts, func(t *testing.T) string {
		start := time.Now()
		model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
		if err != nil {
			t.Fatal(err)
		}
		_ = model.Init()
		model, _ = model.Update(tea.WindowSizeMsg{Width: winW, Height: winH})
		out := model.View().Content
		took := time.Since(start)
		if !strings.Contains(ansi.Strip(out), "local") {
			t.Fatalf("first frame has no connection list:\n%s", ansi.Strip(out))
		}
		if took > 100*time.Millisecond {
			return fmt.Sprintf("first paint took %v, want < 100ms", took)
		}
		return ""
	})
}

func TestThemeSwitchPalette(t *testing.T) {
	h := newHarness(t)
	h.openPeople()
	dark := h.raw()
	if !strings.Contains(dark, "38;2;232;236;244") {
		t.Fatalf("dark foreground missing from the dark frame")
	}
	h.keys(":")
	h.typ("theme tobira-light")
	h.keys("enter")
	h.pump(func() bool { return h.m.th.Name == "tobira-light" })
	light := h.raw()
	if strings.Contains(light, "38;2;232;236;244") || !strings.Contains(light, "38;2;15;23;42") {
		t.Fatalf("light theme not applied to every pane")
	}
	// Grid state survives the switch.
	if !strings.Contains(ansi.Strip(light), "Ada Lovelace") {
		t.Fatalf("rows lost on theme switch:\n%s", ansi.Strip(light))
	}
	checkFrame(t, h, winW, winH)
}

func TestKeymapSwitchPalette(t *testing.T) {
	h := newHarness(t)
	h.openPeople()
	h.keys(":")
	h.typ("keymap vim")
	h.keys("enter")
	h.pump(func() bool { return h.m.km.Name == "vim" })
	h.keys("j", "j")
	if row, _ := h.m.gr.Cursor(); row != 2 {
		t.Fatalf("after :keymap vim, jj moved the cursor to row %d, want 2", row)
	}
}

func TestBackgroundColorSelectsAutoTheme(t *testing.T) {
	f := drivertest.NewFake()
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{}, reg, secrets.Resolver{}, false)
	model, err := New(Options{Manager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	m := model.(*Model)
	if m.th.Name != "tobira-dark" {
		t.Fatalf("default theme = %s, want tobira-dark until the background arrives", m.th.Name)
	}
	m.Update(tea.BackgroundColorMsg{Color: lightColor{}})
	if m.th.Name != "tobira-light" {
		t.Fatalf("theme after a light background = %s", m.th.Name)
	}
	forced, _ := New(Options{Manager: mgr, Theme: "tobira-dark"})
	forced.Update(tea.BackgroundColorMsg{Color: lightColor{}})
	if forced.(*Model).th.Name != "tobira-dark" {
		t.Fatal("a forced theme must ignore the terminal background")
	}
}

type lightColor struct{}

func (lightColor) RGBA() (r, g, b, a uint32) { return 0xffff, 0xffff, 0xffff, 0xffff }

func TestNewRejectsBadOptions(t *testing.T) {
	f := drivertest.NewFake()
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "a", Driver: "fake", DSN: "fake://a"}}}, reg, secrets.Resolver{}, false)
	for name, o := range map[string]Options{
		"no manager": {},
		"bad theme":  {Manager: mgr, Theme: "neon"},
		"bad keymap": {Manager: mgr, Keymap: "emacs"},
		"bad focus":  {Manager: mgr, Focus: "nope"},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestFocusOptionExpandsConnection(t *testing.T) {
	h := newHarness(t)
	model, err := New(Options{Manager: h.mgr, Theme: "tobira-dark", Focus: "ro"})
	if err != nil {
		t.Fatal(err)
	}
	h2 := &harness{t: t, m: model.(*Model), fake: h.fake, mgr: h.mgr, ch: make(chan tea.Msg, 64)}
	h2.run(h2.m.Init())
	h2.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	h2.until("main")
}

func TestReadOnlyBadge(t *testing.T) {
	h := newHarness(t)
	h.openPeople()
	if strings.Contains(h.text(), "🔒") {
		t.Fatalf("read-write connection shows the lock:\n%s", h.text())
	}
	h = newHarness(t)
	h.keys("down", "enter") // ro
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.keys("down", "enter")
	h.until("Ada Lovelace")
	if !strings.Contains(h.text(), "🔒") {
		t.Fatalf("read-only connection has no lock badge:\n%s", h.text())
	}
}

func TestAskOpensThePanelAndRealEdit(t *testing.T) {
	h := newHarness(t)
	h.keys("a")
	if !h.m.ai.open || !strings.Contains(h.text(), "AI is not configured") || strings.Contains(h.text(), "arrives in") {
		t.Fatalf("a: no AI panel:\n%s", h.text())
	}
	h.keys("esc", "a")
	h.m.focus = focusSidebar
	h.openPeople()
	h.pump(func() bool { return h.m.src.Described() })
	h.keys("e")
	if strings.Contains(h.text(), "arrives in M3") || h.m.cell == nil {
		t.Fatalf("e in the grid did not open the cell editor:\n%s", h.text())
	}
}

func TestTextInputSuppressesLetters(t *testing.T) {
	letters := "qfsaeyYgGnED:"
	t.Run("filter bar", func(t *testing.T) {
		h := newHarness(t)
		h.openPeople()
		h.keys("f")
		if !h.m.fl.Focused() {
			t.Fatal("f did not open the filter")
		}
		h.typ(letters)
		if h.quitted() || !h.m.fl.Focused() || h.m.fl.Value() != letters {
			t.Fatalf("filter swallowed %q wrongly: quit=%v focused=%v value=%q", letters, h.quitted(), h.m.fl.Focused(), h.m.fl.Value())
		}
		if strings.Contains(h.text(), "arrives in") {
			t.Fatal("a placeholder fired while typing")
		}
	})
	t.Run("palette", func(t *testing.T) {
		h := newHarness(t)
		h.keys(":")
		h.typ("qfsaeyYgGnED")
		if h.quitted() || h.m.pal.Value() != "qfsaeyYgGnED" {
			t.Fatalf("palette: quit=%v value=%q", h.quitted(), h.m.pal.Value())
		}
		if strings.Contains(h.text(), "arrives in") {
			t.Fatal("a placeholder fired while typing")
		}
	})
	t.Run("sidebar filter", func(t *testing.T) {
		h := newHarness(t)
		h.keys("/")
		if !h.m.sb.FilterFocused() {
			t.Fatal("/ did not focus the sidebar filter")
		}
		h.typ(letters)
		if h.quitted() || strings.Contains(h.text(), "arrives in") || !h.m.sb.FilterFocused() {
			t.Fatalf("sidebar filter: quit=%v\n%s", h.quitted(), h.text())
		}
		if !strings.Contains(h.text(), letters) {
			t.Fatalf("typed text not shown in the sidebar filter:\n%s", h.text())
		}
		h.keys("ctrl+c")
		if !h.quitted() {
			t.Fatal("ctrl+c must still be handled by the root while an input has focus")
		}
	})
}

func TestMouseClickGridCell(t *testing.T) {
	h := newHarness(t)
	h.openPeople()
	g := h.m.lay.grid
	// Row 3 (index 2): header is at g.y, rows follow. Column 1 starts after
	// id's width and the separator.
	h.send(tea.MouseClickMsg{X: g.x + 1, Y: g.y + 1 + 2, Button: tea.MouseLeft})
	if row, col := h.m.gr.Cursor(); row != 2 || col != 0 {
		t.Fatalf("click on (row 2, col 0) moved the cursor to (%d, %d)", row, col)
	}
	if h.m.focus != focusGrid {
		t.Fatal("clicking the grid did not focus it")
	}
	// A click in the header or the title row must not move the cursor.
	h.send(tea.MouseClickMsg{X: g.x + 1, Y: g.y, Button: tea.MouseLeft})
	if row, _ := h.m.gr.Cursor(); row != 2 {
		t.Fatalf("header click moved the cursor to row %d", row)
	}
	// Second click on the cursor cell opens the detail.
	h.send(tea.MouseClickMsg{X: g.x + 1, Y: g.y + 1 + 2, Button: tea.MouseLeft})
	h.pump(func() bool { return h.m.det != nil })
}

func TestMouseClickSidebar(t *testing.T) {
	h := newHarness(t)
	s := h.m.lay.sidebar
	h.send(tea.MouseClickMsg{X: s.x + 2, Y: s.y + 1, Button: tea.MouseLeft}) // select "ro"
	if conn, _ := h.m.sb.Selected(); conn != "ro" {
		t.Fatalf("selected %q after clicking the second row", conn)
	}
	h.send(tea.MouseClickMsg{X: s.x + 2, Y: s.y + 1, Button: tea.MouseLeft}) // activate
	h.until("main")
	if h.m.focus != focusSidebar {
		t.Fatal("sidebar click lost focus")
	}
}

func TestConnectingTicksOnlyWhileConnecting(t *testing.T) {
	h := newHarness(t)
	if h.m.ticking {
		t.Fatal("a tick is armed at startup with nothing connecting")
	}
	h.fake.SetLatency(300 * time.Millisecond)
	h.keys("enter") // expand local: it starts connecting
	if !h.m.ticking {
		t.Fatal("no tick armed when a connection is about to connect")
	}
	h.pump(func() bool { return h.mgr.State("local").Status == session.Connecting })
	_, cmd := h.m.Update(tickMsg{})
	if cmd == nil || !h.m.ticking {
		t.Fatal("tick not re-armed while a connection is Connecting")
	}
	h.pump(func() bool { return h.mgr.State("local").Status == session.Ready })
	h.pump(func() bool { return strings.Contains(h.text(), "main") })
	_, cmd = h.m.Update(tickMsg{})
	if cmd != nil || h.m.ticking {
		t.Fatalf("tick kept running with nothing connecting (cmd=%v ticking=%v)", cmd != nil, h.m.ticking)
	}
}

func TestFlashRedacted(t *testing.T) {
	res := secrets.Resolver{Getenv: func(k string) string {
		if k == secrets.EnvName("local") {
			return "pw-Secret-9"
		}
		return ""
	}}
	h := newHarnessWith(t, res)
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.fake.FailNext("browse", errors.New("server said pw-Secret-9 is wrong \x1b[31mred"))
	h.keys("down", "enter")
	h.until("xxxxx")
	out := h.raw()
	if strings.Contains(out, "pw-Secret-9") {
		t.Fatalf("secret leaked:\n%s", ansi.Strip(out))
	}
	tuitest.NoInjectedControls(t, out)
	if !strings.Contains(h.statusLine(), "xxxxx") {
		t.Fatalf("status bar does not show the redacted error: %q", h.statusLine())
	}
}

func (h *harness) statusLine() string {
	lines := strings.Split(h.text(), "\n")
	return lines[len(lines)-1]
}

func TestPaletteCommands(t *testing.T) {
	cmdFlash := func(t *testing.T, line, want string) {
		t.Helper()
		h := newHarness(t)
		h.keys(":")
		h.typ(line)
		h.keys("enter")
		h.pump(func() bool { return strings.Contains(h.statusLine(), want) })
	}
	t.Run("ask without ai", func(t *testing.T) { cmdFlash(t, "ask why", "AI is not configured") })
	t.Run("export usage", func(t *testing.T) {
		h := newHarness(t)
		h.keys(":")
		h.typ("export csv")
		h.keys("enter")
		if !strings.Contains(h.text(), "usage: export csv|json|ndjson <path>") {
			t.Fatalf("no usage hint:\n%s", h.text())
		}
	})
	t.Run("connection add", func(t *testing.T) {
		cmdFlash(t, "connection add", "no config file path")
	})
	t.Run("connection rm", func(t *testing.T) {
		cmdFlash(t, "connection rm x", "no config file path")
	})
	t.Run("unknown connect", func(t *testing.T) { cmdFlash(t, "connect nope", `unknown connection "nope"`) })
	t.Run("unknown connect escaped", func(t *testing.T) {
		h := newHarness(t)
		h.keys(":")
		h.send(tea.KeyPressMsg{Code: 'c', Text: "connect a\x1b[2Jb"})
		h.keys("enter")
		h.pump(func() bool { return strings.Contains(h.statusLine(), "unknown connection") })
		tuitest.NoInjectedControls(t, h.raw())
	})
	t.Run("connect expands", func(t *testing.T) {
		h := newHarness(t)
		h.keys(":")
		h.typ("connect ro")
		h.keys("enter")
		h.until("main")
	})
	t.Run("help", func(t *testing.T) {
		h := newHarness(t)
		h.keys(":")
		h.typ("help")
		h.keys("enter")
		h.pump(func() bool { return h.m.help })
		h.until("Navigation")
	})
	t.Run("reload", func(t *testing.T) {
		h := newHarness(t)
		h.openPeople()
		h.keys("down", "down")
		h.keys(":")
		h.typ("reload")
		h.keys("enter")
		h.pump(func() bool { r, _ := h.m.gr.Cursor(); return r == 0 })
		h.until("Ada Lovelace")
	})
	t.Run("reload without collection", func(t *testing.T) { cmdFlash(t, "reload", "nothing to reload") })
	t.Run("palette error takes two lines", func(t *testing.T) {
		h := newHarness(t)
		before := h.m.lay.sidebar.h
		h.keys(":")
		h.typ("bogus")
		h.keys("enter")
		if !strings.Contains(h.text(), "unknown command") {
			t.Fatalf("no palette error:\n%s", h.text())
		}
		if got := h.m.lay.sidebar.h; got != before-2 {
			t.Fatalf("sidebar height with palette error = %d, want %d", got, before-2)
		}
		checkFrame(t, h, winW, winH)
		h.keys("esc")
		if got := h.m.lay.sidebar.h; got != before {
			t.Fatalf("sidebar height after closing the palette = %d, want %d", got, before)
		}
	})
}

func TestCopy(t *testing.T) {
	h := newHarness(t)
	h.openPeople()
	h.keys("right", "y") // name column of row 0
	h.drain(30 * time.Millisecond)
	if len(h.clip) == 0 || !strings.Contains(h.clip[len(h.clip)-1], "Ōtani") {
		t.Fatalf("clipboard after y = %q", h.clip)
	}
	if !strings.Contains(h.statusLine(), "copied cell") {
		t.Fatalf("no copy confirmation: %q", h.statusLine())
	}
	h.keys("Y")
	h.drain(30 * time.Millisecond)
	last := h.clip[len(h.clip)-1]
	if !strings.HasPrefix(last, `{"id":1,`) || !strings.Contains(last, `"name":"Ōtani Shōhei"`) {
		t.Fatalf("clipboard after Y = %q", last)
	}
	if !strings.Contains(h.statusLine(), "copied row") {
		t.Fatalf("no copy confirmation: %q", h.statusLine())
	}
}

func TestFocusCycling(t *testing.T) {
	h := newHarness(t)
	if h.m.focus != focusSidebar {
		t.Fatal("sidebar should start focused")
	}
	h.keys("tab")
	if h.m.focus != focusGrid {
		t.Fatal("tab did not focus the grid")
	}
	h.keys("shift+tab")
	if h.m.focus != focusSidebar {
		t.Fatal("shift+tab did not go back")
	}
}

func TestNarrowShowsFocusedPane(t *testing.T) {
	h := newHarness(t)
	h.send(tea.WindowSizeMsg{Width: 59, Height: 12})
	if !strings.Contains(h.text(), "connections") {
		t.Fatalf("narrow window should show the focused sidebar:\n%s", h.text())
	}
	h.keys("tab")
	if strings.Contains(h.text(), "● local") {
		t.Fatalf("narrow window should hide the sidebar while the grid is focused:\n%s", h.text())
	}
	checkFrame(t, h, 59, 12)
}

func TestOpenNonBrowser(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople(), drivertest.WithoutCapabilities(driver.CapBrowser))
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, _ := New(Options{Manager: mgr, Theme: "tobira-dark"})
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 64)}
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.keys("down", "enter")
	h.until("browsing not supported by fake")
	h.keys("tab", "f")
	if h.m.fl.Focused() {
		t.Fatal("filter bar opened for a collection that cannot browse")
	}
}

// TestProgramBrowseFlow runs the real program under teatest.
func TestProgramBrowseFlow(t *testing.T) {
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
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("tobira_people")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("Ada Lovelace")
	tm.Send(key("ctrl+c")) // nothing loading: quits
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	out := ansi.Strip(final.View().Content)
	if !strings.Contains(out, "Ada Lovelace") || !strings.Contains(out, "tobira_people") {
		t.Fatalf("final frame:\n%s", out)
	}
}

// firstWrite discards what it is given and closes wrote on the first write.
type firstWrite struct {
	once  sync.Once
	wrote chan struct{}
}

func (w *firstWrite) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.wrote) })
	return len(p), nil
}

func TestRunClosesConnections(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	if _, err := mgr.Open(context.Background(), "local"); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	out := &firstWrite{wrote: make(chan struct{})}
	go func() { done <- Run(ctx, Options{Manager: mgr, Theme: "tobira-dark"}, pr, out) }()
	select { // the program is running once it draws
	case <-out.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the program never drew")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after a cancelled context, want a clean exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	if st := mgr.State("local").Status; st != session.Idle {
		t.Fatalf("connection status after Run = %v, want idle (CloseAll)", st)
	}
}

// bothExpanded opens local's People, then expands ro so the sidebar rows are
// local(0) main(1) tobira_people(2) ro(3) main(4) tobira_people(5). It leaves
// focus on the sidebar with the cursor on row 5.
func (h *harness) bothExpanded() {
	h.t.Helper()
	h.openPeople()
	h.keys("tab", "down", "enter")
	h.pump(func() bool { return h.mgr.State("ro").Status == session.Ready })
	h.pump(func() bool {
		for _, l := range strings.Split(h.text(), "\n") {
			if strings.HasPrefix(l, "  ▸ main") {
				return true
			}
		}
		return false
	})
	h.keys("down", "enter")
	h.pump(func() bool {
		n := 0
		for _, l := range strings.Split(h.text(), "\n") {
			if strings.HasPrefix(l, "      tobira_people") {
				n++
			}
		}
		return n == 2
	})
	h.keys("down")
	if conn, ref := h.m.sb.Selected(); conn != "ro" || ref == nil {
		h.t.Fatalf("cursor on %q %v, want ro's collection", conn, ref)
	}
}

func TestStaleErrorNotFlashed(t *testing.T) {
	res := secrets.Resolver{Getenv: func(k string) string {
		switch k {
		case secrets.EnvName("local"):
			return "pwAsecret"
		case secrets.EnvName("ro"):
			return "pwBsecret"
		}
		return ""
	}}
	h := newHarnessWith(t, res)
	h.bothExpanded()
	h.keys("enter") // switch to ro's collection; local's loads are now stale
	h.pump(func() bool { return strings.Contains(h.text(), "ro ▸ main ▸ tobira_people · 57 rows") })
	// A late failure of the previous collection's request (id 1 is long
	// gone) must be dropped, not flashed under the new connection.
	h.send(grid.PageMsg{ID: 1, Err: errors.New("auth failed for pwAsecret")})
	h.drain(30 * time.Millisecond)
	if out := h.text(); strings.Contains(out, "pwAsecret") || strings.Contains(out, "auth failed") {
		t.Fatalf("a stale error from the previous collection reached the screen:\n%s", out)
	}
	// A live failure is still flashed, redacted with the issuing connection.
	h.fake.FailNext("browse", errors.New("denied for pwBsecret"))
	h.keys("s") // re-browse with a sort
	h.pump(func() bool { return strings.Contains(h.statusLine(), "denied for xxxxx") })
	if strings.Contains(h.raw(), "pwBsecret") {
		t.Fatalf("live error leaked its secret:\n%s", h.text())
	}
}

func TestSwitchThenCancelKeepsLabelsAndRowsTogether(t *testing.T) {
	h := newHarness(t)
	h.bothExpanded()
	h.fake.SetLatency(3 * time.Second)
	h.keys("enter") // open ro's collection; it takes a while
	h.drain(30 * time.Millisecond)
	h.keys("ctrl+c")
	if h.quitted() {
		t.Fatal("ctrl+c quit instead of cancelling")
	}
	out := h.text()
	if strings.Contains(out, "Ada Lovelace") {
		t.Fatalf("rows of the previous collection are still shown under the new labels:\n%s", out)
	}
	if !strings.Contains(out, "ro ▸ main ▸ tobira_people") || !strings.Contains(out, "🔒") {
		t.Fatalf("labels do not describe the collection shown:\n%s", out)
	}
	if h.m.cur.conn != "ro" {
		t.Fatalf("current = %+v", h.m.cur)
	}
}

func TestCtrlCCancelsSidebarLoad(t *testing.T) {
	h := newHarness(t)
	h.fake.SetLatency(3 * time.Second)
	h.keys("enter") // expanding local: opening the connection
	if !h.m.sb.Loading() {
		t.Fatal("sidebar not loading")
	}
	h.keys("ctrl+c")
	if h.quitted() || h.m.sb.Loading() {
		t.Fatalf("ctrl+c should cancel the sidebar load first (quit=%v)", h.didQuit)
	}
	h.keys("ctrl+c")
	if !h.quitted() {
		t.Fatal("second ctrl+c did not quit")
	}
}

func TestPasteGoesToFocusedInput(t *testing.T) {
	h := newHarness(t)
	h.keys(":")
	h.send(tea.PasteMsg{Content: "theme tobira-light"})
	if got := h.m.pal.Value(); got != "theme tobira-light" {
		t.Fatalf("palette value after paste = %q", got)
	}
}

func TestThemeSwitchKeepsOpenPalette(t *testing.T) {
	h := newHarness(t)
	h.keys(":")
	h.typ("hel")
	h.send(tea.BackgroundColorMsg{Color: lightColor{}}) // forced theme: ignored, but must not reset input
	h.m.setTheme("tobira-light")
	if !h.m.pal.Focused() || h.m.pal.Value() != "hel" {
		t.Fatalf("palette reset by a theme change: focused=%v value=%q", h.m.pal.Focused(), h.m.pal.Value())
	}
}

// runMid runs the real program, puts a page load in flight (Fake latency
// 20s) and stops it with quit. Run must return promptly and leave no
// goroutine behind: the quit path has to cancel the load itself.
func runMid(t *testing.T, quit func(pw *io.PipeWriter, cancel context.CancelFunc)) {
	ign := goleak.IgnoreCurrent() // at the start: only goroutines this test leaks count
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	f.SetLatency(150 * time.Millisecond)
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{Manager: mgr, Theme: "tobira-dark"}, pr, io.Discard) }()
	send := func(s string) { _, _ = pw.Write([]byte(s)); time.Sleep(400 * time.Millisecond) }
	send("\r")       // expand local
	send("\x1b[B\r") // expand main
	f.SetLatency(20 * time.Second)
	send("\x1b[B\r") // open the collection: the page load is now in flight
	start := time.Now()
	quit(pw, cancel)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Run took %v to return", d)
	}
	_ = pw.Close()
	goleak.VerifyNone(t, ign)
}

func TestRunQuitMidLoadStopsWork(t *testing.T) {
	t.Run("q", func(t *testing.T) {
		runMid(t, func(pw *io.PipeWriter, _ context.CancelFunc) { _, _ = pw.Write([]byte("q")) })
	})
	t.Run("context cancelled", func(t *testing.T) {
		runMid(t, func(_ *io.PipeWriter, cancel context.CancelFunc) { cancel() })
	})
}

const longConnErr = "failed to connect to `host=db.internal.example.com user=app database=orders`: " +
	"dial error (dial tcp 10.20.30.40:5432: connect: connection refused)"

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestFailedConnectionShownInGridPane(t *testing.T) {
	h := newHarness(t)
	h.fake.FailNext("open", errors.New(longConnErr+" password=hunter2"))
	h.keys("enter") // expand local: the open fails
	h.pump(func() bool { return h.mgr.State("local").Status == session.Failed })
	h.drain(30 * time.Millisecond)
	pane := squash(h.paneText()) // wrapping may split at spaces
	for _, want := range []string{"connection local failed:", "connection refused)", "press enter to retry"} {
		if !strings.Contains(pane, want) {
			t.Fatalf("grid pane lacks %q:\n%s", want, h.text())
		}
	}
	if strings.Contains(pane, "hunter2") {
		t.Fatalf("pane leaks the password: %q", pane)
	}
	tuitest.NoInjectedControls(t, h.raw())
	// Enter retries, and the pane gives way to the namespaces.
	h.keys("enter")
	h.until("main")
	if strings.Contains(h.text(), "failed:") {
		t.Fatalf("failure pane still shown after a successful retry:\n%s", h.text())
	}
}

// paneText is the screen to the right of the sidebar divider, without the
// status bar.
func (h *harness) paneText() string {
	var out []string
	lines := strings.Split(h.text(), "\n")
	for _, l := range lines[:len(lines)-1] {
		if _, right, ok := strings.Cut(l, "│"); ok {
			out = append(out, right)
		}
	}
	return strings.Join(out, "\n")
}

func TestErrorFlashFullWidthAndDismissedByKey(t *testing.T) {
	h := newHarness(t)
	h.fake.FailNext("open", errors.New("dial tcp 10.20.30.40:5432: connect: connection refused"))
	h.keys("enter")
	h.pump(func() bool { return h.mgr.State("local").Status == session.Failed })
	h.drain(30 * time.Millisecond)
	if st := h.statusLine(); !strings.Contains(st, "connection refused") {
		t.Fatalf("status bar flash truncated: %q", st)
	}
	h.keys("down")
	if st := h.statusLine(); strings.Contains(st, "connection refused") {
		t.Fatalf("error flash survived a key press: %q", st)
	}
}

func TestPlainProfileSwitchesSidebarDot(t *testing.T) {
	h := newHarness(t)
	h.send(tea.ColorProfileMsg{Profile: colorprofile.ASCII})
	sb := strings.Split(h.text(), "\n")[1]
	if strings.Contains(sb, "●") || !strings.Contains(sb, "*") {
		t.Fatalf("sidebar still uses the emoji dot in plain mode: %q", sb)
	}
}

func TestWheelScrollsHelpAndDetail(t *testing.T) {
	h := newHarness(t)
	h.send(tea.WindowSizeMsg{Width: winW, Height: 12})
	g := h.m.lay.grid
	wheel := func(b tea.MouseButton) {
		h.send(tea.MouseWheelMsg{X: g.x + 2, Y: g.y + 2, Button: b})
	}
	h.keys("?")
	if !h.m.help || h.m.helpOff != 0 {
		t.Fatal("help not open")
	}
	wheel(tea.MouseWheelDown)
	if h.m.helpOff == 0 {
		t.Fatal("wheel did not scroll help")
	}
	wheel(tea.MouseWheelUp)
	if h.m.helpOff != 0 {
		t.Fatalf("helpOff = %d after wheel up", h.m.helpOff)
	}
	h.keys("esc", "tab") // back to the sidebar

	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	h.openPeople()
	h.keys("enter") // row detail
	if h.m.det == nil {
		t.Fatal("detail not open")
	}
	h.send(tea.WindowSizeMsg{Width: winW, Height: 5})
	g = h.m.lay.grid
	before := h.text()
	wheel(tea.MouseWheelDown)
	if h.text() == before {
		t.Fatalf("wheel did not move the detail view:\n%s", before)
	}
	if h.m.det == nil {
		t.Fatal("wheel closed the detail")
	}
}

func TestEnterOnThePageErrorRetries(t *testing.T) {
	h := newHarness(t)
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.fake.FailNext("browse", errors.New("server is warming up"))
	h.keys("down", "enter") // open the collection: page 0 fails
	h.until("server is warming up")
	if h.m.gr.Status().Loaded != 0 || !h.m.gr.Failed() {
		t.Fatal("page 0 should have failed with nothing loaded")
	}
	h.keys("enter") // retry from the error message
	h.until("Ada Lovelace")
	if strings.Contains(h.text(), "warming up") {
		t.Fatalf("the error should be gone after a successful retry:\n%s", h.text())
	}
	if h.m.det != nil {
		t.Fatal("enter on an error must retry, not open a detail")
	}
}

func TestAutoThemeBackgroundEndToEnd(t *testing.T) {
	build := func(theme string) *harness {
		f := drivertest.NewFake(drivertest.WithPeople())
		reg := driver.NewRegistry()
		_ = reg.Add(f.Driver())
		cfg := &config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}
		mgr := session.New(cfg, reg, secrets.Resolver{}, false)
		t.Cleanup(func() { _ = mgr.CloseAll() })
		model, err := New(Options{Manager: mgr, Theme: theme})
		if err != nil {
			t.Fatal(err)
		}
		h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
		h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
		return h
	}
	auto, forcedLight := build("auto"), build("tobira-light")

	// Init asks the terminal for its background colour; the answer comes back
	// as a message and restyles every pane.
	if auto.m.Init() == nil {
		t.Fatal("auto theme should ask the terminal for its background")
	}
	if forcedLight.m.Init() != nil {
		t.Fatal("a forced theme must not ask")
	}
	darkFrame := auto.raw()
	auto.send(tea.BackgroundColorMsg{Color: lightColor{}})
	if auto.m.th.Name != "tobira-light" {
		t.Fatalf("theme = %s after a light background", auto.m.th.Name)
	}
	if auto.raw() == darkFrame {
		t.Fatal("the frame did not change after the background arrived")
	}
	if auto.raw() != forcedLight.raw() {
		t.Fatalf("auto on a light background must draw exactly like tobira-light:\n%s\n---\n%s", auto.text(), forcedLight.text())
	}
	// And a dark background flips it back.
	auto.send(tea.BackgroundColorMsg{Color: darkColor{}})
	if auto.m.th.Name != "tobira-dark" {
		t.Fatalf("theme = %s after a dark background", auto.m.th.Name)
	}
}

type darkColor struct{}

func (darkColor) RGBA() (r, g, b, a uint32) { return 0, 0, 0, 0xffff }

func TestSidebarSyncsSessionsOnTheConnectingTick(t *testing.T) {
	h := newHarness(t)
	h.keys("enter") // open local
	h.until("main")
	if err := h.mgr.Close("local"); err != nil {
		t.Fatal(err)
	}
	h.keys("down") // a key does not look at the session
	if !strings.Contains(h.text(), "main") {
		t.Fatalf("the tree should be left alone until a status change is announced:\n%s", h.text())
	}
	h.send(tickMsg{})
	if strings.Contains(h.text(), "main") {
		t.Fatalf("the tick should collapse the closed connection:\n%s", h.text())
	}
}

func TestCopyNullCellCopiesNothing(t *testing.T) {
	h := newHarness(t)
	h.m.gr.SetColumns([]driver.Column{{Name: "a"}, {Name: "b"}})
	h.m.gr.SetRows(0, []driver.Row{{driver.Int(1), driver.Null()}})
	h.m.gr.SetTotal(1, true)
	h.m.focus = focusGrid
	h.keys("right", "y")
	h.drain(30 * time.Millisecond)
	if len(h.clip) != 1 || h.clip[0] != "" {
		t.Fatalf("y on NULL put %q on the clipboard, want one empty string", h.clip)
	}
	h.keys("Y")
	h.drain(30 * time.Millisecond)
	if last := h.clip[len(h.clip)-1]; last != `{"a":1,"b":null}` {
		t.Fatalf("Y copied %q, want the JSON null kept", last)
	}
}

func TestHugeDetailLoadsInTheBackgroundAndClosingCancelsIt(t *testing.T) {
	h := newHarness(t)
	var sb strings.Builder
	sb.WriteByte('[')
	for sb.Len() < 300*1024 {
		sb.WriteString(`"xxxxxxxx",`)
	}
	sb.WriteString(`"end"]`)
	h.m.gr.SetColumns([]driver.Column{{Name: "id"}, {Name: "doc"}})
	h.m.gr.SetRows(0, []driver.Row{{driver.Int(1), driver.JSON([]byte(sb.String()))}})
	h.m.gr.SetTotal(1, true)
	h.m.focus = focusGrid
	h.keys("enter")
	h.until("items]") // the tree arrived through a BuiltMsg
	if strings.Contains(h.text(), "loading…") {
		t.Fatalf("the loading line should be gone:\n%s", h.text())
	}
	h.keys("esc")
	if h.m.det != nil {
		t.Fatal("esc should close the detail")
	}
	// A result that arrives for a closed detail is dropped without a panic.
	h.send(detail.BuiltMsg{})
}

func TestNoticeIsFlashedAtStartup(t *testing.T) {
	h := newHarness(t)
	model, err := New(Options{Manager: h.mgr, Theme: "tobira-dark", Notice: "config /x/c.yaml: bad \x1b[31mred; continuing without it"})
	if err != nil {
		t.Fatal(err)
	}
	h2 := &harness{t: t, m: model.(*Model), fake: h.fake, mgr: h.mgr, ch: make(chan tea.Msg, 4096)}
	h2.run(h2.m.Init())
	h2.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	h2.until("continuing without it")
	tuitest.NoInjectedControls(t, h2.raw())
}

func TestAppKeysClearAPendingG(t *testing.T) {
	h := newHarness(t)
	h.m.setKeymap("vim")
	h.m.gr.SetColumns([]driver.Column{{Name: "a"}})
	rows := make([]driver.Row, 10)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i))}
	}
	h.m.gr.SetRows(0, rows)
	h.m.gr.SetTotal(10, true)
	h.m.focus = focusGrid
	h.keys("j", "j", "j")
	// g, then a key the app handles itself, then g: not a gg.
	h.keys("g", "y", "g")
	if r, _ := h.m.gr.Cursor(); r != 3 {
		t.Fatalf("cursor = %d: a g separated by y must not complete gg", r)
	}
	h.keys("g", "g")
	if r, _ := h.m.gr.Cursor(); r != 0 {
		t.Fatalf("cursor = %d, a real gg goes to the top", r)
	}
}
