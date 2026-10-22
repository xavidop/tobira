package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	return sharedBuild(t, "")
}

// isolatedEnv keeps the binary away from the developer's real config.
func isolatedEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return append(os.Environ(),
		"TOBIRA_CONFIG="+filepath.Join(dir, "missing.toml"),
		"HOME="+dir,
		"XDG_CONFIG_HOME="+dir,
		"XDG_STATE_HOME="+dir,
	)
}

// run executes the binary and returns stdout and the exit code.
func run(t *testing.T, bin string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), 0
	case errors.As(err, &ee):
		return stdout.String(), ee.ExitCode()
	default:
		t.Fatalf("run %v: %v", args, err)
		return "", -1
	}
}

func TestMainBuilds(t *testing.T) {
	bin := buildBinary(t)
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("run version: %v", err)
	}
	if !strings.HasPrefix(string(out), "tobira dev") {
		t.Errorf("output = %q, want prefix %q", out, "tobira dev")
	}
}

// newSQLiteFile creates a database with one table, through the binary itself.
func newSQLiteFile(t *testing.T, bin string, env []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "e2e.db")
	if out, code := run(t, bin, env, "query", "sqlite:"+path, "create table t (id integer primary key)"); code != 0 {
		t.Fatalf("create table: exit %d (stdout %q)", code, out)
	}
	return path
}

func TestEndToEndSQLite(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := newSQLiteFile(t, bin, env)

	out, code := run(t, bin, env, "query", "sqlite:"+path, "select 1 as one", "--format", "json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stdout %q)", code, out)
	}
	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if len(doc.Rows) != 1 || doc.Rows[0]["one"] != float64(1) {
		t.Errorf("rows = %v, want one row with one=1", doc.Rows)
	}

	if _, code := run(t, bin, env, "query", "nope", "x"); code != 1 {
		t.Errorf("unknown connection exit = %d, want 1", code)
	}
}

func TestEndToEndSQLiteReadOnly(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := newSQLiteFile(t, bin, env)
	_, code := run(t, bin, env, "query", "sqlite:"+path, "delete from t", "--read-only")
	if code != 4 {
		t.Errorf("exit = %d, want 4", code)
	}
}

func TestEndToEndReadOnlyFlagDoesNotCreateFile(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := filepath.Join(t.TempDir(), "new.db")
	_, code := run(t, bin, env, "query", "--read-only", "sqlite:"+path, "select 1")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (could not open the connection)", code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--read-only created %s (stat err %v)", path, err)
	}
}

// TestSecondInterruptKills blocks the binary on a stdin read that ignores
// cancellation: the first SIGINT cancels the context, the second must kill
// the process.
func TestSecondInterruptKills(t *testing.T) {
	bin := buildBinary(t)
	_ = exec.Command(bin, "version").Run() // warm up: the first exec of a new binary can be slow
	// There is no readiness signal, so a process that dies on the first
	// interrupt had not installed its handlers yet: retry with a longer wait.
	for _, wait := range []time.Duration{300 * time.Millisecond, time.Second, 3 * time.Second} {
		if secondInterruptKills(t, bin, wait) {
			return
		}
	}
	t.Fatal("the binary never survived a first interrupt")
}

// secondInterruptKills reports false when the process exited on the first
// interrupt, and fails the test when the second one does not stop it.
func secondInterruptKills(t *testing.T, bin string, wait time.Duration) bool {
	t.Helper()
	cmd := exec.Command(bin, "query", "sqlite:"+filepath.Join(t.TempDir(), "x.db"), "-f", "-")
	cmd.Env = isolatedEnv(t)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	time.Sleep(wait)
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
		return false
	case <-time.After(200 * time.Millisecond):
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("second interrupt did not stop the process")
	}
	return true
}

func TestEndToEndConnections(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	env := append(isolatedEnv(t), "TOBIRA_CONFIG="+filepath.Join(dir, "config.yaml"))
	db := filepath.Join(dir, "x.db")
	if out, code := run(t, bin, env, "query", "sqlite:"+db, "create table t (a)"); code != 0 {
		t.Fatalf("create db: exit %d (%q)", code, out)
	}
	if out, code := run(t, bin, env, "connections", "add", "--name", "x", "--driver", "sqlite", "--dsn", "sqlite:"+db); code != 0 {
		t.Fatalf("add: exit %d (%q)", code, out)
	}
	out, code := run(t, bin, env, "connections", "list")
	if code != 0 {
		t.Fatalf("list: exit %d", code)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list is not JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0]["name"] != "x" || rows[0]["driver"] != "sqlite" {
		t.Errorf("rows = %v", rows)
	}
	if strings.Contains(strings.ToLower(out), "password\"") {
		t.Errorf("list exposes a password field: %s", out)
	}
	if out, code := run(t, bin, env, "connections", "test", "x"); code != 0 || !strings.HasPrefix(out, "ok (") {
		t.Errorf("test: exit %d out %q", code, out)
	}
	if _, code := run(t, bin, env, "connections", "add", "--name", "x", "--driver", "sqlite", "--dsn", "sqlite:"+db); code != 1 {
		t.Errorf("duplicate add exit = %d, want 1", code)
	}
	if _, code := run(t, bin, env, "connections", "rm", "x", "--yes"); code != 0 {
		t.Errorf("rm: exit %d", code)
	}
	out, _ = run(t, bin, env, "connections", "list")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("list after rm = %q, want []", out)
	}
	// Bare tobira with no terminal prints help instead of starting the TUI.
	if out, code := run(t, bin, env); code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("bare: exit %d out %q", code, out)
	}
}

// TestKeyringSchemeIsRegistered shows the binary registers keyring: at start:
// a config entry using it loads, and set-password refuses to read a password
// off a terminal. Neither touches the OS keychain.
func TestKeyringSchemeIsRegistered(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	body := "connections:\n  - name: kr\n    driver: sqlite\n    dsn: sqlite://file.db\n    password: keyring:kr\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, bin, env, "--config", cfg, "connections", "list")
	if code != 0 || !strings.Contains(out, `"password_ref": "keyring:kr"`) || !strings.Contains(out, `"broken": ""`) {
		t.Fatalf("code=%d out=%s", code, out)
	}
	cmd := exec.Command(bin, "--config", cfg, "connections", "set-password", "kr")
	cmd.Env = env
	cmd.Stdin = strings.NewReader("pw\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(stderr.String(), "--stdin") {
		t.Fatalf("err=%v stderr=%q", err, stderr.String())
	}
}
