package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// doctorRow returns doctor's section/name row from its JSON output.
func doctorRow(t *testing.T, out, section, name string) (status, detail string) {
	t.Helper()
	var rep struct {
		Checks []struct{ Section, Name, Status, Detail string } `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, out)
	}
	for _, c := range rep.Checks {
		if c.Section == section && c.Name == name {
			return c.Status, c.Detail
		}
	}
	t.Fatalf("no %s/%s row in %s", section, name, out)
	return "", ""
}

// TestHistoryPathConfigKey: history.path (design §5) moves the history file;
// variables and ~ expand, a missing parent is created 0700 without touching
// an existing shared directory, the file is 0600, and query, history and
// doctor all use it.
func TestHistoryPathConfigKey(t *testing.T) {
	bin := buildBinary(t)
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "app.db")
	base := isolatedEnv(t)
	home := ""
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	for _, tt := range []struct{ name, configured, want string }{
		{"env", "$HIST_DIR/sub/history.db", filepath.Join(shared, "sub", "history.db")},
		{"home", "~/hist/h.db", filepath.Join(home, "hist", "h.db")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := writeConfig(t, withEnv(base, "HIST_DIR="+shared),
				"connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+db+"\nhistory:\n  path: \""+tt.configured+"\"\n")
			if so, se, code := runBoth(t, bin, env, "query", "lite", "select 41 + 1 as answer"); code != 0 {
				t.Fatalf("query: exit %d\n%s\n%s", code, so, se)
			}
			fi, err := os.Stat(tt.want)
			if err != nil {
				t.Fatalf("history.path file not created: %v", err)
			}
			if runtime.GOOS != "windows" {
				if fi.Mode().Perm() != 0o600 {
					t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
				}
				if di, _ := os.Stat(filepath.Dir(tt.want)); di.Mode().Perm() != 0o700 {
					t.Errorf("created dir mode = %v, want 0700", di.Mode().Perm())
				}
				if si, _ := os.Stat(shared); si.Mode().Perm() != 0o755 {
					t.Errorf("shared dir mode = %v, want unchanged 0755", si.Mode().Perm())
				}
			}
			if _, err := os.Stat(historyPath(t, env)); !os.IsNotExist(err) {
				t.Errorf("the default history file was created too (err=%v)", err)
			}
			out, code := run(t, bin, env, "history", "--format", "json")
			if code != 0 || !strings.Contains(out, "select 41 + 1") {
				t.Errorf("history: exit %d, out %s", code, out)
			}
			out, _ = run(t, bin, env, "doctor", "--no-ping")
			st, detail := doctorRow(t, out, "history", "db")
			if st != "ok" || !strings.Contains(detail, tt.want) || !strings.Contains(detail, "schema v2") {
				t.Errorf("doctor history = %s %q, want ok naming %s", st, detail, tt.want)
			}
			if st, detail := doctorRow(t, out, "permissions", "history"); st != "ok" || !strings.Contains(detail, tt.want) {
				t.Errorf("doctor permissions history = %s %q", st, detail)
			}
		})
	}

	// A variable that is not set is an error, never a silent fallback.
	env := writeConfig(t, base, "history:\n  path: $TOBIRA_TEST_NO_SUCH_DIR/h.db\n")
	so, se, code := runBoth(t, bin, env, "history")
	if code == 0 || !strings.Contains(se, "TOBIRA_TEST_NO_SUCH_DIR is not set") {
		t.Errorf("history with an unset variable: exit %d, stdout %q, stderr %q", code, so, se)
	}
	out, _ := run(t, bin, env, "doctor", "--no-ping")
	if st, detail := doctorRow(t, out, "history", "db"); st != "fail" || !strings.Contains(detail, "is not set") {
		t.Errorf("doctor history = %s %q, want a fail", st, detail)
	}
}

// TestDoctorHistoryChangesNothing: doctor inspects the history file
// read-only. A loose mode stays loose (the permissions row warns about it),
// a pre-M5 file is not migrated, and the schema and size are reported.
func TestDoctorHistoryChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := historyPath(t, env)
	seedOldHistory(t, path)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := run(t, bin, env, "doctor", "--no-ping")
	st, detail := doctorRow(t, out, "history", "db")
	if st != "ok" || !strings.Contains(detail, "schema v1") || !strings.Contains(detail, "KiB") || !strings.Contains(detail, "writable") {
		t.Errorf("doctor history = %s %q, want ok with schema v1, the size and writable", st, detail)
	}
	if st, _ := doctorRow(t, out, "permissions", "history"); st != "warn" {
		t.Errorf("permissions history = %s, want warn for 0644", st)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("doctor changed the history mode to %v", fi.Mode().Perm())
	}
	if clientColumnPresent(t, path) {
		t.Error("doctor migrated the history schema")
	}

	// A file tobira cannot write is reported, still without changing it.
	if os.Geteuid() == 0 {
		return
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, bin, env, "doctor", "--no-ping")
	if st, detail := doctorRow(t, out, "history", "db"); st != "warn" || !strings.Contains(detail, "not writable") {
		t.Errorf("doctor history on a 0400 file = %s %q, want warn not writable", st, detail)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o400 {
		t.Errorf("doctor changed a 0400 history file to %v", fi.Mode().Perm())
	}
}
