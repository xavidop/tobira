//go:build integration

package clickhouse

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/xavidop/tobira/driver"
)

// writeProbes are the write and escape families a read-only call must
// refuse. server reports whether readonly=1 alone refuses the probe (code
// 164); KILL QUERY is the one family only the allow-list stops.
var writeProbes = []struct {
	name, text string
	server     bool
}{
	{"set readonly", "SET readonly = 0", true},
	{"set setting", "SET max_threads = 1", true},
	{"settings clause readonly", "SELECT * FROM tobira_people SETTINGS readonly = 0", true},
	{"settings clause other", "SELECT 1 SETTINGS max_threads = 1", true},
	{"insert", "INSERT INTO tobira_people (id, name, email, score, active, created_at) VALUES (900, 'x', 'x', 0, true, now())", true},
	{"insert select", "INSERT INTO tobira_people (id, name, email, score, active, created_at) SELECT 901, 'x', 'x', 0, true, now()", true},
	{"insert into file", "INSERT INTO FUNCTION file('tobira_probe.csv', 'CSV', 'a Int32') SELECT 1", true},
	{"insert into url", "INSERT INTO FUNCTION url('http://127.0.0.1:1/x', 'CSV', 'a Int32') SELECT 1", true},
	{"insert into s3", "INSERT INTO FUNCTION s3('http://127.0.0.1:1/bucket/k.csv', 'CSV', 'a Int32') SELECT 1", true},
	{"insert into remote", "INSERT INTO FUNCTION remote('127.0.0.1', tobira.tobira_people) (id, name, email, score, active, created_at) SELECT 902, 'x', 'x', 0, true, now()", true},
	{"read file", "SELECT * FROM file('tobira_probe.csv', 'CSV', 'a Int32')", true},
	{"read url", "SELECT * FROM url('http://127.0.0.1:1/x', 'CSV', 'a Int32')", true},
	{"read s3", "SELECT * FROM s3('http://127.0.0.1:1/bucket/k.csv', 'CSV', 'a Int32')", true},
	{"read remote", "SELECT * FROM remote('127.0.0.1', tobira.tobira_people)", true},
	{"cte reading url", "WITH (SELECT count() FROM url('http://127.0.0.1:1/x', 'CSV', 'a Int32')) AS n SELECT n", true},
	{"alter delete", "ALTER TABLE tobira_people DELETE WHERE id = 1", true},
	{"alter update", "ALTER TABLE tobira_people UPDATE name = 'x' WHERE id = 1", true},
	{"alter add column", "ALTER TABLE tobira_people ADD COLUMN probe Int8", true},
	{"lightweight delete", "DELETE FROM tobira_people WHERE id = 1", true},
	{"lightweight update", "UPDATE tobira_people SET name = 'x' WHERE id = 1", true},
	{"create table", "CREATE TABLE tobira_probe (a Int8) ENGINE = Memory", true},
	{"create database", "CREATE DATABASE tobira_probe_db", true},
	{"drop view", "DROP VIEW tobira_people_view", true},
	{"truncate", "TRUNCATE TABLE tobira_big", true},
	{"rename", "RENAME TABLE tobira_big TO tobira_big2", true},
	{"system flush logs", "SYSTEM FLUSH LOGS", true},
	{"system stop merges", "SYSTEM STOP MERGES tobira_people", true},
	{"optimize", "OPTIMIZE TABLE tobira_people FINAL", true},
	{"kill query", "KILL QUERY WHERE query_id = 'tobira-none'", false},
	{"kill mutation", "KILL MUTATION WHERE mutation_id = 'tobira-none'", false},
	{"two statements", "SELECT 1; INSERT INTO tobira_people (id) VALUES (903)", true},
	{"create temporary table", "CREATE TEMPORARY TABLE tobira_tmp (a Int8)", true},
	{"read executable", "SELECT * FROM executable('ls', 'TSV', 'a String')", true},
	{"read mysql", "SELECT * FROM mysql('127.0.0.1:1', 'd', 't', 'u', 'p')", true},
	{"read postgresql", "SELECT * FROM postgresql('127.0.0.1:1', 'd', 't', 'u', 'p')", true},
	{"read jdbc", "SELECT * FROM jdbc('jdbc:x://h', 'd', 't')", true},
	{"create dictionary", "CREATE DICTIONARY tobira_dict (id UInt64) PRIMARY KEY id SOURCE(NULL()) LAYOUT(FLAT()) LIFETIME(0)", true},
	{"system reload dictionary", "SYSTEM RELOAD DICTIONARY tobira_dict", true},
}

