package connform

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/tui/theme"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

var drivers = []driver.Info{
	{Name: "postgres", Kind: driver.KindSQL, Schemes: []string{"postgres", "postgresql"}},
	{Name: "redis", Kind: driver.KindKeyValue, Schemes: []string{"redis"}},
	{Name: "sqlite", Kind: driver.KindSQL, Schemes: []string{"sqlite", "file"}},
}

// harness drives a Model the way Bubble Tea would: it feeds commands'
// messages back into Update. Commands that do not finish quickly (cursor
// blink timers) are abandoned, which keeps tests fast.
type harness struct {
	t   *testing.T
	m   Model
	out []tea.Msg // SavedMsg and CancelledMsg emitted by the form
}

func newHarness(t *testing.T, m Model) *harness {
	t.Helper()
	h := &harness{t: t, m: m.Resize(80, 30)}
	h.pump(m.Init())
	return h
}

func (h *harness) pump(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(15 * time.Millisecond):
		return
	}
	if msg == nil {
		return
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
		for i := range v.Len() {
			h.pump(v.Index(i).Interface().(tea.Cmd))
		}
		return
	}
	h.send(msg)
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	switch msg.(type) {
	case SavedMsg, CancelledMsg:
		h.out = append(h.out, msg)
		return
	}
	var cmd tea.Cmd
	h.m, cmd = h.m.Update(msg)
	h.pump(cmd)
}

func (h *harness) typ(s string) {
	for _, r := range s {
		h.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func (h *harness) key(code rune, mod tea.KeyMod) {
	h.send(tea.KeyPressMsg{Code: code, Mod: mod})
}

func (h *harness) enter() { h.key(tea.KeyEnter, 0) }

func (h *harness) view() string { return ansi.Strip(h.m.View()) }

// fill completes the form for conn up to (not including) the final enter.
func (h *harness) fillAll(name, dsn, ref, tags string) {
	h.typ(name)
	h.enter() // name -> driver (default postgres)
	h.enter() // driver -> dsn
	h.typ(dsn)
	h.enter() // dsn -> password
	h.typ(ref)
	h.enter() // password -> read only
	h.enter() // read only -> color
	h.enter() // color -> tags
	h.typ(tags)
}

func okTester(context.Context, config.Connection) error { return nil }

func TestFormSubmitAdd(t *testing.T) {
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester))
	h.fillAll("web", "postgres://u@h/db", "env:PW", "a, b")
	h.enter()
	if len(h.out) != 1 {
		t.Fatalf("messages = %v\n%s", h.out, h.view())
	}
	got, ok := h.out[0].(SavedMsg)
	if !ok {
		t.Fatalf("got %T", h.out[0])
	}
	want := config.Connection{Name: "web", Driver: "postgres", DSN: "postgres://u@h/db", Password: "env:PW", Tags: []string{"a", "b"}}
	if !reflect.DeepEqual(got.Conn, want) || got.OldName != "" {
		t.Fatalf("saved = %+v, want %+v", got, want)
	}
}

func TestFormEditPrefillsAndKeepsOptions(t *testing.T) {
	ex := config.Connection{Name: "cache", Driver: "redis", DSN: "redis://h:6379/0", Color: "blue", ReadOnly: true,
		Tags: []string{"x"}, Options: map[string]string{"k": "v"}}
	h := newHarness(t, New(Edit, &ex, drivers, theme.Dark(), okTester))
	v := h.view()
	for _, s := range []string{"cache", "redis://h:6379/0", "x"} {
		if !strings.Contains(v, s) {
			t.Errorf("view lacks %q:\n%s", s, v)
		}
	}
	h.key('s', tea.ModCtrl) // save now
	if len(h.out) != 1 {
		t.Fatalf("messages = %v\n%s", h.out, h.view())
	}
	got := h.out[0].(SavedMsg)
	if !reflect.DeepEqual(got.Conn, ex) || got.OldName != "cache" {
		t.Fatalf("saved = %+v, want %+v", got, ex)
	}
}

