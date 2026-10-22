package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/palette"
	"github.com/xavidop/tobira/internal/tui/sidebar"
)

// peopleRef is the collection openPeople opens.
var peopleRef = driver.CollectionRef{Namespace: "main", Name: "tobira_people"}

// watchAt is the fixed clock of the live tail tests' events.
var watchAt = time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)

// startWatch opens the people collection and its live tail with W, and
// waits until the watch is established.
func startWatch(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.m.loc = time.UTC
	h.openPeople()
	h.keys("W")
	h.pump(func() bool { return h.fake.Watchers() == 1 && h.watchTab() != nil && h.watchTab().watch.live })
	return h
}

// watchTab is the active tab when it is a live tail.
func (h *harness) watchTab() *resultTab {
	if t := h.m.activeTab(); t != nil && t.watch != nil {
		return t
	}
	return nil
}

// emit sends n events on the people collection and waits until the tab
// has counted them.
func (h *harness) emit(evs ...driver.Event) {
	h.t.Helper()
	w := h.watchTab().watch
	want := w.total + len(evs)
	for _, ev := range evs {
		h.fake.Emit(peopleRef, ev)
	}
	h.pump(func() bool { return w.total >= want })
}

func personEvent(i int, op string) driver.Event {
	return driver.Event{
		Time: watchAt.Add(time.Duration(i) * time.Second), Op: op, Channel: "tobira_people",
		Key:     driver.Object{{Key: "id", Value: driver.Int(int64(i))}},
		Payload: driver.ObjectValue(driver.Member{Key: "name", Value: driver.String(fmt.Sprintf("person %d", i))}),
	}
}

func TestWatchTailsEventsNewestFirst(t *testing.T) {
	h := startWatch(t)
	h.until("waiting for events")
	h.emit(personEvent(1, "insert"), personEvent(2, "update"), personEvent(3, "delete"))
	h.until("person 3")
	txt := h.text()
	i1, i3 := strings.Index(txt, "person 1"), strings.Index(txt, "person 3")
	if i1 < 0 || i3 < 0 || i3 > i1 {
		t.Fatalf("events are not newest first:\n%s", txt)
	}
	for _, want := range []string{"09:30:03.000", "delete", "insert", "watch tobira_people", "live", "3 events"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("live tail lacks %q:\n%s", want, txt)
		}
	}
	checkFrame(t, h, winW, winH)
	golden(t, "watch_live", txt)
}

func TestWatchEscapesThePreview(t *testing.T) {
	h := startWatch(t)
	ev := personEvent(1, "insert\x1b[31m")
	ev.Payload = driver.String("line1\nline2\x1b]0;pwned\x07")
	h.emit(ev)
	h.until("line1")
	checkFrame(t, h, winW, winH)
	if strings.Contains(h.text(), "line2\n") {
		t.Fatalf("a newline in the payload broke the line:\n%s", h.text())
	}
}

func TestWatchPauseHoldsNewEvents(t *testing.T) {
	h := startWatch(t)
	h.emit(personEvent(1, "insert"))
	h.until("person 1")
	h.keys("p")
	h.until("paused")
	h.emit(personEvent(2, "insert"))
	h.until("1 new")
	if strings.Contains(h.text(), "person 2") {
		t.Fatalf("a paused tail shows a new event:\n%s", h.text())
	}
	h.keys("p")
	h.until("person 2")
	if strings.Contains(h.text(), "paused") {
		t.Fatalf("resume left the tail paused:\n%s", h.text())
	}
}

func TestWatchClearDropsTheEvents(t *testing.T) {
	h := startWatch(t)
	h.emit(personEvent(1, "insert"), personEvent(2, "insert"))
	h.until("person 2")
	h.keys("c")
	h.until("waiting for events")
	if strings.Contains(h.text(), "person") || h.watchTab().watch.ring.len() != 0 {
		t.Fatalf("clear left events:\n%s", h.text())
	}
	h.emit(personEvent(3, "insert")) // still live after a clear
	h.until("person 3")
}

