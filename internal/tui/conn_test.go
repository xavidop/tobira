package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/secrets/keyring"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

const connSeed = `# my connections
connections:
  - name: local
    driver: fake
    dsn: fake://local
    color: green
  - name: ro
    driver: fake
    dsn: fake://ro
    read_only: true
`

// newConnHarness is a harness whose manager and config file agree, with the
// file path handed to the app.
func newConnHarness(t *testing.T) (*harness, string) { return newConnHarnessWith(t, connSeed) }

func newConnHarnessWith(t *testing.T, seed string) (*harness, string) {
	return newConnHarnessKeyring(t, seed, nil)
}

// registerKeyring registers the keyring: scheme once per test binary, as
// cmd/tobira does at start, so keyring:<name> refs load.
var registerKeyring = sync.OnceFunc(func() {
	mamori.Register(keyring.NewProvider(&memStore{m: map[string]string{}}))
})

func newConnHarnessKeyring(t *testing.T, seed string, store keyring.Store) (*harness, string) {
	t.Helper()
	registerKeyring()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, stat, err := config.LoadWithStat(path)
	if err != nil {
		t.Fatal(err)
	}
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark", ConfigPath: path, ConfigStat: stat, Keyring: store})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.run(h.m.Init())
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	return h, path
}

// settle feeds command results to the model until none arrive for a moment.
// Form navigation is message driven, so typing must wait for it.
func (h *harness) settle() {
	h.t.Helper()
	for {
		select {
		case msg := <-h.ch:
			h.send(msg)
		case <-time.After(10 * time.Millisecond):
			return
		}
	}
}

// step sends keys one at a time, letting form navigation settle after each
// non-text key.
func (h *harness) step(ks ...string) {
	h.t.Helper()
	for _, k := range ks {
		switch k {
		case "ctrl+s":
			h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		case "ctrl+t":
			h.send(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
		default:
			h.send(key(k))
		}
		h.settle()
	}
}

func fileText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *harness) names() []string { return h.mgr.Names() }

// fillNew types the add form for a connection called name and leaves it on
// the last field.
func (h *harness) fillNew(name string) {
	h.t.Helper()
	h.typ(name)
	h.step("enter", "enter") // name, driver (the only one)
	h.typ("fake://" + name)
	h.step("enter", "enter", "enter", "enter") // dsn, ref, read-only, color
	h.typ("t1, t2")
}

func TestAppAddFlow(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("n")
	if v := h.text(); !strings.Contains(v, "Name") || !strings.Contains(v, "DSN") {
		t.Fatalf("add form not shown:\n%s", v)
	}
	h.fillNew("fresh")
	h.step("enter")
	h.until("saved fresh")
	if v := h.text(); !strings.Contains(v, "fresh") || strings.Contains(v, "DSN") {
		t.Fatalf("sidebar should list fresh and the form close:\n%s", v)
	}
	got := fileText(t, path)
	if !strings.Contains(got, "# my connections") || !strings.Contains(got, "  - name: fresh\n    driver: fake\n    dsn: fake://fresh\n    tags: [t1, t2]\n") {
		t.Fatalf("config file:\n%s", got)
	}
	if got := strings.Join(h.names(), ","); got != "local,ro,fresh" {
		t.Fatalf("manager names = %s", got)
	}
	if conn, _ := h.m.sb.Selected(); conn != "fresh" {
		t.Fatalf("sidebar cursor on %q, want the new connection", conn)
	}
	// And it works: expand it.
	h.step("enter")
	h.until("main")
}

func TestAppAddCancelWritesNothing(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("n")
	h.typ("abc")
	h.step("esc")
	if strings.Contains(h.text(), "DSN") {
		t.Fatal("form still open after esc")
	}
	if fileText(t, path) != connSeed {
		t.Fatal("cancelled form wrote the file")
	}
}

func TestAppFormSuppressesGlobalKeys(t *testing.T) {
	h, _ := newConnHarness(t)
	h.step("n")
	h.typ("q:fEDn?")
	if h.quitted() {
		t.Fatal("q quit the app while the form was open")
	}
	if h.m.pal.Focused() || h.m.help || h.m.confirm != nil {
		t.Fatal("a global binding fired inside the form")
	}
	if v := h.text(); !strings.Contains(v, "q:fEDn?") {
		t.Fatalf("typed text did not reach the name field:\n%s", v)
	}
}