func TestFormValidation(t *testing.T) {
	tests := []struct {
		name    string
		steps   func(h *harness)
		wantErr string
	}{
		{"bad name", func(h *harness) { h.typ("bad name!"); h.enter() }, "name"},
		{"empty name", func(h *harness) { h.enter() }, "name"},
		{"dsn scheme vs driver", func(h *harness) {
			h.typ("a")
			h.enter()
			h.enter() // driver postgres
			h.typ("redis://h")
			h.enter()
		}, "does not match"},
		{"dsn with password", func(h *harness) {
			h.typ("a")
			h.enter()
			h.enter()
			h.typ("postgres://u:pw@h/db")
			h.enter()
		}, "DSN contains a password"},
		{"dsn empty", func(h *harness) {
			h.typ("a")
			h.enter()
			h.enter()
			h.enter()
		}, "DSN is required"},
		{"bad ref", func(h *harness) {
			h.typ("a")
			h.enter()
			h.enter()
			h.typ("postgres://h/db")
			h.enter()
			h.typ("op//x")
			h.enter()
		}, "not a secret reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester))
			tt.steps(h)
			if v := h.view(); !strings.Contains(v, tt.wantErr) {
				t.Fatalf("view lacks %q:\n%s", tt.wantErr, v)
			}
			if len(h.out) != 0 {
				t.Fatalf("form submitted despite error: %v", h.out)
			}
		})
	}
}

func TestFormSaveNowValidatesEverything(t *testing.T) {
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester))
	h.typ("a")
	h.key('s', tea.ModCtrl)
	if len(h.out) != 0 {
		t.Fatal("saved an incomplete form")
	}
	if v := h.view(); !strings.Contains(v, "DSN is required") {
		t.Fatalf("no inline error:\n%s", v)
	}
	// The error clears the form for editing; values typed so far remain.
	if !strings.Contains(h.view(), "a") {
		t.Fatal("values lost")
	}
}

func TestFormTakenName(t *testing.T) {
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester).WithTaken("web", "db"))
	h.typ("web")
	h.enter()
	if v := h.view(); !strings.Contains(v, "already exists") {
		t.Fatalf("no duplicate error:\n%s", v)
	}
	// Editing may keep its own name.
	ex := config.Connection{Name: "web", Driver: "postgres", DSN: "postgres://h/db"}
	h = newHarness(t, New(Edit, &ex, drivers, theme.Dark(), okTester).WithTaken("web", "db"))
	h.key('s', tea.ModCtrl)
	if len(h.out) != 1 {
		t.Fatalf("edit with own name refused:\n%s", h.view())
	}
}

func TestFormTestConnection(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	var mu sync.Mutex
	var tested []config.Connection
	tester := func(ctx context.Context, c config.Connection) error {
		mu.Lock()
		tested = append(tested, c)
		mu.Unlock()
		conn, err := f.Driver().Open(ctx, driver.ConnConfig{DSN: c.DSN})
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return conn.Ping(ctx)
	}
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), tester))
	h.typ("web")
	h.enter()
	h.enter()
	h.typ("postgres://u@h/db")
	h.key('t', tea.ModCtrl)
	waitFor(t, h, "ok (")
	if len(h.out) != 0 {
		t.Fatalf("test saved or cancelled the form: %v", h.out)
	}
	if len(tested) != 1 || tested[0].DSN != "postgres://u@h/db" || tested[0].Driver != "postgres" {
		t.Fatalf("tested = %+v", tested)
	}

	// A failure shows the error, with control characters escaped.
	f.FailNext("open", errors.New("connection refused \x1b[31mred"))
	h.key('t', tea.ModCtrl)
	waitFor(t, h, "connection refused")
	raw := h.m.View()
	tuitest.NoInjectedControls(t, raw)
	if !strings.Contains(h.view(), "failed") {
		t.Fatalf("no failure marker:\n%s", h.view())
	}
	if len(h.out) != 0 {
		t.Fatal("form emitted a message after a test")
	}
}

func TestFormTestRedactsAndNeedsValidInput(t *testing.T) {
	tester := func(context.Context, config.Connection) error {
		return errors.New("dial postgres://u:s3cr3t@h/db failed")
	}
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), tester))
	h.typ("web")
	h.enter()
	h.enter()
	h.typ("postgres://u@h/db")
	h.key('t', tea.ModCtrl)
	waitFor(t, h, "failed")
	if strings.Contains(h.view(), "s3cr3t") {
		t.Fatalf("test result leaks a password:\n%s", h.view())
	}
	// An invalid DSN is reported without calling the tester.
	called := false
	h = newHarness(t, New(Add, nil, drivers, theme.Dark(), func(context.Context, config.Connection) error { called = true; return nil }))
	h.key('t', tea.ModCtrl)
	if called || !strings.Contains(h.view(), "DSN is required") {
		t.Fatalf("called=%v view:\n%s", called, h.view())
	}
}