func TestWatchEnterShowsTheEventDetail(t *testing.T) {
	h := startWatch(t)
	h.emit(personEvent(1, "insert"), personEvent(2, "update"))
	h.until("person 2")
	h.keys("down", "enter") // the second row: the older event
	h.pump(func() bool { return h.m.det != nil })
	h.keys("]") // expand every node
	h.until("person 1")
	txt := h.text()
	for _, want := range []string{"row detail", "payload", "person 1", "insert"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("event detail lacks %q:\n%s", want, txt)
		}
	}
	golden(t, "watch_detail", txt)
	h.keys("esc")
	if h.m.det != nil || h.watchTab() == nil || !h.watchTab().watch.live {
		t.Fatal("esc on the detail did not go back to the live tail")
	}
}

func TestWatchEscClosesAndCancels(t *testing.T) {
	ign := goleak.IgnoreCurrent()
	h := startWatch(t)
	h.emit(personEvent(1, "insert"))
	h.keys("esc")
	if h.watchTab() != nil || len(h.m.tabs) != 0 {
		t.Fatalf("esc did not close the live tail tab:\n%s", h.text())
	}
	h.pump(func() bool { return h.fake.Watchers() == 0 })
	h.drain(50 * time.Millisecond)
	goleak.VerifyNone(t, ign, goleak.IgnoreTopFunction("time.Sleep"),
		goleak.IgnoreAnyFunction("charm.land/bubbletea/v2.Tick.func1"))
}

func TestWatchUnwatchAndCloseTabCancel(t *testing.T) {
	h := startWatch(t)
	h.command("unwatch")
	h.pump(func() bool { return h.fake.Watchers() == 0 })
	if len(h.m.tabs) != 0 {
		t.Fatal(":unwatch left the tab open")
	}
	h.command("unwatch")
	h.until("no live tail is open")

	h.keys("W")
	h.pump(func() bool { return h.fake.Watchers() == 1 })
	h.keys("x") // close-tab
	h.pump(func() bool { return h.fake.Watchers() == 0 })
}

func TestWatchOneWatchPerTab(t *testing.T) {
	h := startWatch(t)
	h.command("watch orders") // on the live tail tab: replaces its watch
	h.pump(func() bool { return h.watchTab().watch.live && h.watchTab().watch.req.Channel == "orders" })
	h.pump(func() bool { return h.fake.Watchers() == 1 }) // the old watch's ctx is cancelled; it unsubscribes
	if len(h.m.tabs) != 1 || h.fake.Watchers() != 1 {
		t.Fatalf("%d tabs, %d watches; want the tab's watch replaced", len(h.m.tabs), h.fake.Watchers())
	}
	h.fake.Emit(driver.CollectionRef{Name: "orders"}, driver.Event{Time: watchAt, Op: "notify", Channel: "orders", Payload: driver.String("paid 42")})
	h.until("paid 42")

	h.keys("[") // back to the browse tab: W there opens a second tab
	h.keys("W")
	h.pump(func() bool { return len(h.m.tabs) == 2 && h.fake.Watchers() == 2 })
	h.m.shutdown() // quitting cancels every watch
	h.pump(func() bool { return h.fake.Watchers() == 0 })
}

func TestWatchNeedsAWatcher(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{without: []driver.Capability{driver.CapWatcher}})
	h.openKind("tobira_people")
	h.keys("W")
	h.until("live tail needs a driver with watch support")
	if len(h.m.tabs) != 0 {
		t.Fatal("a tab opened without a Watcher")
	}
}

func TestWatchNeedsACollectionOrATarget(t *testing.T) {
	h := newHarness(t)
	h.send(palette.WatchCmd{})
	h.until("open a collection to watch it")
}