// state fingerprints everything a probe could change.
func state(t *testing.T) string {
	t.Helper()
	db := admin(t)
	var mutations uint64
	if err := db.QueryRow(ctxT(t), "SELECT count() FROM system.mutations WHERE database = 'tobira'").Scan(&mutations); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("people %s; big %s; view %s; probe %s; mutations %d",
		tableState(t, db, "tobira.tobira_people"), tableState(t, db, "tobira.tobira_big"),
		tableState(t, db, "tobira.tobira_people_view"), tableState(t, db, "tobira.tobira_probe"), mutations)
}

func TestReadOnlyProbes(t *testing.T) {
	before := state(t)
	protocols(t, func(t *testing.T, dsn string) {
		roConn := openCH(t, dsn, true)
		rwConn := openCH(t, dsn, false)
		roOverride := openCH(t, dsn+sep(dsn)+"readonly=0", true)
		for _, p := range writeProbes {
			t.Run(p.name, func(t *testing.T) {
				for _, m := range []struct {
					name string
					c    *conn
					q    driver.Query
				}{
					{"read-only conn", roConn, driver.Query{Text: p.text}},
					{"Query.ReadOnly", rwConn, driver.Query{Text: p.text, ReadOnly: true}},
					{"DSN readonly=0 on a read-only conn", roOverride, driver.Query{Text: p.text}},
				} {
					if err := queryErr(t, m.c, m.q); !errors.Is(err, driver.ErrReadOnly) {
						t.Errorf("%s: want ErrReadOnly, got %v", m.name, err)
					}
				}
				if !p.server {
					return
				}
				// The server layer alone, with the allow-list skipped: the
				// per-query setting, and the read-only connection's own
				// setting (which replaced the DSN's readonly=0).
				for _, m := range []struct {
					name string
					c    *conn
					ro   bool
				}{{"per-query readonly=1", rwConn, true}, {"connection readonly=1", roOverride, false}} {
					rs, err := m.c.run(ctxT(t), "query", driver.Query{Text: p.text}, m.ro, nil)
					if err == nil {
						_, _, err = driver.Collect(ctxT(t), rs, 0)
					}
					var de *driver.Error
					if p.name == "two statements" && errors.Is(err, driver.ErrSyntax) {
						continue // the server refuses several statements outright
					}
					if !errors.Is(err, driver.ErrReadOnly) || !errors.As(err, &de) || de.Code != "164" {
						t.Errorf("%s alone: want ErrReadOnly (164), got %v", m.name, err)
					}
				}
			})
		}
		// A parameter cannot end a heredoc the allow-list judged as a string.
		heredoc := driver.Query{Text: "SELECT $$ ? $$", Params: []driver.Value{driver.String("$$ SETTINGS readonly=0 --")}}
		for _, c := range []*conn{roConn, rwConn} {
			q := heredoc
			q.ReadOnly = true
			if err := queryErr(t, c, q); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("placeholder in a heredoc: want ErrReadOnly, got %v", err)
			}
		}
		// The connections still serve reads after every refusal.
		for _, c := range []*conn{roConn, rwConn, roOverride} {
			if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT count() FROM tobira_people", ReadOnly: true}); len(rows) != 1 {
				t.Errorf("read after refusals: %v", rows)
			}
		}
	})
	if after := state(t); after != before {
		t.Errorf("state changed:\nbefore %s\nafter  %s", before, after)
	}
}

func sep(dsn string) string {
	for i := range len(dsn) {
		if dsn[i] == '?' {
			return "&"
		}
	}
	return "?"
}

