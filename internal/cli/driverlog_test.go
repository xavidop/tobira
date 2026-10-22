package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui"
)

// warnDriver is the fake driver that logs a warning through
// ConnConfig.Logger on every Open, as qdrant and influxdb do for a
// credential sent without TLS.
type warnDriver struct{ driver.Driver }

func (d warnDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	cfg.Log().Warn("driver warning from open")
	return d.Driver.Open(ctx, cfg)
}

func warnHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	state := t.TempDir()
	h.env = map[string]string{"XDG_STATE_HOME": state}
	h.wrapDriver = func(d driver.Driver) driver.Driver { return warnDriver{Driver: d} }
	return h, filepath.Join(state, "tobira", "tobira.log")
}

// A driver warning reaches the --debug log on the direct-open path (query).
func TestDriverWarningReachesDebugLogQuery(t *testing.T) {
	h, path := warnHarness(t)
	if _, errOut, code := h.exec(t, context.Background(), "--debug", "query", "local-pg", selectPeople); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "driver warning from open") {
		t.Errorf("debug log lacks the driver warning:\n%s", data)
	}
}

// A driver warning reaches the --debug log through the session Manager
// (connect, the TUI path).
func TestDriverWarningReachesDebugLogSession(t *testing.T) {
	h, path := warnHarness(t)
	h.tty = true
	h.runTUI = func(ctx context.Context, o tui.Options) error {
		_, err := o.Manager.Open(ctx, "local-pg")
		return err
	}
	if _, errOut, code := h.exec(t, context.Background(), "--debug", "connect", "local-pg"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "driver warning from open") {
		t.Errorf("debug log lacks the driver warning:\n%s", data)
	}
}

// A driver warning reaches the --debug log from connections test.
func TestDriverWarningReachesDebugLogConnectionsTest(t *testing.T) {
	h, path := warnHarness(t)
	if _, errOut, code := h.exec(t, context.Background(), "--debug", "connections", "test", "local-pg"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "driver warning from open") {
		t.Errorf("debug log lacks the driver warning:\n%s", data)
	}
}

// Without --debug the warning goes nowhere: not to stderr.
func TestDriverWarningWithoutDebugIsSilent(t *testing.T) {
	h, _ := warnHarness(t)
	if _, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople); code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}