func TestAppEditOpenConnectionReopens(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("enter") // expand local: opens it
	h.until("main")
	if h.mgr.State("local").Status != session.Ready {
		t.Fatal("local not open")
	}
	opens := func() int {
		n := 0
		for _, c := range h.fake.Calls() {
			if c.Op == "open" {
				n++
			}
		}
		return n
	}
	before := opens()
	h.step("E")
	if v := h.text(); !strings.Contains(v, "fake://local") {
		t.Fatalf("edit form not prefilled:\n%s", v)
	}
	h.step("tab", "tab", "tab", "tab") // name, driver, dsn, password -> read-only
	h.step("y")
	h.step("ctrl+s")
	h.until("saved local")
	if st := h.mgr.State("local"); st.Status != session.Idle || !st.ReadOnly {
		t.Fatalf("local after edit = %+v, want closed and read-only", st)
	}
	if !strings.Contains(fileText(t, path), "    read_only: true\n  - name: ro") {
		t.Fatalf("config file:\n%s", fileText(t, path))
	}
	if strings.Contains(h.text(), "main") {
		t.Fatalf("edited connection should collapse:\n%s", h.text())
	}
	h.step("enter") // lazily reopens
	h.until("main")
	if opens() != before+1 {
		t.Fatalf("opens = %d, want %d", opens(), before+1)
	}
}

func TestAppEditRenames(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("E")
	h.typ("2")
	h.step("ctrl+s")
	h.until("saved local2")
	if got := strings.Join(h.names(), ","); got != "local2,ro" {
		t.Fatalf("names = %s", got)
	}
	if !strings.Contains(fileText(t, path), "name: local2") || strings.Contains(fileText(t, path), "name: local\n") {
		t.Fatalf("config file:\n%s", fileText(t, path))
	}
}

func TestAppDeleteRequiresTypedName(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("down", "D") // ro
	v := h.text()
	if !strings.Contains(v, "ro") || !strings.Contains(v, "Type") {
		t.Fatalf("confirmation not shown:\n%s", v)
	}
	h.step("enter") // empty
	if fileText(t, path) != connSeed {
		t.Fatal("deleted without confirmation")
	}
	h.typ("r")
	h.step("enter")
	if fileText(t, path) != connSeed || !strings.Contains(h.text(), "does not match") {
		t.Fatalf("partial name accepted:\n%s", h.text())
	}
	h.typ("o")
	h.step("enter")
	h.until("removed ro")
	if strings.Contains(fileText(t, path), "name: ro") || strings.Contains(h.text(), "Type") {
		t.Fatalf("not removed:\n%s\n%s", fileText(t, path), h.text())
	}
	if got := strings.Join(h.names(), ","); got != "local" {
		t.Fatalf("names = %s", got)
	}
}

func TestAppDeleteCancel(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("D")
	h.typ("local")
	h.step("esc")
	if strings.Contains(h.text(), "Type") || fileText(t, path) != connSeed {
		t.Fatal("esc did not cancel the deletion")
	}
	// Bare-letter bindings do not fire while confirming.
	h.step("D")
	h.typ("qn")
	if h.quitted() {
		t.Fatal("q quit during confirmation")
	}
}

func TestAppRemovingTheOpenConnectionClearsTheGrid(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("D")
	h.typ("local")
	h.step("enter")
	h.until("removed local")
	v := h.text()
	if strings.Contains(v, "Ada Lovelace") || !strings.Contains(v, "no collection open") {
		t.Fatalf("grid still shows a removed connection's data:\n%s", v)
	}
	if h.m.cur.set {
		t.Fatal("current collection not cleared")
	}
}