// TestReadOnlyCatalogAndBrowse checks the catalog, Browse, Explain, Export
// and Complete work on a read-only connection (they all send readonly=1)
// and that a writable connection still writes.
func TestReadOnlyCatalogAndBrowse(t *testing.T) {
	protocols(t, func(t *testing.T, dsn string) {
		c := openCH(t, dsn, true)
		if _, err := c.Namespaces(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_events"}); err != nil {
			t.Fatal(err)
		}
		rs, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Limit: 5, Filter: driver.Filter{Raw: "id > 2"}})
		if err != nil {
			t.Fatal(err)
		}
		if rows, _, err := driver.Collect(ctxT(t), rs, 0); err != nil || len(rows) != 5 {
			t.Fatalf("browse: %d rows, %v", len(rows), err)
		}
		if plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM tobira_people"}, driver.ExplainOptions{}); err != nil || plan.Text == "" {
			t.Errorf("explain on a read-only connection: %v", err)
		}
		var buf bytes.Buffer
		if err := c.Export(ctxT(t), driver.Query{Text: "SELECT id FROM tobira_people ORDER BY id LIMIT 2"}, "csv", &buf); err != nil || buf.String() != "id\n1\n2\n" && buf.String() != "\"id\"\n1\n2\n" {
			t.Errorf("export on a read-only connection: %q, %v", buf.String(), err)
		}
		if got, err := c.Complete(ctxT(t), driver.CompletionRequest{Text: "select * from tobira_pe", Cursor: 23}); err != nil || len(got) == 0 {
			t.Errorf("complete on a read-only connection: %v, %v", got, err)
		}
		rs, err = c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Filter: driver.Filter{Raw: "id IN (SELECT a FROM url('http://127.0.0.1:1/x', 'CSV', 'a Int32'))"}})
		if err == nil {
			_, _, err = driver.Collect(ctxT(t), rs, 0)
		}
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("raw filter reading url(): want ErrReadOnly, got %v", err)
		}

		rw := openCH(t, dsn, false)
		db := admin(t)
		mustExec(t, db, "CREATE TABLE IF NOT EXISTS tobira.tobira_rw (a Int32) ENGINE = Memory")
		t.Cleanup(func() { mustExec(t, db, "DROP TABLE IF EXISTS tobira.tobira_rw") })
		rows, st, cols := collect(t, rw, driver.Query{Text: "INSERT INTO tobira_rw VALUES (1)"})
		if len(rows) != 0 || len(cols) != 0 || st.RowsAffected != -1 {
			t.Errorf("insert result: rows %v cols %v stats %+v", rows, cols, st)
		}
		if got := tableState(t, db, "tobira.tobira_rw"); got[:6] == "0 rows" {
			t.Errorf("writable connection did not write: %s", got)
		}
	})
}

