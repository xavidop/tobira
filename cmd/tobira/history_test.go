package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/history"
)

// runBoth is run plus stderr.
func runBoth(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return so.String(), se.String(), ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return so.String(), se.String(), 0
}

func stateDir(t *testing.T, env []string) string {
	t.Helper()
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "XDG_STATE_HOME="); ok {
			return v
		}
	}
	t.Fatal("no XDG_STATE_HOME in env")
	return ""
}

type histRow struct {
	At, Conn, Source, Query string
	OK                      bool
	Rows, Ms                float64
}

func listHistory(t *testing.T, bin string, env []string, args ...string) []histRow {
	t.Helper()
	out, code := run(t, bin, env, append([]string{"history", "--format", "json"}, args...)...)
	if code != 0 {
		t.Fatalf("history exit %d (stdout %q)", code, out)
	}
	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	var rows []map[string]any
	if strings.HasPrefix(out, "[") {
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
	} else {
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		rows = doc.Rows
	}
	var res []histRow
	for _, r := range rows {
		h := histRow{}
		h.At, _ = r["at"].(string)
		h.Conn, _ = r["conn"].(string)
		h.Source, _ = r["source"].(string)
		h.Query, _ = r["query"].(string)
		h.OK, _ = r["ok"].(bool)
		h.Rows, _ = r["rows"].(float64)
		h.Ms, _ = r["ms"].(float64)
		res = append(res, h)
	}
	return res
}

func TestHistoryEmptyWithoutFile(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	out, code := run(t, bin, env, "history", "--format", "json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("exit %d, out %q; want 0 and []", code, out)
	}
	if out, code := run(t, bin, env, "history", "--format", "csv"); code != 0 || out != "" {
		t.Fatalf("csv: exit %d, out %q; want 0 and nothing", code, out)
	}
	if _, err := os.Stat(filepath.Join(stateDir(t, env), "tobira", "history.db")); err == nil {
		t.Error("listing history created the file")
	}
}