func TestAppPaletteConnectionCommands(t *testing.T) {
	h, _ := newConnHarness(t)
	h.keys(":")
	h.typ("connection add")
	h.step("enter")
	if !strings.Contains(h.text(), "DSN") {
		t.Fatalf(":connection add did not open the form:\n%s", h.text())
	}
	h.step("esc")
	h.keys(":")
	h.typ("connection edit ro")
	h.step("enter")
	if v := h.text(); !strings.Contains(v, "fake://ro") {
		t.Fatalf(":connection edit ro:\n%s", v)
	}
	h.step("esc")
	h.keys(":")
	h.typ("connection rm local")
	h.step("enter")
	if v := h.text(); !strings.Contains(v, "Type") {
		t.Fatalf(":connection rm local:\n%s", v)
	}
	h.step("esc")
	h.keys(":")
	h.typ("connection edit nope")
	h.step("enter")
	if v := h.text(); !strings.Contains(v, "nope") || strings.Contains(v, "DSN") {
		t.Fatalf("unknown connection should flash, not open a form:\n%s", v)
	}
}

func TestAppConflictShownInline(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("n")
	h.fillNew("fresh")
	if err := os.WriteFile(path, []byte(connSeed+"# edited elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.step("enter")
	h.until("changed on disk; reload with :reload")
	if !strings.Contains(h.text(), "DSN") {
		t.Fatalf("form should stay open on conflict:\n%s", h.text())
	}
	if strings.Contains(fileText(t, path), "fresh") {
		t.Fatal("conflicting write went through")
	}
	if len(h.names()) != 2 {
		t.Fatalf("manager changed on a failed save: %v", h.names())
	}
}

func TestAppFormTestConnectionNeverSaves(t *testing.T) {
	h, path := newConnHarness(t)
	h.step("n")
	h.fillNew("probe")
	h.step("ctrl+t")
	h.until("ok (")
	if fileText(t, path) != connSeed || len(h.names()) != 2 {
		t.Fatal("test connection saved something")
	}
	h.fake.FailNext("open", errBoom("dial fake://probe: refused"))
	h.step("ctrl+t")
	h.until("failed")
	tuitest.NoInjectedControls(t, h.raw())
}

func TestAppCtrlCCancelsTestThenForm(t *testing.T) {
	h, _ := newConnHarness(t)
	h.step("n")
	h.fillNew("slow")
	h.fake.SetLatency(5 * time.Second)
	h.step("ctrl+t")
	if !h.m.form.Testing() {
		t.Fatal("test not running")
	}
	h.step("ctrl+c")
	if h.m.form == nil || h.m.form.Testing() {
		t.Fatal("first ctrl+c should cancel only the test")
	}
	h.step("ctrl+c")
	if h.m.form != nil {
		t.Fatal("second ctrl+c should close the form")
	}
	if h.quitted() {
		t.Fatal("ctrl+c quit the app from inside the form")
	}
}

func TestAppNoConfigPath(t *testing.T) {
	h := newHarness(t)
	h.step("n")
	if v := h.text(); strings.Contains(v, "DSN") || !strings.Contains(v, "config") {
		t.Fatalf("expected a flash about the missing config path:\n%s", v)
	}
}

func TestAppConnFormTinyWindow(t *testing.T) {
	h, _ := newConnHarness(t)
	h.step("n")
	for _, sz := range [][2]int{{20, 5}, {1, 1}, {60, 8}, {200, 60}} {
		h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		h.settle()
		lines := strings.Split(h.text(), "\n")
		if len(lines) != sz[1] {
			t.Fatalf("%v: %d lines", sz, len(lines))
		}
	}
	h.step("esc")
	h.step("D")
	h.send(tea.WindowSizeMsg{Width: 20, Height: 5})
	_ = h.text()
}

type errBoom string

func (e errBoom) Error() string { return string(e) }

func TestAppConnGolden(t *testing.T) {
	h, _ := newConnHarness(t)
	h.step("n")
	h.typ("db")
	golden(t, "conn_add_form", h.text())
	h.step("esc")
	h.step("D")
	h.typ("lo")
	golden(t, "conn_delete_confirm", h.text())
}

func TestAppGridKeptUntilWriteSucceeds(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("E")
	if !h.m.cur.set {
		t.Fatal("opening the form must not touch the grid")
	}
	h.typ("2") // rename: the connection is closed and the grid must go
	// No settle: the write result has not been fed back yet.
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	h.sendSaved() // the SavedMsg
	if !h.m.saving {
		t.Fatal("write should be in flight")
	}
	if !h.m.cur.set || h.m.gr.Status().Loaded == 0 {
		t.Fatalf("grid cleared before the write finished:\n%s", h.text())
	}
	h.pump(func() bool { return !h.m.saving })
	if h.m.cur.set || h.m.gr.Status().Loaded != 0 {
		t.Fatalf("grid still bound to a closed connection after the write:\n%s", h.text())
	}
}

func TestAppFailedWriteKeepsGridAndResumesLoad(t *testing.T) {
	h, path := newConnHarness(t)
	h.openPeople()
	h.fake.SetLatency(5 * time.Second)
	h.keys("s") // a sort starts a page load
	if !h.m.gr.Loading() {
		t.Fatal("no load running")
	}
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("E")
	// Change the file under the form so the save conflicts and fails.
	if err := os.WriteFile(path, []byte(connSeed+"# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.typ("2")
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	h.sendSaved() // the SavedMsg: the write starts and the load is cancelled
	if h.m.gr.Loading() {
		t.Fatal("load must be cancelled before the write")
	}
	h.pump(func() bool { return !h.m.saving }) // the write fails
	if h.m.form == nil {
		t.Fatalf("form closed on a failed write:\n%s", h.text())
	}
	if !h.m.cur.set || h.m.src == nil {
		t.Fatal("a failed write cleared the grid")
	}
	if !h.m.gr.Loading() {
		t.Fatal("the cancelled load was not restarted")
	}
	h.step("esc")
	h.m.shutdown() // abandon the slow load
}

func TestAppFailedRemoveKeepsGrid(t *testing.T) {
	h, path := newConnHarness(t)
	h.openPeople()
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("D")
	h.typ("local")
	if err := os.WriteFile(path, []byte(connSeed+"# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.step("enter")
	h.pump(func() bool { return !h.m.saving })
	if h.m.confirm == nil || h.m.confirm.err == "" {
		t.Fatalf("expected an inline conflict error:\n%s", h.text())
	}
	if !h.m.cur.set || h.m.gr.Status().Loaded == 0 {
		t.Fatalf("failed removal cleared the grid:\n%s", h.text())
	}
	h.step("esc")
	if !strings.Contains(h.text(), "Ada Lovelace") {
		t.Fatalf("grid not shown after cancelling the confirmation:\n%s", h.text())
	}
}

const brokenSeed = connSeed + `  - name: leaky
    driver: fake
    dsn: fake://leaky
    password: hunter2-SUPERSECRET
`

func TestAppBrokenEntryShownAndRepairable(t *testing.T) {
	h, path := newConnHarnessWith(t, brokenSeed)
	if got := strings.Join(h.names(), ","); got != "local,ro,leaky" {
		t.Fatalf("names = %s", got)
	}
	h.step("down", "down") // leaky
	h.settle()
	pane := squash(h.paneText())
	for _, want := range []string{"connection leaky is misconfigured:", "password must be a secret reference"} {
		if !strings.Contains(pane, want) {
			t.Fatalf("grid pane lacks %q:\n%s", want, h.text())
		}
	}
	if strings.Contains(h.text(), "SUPERSECRET") {
		t.Fatalf("screen leaks the password:\n%s", h.text())
	}
	// Enter does not try to open it.
	before := len(h.fake.Calls())
	h.step("enter")
	if len(h.fake.Calls()) != before || h.mgr.State("leaky").Status != session.Failed {
		t.Fatal("enter opened a broken entry")
	}
	// Edit pre-fills what parsed safely, never the password, and saves a fix.
	h.step("E")
	if v := h.text(); !strings.Contains(v, "fake://leaky") || strings.Contains(v, "SUPERSECRET") {
		t.Fatalf("edit form:\n%s", v)
	}
	h.step("tab", "tab", "tab") // name, driver, dsn -> password ref
	h.typ("env:LEAKY_PW")
	h.step("ctrl+s")
	h.until("saved leaky")
	if st := h.mgr.State("leaky"); st.Broken || st.Status != session.Idle {
		t.Fatalf("leaky after repair = %+v", st)
	}
	if got := fileText(t, path); strings.Contains(got, "SUPERSECRET") || !strings.Contains(got, "password: env:LEAKY_PW") {
		t.Fatalf("config file:\n%s", got)
	}
}

func TestAppBrokenEntryCanBeDeleted(t *testing.T) {
	h, path := newConnHarnessWith(t, brokenSeed)
	h.step("down", "down", "D")
	h.typ("leaky")
	h.step("enter")
	h.until("removed leaky")
	if strings.Contains(fileText(t, path), "leaky") || strings.Join(h.names(), ",") != "local,ro" {
		t.Fatalf("not removed: %s / %v", fileText(t, path), h.names())
	}
}

const dupSeed = connSeed + `  - name: local
    driver: fake
    dsn: fake://second-local
  - driver: fake
    dsn: fake://noname
`

func TestAppDeleteTargetsTheBrokenDuplicateByLine(t *testing.T) {
	h, path := newConnHarnessWith(t, dupSeed)
	if got := strings.Join(h.names(), ","); got != "local,ro,(entry at line 11),(entry at line 14)" {
		t.Fatalf("names = %s", got)
	}
	h.step("down", "down", "D") // the duplicate local
	h.typ("(entry at line 11)")
	h.step("enter")
	h.until("removed (entry at line 11)")
	got := fileText(t, path)
	if strings.Contains(got, "second-local") || !strings.Contains(got, "fake://local\n") || !strings.Contains(got, "noname") {
		t.Fatalf("wrong entry removed:\n%s", got)
	}
}

func TestAppEditUnnamedBrokenEntryByLine(t *testing.T) {
	h, path := newConnHarnessWith(t, dupSeed)
	h.step("down", "down", "down", "E") // the unnamed entry
	if v := h.text(); !strings.Contains(v, "fake://noname") {
		t.Fatalf("form not prefilled:\n%s", v)
	}
	h.typ("named")
	h.step("ctrl+s")
	h.until("saved named")
	got := fileText(t, path)
	if !strings.Contains(got, "name: named") || !strings.Contains(got, "fake://second-local") || strings.Count(got, "fake://noname") != 1 {
		t.Fatalf("config:\n%s", got)
	}
}

const twoUnnamed = connSeed + `  - driver: fake
    dsn: fake://first-noname
  - driver: fake
    dsn: fake://second-noname
`

func TestAppStaleSidebarCannotDeleteTheWrongEntry(t *testing.T) {
	h2, path2 := newConnHarnessWith(t, twoUnnamed)
	// Outside tobira the first unnamed entry is deleted, so the second one
	// moves up to its line.
	external := connSeed + "  - driver: fake\n    dsn: fake://second-noname\n"
	if err := os.WriteFile(path2, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	h2.step("down", "down", "D") // the stale first unnamed row
	h2.settle()
	if fileText(t, path2) != external {
		t.Fatal("file touched")
	}
	if strings.Contains(h2.text(), "Type") {
		t.Fatalf("confirmation opened on a stale sidebar:\n%s", h2.text())
	}
	if !strings.Contains(h2.text(), "config file changed on disk; reload with :reload") {
		t.Fatalf("no conflict flash:\n%s", h2.text())
	}
}

func TestAppStaleWriteIsAConflictEvenIfOpenedBeforeTheChange(t *testing.T) {
	h, path := newConnHarnessWith(t, twoUnnamed)
	h.step("down", "down", "D")
	h.typ("(entry at line 11)")
	external := connSeed + "  - driver: fake\n    dsn: fake://second-noname\n"
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	h.step("enter")
	h.until("config file changed on disk; reload with :reload")
	if fileText(t, path) != external {
		t.Fatal("file touched")
	}
}

func TestAppReloadCommandRereadsTheConfig(t *testing.T) {
	h, path := newConnHarnessWith(t, twoUnnamed)
	external := connSeed + "  - driver: fake\n    dsn: fake://second-noname\n"
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys(":")
	h.typ("reload")
	h.step("enter")
	h.until("reloaded the config file")
	if got := strings.Join(h.names(), ","); got != "local,ro,(entry at line 11)" {
		t.Fatalf("names = %s", got)
	}
	// With the new version loaded, a delete works again.
	h.step("down", "down", "D")
	h.typ("(entry at line 11)")
	h.step("enter")
	h.until("removed (entry at line 11)")
	if strings.Contains(fileText(t, path), "second-noname") {
		t.Fatalf("not removed:\n%s", fileText(t, path))
	}
}

func TestAppUnknownKeyNeverLeaksIntoThePane(t *testing.T) {
	h, _ := newConnHarnessWith(t, connSeed+"  - {name: leak, driver: fake, password:hunter2SECRET}\n")
	h.step("down", "down")
	h.settle()
	if v := h.text(); strings.Contains(v, "SECRET") || !strings.Contains(squash(h.paneText()), "did you mean") {
		t.Fatalf("pane:\n%s", v)
	}
}

func TestAppReloadWithBrokenConfigLeavesTheGridAlone(t *testing.T) {
	h, path := newConnHarness(t)
	h.openPeople()
	h.fake.SetLatency(5 * time.Second)
	h.keys("s") // a sort starts a page load
	if !h.m.gr.Loading() {
		t.Fatal("no load running")
	}
	if err := os.WriteFile(path, []byte("connections: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys(":")
	h.typ("reload")
	h.step("enter")
	h.pump(func() bool { return !h.m.saving })
	h.drain(50 * time.Millisecond)
	if !h.m.cur.set || !h.m.gr.Loading() {
		t.Fatalf("a failed config reload must leave the running load alone:\n%s", h.text())
	}
	h.m.shutdown() // abandon the slow load
}

func TestAppDeleteHintNamesTheLabelForUnnamedEntries(t *testing.T) {
	h, _ := newConnHarnessWith(t, twoUnnamed)
	h.step("down", "down", "D")
	if v := squash(h.paneText()); !strings.Contains(v, "Type the label shown ((entry at line 11)) to confirm") {
		t.Fatalf("hint:\n%s", h.text())
	}
	h.step("esc", "up", "up", "D") // a named entry
	if v := squash(h.paneText()); !strings.Contains(v, "Type its name to confirm") {
		t.Fatalf("hint:\n%s", h.text())
	}
}

// filterPeople applies "id > 15" in the grid's filter bar and waits for it.
func (h *harness) filterPeople() {
	h.t.Helper()
	h.keys("f")
	h.typ("id > 15")
	h.keys("enter")
	h.pump(func() bool { return h.m.filterText == "id > 15" && !h.m.gr.Loading() && h.m.gr.Status().Loaded > 0 })
}

func TestAppFailedEditResumesWithSortAndFilterIntact(t *testing.T) {
	h, path := newConnHarness(t)
	h.openPeople()
	h.filterPeople()
	loaded := h.m.gr.Status().Loaded
	h.keys("right", "s") // sort by the name column
	h.pump(func() bool { return !h.m.gr.Loading() })
	h.fake.SetLatency(5 * time.Second)
	h.keys("s") // sort descending: a load starts
	if !h.m.gr.Loading() {
		t.Fatal("no load running")
	}
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("E")
	if err := os.WriteFile(path, []byte(connSeed+"# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.typ("2")
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	h.sendSaved() // the SavedMsg: the write starts and the load is cancelled
	h.fake.SetLatency(0)
	h.pump(func() bool { return !h.m.saving }) // the write fails
	h.pump(func() bool { return !h.m.gr.Loading() && h.m.gr.Status().Loaded > 0 })
	if h.m.filterText != "id > 15" {
		t.Fatalf("filterText = %q, want the filter still applied", h.m.filterText)
	}
	st := h.m.gr.Status()
	if st.Loaded != loaded || len(st.Sort) != 1 || !st.Sort[0].Desc {
		t.Fatalf("grid state after the resumed load: %+v, want %d filtered rows sorted descending", st, loaded)
	}
	h.step("esc")
}

func TestAppUnchangedEditResumesTheCancelledLoad(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.fake.SetLatency(5 * time.Second)
	h.keys("s") // a load starts
	if !h.m.gr.Loading() {
		t.Fatal("no load running")
	}
	h.m.sb = h.m.sb.Select("local")
	h.m.focus = focusSidebar
	h.step("E")
	h.send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}) // save without changing anything
	h.sendSaved()                                        // the SavedMsg
	if h.m.gr.Loading() {
		t.Fatal("the load must be cancelled while the write runs")
	}
	h.fake.SetLatency(0)
	h.pump(func() bool { return !h.m.saving })
	if !h.m.cur.set {
		t.Fatalf("an unchanged connection must keep its grid:\n%s", h.text())
	}
	h.pump(func() bool { return !h.m.gr.Loading() && h.m.gr.Status().Loaded > 0 })
	if s := h.m.gr.Status().Sort; len(s) != 1 {
		t.Fatalf("sort lost on resume: %v", s)
	}
}

func TestAppReloadWithBrokenConfigRetriesAFailedPage(t *testing.T) {
	h, path := newConnHarness(t)
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.fake.FailNext("browse", errors.New("server is warming up"))
	h.keys("down", "enter") // page 0 fails
	h.until("server is warming up")
	if !h.m.gr.Failed() {
		t.Fatal("the page should be in an error state")
	}
	if err := os.WriteFile(path, []byte("connections: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(h.fake.Calls())
	h.keys(":")
	h.typ("reload")
	h.step("enter")
	h.pump(func() bool { return !h.m.saving })
	h.until("Ada Lovelace") // the grid recovered
	if len(h.fake.Calls()) <= before {
		t.Fatal("the failed page was not fetched again")
	}
}

func TestAppFailedConfigReloadKeepsTheGridState(t *testing.T) {
	h, path := newConnHarness(t)
	h.openPeople()
	h.filterPeople()
	loaded := h.m.gr.Status().Loaded
	calls := len(h.fake.Calls())
	if err := os.WriteFile(path, []byte("connections: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys(":")
	h.typ("reload")
	h.step("enter")
	h.pump(func() bool { return !h.m.saving })
	h.drain(50 * time.Millisecond)
	if h.m.filterText != "id > 15" {
		t.Fatalf("filterText = %q: the grid still has its filter", h.m.filterText)
	}
	if got := h.m.gr.Status().Loaded; got != loaded || len(h.fake.Calls()) != calls {
		t.Fatalf("grid reloaded: loaded %d (want %d), driver calls %d -> %d", got, loaded, calls, len(h.fake.Calls()))
	}
}

const dsnNameSeed = connSeed + `  - name: "postgres://u:supersecret@h/db"
`

func TestAppBrokenEntryNameIsRedacted(t *testing.T) {
	h, _ := newConnHarnessWith(t, dsnNameSeed)
	if got := strings.Join(h.names(), ","); strings.Contains(got, "supersecret") || strings.Count(got, ",") != 2 {
		t.Fatalf("sidebar names leak the password: %s", got)
	}
	h.step("down", "down") // the broken entry
	h.settle()
	if !strings.Contains(squash(h.paneText()), "is misconfigured:") {
		t.Fatalf("no misconfigured pane:\n%s", h.text())
	}
	if strings.Contains(h.text(), "supersecret") {
		t.Fatalf("screen leaks the password:\n%s", h.text())
	}
}

func TestAppReloadDoesNotFreezeQuit(t *testing.T) {
	h, path := newConnHarness(t)
	h.openPeople() // local is open, so a changed local is closed by the reload
	h.fake.SetCloseLatency(time.Second)
	changed := strings.Replace(connSeed, "fake://local", "fake://local2", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys(":")
	h.typ("reload")
	h.step("enter")
	if !h.m.saving {
		t.Fatal("the reload should still be running")
	}
	start := time.Now()
	h.step("ctrl+c") // cancels the page load in flight, if any
	if !h.quitted() {
		h.step("ctrl+c")
	}
	if !h.quitted() {
		t.Fatal("ctrl+c did not quit while the reload was running")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("quit took %v: the keyboard waited on the reload", d)
	}
}

func TestAppReloadBlocksOnlyEditsNotQ(t *testing.T) {
	h, path := newConnHarness(t)
	h.fake.SetCloseLatency(time.Second)
	h.openPeople()
	changed := strings.Replace(connSeed, "fake://local", "fake://local2", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys(":")
	h.typ("reload")
	h.step("enter")
	h.step("tab") // focus the sidebar
	h.step("D", "E")
	if h.m.confirm != nil || h.m.form != nil {
		t.Fatal("edit/delete must not open while a config write is in flight")
	}
	h.step("q")
	if !h.quitted() {
		t.Fatal("q did not quit while the reload was running")
	}
}

func TestAppEditFormOfDSNNamedBrokenEntryHidesPassword(t *testing.T) {
	h, path := newConnHarnessWith(t, dsnNameSeed)
	h.step("down", "down", "E")
	if v := h.text(); strings.Contains(v, "supersecret") || !strings.Contains(v, "Name") {
		t.Fatalf("edit form:\n%s", v)
	}
	h.typ("fixed")
	h.step("enter", "enter") // name, driver
	h.typ("fake://fixed")
	h.step("ctrl+s")
	h.until("saved fixed")
	got := fileText(t, path)
	if strings.Contains(got, "supersecret") || !strings.Contains(got, "name: fixed") {
		t.Fatalf("config file:\n%s", got)
	}
}

// memStore is a map-backed keyring.Store; no test touches the OS keychain.
type memStore struct {
	m      map[string]string
	delErr error
}

func (s *memStore) Get(_, a string) (string, error) {
	if v, ok := s.m[a]; ok {
		return v, nil
	}
	return "", keyring.ErrNotFound
}
func (s *memStore) Set(_, a, v string) error { s.m[a] = v; return nil }
func (s *memStore) Delete(_, a string) error {
	if s.delErr != nil {
		return s.delErr
	}
	delete(s.m, a)
	return nil
}

const keyringSeed = connSeed + `  - name: kr
    driver: fake
    dsn: fake://kr
    password: keyring:kr
  - name: other
    driver: fake
    dsn: fake://other
    password: keyring:somewhere-else
`

func removeConn(h *harness, name string, downs int) {
	for range downs {
		h.step("down")
	}
	h.step("D")
	h.typ(name)
	h.step("enter")
}

func TestAppDeleteRemovesItsKeyringEntry(t *testing.T) {
	store := &memStore{m: map[string]string{"kr": "pw", "somewhere-else": "pw2"}}
	h, path := newConnHarnessKeyring(t, keyringSeed, store)
	removeConn(h, "kr", 2)
	h.until("removed kr")
	if strings.Contains(fileText(t, path), "name: kr") {
		t.Fatal("not removed from the config")
	}
	if _, ok := store.m["kr"]; ok {
		t.Error("keyring entry left behind")
	}
	if _, ok := store.m["somewhere-else"]; !ok {
		t.Error("an unrelated entry was deleted")
	}
}

func TestAppDeleteLeavesForeignKeyringRefs(t *testing.T) {
	store := &memStore{m: map[string]string{"other": "pw", "somewhere-else": "pw2"}}
	h, _ := newConnHarnessKeyring(t, keyringSeed, store)
	removeConn(h, "other", 3)
	h.until("removed other")
	if len(store.m) != 2 {
		t.Errorf("store = %v, want both entries kept", store.m)
	}
}

func TestAppDeleteKeyringFailureIsAWarning(t *testing.T) {
	store := &memStore{m: map[string]string{"kr": "pw"}, delErr: errors.New("locked, secret pw")}
	h, path := newConnHarnessKeyring(t, keyringSeed, store)
	removeConn(h, "kr", 2)
	h.until("removed kr")
	if strings.Contains(fileText(t, path), "name: kr") {
		t.Fatal("not removed from the config")
	}
	v := h.text()
	if !strings.Contains(v, "keyring") || strings.Contains(v, "secret pw") {
		t.Errorf("view = %s", v)
	}
}

func TestAppDeleteKeepsAnEntryAnotherConnectionStillUses(t *testing.T) {
	store := &memStore{m: map[string]string{"kr": "pw"}}
	seed := keyringSeed + "  - name: sharer\n    driver: fake\n    dsn: fake://s\n    password: keyring:kr\n" +
		"  - name: sharer2\n    dsn: no-scheme\n    password: keyring:kr\n"
	h, _ := newConnHarnessKeyring(t, seed, store)
	removeConn(h, "kr", 2)
	h.until("removed kr")
	if _, ok := store.m["kr"]; !ok {
		t.Error("a shared keyring entry was deleted")
	}
}