// TestServerReadOnlyUsers covers users whose profile already sets readonly
// (1 or 2): the server refuses any change to readonly, so the driver must
// not send it (nor, over HTTP, the cancel setting), and the server's own
// read-only mode keeps the guarantee.
func TestServerReadOnlyUsers(t *testing.T) {
	before := state(t)
	for _, u := range []struct{ user, pass string }{{"ro1", "ro1-pass"}, {"ro2", "ro2-pass"}} {
		for _, p := range []struct{ name, addr, params string }{{"native", nativeAddr, ""}, {"http", httpAddr, "?protocol=http"}} {
			t.Run(u.user+"/"+p.name, func(t *testing.T) {
				dsn := "clickhouse://" + u.user + ":" + u.pass + "@" + p.addr + "/" + testDB + p.params
				for _, ro := range []bool{true, false} {
					c := openCH(t, dsn, ro)
					if !c.serverRO {
						t.Fatalf("read-only=%v: the server's readonly was not detected", ro)
					}
					if _, err := c.Namespaces(ctxT(t)); err != nil {
						t.Errorf("read-only=%v: namespaces: %v", ro, err)
					}
					if _, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people"}); err != nil {
						t.Errorf("read-only=%v: describe: %v", ro, err)
					}
					rs, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Limit: 3, ReadOnly: true})
					if err == nil {
						_, _, err = driver.Collect(ctxT(t), rs, 0)
					}
					if err != nil {
						t.Errorf("read-only=%v: browse: %v", ro, err)
					}
					if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT count() FROM tobira_people", ReadOnly: true}); len(rows) != 1 {
						t.Errorf("read-only=%v: count: %v", ro, rows)
					}
					if _, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT 1", ReadOnly: true}, driver.ExplainOptions{}); err != nil {
						t.Errorf("read-only=%v: explain: %v", ro, err)
					}
					var buf bytes.Buffer
					if err := c.Export(ctxT(t), driver.Query{Text: "SELECT 1 AS x"}, "csv", &buf); err != nil {
						t.Errorf("read-only=%v: export: %v", ro, err)
					}
					if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT label FROM things", Namespace: "tobira_ns", ReadOnly: true}); len(rows) != 1 {
						t.Errorf("read-only=%v: namespaced query: %v", ro, rows)
					}
					for _, w := range []string{
						"INSERT INTO tobira_people (id, name, email, score, active, created_at) VALUES (904, 'x', 'x', 0, true, now())",
						"ALTER TABLE tobira_people DELETE WHERE id = 1",
						"SELECT * FROM url('http://127.0.0.1:1/x', 'CSV', 'a Int32')",
					} {
						// Even with the allow-list skipped, the server refuses:
						// ro1 holds the grants, so its readonly=1 refuses
						// (READONLY); ro2 lacks the table function grants.
						rs, err := c.run(ctxT(t), "query", driver.Query{Text: w}, ro, nil)
						if err == nil {
							_, _, err = driver.Collect(ctxT(t), rs, 0)
						}
						if u.user == "ro1" && !errors.Is(err, driver.ErrReadOnly) {
							t.Errorf("read-only=%v: %q: want ErrReadOnly, got %v", ro, w, err)
						}
						if u.user == "ro2" && !errors.Is(err, driver.ErrReadOnly) && !errors.Is(err, driver.ErrPermission) {
							t.Errorf("read-only=%v: %q: want ErrReadOnly or ErrPermission, got %v", ro, w, err)
						}
					}
					// On the read-only path the driver itself refuses a
					// table function, before the server sees it.
					for _, q := range tableFunctionReads {
						if err := queryErr(t, c, driver.Query{Text: q, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
							t.Errorf("read-only=%v: %q: want ErrReadOnly from the driver, got %v", ro, q, err)
						}
					}
				}
			})
		}
	}
	if after := state(t); after != before {
		t.Errorf("state changed:\nbefore %s\nafter  %s", before, after)
	}
}

// tableFunctionReads are reads through table functions that reach outside
// the server (or its tables), as a read-only call would send them.
var tableFunctionReads = []string{
	"SELECT * FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String')",
	"SELECT * FROM tobira_people AS p JOIN url('http://127.0.0.1:8123/ping', 'CSV', 'a String') AS u ON 1",
	"SELECT * FROM file('x.csv', 'CSV', 'a String')",
	"SELECT id FROM tobira_people WHERE name IN (SELECT a FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String'))",
	"DESCRIBE TABLE url('http://127.0.0.1:8123/ping', 'CSV', 'a String')",
}