func TestHistoryRecordsQueries(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	db := filepath.Join(t.TempDir(), "h.db")
	dsn := "sqlite:" + db
	if _, code := run(t, bin, env, "query", dsn, "create table t (id integer primary key)"); code != 0 {
		t.Fatalf("create: %d", code)
	}
	if _, code := run(t, bin, env, "query", dsn, "insert into t values (1),(2)"); code != 0 {
		t.Fatalf("insert: %d", code)
	}
	if _, code := run(t, bin, env, "query", dsn, "select * from t", "--format", "json"); code != 0 {
		t.Fatalf("select: %d", code)
	}
	if _, code := run(t, bin, env, "query", dsn, "select * from missing"); code == 0 {
		t.Fatal("bad query succeeded")
	}

	got := listHistory(t, bin, env)
	if len(got) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(got), got)
	}
	// newest first
	if got[0].Query != "select * from missing" || got[0].OK {
		t.Errorf("newest = %+v, want the failed select", got[0])
	}
	if got[1].Query != "select * from t" || !got[1].OK || got[1].Rows != 2 || got[1].Source != "cli" {
		t.Errorf("select entry = %+v", got[1])
	}
	if got[2].Rows != 2 {
		t.Errorf("insert entry rows = %v, want 2 affected", got[2].Rows)
	}
	if !strings.HasPrefix(got[1].Conn, "sqlite:") {
		t.Errorf("conn = %q", got[1].Conn)
	}
	if got[1].At == "" {
		t.Error("missing timestamp")
	}

	if n := len(listHistory(t, bin, env, "--limit", "2")); n != 2 {
		t.Errorf("--limit 2 gave %d", n)
	}
	if s := listHistory(t, bin, env, "--search", "INSERT"); len(s) != 1 || s[0].Rows != 2 {
		t.Errorf("--search INSERT = %+v", s)
	}
	if s := listHistory(t, bin, env, "--conn", "nope"); len(s) != 0 {
		t.Errorf("--conn nope = %+v", s)
	}
	if s := listHistory(t, bin, env, "--conn", got[0].Conn); len(s) != 4 {
		t.Errorf("--conn %s gave %d", got[0].Conn, len(s))
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(stateDir(t, env), "tobira", "history.db"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("history.db mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestHistoryRedactsSecrets(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	db := filepath.Join(t.TempDir(), "h.db")
	q := "select 'postgres://u:s3cretpw@h/db' as dsn"
	if _, code := run(t, bin, env, "query", "sqlite:"+db, q); code != 0 {
		t.Fatalf("query exit %d", code)
	}
	got := listHistory(t, bin, env)
	if len(got) != 1 || strings.Contains(got[0].Query, "s3cretpw") || !strings.Contains(got[0].Query, "xxxxx") {
		t.Fatalf("entry = %+v, want a masked password", got)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir(t, env), "tobira", "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("s3cretpw")) {
		t.Error("the password is in the history file's raw bytes")
	}
}

func TestHistoryFailureIsInvisible(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "h.db")
	args := []string{"query", "sqlite:" + db, "select 1 as one", "--format", "json"}

	good := isolatedEnv(t)
	wantOut, wantErr, wantCode := runBoth(t, bin, good, args...)

	// XDG_STATE_HOME points at a regular file, so the history directory
	// cannot be created.
	bad := isolatedEnv(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, kv := range bad {
		if strings.HasPrefix(kv, "XDG_STATE_HOME=") {
			bad[i] = "XDG_STATE_HOME=" + blocker
		}
	}
	gotOut, gotErr, gotCode := runBoth(t, bin, bad, args...)
	if gotOut != wantOut || gotErr != wantErr || gotCode != wantCode {
		t.Errorf("output differs with unwritable history:\nwant %q %q %d\ngot  %q %q %d",
			wantOut, wantErr, wantCode, gotOut, gotErr, gotCode)
	}
}

func historyPath(t *testing.T, env []string) string {
	return filepath.Join(stateDir(t, env), "tobira", "history.db")
}

func withEnv(env []string, kv ...string) []string {
	return append(append([]string{}, env...), kv...)
}

func writeConfig(t *testing.T, env []string, body string) []string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return withEnv(env, "TOBIRA_CONFIG="+cfg)
}

func TestHistoryRedactsResolvedPasswordFromRef(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "h.db")
	env := writeConfig(t, isolatedEnv(t), "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+db+"\n    password: env:HIST_PW\n")
	env = withEnv(env, "HIST_PW=zebra-correct-horse")
	if _, _, code := runBoth(t, bin, env, "query", "lite", "select 'zebra-correct-horse' as v"); code != 0 {
		t.Fatalf("query exit %d", code)
	}
	assertMasked(t, bin, env, "zebra-correct-horse")
}

func TestHistoryRedactsEnvVarPassword(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "h.db")
	env := writeConfig(t, isolatedEnv(t), "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+db+"\n")
	env = withEnv(env, "TOBIRA_CONN_LITE_PASSWORD=quokka-battery-staple")
	if _, _, code := runBoth(t, bin, env, "query", "lite", "select 'quokka-battery-staple' as v"); code != 0 {
		t.Fatalf("query exit %d", code)
	}
	assertMasked(t, bin, env, "quokka-battery-staple")
}

func assertMasked(t *testing.T, bin string, env []string, secret string) {
	t.Helper()
	got := listHistory(t, bin, env)
	if len(got) != 1 || strings.Contains(got[0].Query, secret) || !strings.Contains(got[0].Query, "xxxxx") {
		t.Fatalf("entry = %+v, want a masked password", got)
	}
	raw, err := os.ReadFile(historyPath(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Error("the password is in the history file's raw bytes")
	}
	// The sqlite WAL or journal could hold it too.
	if matches, _ := filepath.Glob(historyPath(t, env) + "-*"); len(matches) > 0 {
		for _, m := range matches {
			if b, _ := os.ReadFile(m); bytes.Contains(b, []byte(secret)) {
				t.Errorf("the password is in %s", m)
			}
		}
	}
}

// seedHistory bulk-inserts n rows ("seed 1".."seed n") through the real
// sqlite driver, after history.Open created the schema.
func seedHistory(t *testing.T, path string, n int) {
	t.Helper()
	ctx := context.Background()
	s, err := history.Open(ctx, driver.Default, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	c, err := driver.Default.Open(ctx, driver.ConnConfig{Driver: "sqlite", DSN: "sqlite:" + path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rs, err := c.Query(ctx, driver.Query{Text: "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?) " +
		"INSERT INTO history (at, conn, source, query, ok, rows, ms) SELECT '2026-01-01T00:00:00Z', 'seed', 'cli', 'seed ' || i, 1, 0, 0 FROM n",
		Params: []driver.Value{driver.Int(int64(n))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver.Collect(ctx, rs, 0); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryRetentionTrimsToLast10000(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	seedHistory(t, path, 10005)
	ctx := context.Background()
	s, err := history.Open(ctx, driver.Default, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Record(ctx, history.Entry{Conn: "c", Source: history.SourceCLI, Query: "newest", OK: true}, driver.ConnConfig{}); err != nil {
		t.Fatal(err)
	}
	all, err := s.List(ctx, history.Filter{Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10000 || all[0].Query != "newest" {
		t.Fatalf("kept %d entries, newest %q; want 10000 and newest first", len(all), all[0].Query)
	}
	// 10006 rows existed; ids 1..6 are gone and "seed 7" is the oldest kept.
	if oldest := all[len(all)-1]; oldest.Query != "seed 7" {
		t.Errorf("oldest kept = %q, want seed 7", oldest.Query)
	}
}

func TestHistoryLimitZeroMeansAll(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	seedHistory(t, historyPath(t, env), 150)
	if n := len(listHistory(t, bin, env)); n != 100 {
		t.Errorf("default gave %d, want 100", n)
	}
	if n := len(listHistory(t, bin, env, "--limit", "0")); n != 150 {
		t.Errorf("--limit 0 gave %d, want all 150", n)
	}
}

func TestHistoryStatePathWithSpecialCharacters(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	weird := filepath.Join(t.TempDir(), "st a?te#1%25")
	for i, kv := range env {
		if strings.HasPrefix(kv, "XDG_STATE_HOME=") {
			env[i] = "XDG_STATE_HOME=" + weird
		}
	}
	db := filepath.Join(t.TempDir(), "h.db")
	if _, code := run(t, bin, env, "query", "sqlite:"+db, "select 1"); code != 0 {
		t.Fatalf("query exit %d", code)
	}
	if got := listHistory(t, bin, env); len(got) != 1 {
		t.Fatalf("got %+v, want one entry", got)
	}
	if _, err := os.Stat(filepath.Join(weird, "tobira", "history.db")); err != nil {
		t.Errorf("history file not at the literal path: %v", err)
	}
}

func TestHistoryRecordsExplainAndRefusals(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	db := filepath.Join(t.TempDir(), "h.db")
	dsn := "sqlite:" + db
	run(t, bin, env, "query", dsn, "create table t (id integer primary key)")
	run(t, bin, env, "query", dsn, "select * from t", "--explain")
	if _, code := run(t, bin, env, "query", dsn, "delete from t", "--read-only"); code != 4 {
		t.Fatalf("refusal exit %d, want 4", code)
	}
	got := listHistory(t, bin, env)
	if len(got) != 3 || got[0].Query != "delete from t" || got[0].OK || got[1].Query != "select * from t" {
		t.Fatalf("entries = %+v", got)
	}
}

func TestHistoryClear(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	db := filepath.Join(t.TempDir(), "h.db")
	dsn := "sqlite:" + db
	for _, q := range []string{"select 'needle-zx81' as a", "select 2", "select 3"} {
		if _, code := run(t, bin, env, "query", dsn, q); code != 0 {
			t.Fatalf("query %q: %d", q, code)
		}
	}

	// No terminal and no --yes: refused, nothing removed.
	if out, errOut, code := runBoth(t, bin, env, "history", "clear"); code != 1 || !strings.Contains(errOut, "--yes") {
		t.Fatalf("clear without --yes: exit %d, stdout %q, stderr %q; want exit 1 naming --yes", code, out, errOut)
	}
	if n := len(listHistory(t, bin, env)); n != 3 {
		t.Fatalf("a refused clear removed entries: %d left, want 3", n)
	}

	out, code := run(t, bin, env, "history", "clear", "--yes", "--search", "NEEDLE-zx81")
	if code != 0 || strings.TrimSpace(out) != "removed 1 entry" {
		t.Fatalf("filtered clear: exit %d, out %q", code, out)
	}
	left := listHistory(t, bin, env)
	if len(left) != 2 || left[0].Query != "select 3" || left[1].Query != "select 2" {
		t.Fatalf("after the filtered clear: %+v", left)
	}
	raw, err := os.ReadFile(historyPath(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("needle-zx81")) {
		t.Error("the cleared query is still in the file's raw bytes (not compacted)")
	}

	out, code = run(t, bin, env, "history", "clear", "--yes")
	if code != 0 || strings.TrimSpace(out) != "removed 2 entries" {
		t.Fatalf("clear all: exit %d, out %q", code, out)
	}
	if n := len(listHistory(t, bin, env)); n != 0 {
		t.Errorf("%d entries left after clear, want none", n)
	}
	out, code = run(t, bin, env, "history", "clear", "--yes")
	if code != 0 || strings.TrimSpace(out) != "removed 0 entries" {
		t.Errorf("clear of an empty log: exit %d, out %q", code, out)
	}

	// A missing file is an empty history: nothing to remove, and no file made.
	fresh := isolatedEnv(t)
	out, code = run(t, bin, fresh, "history", "clear", "--yes")
	if code != 0 || strings.TrimSpace(out) != "removed 0 entries" {
		t.Errorf("clear without a file: exit %d, out %q", code, out)
	}
	if _, err := os.Stat(historyPath(t, fresh)); err == nil {
		t.Error("clearing created the history file")
	}
	if _, errOut, code := runBoth(t, bin, env, "history", "clear", "--yes", "--source", "nope"); code != 1 || !strings.Contains(errOut, "--source must be one of") {
		t.Errorf("bad --source: exit %d, stderr %q", code, errOut)
	}
}

func TestHistoryMaxEntriesConfig(t *testing.T) {
	bin := buildBinary(t)
	env := writeConfig(t, isolatedEnv(t), "history:\n  max_entries: 3\n")
	db := filepath.Join(t.TempDir(), "h.db")
	dsn := "sqlite:" + db
	for _, q := range []string{"select 1", "select 2", "select 3", "select 4", "select 5"} {
		if _, code := run(t, bin, env, "query", dsn, q); code != 0 {
			t.Fatalf("query %q: %d", q, code)
		}
	}
	got := listHistory(t, bin, env, "--limit", "0")
	if len(got) != 3 || got[0].Query != "select 5" || got[2].Query != "select 3" {
		t.Fatalf("kept %+v, want the last 3", got)
	}
	// A config that cannot load warns once and lists with the default cap.
	if _, errOut, code := runBoth(t, bin, writeConfig(t, isolatedEnv(t), "history:\n  max_entries: 0\n"), "history"); code != 0 || !strings.Contains(errOut, "history.max_entries: must be from 1 to 1000000") {
		t.Errorf("max_entries 0: exit %d, stderr %q; want exit 0 and a warning naming the key", code, errOut)
	}
}