func TestFormCancelTest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	tester := func(ctx context.Context, c config.Connection) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), tester))
	h.typ("web")
	h.enter()
	h.enter()
	h.typ("postgres://u@h/db")
	var cmd tea.Cmd
	h.m, cmd = h.m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	resc := make(chan tea.Msg, 1)
	go func() { resc <- cmd() }()
	<-started
	if !h.m.Testing() {
		t.Fatal("Testing() = false while a test runs")
	}
	if !h.m.CancelTest() {
		t.Fatal("CancelTest() = false")
	}
	<-cancelled
	h.send(<-resc)
	if h.m.Testing() {
		t.Fatal("still testing")
	}
	if !strings.Contains(h.view(), "cancelled") {
		t.Fatalf("no cancelled marker:\n%s", h.view())
	}
	if h.m.CancelTest() {
		t.Fatal("CancelTest() = true with nothing running")
	}
}

func TestFormEscCancelsAndStopsTest(t *testing.T) {
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester))
	h.key(tea.KeyEscape, 0)
	if len(h.out) != 1 {
		t.Fatalf("out = %v", h.out)
	}
	if _, ok := h.out[0].(CancelledMsg); !ok {
		t.Fatalf("got %T", h.out[0])
	}
}

func TestFormStaleTestResultDropped(t *testing.T) {
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester))
	h.send(testDoneMsg{id: 999})
	if strings.Contains(h.view(), "ok (") {
		t.Fatal("stale result shown")
	}
}

func TestFormSetErrorKeepsValues(t *testing.T) {
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester))
	h.fillAll("web", "postgres://u@h/db", "", "")
	h.m = h.m.SetError("connection name already exists: \"web\"\x1b[2J")
	h.pump(h.m.Init())
	v := h.view()
	if !strings.Contains(v, "already exists") || !strings.Contains(v, "postgres://u@h/db") {
		t.Fatalf("view:\n%s", v)
	}
	tuitest.NoInjectedControls(t, h.m.View())
	// The rebuilt form still submits the values.
	h.key('s', tea.ModCtrl)
	if len(h.out) != 1 {
		t.Fatalf("resubmit failed:\n%s", h.view())
	}
}

func TestFormNoDriversDoesNotPanic(t *testing.T) {
	h := newHarness(t, New(Add, nil, nil, theme.Dark(), nil))
	_ = h.view()
	h.typ("a")
	h.key('s', tea.ModCtrl)
	h.key('t', tea.ModCtrl)
	if len(h.out) != 0 {
		t.Fatal("saved without a driver")
	}
	h.m = h.m.Resize(5, 3)
	_ = h.m.View()
}