// TestServerReadOnly2WideGrants pins what readonly=2 allows a user whose
// grants cover table functions: the server itself runs url(), so the
// driver, which may not send readonly=1 to such a user, refuses every
// table function on the read-only path (ErrReadOnly), on every entry point.
// numbers() stays allowed, and writes are still the server's to refuse.
func TestServerReadOnly2WideGrants(t *testing.T) {
	before := state(t)
	for _, p := range []struct{ name, addr, params string }{{"native", nativeAddr, ""}, {"http", httpAddr, "?protocol=http"}} {
		t.Run(p.name, func(t *testing.T) {
			dsn := "clickhouse://ro2wide:ro2wide-pass@" + p.addr + "/" + testDB + p.params
			c := openCH(t, dsn, true)
			if !c.serverRO {
				t.Fatal("the server's readonly was not detected")
			}
			// The server layer alone lets the table function through.
			rs, err := c.run(ctxT(t), "query", driver.Query{Text: tableFunctionReads[0]}, true, nil)
			var rows []driver.Row
			if err == nil {
				rows, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if err != nil || len(rows) != 1 {
				t.Fatalf("readonly=2 with grants: the server ran url(): want 1 row, got %v, %v", rows, err)
			}
			for _, q := range tableFunctionReads {
				if err := queryErr(t, c, driver.Query{Text: q}); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("query %q: want ErrReadOnly, got %v", q, err)
				}
				if _, err := c.Explain(ctxT(t), driver.Query{Text: q}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("explain %q: want ErrReadOnly, got %v", q, err)
				}
				var buf bytes.Buffer
				if err := c.Export(ctxT(t), driver.Query{Text: q}, "csv", &buf); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("export %q: want ErrReadOnly, got %v", q, err)
				}
			}
			rs, err = c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{
				Filter: driver.Filter{Raw: "name IN (SELECT a FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String'))"}})
			if err == nil {
				_, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("browse filter with url(): want ErrReadOnly, got %v", err)
			}
			if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT count() FROM numbers(3)"}); len(rows) != 1 {
				t.Errorf("numbers(): %v", rows)
			}
			// A writable connection is not the read-only path: the server's
			// readonly=2 still refuses the write.
			rw := openCH(t, dsn, false)
			if err := queryErr(t, rw, driver.Query{Text: "ALTER TABLE tobira_people DELETE WHERE id = 1"}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("write as ro2wide: want ErrReadOnly, got %v", err)
			}
		})
	}
	if after := state(t); after != before {
		t.Errorf("state changed:\nbefore %s\nafter  %s", before, after)
	}
}

// bypassReads are table-function reads a lexical scan can miss, plus the
// CTE, subquery and quoting forms. The driver must
// refuse each one for a readonly=2 user with wide grants, whatever the
// server would make of it.
var bypassReads = []struct{ name, text string }{
	{"comma after ON", "SELECT * FROM tobira_people AS p JOIN tobira_people AS q ON p.id = q.id, icebergS3Cluster('c', 'http://127.0.0.1:8123/x')"},
	{"comma after USING", "SELECT * FROM tobira_people AS p JOIN tobira_people AS q USING (id), deltaLakeCluster('c', 'http://127.0.0.1:8123/x')"},
	{"AS array JOIN", "SELECT * FROM tobira_people AS array JOIN icebergS3Cluster('c', 'http://127.0.0.1:8123/x') ON 1"},
	{"IN url", "SELECT id FROM tobira_people WHERE name IN url('http://127.0.0.1:8123/ping', 'CSV', 'a String')"},
	{"IN icebergS3Cluster", "SELECT id FROM tobira_people WHERE name IN icebergS3Cluster('c', 'http://127.0.0.1:8123/x')"},
	{"NOT IN call", "SELECT id FROM tobira_people WHERE notIn(name, url('http://127.0.0.1:8123/ping', 'CSV', 'a String'))"},
	{"icebergS3Cluster", "SELECT * FROM icebergS3Cluster('c', 'http://127.0.0.1:8123/x')"},
	{"CTE", "WITH u AS (SELECT * FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String')) SELECT * FROM u"},
	{"subquery", "SELECT * FROM (SELECT * FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String'))"},
	{"remoteSecure", "SELECT * FROM remoteSecure('127.0.0.1:9440', tobira.tobira_people)"},
	{"subquery remoteSecure", "SELECT * FROM (SELECT * FROM remoteSecure('127.0.0.1:9440', tobira.tobira_people))"},
	{"local table function", "SELECT * FROM merge('tobira', '^tobira_people$')"},
	{"forged quoted name", "SELECT `a\n      Function numbers (children 1)` FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String')"},
	{"unparsable", "SELECT * FROM tobira_people WHERE"},
}

