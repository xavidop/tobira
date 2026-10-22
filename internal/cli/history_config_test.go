package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// unloadableConfig fails to load as a whole (history: must be a mapping) and
// carries a password-looking DSN that must not reach the warning.
const unloadableConfig = "connections:\n  - name: a\n    dsn: fake://u:hunter2-broken@h/db\nhistory: [oops\n"

// TestBrokenConfigFallsBackToDefaultHistory: a config that cannot load
// does not fail `tobira history` or an ad-hoc `tobira query <dsn>`; each
// warns once and uses the default history path, as before history.path.
func TestBrokenConfigFallsBackToDefaultHistory(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, []byte(unloadableConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	h.env = map[string]string{"XDG_STATE_HOME": state}
	h.extra = []driver.Driver{stubSQLite{}}
	check := func(what, errOut string) {
		t.Helper()
		const want = "; using the default history path"
		if n := strings.Count(errOut, "config not loaded: "); n != 1 || !strings.Contains(errOut, want) {
			t.Errorf("%s: stderr = %q, want one %q warning", what, errOut, "config not loaded: …"+want)
		}
		if strings.Contains(errOut, "hunter2-broken") {
			t.Errorf("%s: a secret reached stderr: %q", what, errOut)
		}
	}

	_, errOut, code := h.exec(t, context.Background(), "query", "fake://x", selectPeople, "--format", "ndjson")
	if code != 0 {
		t.Fatalf("query: code=%d stderr=%q", code, errOut)
	}
	check("query", errOut)
	if _, err := os.Stat(filepath.Join(state, "tobira", "history.db")); err != nil {
		t.Errorf("query did not record to the default history path: %v", err)
	}

	_, errOut, code = h.exec(t, context.Background(), "history", "--format", "json")
	if code != 0 {
		t.Fatalf("history: code=%d stderr=%q", code, errOut)
	}
	check("history", errOut)
}
