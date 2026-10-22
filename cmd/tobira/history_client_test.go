package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/history"
)

// oldSchema is the history table before the client column existed.
const oldSchema = `CREATE TABLE history (
  id INTEGER PRIMARY KEY,
  at TEXT NOT NULL,
  conn TEXT NOT NULL,
  source TEXT NOT NULL,
  query TEXT NOT NULL,
  ok INTEGER NOT NULL,
  rows INTEGER NOT NULL,
  ms INTEGER NOT NULL
)`

// seedOldHistory builds a history file with the old schema and two rows.
func seedOldHistory(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := driver.Default.Open(ctx, driver.ConnConfig{Driver: "sqlite", DSN: "sqlite:" + path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, q := range []string{
		oldSchema,
		"INSERT INTO history (at, conn, source, query, ok, rows, ms) VALUES ('2026-01-01T00:00:00Z', 'old', 'cli', 'select 1', 1, 1, 3)",
		"INSERT INTO history (at, conn, source, query, ok, rows, ms) VALUES ('2026-01-02T00:00:00Z', 'old', 'tui', 'select 2', 1, 1, 4)",
	} {
		rs, err := c.Query(ctx, driver.Query{Text: q})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := driver.Collect(ctx, rs, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func clientColumnPresent(t *testing.T, path string) bool {
	t.Helper()
	ctx := context.Background()
	c, err := driver.Default.Open(ctx, driver.ConnConfig{Driver: "sqlite", DSN: "sqlite:" + path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rs, err := c.Query(ctx, driver.Query{Text: "SELECT COUNT(*) FROM pragma_table_info('history') WHERE name = 'client'"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := rows[0][0].AsInt()
	return n == 1
}

func TestHistoryMigratesOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	seedOldHistory(t, path)
	ctx := context.Background()
	// Twice: the second Open finds the column and must not fail.
	for i := range 2 {
		s, err := history.Open(ctx, driver.Default, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		got, err := s.List(ctx, history.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Query != "select 2" || got[1].Query != "select 1" || got[0].Client != "" {
			t.Fatalf("open %d: rows = %+v", i, got)
		}
		_ = s.Close()
	}
	if !clientColumnPresent(t, path) {
		t.Fatal("client column missing after Open")
	}
	s, err := history.Open(ctx, driver.Default, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	err = s.Record(ctx, history.Entry{Conn: "c", Source: history.SourceMCP, Query: "select 3", OK: true, Client: "claude-code 2.1"}, driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(ctx, history.Filter{Limit: 1})
	if len(got) != 1 || got[0].Client != "claude-code 2.1" || got[0].Source != history.SourceMCP {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestHistoryConcurrentOpenOfOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	seedOldHistory(t, path)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := history.Open(context.Background(), driver.Default, path)
			if err == nil {
				// Every opener, including the ones that lost the race, keeps the client.
				err = s.Record(context.Background(), history.Entry{Conn: "c", Source: history.SourceMCP, Query: "q", OK: true, Client: "racer"}, driver.ConnConfig{})
				_ = s.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("open %d: %v", i, err)
		}
	}
	if !clientColumnPresent(t, path) {
		t.Fatal("client column missing")
	}
	s, err := history.Open(context.Background(), driver.Default, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, _ := s.List(context.Background(), history.Filter{Source: history.SourceMCP})
	if len(got) != n {
		t.Fatalf("%d mcp rows, want %d", len(got), n)
	}
	for _, e := range got {
		if e.Client != "racer" {
			t.Errorf("client = %q, want racer", e.Client)
		}
	}
}

// seedMixed records one entry per source, the MCP ones with a client.
func seedMixed(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	s, err := history.Open(ctx, driver.Default, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i, e := range []history.Entry{
		{Conn: "a", Source: history.SourceCLI, Query: "q-cli", OK: true},
		{Conn: "a", Source: history.SourceTUI, Query: "q-tui", OK: true},
		{Conn: "a", Source: history.SourceAI, Query: "q-ai", OK: true},
		{Conn: "a", Source: history.SourceMCP, Query: "q-mcp1", OK: true, Client: "claude-code 2.1"},
		{Conn: "b", Source: history.SourceMCP, Query: "q-mcp2", OK: false, Client: "cursor"},
		{Conn: "b", Source: history.SourceMCP, Query: "q-mcp3", OK: true},
	} {
		e.At = at.Add(time.Duration(i) * time.Minute)
		if err := s.Record(ctx, e, driver.ConnConfig{}); err != nil {
			t.Fatal(err)
		}
	}
}

func queriesOf(rows []map[string]any) []string {
	var q []string
	for _, r := range rows {
		q = append(q, r["query"].(string))
	}
	return q
}

func jsonRows(t *testing.T, out string) []map[string]any {
	t.Helper()
	if strings.TrimSpace(out) == "[]" {
		return nil
	}
	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return doc.Rows
}

func TestHistorySourceFilter(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	seedMixed(t, historyPath(t, env))

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--source", "cli"}, "q-cli"},
		{[]string{"--source", "tui"}, "q-tui"},
		{[]string{"--source", "ai"}, "q-ai"},
		{[]string{"--source", "mcp"}, "q-mcp3 q-mcp2 q-mcp1"},
		{[]string{"--mcp"}, "q-mcp3 q-mcp2 q-mcp1"},
		{[]string{"--mcp", "--source", "mcp"}, "q-mcp3 q-mcp2 q-mcp1"},
		{[]string{"--source", "mcp", "--conn", "b"}, "q-mcp3 q-mcp2"},
		{[]string{"--source", "mcp", "--search", "mcp1"}, "q-mcp1"},
		{nil, "q-mcp3 q-mcp2 q-mcp1 q-ai q-tui q-cli"},
	} {
		out, code := run(t, bin, env, append([]string{"history", "--format", "json"}, tt.args...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d", tt.args, code)
		}
		if got := strings.Join(queriesOf(jsonRows(t, out)), " "); got != tt.want {
			t.Errorf("%v: got %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestHistorySourceUsageErrors(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	for _, args := range [][]string{
		{"--source", "web"},
		{"--source", ""},
		{"--source", "CLI"},
		{"--mcp", "--source", "cli"},
	} {
		so, se, code := runBoth(t, bin, env, append([]string{"history"}, args...)...)
		if code != 1 || so != "" || !strings.Contains(se, "--source") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit 1 naming --source", args, code, so, se)
		}
	}
}

func TestHistoryClientColumn(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	seedMixed(t, historyPath(t, env))

	// json always carries client, null when there is none.
	out, _ := run(t, bin, env, "history", "--format", "json")
	byQuery := map[string]any{}
	for _, r := range jsonRows(t, out) {
		c, has := r["client"]
		if !has {
			t.Fatalf("row without a client key: %v", r)
		}
		byQuery[r["query"].(string)] = c
	}
	if byQuery["q-mcp1"] != "claude-code 2.1" || byQuery["q-mcp3"] != nil || byQuery["q-cli"] != nil {
		t.Errorf("clients = %v", byQuery)
	}
	nd, _ := run(t, bin, env, "history", "--format", "ndjson", "--source", "cli")
	if !strings.Contains(nd, `"client":null`) {
		t.Errorf("ndjson = %q, want client null", nd)
	}

	// table and csv: client appears for an MCP listing, or any listing with MCP rows.
	for _, args := range [][]string{{"--mcp"}, {"--source", "mcp"}, {}} {
		for _, f := range []string{"table", "csv"} {
			out, _ := run(t, bin, env, append([]string{"history", "--format", f}, args...)...)
			if !strings.Contains(out, "client") || !strings.Contains(out, "cursor") {
				t.Errorf("%s %v: no client column:\n%s", f, args, out)
			}
		}
	}
	// No MCP rows in the listing: no client column.
	out, _ = run(t, bin, env, "history", "--format", "table", "--source", "cli")
	if strings.Contains(out, "client") {
		t.Errorf("client column on a cli listing:\n%s", out)
	}
	// An MCP listing with nothing in it still prints nothing.
	env2 := isolatedEnv(t)
	if out, code := run(t, bin, env2, "history", "--mcp", "--format", "table"); code != 0 || out != "" {
		t.Errorf("empty: exit %d, out %q", code, out)
	}
}

func TestHistoryOldDatabaseThroughCLI(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	seedOldHistory(t, historyPath(t, env))
	out, code := run(t, bin, env, "history", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	rows := jsonRows(t, out)
	if len(rows) != 2 || rows[0]["client"] != nil {
		t.Fatalf("rows = %v", rows)
	}
	if out, code := run(t, bin, env, "history", "--mcp", "--format", "json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("--mcp on an old db: exit %d, out %q", code, out)
	}
}

// A pre-M5 history.db the user cannot write still lists, through the library
// and through the CLI; the client column just reads as empty.
func TestHistoryReadOnlyOldDatabaseStillLists(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0400 file")
	}
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := historyPath(t, env)
	seedOldHistory(t, path)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := history.Open(ctx, driver.Default, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := s.List(ctx, history.Filter{})
	_ = s.Close()
	if err != nil || len(got) != 2 {
		t.Fatalf("List = %+v, %v; want the 2 old rows", got, err)
	}
	out, code := run(t, bin, env, "history", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if rows := jsonRows(t, out); len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	if clientColumnPresent(t, path) {
		t.Error("a read-only file gained a column")
	}
}