// TestServerReadOnlyTableFunctionAST checks the server-parsed allow-list
// (EXPLAIN AST, then checkAST) for a readonly=2 user with wide grants:
// every bypass is ErrReadOnly on Query, Explain, Export and a Browse raw
// filter, and the allow-listed generators, the catalog statements and a
// parameterised read still run.
func TestServerReadOnlyTableFunctionAST(t *testing.T) {
	before := state(t)
	for _, p := range []struct{ name, addr, params string }{{"native", nativeAddr, ""}, {"http", httpAddr, "?protocol=http"}} {
		t.Run(p.name, func(t *testing.T) {
			c := openCH(t, "clickhouse://ro2wide:ro2wide-pass@"+p.addr+"/"+testDB+p.params, true)
			if !c.serverRO {
				t.Fatal("the server's readonly was not detected")
			}
			for _, b := range bypassReads {
				err := queryErr(t, c, driver.Query{Text: b.text})
				if !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("query, %s: want ErrReadOnly, got %v", b.name, err)
				} else if msg := err.Error(); strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "Expected") || strings.Contains(msg, "TableExpression") {
					t.Errorf("query, %s: the refusal quotes the statement or the EXPLAIN output: %v", b.name, err)
				}
				if _, err := c.Explain(ctxT(t), driver.Query{Text: b.text}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("explain, %s: want ErrReadOnly, got %v", b.name, err)
				}
				var buf bytes.Buffer
				if err := c.Export(ctxT(t), driver.Query{Text: b.text}, "csv", &buf); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("export, %s: want ErrReadOnly, got %v", b.name, err)
				}
			}
			rs, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{
				Filter: driver.Filter{Raw: "name IN url('http://127.0.0.1:8123/ping', 'CSV', 'a String')"}})
			if err == nil {
				_, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("browse filter IN url(): want ErrReadOnly, got %v", err)
			}
			for _, a := range []struct {
				text   string
				params []driver.Value
				rows   int
			}{
				{text: "SELECT * FROM numbers(10)", rows: 10},
				{text: "SELECT * FROM zeros(2) AS z, numbers_mt(2) AS n", rows: 4},
				{text: "SELECT * FROM generateRandom('a Int8') LIMIT 3", rows: 3},
				{text: "SELECT * FROM values('a Int8', 1, 2)", rows: 2},
				{text: "SELECT * FROM null('a Int8')", rows: 0},
				{text: "SELECT id FROM tobira_people WHERE id IN (1, 2) OR id IN (SELECT number + 3 FROM numbers(1)) ORDER BY id", rows: 3},
				{text: "SELECT name FROM system.databases WHERE name = 'tobira'", rows: 1},
				{text: "SELECT count() FROM tobira_people WHERE id > ? AND name != ?", params: []driver.Value{driver.Int(0), driver.String("x\n Function url")}, rows: 1},
				{text: "SELECT 1;", rows: 1},
				{text: "SHOW TABLES LIKE 'tobira_people'", rows: 1},
				{text: "EXISTS TABLE tobira_people", rows: 1},
			} {
				rows, _, err := collectErr(t, c, driver.Query{Text: a.text, Params: a.params})
				if err != nil || len(rows) != a.rows {
					t.Errorf("%q: want %d rows, got %d, %v", a.text, a.rows, len(rows), err)
				}
			}
			if _, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT count() FROM numbers(10)"}, driver.ExplainOptions{}); err != nil {
				t.Errorf("explain numbers(10): %v", err)
			}
			var buf bytes.Buffer
			if err := c.Export(ctxT(t), driver.Query{Text: "SELECT number FROM numbers(2)"}, "csv", &buf); err != nil || buf.String() != "number\n0\n1\n" && buf.String() != "\"number\"\n0\n1\n" {
				t.Errorf("export numbers(2): %q, %v", buf.String(), err)
			}
			if rows, _, err := collectErr(t, c, driver.Query{Text: "DESCRIBE TABLE tobira_people"}); err != nil || len(rows) == 0 {
				t.Errorf("describe: %d rows, %v", len(rows), err)
			}
		})
	}
	if after := state(t); after != before {
		t.Errorf("state changed:\nbefore %s\nafter  %s", before, after)
	}
}

// collectErr is collect returning the error instead of failing the test.
func collectErr(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Row, driver.Stats, error) {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		return nil, driver.Stats{}, err
	}
	return driver.Collect(ctxT(t), rs, 0)
}