func TestWatchRingKeepsTheLast1000(t *testing.T) {
	h := startWatch(t)
	n := 0
	for n < 1200 {
		batch := make([]driver.Event, 0, 100)
		for range 100 {
			batch = append(batch, personEvent(n, "insert"))
			n++
		}
		h.emit(batch...)
	}
	w := h.watchTab().watch
	if w.ring.len() != watchKeep || w.total != 1200 {
		t.Fatalf("ring holds %d of %d events, want %d", w.ring.len(), w.total, watchKeep)
	}
	if got := w.ring.newest(0).Key[0].Value.String(); got != "1199" {
		t.Fatalf("newest event id = %s, want 1199", got)
	}
	if got := w.ring.newest(watchKeep - 1).Key[0].Value.String(); got != "200" {
		t.Fatalf("oldest kept event id = %s, want 200", got)
	}
	h.until("1,200 events")
	h.until("last 1,000 kept")
}

func TestEventRing(t *testing.T) {
	var r eventRing
	for i := range 2500 {
		r.push(tailEvent{Event: driver.Event{Op: fmt.Sprint(i)}})
		if r.len() != min(i+1, watchKeep) || r.newest(0).Op != fmt.Sprint(i) {
			t.Fatalf("after %d pushes: len %d, newest %q", i+1, r.len(), r.newest(0).Op)
		}
	}
	if r.newest(watchKeep-1).Op != "1500" {
		t.Fatalf("oldest = %q", r.newest(watchKeep-1).Op)
	}
	r.clear()
	if r.len() != 0 {
		t.Fatal("clear kept events")
	}
}

func TestWatchShowsDriverErrorsRedacted(t *testing.T) {
	h := startWatch(t)
	h.emit(personEvent(1, "insert"))
	h.fake.Emit(peopleRef, driver.Event{Time: watchAt, Err: errors.New("stream broke: password=hunter2xyz")})
	h.until("stream broke")
	if strings.Contains(h.text(), "hunter2xyz") {
		t.Fatalf("the error was not redacted:\n%s", h.text())
	}
	h.pump(func() bool { return h.fake.Watchers() == 0 }) // a terminal error ends the watch
	if w := h.watchTab().watch; w.live || !strings.Contains(h.text(), "person 1") {
		t.Fatalf("after the error: live %v\n%s", w.live, h.text())
	}
}

func TestWatchOpenFailureShowsInThePane(t *testing.T) {
	h := newHarness(t)
	h.openPeople()
	h.fake.FailNext("watch", driver.Wrap(driver.ErrUnsupported, "fake", "watch", errors.New("no change streams here")))
	h.keys("W")
	h.until("no change streams here")
	if w := h.watchTab().watch; w.live {
		t.Fatal("a failed watch is live")
	}
}

// openRO opens the read-only connection's people collection in the grid.
func (h *harness) openRO() {
	h.t.Helper()
	if _, err := h.mgr.Open(context.Background(), "ro"); err != nil {
		h.t.Fatal(err)
	}
	h.send(sidebar.OpenCollectionMsg{Conn: "ro", Ref: peopleRef})
	h.pump(func() bool { return h.m.cur.conn == "ro" && !h.m.gr.Loading() })
}

func TestWatchWorksOnReadOnlyConnections(t *testing.T) {
	h := newHarness(t)
	h.openRO()
	if !h.mgr.State("ro").ReadOnly {
		t.Fatal("ro is not read-only")
	}
	h.keys("W")
	h.pump(func() bool { return h.fake.Watchers() == 1 && h.watchTab() != nil && h.watchTab().watch.live })
}

func TestWatchStopsWhenTheConnectionSwitches(t *testing.T) {
	h := startWatch(t)
	h.keys("[") // the browse tab
	h.openRO()
	h.pump(func() bool { return h.fake.Watchers() == 0 })
	if w := h.m.tabs[0].watch; w.live || !strings.Contains(w.note, "switched") {
		t.Fatalf("watch after a switch: live %v note %q", w.live, w.note)
	}
}

func TestWatchTabEvictionCancels(t *testing.T) {
	h := startWatch(t)
	for range maxTabs {
		h.m.addTab("local", "select 1", false)
	}
	h.pump(func() bool { return h.fake.Watchers() == 0 })
}

