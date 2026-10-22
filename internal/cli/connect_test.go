package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/tui"
)

func TestBareRootOnTTYRunsTUI(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	var got tui.Options
	called := 0
	h.runTUI = func(_ context.Context, o tui.Options) error { called++; got = o; return nil }
	_, errOut, code := h.exec(t, context.Background())
	if code != 0 || called != 1 {
		t.Fatalf("code=%d called=%d stderr=%q", code, called, errOut)
	}
	if got.Manager == nil || got.Focus != "" || got.ConfigPath != h.cfg {
		t.Errorf("options = %+v, want manager, no focus, ConfigPath %q", got, h.cfg)
	}
	if got.Theme != "auto" || got.Keymap != "default" {
		t.Errorf("theme/keymap = %q/%q", got.Theme, got.Keymap)
	}
	if names := got.Manager.Names(); len(names) != 3 || names[0] != "local-pg" {
		t.Errorf("manager names = %v, want the configured connections", names)
	}
}

func TestBareRootNotTTYPrintsHelp(t *testing.T) {
	h := newHarness(t)
	called := false
	h.runTUI = func(context.Context, tui.Options) error { called = true; return nil }
	out, _, code := h.exec(t, context.Background())
	if code != 0 || called {
		t.Fatalf("code=%d called=%v", code, called)
	}
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "connect") {
		t.Errorf("help output = %q", out)
	}
}

func TestConnectByName(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	_, errOut, code := h.exec(t, context.Background(), "connect", "local-pg")
	if code != 0 || got.Focus != "local-pg" {
		t.Fatalf("code=%d focus=%q stderr=%q", code, got.Focus, errOut)
	}
}

func TestConnectByDSN(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	_, errOut, code := h.exec(t, context.Background(), "connect", "fake://bob:hunter2pw@host/db")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.Focus == "" || strings.Contains(got.Focus, "hunter2pw") {
		t.Errorf("focus = %q, want the redacted DSN", got.Focus)
	}
	found := false
	for _, n := range got.Manager.Names() {
		found = found || n == got.Focus
	}
	if !found {
		t.Errorf("focus %q is not a manager connection: %v", got.Focus, got.Manager.Names())
	}
}

func TestConnectReadOnly(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.runTUI = func(ctx context.Context, o tui.Options) error {
		_, err := o.Manager.Open(ctx, o.Focus)
		return err
	}
	_, errOut, code := h.exec(t, context.Background(), "connect", "local-pg", "--read-only")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(h.opened) != 1 || !h.opened[0].ReadOnly {
		t.Errorf("opened = %+v, want one read-only open", h.opened)
	}
}