// TestServerReadOnlyDialect: when the DSN or the user's profile sets
// dialect to prql or kusto, the server parses neither the query nor the
// guard's EXPLAIN AST as ClickHouse SQL. The driver must fail closed:
// either Open fails (it does on 25.8: its probe is SQL) or every read is
// ErrReadOnly; and system.query_log shows that no query holding the
// marker ever ran.
func TestServerReadOnlyDialect(t *testing.T) {
	for _, u := range []struct{ name, user, pass, params string }{
		{"profile prql", "roprql", "roprql-pass", ""},
		{"profile kusto", "rokusto", "rokusto-pass", ""},
		{"dsn prql", "ro2wide", "ro2wide-pass", "allow_experimental_prql_dialect=1&dialect=prql"},
		{"dsn kusto", "ro2wide", "ro2wide-pass", "allow_experimental_kusto_dialect=1&dialect=kusto"},
	} {
		for _, p := range []struct{ name, addr, params string }{{"native", nativeAddr, ""}, {"http", httpAddr, "protocol=http"}} {
			t.Run(u.name+"/"+p.name, func(t *testing.T) {
				params := strings.Trim(u.params+"&"+p.params, "&")
				dsn := "clickhouse://" + u.user + ":" + u.pass + "@" + p.addr + "/" + testDB
				if params != "" {
					dsn += "?" + params
				}
				marker := fmt.Sprintf("tobira-dialect-%d", time.Now().UnixNano())
				defer func() {
					db := admin(t)
					mustExec(t, db, "SYSTEM FLUSH LOGS")
					var ran uint64
					if err := db.QueryRow(ctxT(t), "SELECT count() FROM system.query_log WHERE user = ? AND position(query, ?) > 0 "+
						"AND NOT startsWith(query, 'EXPLAIN') AND type IN ('QueryStart', 'QueryFinish')", u.user, marker).Scan(&ran); err != nil {
						t.Fatal(err)
					}
					if ran != 0 {
						t.Errorf("%d queries holding the marker ran as %s", ran, u.user)
					}
				}()
				// On 25.8 Open's own probe (SQL) already fails under these
				// dialects, so the connection never opens: fail closed.
				dc, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsn, ReadOnly: true})
				if err != nil {
					return
				}
				t.Cleanup(func() { _ = dc.Close() })
				c := dc.(*conn)
				if !c.serverRO {
					t.Fatal("the server's readonly was not detected")
				}
				for _, q := range []string{
					"SELECT '" + marker + "'",
					"SELECT a FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String') WHERE a != '" + marker + "'",
					"WITH '" + marker + "' AS m SELECT m",
				} {
					if err := queryErr(t, c, driver.Query{Text: q}); !errors.Is(err, driver.ErrReadOnly) {
						t.Errorf("%q: want ErrReadOnly, got %v", q, err)
					}
					var buf bytes.Buffer
					if err := c.Export(ctxT(t), driver.Query{Text: q}, "csv", &buf); !errors.Is(err, driver.ErrReadOnly) {
						t.Errorf("export %q: want ErrReadOnly, got %v", q, err)
					}
				}
			})
		}
	}
}