// waitFor pumps the pending test result into the form until the view shows
// want (the tester runs in a command; the harness only runs commands that
// finish quickly, so poll the command output the model produced).
func waitFor(t *testing.T, h *harness, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.view(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("view never showed %q:\n%s", want, h.view())
}

func runForm(t *testing.T, keys ...string) (config.Connection, error) {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		c   config.Connection
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := Run(ctx, pr, io.Discard, New(Add, nil, drivers, theme.Dark(), okTester))
		done <- result{c, err}
	}()
	for _, k := range keys {
		time.Sleep(150 * time.Millisecond)
		_, _ = pw.Write([]byte(k))
	}
	select {
	case r := <-done:
		_ = pw.Close()
		return r.c, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	return config.Connection{}, nil
}

func TestRunStandalone(t *testing.T) {
	c, err := runForm(t, "web", "\r", "\r", "postgres://h/db", "\x13") // name, driver, dsn, ctrl+s
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "web" || c.Driver != "postgres" || c.DSN != "postgres://h/db" {
		t.Fatalf("connection = %+v", c)
	}
	if _, err := runForm(t, "web", "\x03"); !errors.Is(err, ErrCancelled) { // ctrl+c
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
}

func TestFormKeepsUnlistedColor(t *testing.T) {
	ex := config.Connection{Name: "a", Driver: "redis", DSN: "redis://h", Color: "teal"}
	h := newHarness(t, New(Edit, &ex, drivers, theme.Dark(), okTester))
	if !strings.Contains(h.view(), "teal") {
		t.Fatalf("unlisted colour not offered:\n%s", h.view())
	}
	h.key('s', tea.ModCtrl)
	if len(h.out) != 1 || h.out[0].(SavedMsg).Conn.Color != "teal" {
		t.Fatalf("colour reset or refused: %v\n%s", h.out, h.view())
	}
	// A new connection may not invent one.
	if err := config.ValidateConnection(config.Connection{Name: "a", Driver: "redis", DSN: "redis://h", Color: "teal"}, drivers); err == nil {
		t.Fatal("unknown colour accepted by validation")
	}
}

func TestFormTestProbeHasName(t *testing.T) {
	var got string
	tester := func(_ context.Context, c config.Connection) error { got = c.Name; return nil }
	h := newHarness(t, New(Add, nil, drivers, theme.Dark(), tester))
	h.enter() // name is empty: invalid, but advance is refused; type DSN via ctrl+t path instead
	h.m.v.driver, h.m.v.dsn = "postgres", "postgres://h/db"
	h.key('t', tea.ModCtrl)
	waitFor(t, h, "ok (")
	if got != "test" {
		t.Fatalf("probe name = %q, want the placeholder", got)
	}
}

func TestFormKeepsUnchangedURLFormRef(t *testing.T) {
	ex := config.Connection{Name: "a", Driver: "postgres", DSN: "postgres://h/db", Password: "op://vault/item/field"}
	h := newHarness(t, New(Edit, &ex, drivers, theme.Dark(), okTester))
	h.key('s', tea.ModCtrl)
	if len(h.out) != 1 || h.out[0].(SavedMsg).Conn.Password != "op://vault/item/field" {
		t.Fatalf("unchanged hand-written ref refused: %v\n%s", h.out, h.view())
	}
	// A changed value goes through the strict check.
	h = newHarness(t, New(Edit, &ex, drivers, theme.Dark(), okTester))
	h.m.v.password = "aws-sm://other"
	h.key('s', tea.ModCtrl)
	if len(h.out) != 0 || !strings.Contains(h.view(), "not a secret reference") {
		t.Fatalf("changed unregistered ref accepted: %v\n%s", h.out, h.view())
	}
	// Add mode never exempts anything.
	h = newHarness(t, New(Add, nil, drivers, theme.Dark(), okTester))
	h.m.v.name, h.m.v.driver, h.m.v.dsn, h.m.v.password = "n", "postgres", "postgres://h/db", "op://vault/item"
	h.key('s', tea.ModCtrl)
	if len(h.out) != 0 {
		t.Fatalf("add accepted an unregistered ref: %v", h.out)
	}
}

func TestFormNeverShowsDSNPassword(t *testing.T) {
	ex := config.Connection{Name: "a", Driver: "postgres", DSN: "postgres://u:hunter2@h/db"}
	h := newHarness(t, New(Edit, &ex, drivers, theme.Dark(), okTester))
	v := h.view()
	if strings.Contains(v, "hunter2") {
		t.Fatalf("form shows the DSN password:\n%s", v)
	}
	for _, want := range []string{"postgres://u:xxxxx@h/db", "the DSN contained a password; move it into a password ref"} {
		if !strings.Contains(v, want) {
			t.Fatalf("form lacks %q:\n%s", want, v)
		}
	}
	// Saving with the placeholder still in place is refused with the hint.
	h.key('s', tea.ModCtrl)
	if len(h.out) != 0 || !strings.Contains(h.view(), "move it into a password ref") {
		t.Fatalf("placeholder DSN saved or not explained: %v\n%s", h.out, h.view())
	}
	if strings.Contains(h.view(), "hunter2") {
		t.Fatalf("error shows the password:\n%s", h.view())
	}
	// Test connection is refused the same way.
	h.key('t', tea.ModCtrl)
	if !strings.Contains(h.view(), "move it into a password ref") {
		t.Fatalf("test did not explain:\n%s", h.view())
	}
	// With the placeholder gone it saves.
	h.m.v.dsn = "postgres://u@h/db"
	h.m.v.password = "env:PW"
	h.key('s', tea.ModCtrl)
	if len(h.out) != 1 || h.out[0].(SavedMsg).Conn.DSN != "postgres://u@h/db" {
		t.Fatalf("clean DSN not saved: %v\n%s", h.out, h.view())
	}
}
