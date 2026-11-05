package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/federate"
)

// The engine's own tests use fakes; these run it against the real sqlite
// driver the binary links: sqlite files as sources and the scratch database
// in between.

// fileOpener stands in for the session manager: it opens each named sqlite
// file once, writable, through the binary's registry.
type fileOpener struct {
	paths map[string]string
	mu    sync.Mutex
	conns map[string]driver.Conn
}

func newFileOpener(t *testing.T, paths map[string]string) *fileOpener {
	t.Helper()
	o := &fileOpener{paths: paths, conns: map[string]driver.Conn{}}
	t.Cleanup(func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		for _, c := range o.conns {
			_ = c.Close()
		}
	})
	return o
}

func (o *fileOpener) Open(ctx context.Context, name string) (driver.Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c, ok := o.conns[name]; ok {
		return c, nil
	}
	path, ok := o.paths[name]
	if !ok {
		return nil, errors.New("unknown connection " + name)
	}
	c, err := driver.Default.Open(ctx, driver.ConnConfig{Name: name, DSN: "sqlite:" + path})
	if err != nil {
		return nil, err
	}
	o.conns[name] = c
	return c, nil
}

// execSQLite runs statements on a sqlite file, creating it when missing.
func execSQLite(t *testing.T, path string, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	conn, err := driver.Default.Open(ctx, driver.ConnConfig{DSN: "sqlite:" + path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, s := range stmts {
		rs, err := conn.Query(ctx, driver.Query{Text: s})
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if _, _, err := driver.Collect(ctx, rs, 0); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// countRows is how many rows a table of a sqlite file has.
func countRows(t *testing.T, path, table string) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := driver.Default.Open(ctx, driver.ConnConfig{DSN: "sqlite:" + path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	rs, err := conn.Query(ctx, driver.Query{Text: "select count(*) from " + table, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := rows[0][0].AsInt()
	return n
}

// shopAndCRM seeds an orders file and a users file and returns an opener
// that knows them as "shop" and "crm".
func shopAndCRM(t *testing.T) (*fileOpener, string) {
	t.Helper()
	dir := t.TempDir()
	orders, users := filepath.Join(dir, "shop.db"), filepath.Join(dir, "crm.db")
	execSQLite(t, orders,
		"create table orders (id integer primary key, user_id integer, total real, paid integer)",
		"insert into orders values (1, 1, 10.5, 1), (2, 1, 4.5, 1), (3, 2, 3.0, 1), (4, 2, 99.0, 0)",
	)
	execSQLite(t, users,
		"create table users (id integer primary key, name text)",
		"insert into users values (1, 'alice'), (2, 'bob')",
	)
	return newFileOpener(t, map[string]string{"shop": orders, "crm": users}), orders
}

// scratchFiles lists the scratch databases left in the temp directory.
func scratchFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tobira-federate-*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func rowText(rows []driver.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(r))
		for j, v := range r {
			cells[j] = v.String()
		}
		out[i] = strings.Join(cells, "|")
	}
	return out
}

func TestFederateJoinsTwoSQLiteFiles(t *testing.T) {
	open, _ := shopAndCRM(t)
	before := len(scratchFiles(t))
	st, err := federate.Parse(strings.Join([]string{
		"-- @source orders = shop: select user_id, total from orders where paid = 1",
		"-- @source users = crm: select id, name from users",
		"select u.name, count(*) as n, sum(o.total) as spent",
		"from orders o join users u on u.id = o.user_id",
		"group by u.name",
		"order by spent desc",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := federate.Run(context.Background(), open, st, federate.Options{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rowText(res.Rows), []string{"alice|2|15", "bob|1|3"}; strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("rows %v, want %v", got, want)
	}
	if len(res.Columns) != 3 || res.Columns[0].Name != "name" || res.Columns[1].Name != "n" || res.Columns[2].Name != "spent" {
		t.Errorf("columns %+v", res.Columns)
	}
	for _, c := range res.Columns {
		if c.Source != nil || c.Key {
			t.Errorf("column %s offers inline editing of the scratch table", c.Name)
		}
	}
	if n, _ := res.Rows[0][1].AsInt(); n != 2 {
		t.Errorf("count column is %v (%s), want the integer 2", res.Rows[0][1].Any(), res.Rows[0][1].Type())
	}
	if f, ok := res.Rows[0][2].AsFloat(); !ok || f != 15 {
		t.Errorf("sum column is %v (%s), want the float 15", res.Rows[0][2].Any(), res.Rows[0][2].Type())
	}
	wantNotes := "source orders (shop): 3 rows;source users (crm): 2 rows"
	if got := strings.Join(res.Stats.Warnings, ";"); got != wantNotes {
		t.Errorf("warnings %q, want %q", got, wantNotes)
	}
	if res.Stats.RowsAffected != -1 || res.Stats.Truncated || res.Stats.Duration <= 0 {
		t.Errorf("stats %+v", res.Stats)
	}
	if len(res.Sources) != 2 || res.Sources[0].Rows != 3 || res.Sources[1].Rows != 2 || res.Sources[0].Err != nil {
		t.Errorf("sources %+v", res.Sources)
	}
	if after := len(scratchFiles(t)); after != before {
		t.Errorf("%d scratch files in the temp directory, %d before the run: the scratch database was not removed", after, before)
	}
}

func TestFederateCapsSourcesAndTheResult(t *testing.T) {
	open, _ := shopAndCRM(t)
	st, err := federate.Parse("-- @source o = shop: select id from orders order by id\nselect id from o order by id")
	if err != nil {
		t.Fatal(err)
	}

	res, err := federate.Run(context.Background(), open, st, federate.Options{MaxSourceRows: 3, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowText(res.Rows); strings.Join(got, ";") != "1;2" {
		t.Errorf("rows %v, want 1 and 2", got)
	}
	if !res.Stats.Truncated {
		t.Error("the final result was cut to --limit but is not marked truncated")
	}
	if s := res.Sources[0]; s.Rows != 3 || !s.Truncated {
		t.Errorf("source run %+v, want 3 rows and more existed", s)
	}
	if want := "source o (shop): 3 rows, more existed"; len(res.Stats.Warnings) != 1 || res.Stats.Warnings[0] != want {
		t.Errorf("warnings %v, want %q", res.Stats.Warnings, want)
	}
}

func TestFederateRefusesAFinalWrite(t *testing.T) {
	open, _ := shopAndCRM(t)
	before := len(scratchFiles(t))
	st, err := federate.Parse("-- @source o = shop: select id from orders\ndelete from o")
	if err != nil {
		t.Fatal(err)
	}
	res, err := federate.Run(context.Background(), open, st, federate.Options{})
	if err == nil || !strings.Contains(err.Error(), "must be a read") {
		t.Fatalf("got %v, want the final statement refused as a write", err)
	}
	if res == nil || len(res.Sources) != 1 || res.Sources[0].Rows != 4 {
		t.Errorf("the staged source is still reported: %+v", res)
	}
	if after := len(scratchFiles(t)); after != before {
		t.Errorf("%d scratch files, %d before: the scratch database survived the refusal", after, before)
	}
}

func TestFederateSourcesAreReadOnlyOnAWritableConnection(t *testing.T) {
	open, orders := shopAndCRM(t)
	st, err := federate.Parse("-- @source o = shop: delete from orders\nselect 1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = federate.Run(context.Background(), open, st, federate.Options{})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("got %v, want a read-only refusal", err)
	}
	var se *federate.SourceError
	if !errors.As(err, &se) || se.Alias != "o" || se.Conn != "shop" {
		t.Errorf("got %v, want a SourceError for o on shop", err)
	}
	if !strings.Contains(err.Error(), "sources are always read-only") {
		t.Errorf("message %q does not say sources are read-only", err)
	}
	if n := countRows(t, orders, "orders"); n != 4 {
		t.Errorf("orders has %d rows after the refused delete, want 4", n)
	}
}

// federateConfig writes a config naming the shop and crm files as
// connections, plus a target file (writable as target, read-only as
// rotarget) with an empty user_activity table, and returns an environment
// that uses it with the shop and target paths.
func federateConfig(t *testing.T, env []string) (useEnv []string, shop, target string) {
	t.Helper()
	dir := t.TempDir()
	shop, crm, target := filepath.Join(dir, "shop.db"), filepath.Join(dir, "crm.db"), filepath.Join(dir, "target.db")
	execSQLite(t, shop,
		"create table orders (id integer primary key, user_id integer, total real, paid integer)",
		"insert into orders values (1, 1, 10.5, 1), (2, 1, 4.5, 1), (3, 2, 3.0, 1), (4, 2, 99.0, 0)",
	)
	execSQLite(t, crm,
		"create table users (id integer primary key, name text)",
		"insert into users values (1, 'alice'), (2, 'bob')",
	)
	execSQLite(t, target, "create table user_activity (name text primary key, n integer, spent real)")
	cfg := filepath.Join(dir, "config.yaml")
	body := "connections:\n" +
		"  - name: shop\n    dsn: sqlite:" + shop + "\n" +
		"  - name: crm\n    dsn: sqlite:" + crm + "\n" +
		"  - name: target\n    dsn: sqlite:" + target + "\n" +
		"  - name: rotarget\n    dsn: sqlite:" + target + "\n    read_only: true\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return append(env, "TOBIRA_CONFIG="+cfg), shop, target
}

const federateQuery = "-- @source o = shop: select user_id, total from orders where paid = 1\n" +
	"-- @source u = crm: select id, name from users\n" +
	"select u.name, count(*) as n, sum(o.total) as spent from o join u on u.id = o.user_id group by u.name order by spent desc"

func TestEndToEndFederate(t *testing.T) {
	bin := buildBinary(t)
	env, shop, _ := federateConfig(t, isolatedEnv(t))

	out, code := run(t, bin, env, "federate", federateQuery, "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stdout %q)", code, out)
	}
	var doc struct {
		Rows  []map[string]any `json:"rows"`
		Stats struct {
			Warnings []string `json:"warnings"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if len(doc.Rows) != 2 || doc.Rows[0]["name"] != "alice" || doc.Rows[0]["n"] != float64(2) || doc.Rows[0]["spent"] != float64(15) ||
		doc.Rows[1]["name"] != "bob" || doc.Rows[1]["n"] != float64(1) {
		t.Errorf("rows = %v, want alice 2 15 then bob 1 3", doc.Rows)
	}
	if len(doc.Stats.Warnings) != 2 || !strings.HasPrefix(doc.Stats.Warnings[0], "source o (shop): 3 rows") {
		t.Errorf("warnings = %v, want one line per source", doc.Stats.Warnings)
	}

	// --explain contacts nothing and prints the plan.
	out, code = run(t, bin, env, "federate", federateQuery, "--explain", "--format", "json")
	if code != 0 || !strings.Contains(out, `"plan":`) || !strings.Contains(out, "source o: shop, read-only") {
		t.Errorf("explain: exit %d out %q", code, out)
	}

	// --output writes the rows to a file and prints only the stderr line.
	target := filepath.Join(t.TempDir(), "weekly.csv")
	stdout, stderr, code := runBoth(t, bin, env, "federate", federateQuery, "--output", target)
	if code != 0 || stdout != "" || !strings.HasPrefix(stderr, "wrote 2 rows to ") {
		t.Errorf("output: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if b, err := os.ReadFile(target); err != nil || strings.Count(string(b), "\n") != 3 {
		t.Errorf("csv file: %v %q", err, b)
	}

	// Refusals: a final write and a source write exit 4; an unknown connection
	// and a bad directive are usage errors.
	if _, code := run(t, bin, env, "federate", "-- @source o = shop: select id from orders\ndelete from o"); code != 4 {
		t.Errorf("final write exit = %d, want 4", code)
	}
	if _, code := run(t, bin, env, "federate", "-- @source o = shop: delete from orders\nselect 1"); code != 4 {
		t.Errorf("source write exit = %d, want 4", code)
	}
	if n := countRows(t, shop, "orders"); n != 4 {
		t.Errorf("orders has %d rows after the refused delete, want 4", n)
	}
	if _, code := run(t, bin, env, "federate", "-- @source o = nope: select 1\nselect 1"); code != 1 {
		t.Errorf("unknown connection exit = %d, want 1", code)
	}
	if _, code := run(t, bin, env, "federate", "select 1"); code != 1 {
		t.Errorf("no directive exit = %d, want 1", code)
	}
	if _, code := run(t, bin, env, "federate", federateQuery, "--max-source-rows", "0"); code != 1 {
		t.Errorf("--max-source-rows 0 exit = %d, want 1", code)
	}

	// History: each source on its own connection, the whole statement under
	// federated.
	conns := map[string]int{}
	for _, h := range listHistory(t, bin, env, "--limit", "0") {
		conns[h.Conn]++
	}
	if conns["federated"] < 3 || conns["shop"] < 3 || conns["crm"] < 2 {
		t.Errorf("history connections %v: want the whole statement under federated and each source on its own connection", conns)
	}
}

// A statement with a @sink writes its rows into the sink collection with
// --apply, prints them with --dry-run, and is refused without either; a
// read-only or missing target is refused before anything is staged.
func TestEndToEndFederateSink(t *testing.T) {
	bin := buildBinary(t)
	env, _, target := federateConfig(t, isolatedEnv(t))
	sinkQuery := "-- @sink target: user_activity\n" + federateQuery

	if _, code := run(t, bin, env, "federate", sinkQuery); code != 1 {
		t.Errorf("sink without --apply exit = %d, want 1", code)
	}
	if _, code := run(t, bin, env, "federate", federateQuery, "--apply"); code != 1 {
		t.Errorf("--apply without a sink exit = %d, want 1", code)
	}
	if _, code := run(t, bin, env, "federate", sinkQuery, "--apply", "--dry-run"); code != 1 {
		t.Errorf("--apply with --dry-run exit = %d, want 1", code)
	}

	stdout, stderr, code := runBoth(t, bin, env, "federate", sinkQuery, "--dry-run", "--format", "json")
	if code != 0 || !strings.Contains(stdout, `"name": "alice"`) || !strings.Contains(stderr, "dry run: 2 rows would be inserted into target") {
		t.Errorf("dry run: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if n := countRows(t, target, "user_activity"); n != 0 {
		t.Errorf("user_activity has %d rows after a dry run, want 0", n)
	}

	stdout, stderr, code = runBoth(t, bin, env, "federate", sinkQuery, "--apply")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "inserted 2 rows into target") {
		t.Errorf("apply: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if n := countRows(t, target, "user_activity"); n != 2 {
		t.Errorf("user_activity has %d rows after --apply, want 2", n)
	}

	// The explain names the sink and writes nothing.
	out, code := run(t, bin, env, "federate", sinkQuery, "--explain", "--format", "json")
	if code != 0 || !strings.Contains(out, "sink: insert the result into target: user_activity") {
		t.Errorf("explain with a sink: exit %d out %q", code, out)
	}

	// Refusals before staging: a read-only target exits 4, a missing
	// collection 3, and the first apply's rows are all that was written.
	if _, code := run(t, bin, env, "federate", "-- @sink rotarget: user_activity\n"+federateQuery, "--apply"); code != 4 {
		t.Errorf("read-only sink exit = %d, want 4", code)
	}
	if _, code := run(t, bin, env, "federate", "-- @sink target: nope\n"+federateQuery, "--apply"); code != 3 {
		t.Errorf("missing sink collection exit = %d, want 3", code)
	}
	if n := countRows(t, target, "user_activity"); n != 2 {
		t.Errorf("user_activity has %d rows after the refusals, want 2", n)
	}
	// Inserting the same names again violates the key: the transaction
	// rolls back and nothing more is written.
	if _, stderr, code := runBoth(t, bin, env, "federate", sinkQuery, "--apply"); code == 0 || !strings.Contains(stderr, "inserted 0 of 2 rows") {
		t.Errorf("duplicate apply: exit %d stderr %q, want a failure that wrote nothing", code, stderr)
	}
	if n := countRows(t, target, "user_activity"); n != 2 {
		t.Errorf("user_activity has %d rows after the failed apply, want 2", n)
	}
}

func TestFederateNamesTheFailingSource(t *testing.T) {
	open, _ := shopAndCRM(t)
	st, err := federate.Parse("-- @source o = shop: select * from missing\n-- @source u = crm: select id from users\nselect 1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = federate.Run(context.Background(), open, st, federate.Options{})
	if err == nil || !strings.HasPrefix(err.Error(), "source o on shop: ") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("got %v, want the failure named after source o on shop", err)
	}
}