// TestServerReadOnlyDialectPerCall drives the guard itself under a foreign
// dialect: a connection that opened (ro2wide, readonly=2) gets dialect
// settings on the call's context, so the guard's EXPLAIN AST and the query
// would both be parsed as PRQL or KQL. The guard's EXPLAIN fails, so the
// read is ErrReadOnly and the query never runs.
func TestServerReadOnlyDialectPerCall(t *testing.T) {
	// Native only: over HTTP, clickhouse-go 2.49 keeps sockets open after a
	// per-call dialect breaks its own requests (goleak flags them). The
	// driver never puts caller settings on a call's context, and a profile
	// dialect fails Open (TestServerReadOnlyDialect), so that path is a
	// test artifact; the guard is the same code on both protocols.
	for _, d := range []struct{ name, allow string }{{"prql", "allow_experimental_prql_dialect"}, {"kusto", "allow_experimental_kusto_dialect"}} {
		for _, p := range []struct{ name, addr, params string }{{"native", nativeAddr, ""}} {
			t.Run(d.name+"/"+p.name, func(t *testing.T) {
				c := openCH(t, "clickhouse://ro2wide:ro2wide-pass@"+p.addr+"/"+testDB+p.params, true)
				marker := fmt.Sprintf("tobira-dialect-call-%d", time.Now().UnixNano())
				ctx := ch.Context(ctxT(t), ch.WithSettings(ch.Settings{d.allow: 1, "dialect": d.name}))
				for _, q := range []string{
					"SELECT '" + marker + "'",
					"SELECT a FROM url('http://127.0.0.1:8123/ping', 'CSV', 'a String') WHERE a != '" + marker + "'",
				} {
					rs, err := c.Query(ctx, driver.Query{Text: q})
					if err == nil {
						_, _, err = driver.Collect(ctx, rs, 0)
					}
					var de *driver.Error
					if !errors.Is(err, driver.ErrReadOnly) || !errors.As(err, &de) || !strings.Contains(err.Error(), "cannot check the statement") {
						t.Errorf("%q: want the guard's ErrReadOnly, got %v", q, err)
					}
				}
				db := admin(t)
				mustExec(t, db, "SYSTEM FLUSH LOGS")
				var ran uint64
				if err := db.QueryRow(ctxT(t), "SELECT count() FROM system.query_log WHERE user = 'ro2wide' AND position(query, ?) > 0 "+
					"AND NOT startsWith(query, 'EXPLAIN') AND type IN ('QueryStart', 'QueryFinish')", marker).Scan(&ran); err != nil {
					t.Fatal(err)
				}
				if ran != 0 {
					t.Errorf("%d queries holding the marker ran", ran)
				}
			})
		}
	}
}

// TestTableFunctionsSnapshot reads system.table_functions and checks two
// things. Every table function off the allow-list is refused in table
// position for a readonly=2 user with wide grants, so the allow-list really
// is the only way in. And the server's list matches testdata/
// table_functions.txt, so a ClickHouse upgrade that adds a table function
// fails here until someone reviews it (see the file's header).
func TestTableFunctionsSnapshot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "table_functions.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			want = append(want, l)
		}
	}
	db := admin(t)
	rows, err := db.Query(ctxT(t), "SELECT name FROM system.table_functions ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		var added, removed []string
		for _, n := range got {
			if !slices.Contains(want, n) {
				added = append(added, n)
			}
		}
		for _, n := range want {
			if !slices.Contains(got, n) {
				removed = append(removed, n)
			}
		}
		t.Errorf("system.table_functions changed: added %q, removed %q; review them (testdata/table_functions.txt header) and update the snapshot", added, removed)
	}
	for n := range tableFunctionsAllowed {
		if !slices.ContainsFunc(got, func(g string) bool { return strings.EqualFold(g, n) }) {
			t.Errorf("allow-listed %s is not a table function on this server", n)
		}
	}
	for _, p := range []struct{ name, addr, params string }{{"native", nativeAddr, ""}, {"http", httpAddr, "?protocol=http"}} {
		t.Run(p.name, func(t *testing.T) {
			c := openCH(t, "clickhouse://ro2wide:ro2wide-pass@"+p.addr+"/"+testDB+p.params, true)
			for _, n := range got {
				if tableFunctionsAllowed[strings.ToLower(n)] {
					continue
				}
				args := map[string]string{"view": "SELECT 1", "viewIfPermitted": "SELECT 1 ELSE null('a Int8')"}[n]
				if args == "" {
					args = "'x'"
				}
				for _, q := range []string{
					"SELECT * FROM `" + n + "`(" + args + ")",
					"SELECT * FROM tobira_people AS p, `" + n + "`(" + args + ") AS f",
				} {
					err := queryErr(t, c, driver.Query{Text: q})
					if !errors.Is(err, driver.ErrReadOnly) || !strings.Contains(err.Error(), " "+strings.ToLower(n)+"()") {
						t.Errorf("%q: want ErrReadOnly naming %s(), got %v", q, strings.ToLower(n), err)
					}
				}
			}
		})
	}
}