func TestConnectUnknown(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.runTUI = func(context.Context, tui.Options) error { t.Error("TUI must not start"); return nil }
	_, errOut, code := h.exec(t, context.Background(), "connect", "local-p")
	if code != 1 || !strings.Contains(errOut, `did you mean "local-pg"`) {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestConnectNeedsTTY(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "connect", "local-pg")
	if code != 1 || !strings.Contains(errOut, "tobira connect needs an interactive terminal") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestTUIErrorIsEscaped(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.runTUI = func(context.Context, tui.Options) error { return errString("boom\x1b[31m") }
	_, errOut, code := h.exec(t, context.Background())
	if code == 0 || strings.Contains(errOut, "\x1b") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestBareRootReadOnly(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.runTUI = func(ctx context.Context, o tui.Options) error {
		_, err := o.Manager.Open(ctx, "local-pg")
		return err
	}
	_, errOut, code := h.exec(t, context.Background(), "--read-only")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(h.opened) != 1 || !h.opened[0].ReadOnly {
		t.Errorf("opened = %+v, want one read-only open", h.opened)
	}
}

func TestConnectDSNIgnoresBrokenConfig(t *testing.T) {
	for name, content := range map[string]string{
		"unparseable":   "connections: [\n  - {name: x\n",
		"bad tui":       "tui:\n  theme: nonsense-theme\n",
		"not a mapping": "- just\n- a list\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.tty = true
			if err := os.WriteFile(h.cfg, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			var got tui.Options
			called := 0
			h.runTUI = func(_ context.Context, o tui.Options) error { called++; got = o; return nil }
			_, errOut, code := h.exec(t, context.Background(), "connect", "fake://bob:hunter2pw@host/db")
			if code != 0 || called != 1 {
				t.Fatalf("code=%d called=%d stderr=%q", code, called, errOut)
			}
			if !strings.Contains(errOut, "warning") || strings.Contains(errOut, "hunter2pw") {
				t.Errorf("stderr should warn about the config and keep the DSN secret: %q", errOut)
			}
			if !strings.Contains(got.Notice, "continuing without the config file") || strings.Contains(got.Notice, "hunter2pw") {
				t.Errorf("Notice = %q, want the warning for the TUI to flash", got.Notice)
			}
			if got.Focus == "" || got.ConfigPath != "" {
				t.Errorf("focus=%q ConfigPath=%q, want a focus and connection management off", got.Focus, got.ConfigPath)
			}
		})
	}
}

func TestConnectNameStillNeedsConfig(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	if err := os.WriteFile(h.cfg, []byte("connections: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.runTUI = func(context.Context, tui.Options) error { t.Fatal("must not run"); return nil }
	if _, _, code := h.exec(t, context.Background(), "connect", "local-pg"); code == 0 {
		t.Fatal("a name with a broken config must fail")
	}
}

func TestConnectDSNMissingConfig(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.cfg = filepath.Join(t.TempDir(), "absent.yaml")
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	_, errOut, code := h.exec(t, context.Background(), "connect", "fake://host/db")
	if code != 0 || got.Focus == "" {
		t.Fatalf("code=%d focus=%q stderr=%q", code, got.Focus, errOut)
	}
}

// stubSQLite stands in for the sqlite driver the history store opens: every
// statement succeeds with no rows, except PRAGMA table_info, which answers
// a current history table (with the client column).
type stubSQLite struct{}

func (stubSQLite) Info() driver.Info {
	return driver.Info{Name: "sqlite", Kind: driver.KindSQL, Schemes: []string{"stub-sqlite"}, Dialect: "sqlite"}
}

func (stubSQLite) Open(context.Context, driver.ConnConfig) (driver.Conn, error) {
	return stubConn{}, nil
}

type stubConn struct{}

func (stubConn) Query(_ context.Context, q driver.Query) (driver.ResultSet, error) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q.Text)), "PRAGMA TABLE_INFO") {
		var rows []driver.Row
		for i, name := range []string{"id", "at", "conn", "source", "query", "ok", "rows", "ms", "client"} {
			rows = append(rows, driver.Row{driver.Int(int64(i)), driver.String(name)})
		}
		return driver.NewSliceResultSet(nil, rows, driver.Stats{}), nil
	}
	return driver.NewSliceResultSet(nil, nil, driver.Stats{}), nil
}
func (stubConn) Ping(context.Context) error { return nil }
func (stubConn) Close() error               { return nil }
func (stubConn) Namespaces(context.Context) ([]driver.Namespace, error) {
	return nil, nil
}
func (stubConn) Collections(context.Context, driver.NamespaceRef) ([]driver.Collection, error) {
	return nil, nil
}
func (stubConn) Describe(context.Context, driver.CollectionRef) (*driver.CollectionSchema, error) {
	return nil, driver.ErrNotFound
}

func TestConnectGivesTheTUIAHistoryStore(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.env = map[string]string{"XDG_STATE_HOME": t.TempDir()}
	h.extra = []driver.Driver{stubSQLite{}}
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	if _, errOut, code := h.exec(t, context.Background(), "connect", "local-pg"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.History == nil {
		t.Fatal("the TUI got no history store")
	}
}

func TestConnectWithoutHistoryPassesNil(t *testing.T) {
	h := newHarness(t) // no sqlite driver: history is unavailable
	h.tty = true
	h.env = map[string]string{"XDG_STATE_HOME": t.TempDir()}
	got := tui.Options{History: stubRecorder{}}
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	if _, errOut, code := h.exec(t, context.Background(), "connect", "local-pg"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.History != nil {
		t.Fatalf("History = %#v, want a nil interface", got.History)
	}
}

type stubRecorder struct{}

func (stubRecorder) Record(context.Context, history.Entry, driver.ConnConfig) error { return nil }

func (stubRecorder) List(context.Context, history.Filter) ([]history.Entry, error) { return nil, nil }

func TestConnectGivesTheTUIStderrForExitNotices(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	if _, errOut, code := h.exec(t, context.Background(), "connect", "local-pg"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.Stderr == nil {
		t.Fatal("the TUI got no stderr for its exit notice")
	}
}