// TestRunQuitStopsTheWatch runs the real program, opens a live tail and
// quits: Run returns promptly, the watch is cancelled and no goroutine is
// left behind.
func TestRunQuitStopsTheWatch(t *testing.T) {
	ign := goleak.IgnoreCurrent()
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), Options{Manager: mgr, Theme: "tobira-dark"}, pr, io.Discard) }()
	send := func(s string) { _, _ = pw.Write([]byte(s)); time.Sleep(300 * time.Millisecond) }
	send("\r")       // expand local
	send("\x1b[B\r") // expand main
	send("\x1b[B\r") // open the collection
	send("W")
	deadline := time.Now().Add(3 * time.Second)
	for f.Watchers() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.Watchers() != 1 {
		t.Fatal("the live tail did not start")
	}
	f.Emit(peopleRef, personEvent(1, "insert"))
	send("q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	_ = pw.Close()
	if n := f.Watchers(); n != 0 {
		t.Fatalf("%d watches outlived the program", n)
	}
	goleak.VerifyNone(t, ign)
}

// startSecretWatch is startWatch on a connection whose password resolves
// to secret.
func startSecretWatch(t *testing.T, secret string) *harness {
	t.Helper()
	t.Setenv("WATCH_TUI_PW", secret)
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "local", Driver: "fake", DSN: "fake://local", Password: "env:WATCH_TUI_PW"},
	}}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.m.loc = time.UTC
	h.run(h.m.Init())
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	h.openPeople()
	h.keys("W")
	h.pump(func() bool { return h.fake.Watchers() == 1 && h.watchTab() != nil && h.watchTab().watch.live })
	return h
}

func TestWatchRedactsEventValues(t *testing.T) {
	const secret = "Tail-S3cret-Pw-42"
	h := startSecretWatch(t, secret)
	ev := personEvent(1, "update")
	ev.Key = driver.Object{{Key: "id", Value: driver.String("key-" + secret)}}
	ev.Payload = driver.ObjectValue(
		driver.Member{Key: "note", Value: driver.String("pw is " + secret)},
		driver.Member{Key: "nested", Value: driver.List(driver.String(secret))},
	)
	h.emit(ev)
	h.until("pw is")
	if strings.Contains(h.text(), secret) {
		t.Fatalf("the list shows the password:\n%s", h.text())
	}
	h.keys("enter")
	h.pump(func() bool { return h.m.det != nil })
	h.keys("]")
	h.until("pw is")
	if txt := h.text(); strings.Contains(txt, secret) {
		t.Fatalf("the detail shows the password:\n%s", txt)
	}
	if got := fmt.Sprint(h.watchTab().watch.ring.newest(0).Payload); strings.Contains(got, secret) {
		t.Fatalf("the ring stores the password: %s", got)
	}
}

func TestWatchCapsLargePayloads(t *testing.T) {
	h := startWatch(t)
	big := strings.Repeat("x", 100<<10)
	str := personEvent(1, "insert")
	str.Payload = driver.String(big)
	obj := personEvent(2, "insert")
	obj.Payload = driver.ObjectValue(driver.Member{Key: "blob", Value: driver.String(big)})
	small := personEvent(3, "insert")
	h.emit(str, obj, small)
	w := h.watchTab().watch
	for i, want := range []bool{false, true, true} { // newest first
		te := w.ring.newest(i)
		if te.truncated != want {
			t.Fatalf("event %d truncated = %v, want %v", i, te.truncated, want)
		}
		if !want {
			continue
		}
		s, _ := te.Payload.AsText()
		if len(s) > maxEventPayload+len(truncatedMark) || !strings.HasSuffix(s, truncatedMark) {
			t.Fatalf("event %d payload is %d bytes, suffix %q", i, len(s), s[max(len(s)-20, 0):])
		}
	}
	h.keys("down", "enter") // the capped object
	h.pump(func() bool { return h.m.det != nil })
	h.keys("]")
	txt := h.text()
	if !strings.Contains(txt, "_truncated") || !strings.Contains(txt, "true") {
		t.Fatalf("the detail does not say the payload was cut:\n%s", txt)
	}
}
