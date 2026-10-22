package libsql

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

var update = flag.Bool("update", false, "rewrite testdata/guard_corpus.txt from the current guards")

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// guardDecisions renders every lexical guard decision on text, in the same
// format as the sqlite driver's corpus.
func guardDecisions(text string) string {
	return "checkReadOnly=" + strconv.Quote(errText(checkReadOnly(text))) +
		" checkSingle=" + strconv.Quote(errText(checkSingle(text))) +
		" returnsRows=" + strconv.FormatBool(returnsRows(text))
}

// TestGuardCorpusMatchesSQLite pins the guard to the sqlite driver's.
// testdata/guard_corpus.txt is drivers/sqlite/testdata/guard_corpus.txt with
// the driver name in the messages changed from "sqlite" to "libsql": over
// 2,600 tricky inputs (quotes, comments, brackets, BOMs, Unicode, PRAGMA
// forms), every decision must be the one the sqlite driver makes.
func TestGuardCorpusMatchesSQLite(t *testing.T) {
	data, err := os.ReadFile("testdata/guard_corpus.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\t") {
			continue
		}
		text, err := strconv.Unquote(line)
		if err != nil {
			t.Fatalf("bad corpus line %q: %v", line, err)
		}
		n++
		got := "\t" + guardDecisions(text)
		out.WriteString(line + "\n" + got + "\n")
		if *update {
			continue
		}
		if !sc.Scan() || sc.Text() != got {
			t.Errorf("input %s:\n got %s\nwant %s", line, got, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 300 {
		t.Fatalf("corpus has %d inputs, want at least 300", n)
	}
	if *update {
		if err := os.WriteFile("testdata/guard_corpus.txt", []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readOnlyDenied are statements the read-only allow-list must refuse before
// anything reaches the server. The integration suite sends the same list to
// sqld on a read-only connection.
var readOnlyDenied = []string{
	// writes and DDL
	"insert into tobira_people(id,name,email) values (1000,'a','zz@x')",
	"update tobira_people set name = 'x'", "delete from tobira_people", "replace into tobira_people(id) values (1)",
	"drop table tobira_people", "create table tt(a)", "create temp table tt(a)", "alter table tobira_people add column z",
	"with c as (select 1) insert into tobira_people(id,name,email) select 1001,'b','b@x' from c",
	"with c as (select 1) delete from tobira_people",
	// ATTACH and friends, behind every prefix
	"ATTACH 'x.db' AS x", "  /* c */ attach database 'x.db' as x", "/**/ATTACH 'x.db' AS x", "-- c\n attach 'x.db' as x",
	"(ATTACH 'x.db' AS x)", "\vATTACH 'x.db' AS x", "\xEF\xBB\xBFATTACH 'x.db' AS x", "EXPLAIN ATTACH 'x.db' AS x",
	"EXPLAIN QUERY PLAN EXPLAIN ATTACH 'x.db' AS x", "DETACH x",
	// extension loading outlives the rollback
	"select load_extension('x')", `select "load_extension"('x')`, "SELECT LOAD_EXTENSION('x', 'init')",
	"VACUUM", "VACUUM INTO 'v.db'", "EXPLAIN VACUUM INTO 'v.db'", "REINDEX", "ANALYZE",
	// transaction control: ending the driver's wrapping transaction
	"COMMIT", "END", "ROLLBACK", "BEGIN", "SAVEPOINT s", "RELEASE s", "RELEASE SAVEPOINT s",
	// assigning pragmas, every lexical form
	"PRAGMA journal_mode=delete", "PRAGMA query_only=0", "PRAGMA query_only(0)", "pragma main.query_only = 0",
	`PRAGMA "query_only"=0`, `PRAGMA 'query_only'=0`, "PRAGMA `writable_schema`=1", "PRAGMA [journal_mode]='delete'",
	`PRAGMA "user_version"=7`, "PRAGMA user_version(5)", "PRAGMA user_version = 5;", "PRAGMA/**/user_version=5",
	"PRAGMA--x\nuser_version=5", `PRAGMA main . "user_version" = 5`, "PRAGMA foreign_keys=OFF",
	`PRAGMA "table_info"=tobira_people`, `PRAGMA "table_ınfo"(tobira_people)`,
	"EXPLAIN PRAGMA user_version=5", `explain query plan pragma "query_only"=0`, "explain explain pragma user_version=5",
	"\xEF\xBB\xBFPRAGMA user_version=5", "PRAGMA user_version\xEF\xBB\xBF=5",
	// argument-less pragmas outside the allow-list
	"PRAGMA optimize", "PRAGMA shrink_memory", "PRAGMA wal_checkpoint", "PRAGMA incremental_vacuum",
	"PRAGMA integrity_check(tobira_people)", "PRAGMA database_list(x)", "PRAGMA table_info(tobira_people) x",
	// unparseable starts
	`PRAGMA "unterminated=0`, "PRAGMA =1", "PRAGMA", "", "   ", "-- only a comment", "\xc2\xa0SELECT 1", "1",
	"EXPLAIN QUERY PRAGMA user_version=5", "EXPLAIN PLAN SELECT 1",
}

// readOnlyAllowed must pass the allow-list.
var readOnlyAllowed = []string{
	"SELECT 1", "select * from tobira_people order by id", "  /* c */ select count(*) from tobira_people;",
	"WITH c AS (SELECT 1 AS n) SELECT n FROM c", "VALUES (1), (2)", "EXPLAIN SELECT 1",
	"EXPLAIN QUERY PLAN SELECT * FROM tobira_people", "\xEF\xBB\xBFSELECT 1",
	"PRAGMA table_info(tobira_people)", `PRAGMA "table_info"(tobira_people)`, `PRAGMA main."table_info"(tobira_people)`,
	"PRAGMA main.table_info('tobira_people');", "PRAGMA index_list(tobira_people)", "PRAGMA foreign_key_list(tobira_people)",
	"PRAGMA database_list", "PRAGMA user_version", "PRAGMA table_list",
}

func TestReadOnlyGuard(t *testing.T) {
	for _, text := range readOnlyDenied {
		if err := readOnlyGuard(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q: got %v, want ErrReadOnly", text, err)
		}
	}
	for _, text := range readOnlyAllowed {
		if err := readOnlyGuard(text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
}

// The writable-CTE check only adds refusals: across the corpus it never
// allows what the sqlite allow-list refuses, and it refuses every WITH that
// hides a write.
func TestReadOnlyGuardOnlyAddsRefusals(t *testing.T) {
	data, err := os.ReadFile("testdata/guard_corpus.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "\t") {
			continue
		}
		text, err := strconv.Unquote(line)
		if err != nil {
			t.Fatal(err)
		}
		if checkReadOnly(text) != nil && readOnlyGuard(text) == nil {
			t.Errorf("%q: allowed by readOnlyGuard but refused by checkReadOnly", text)
		}
	}
	for _, text := range []string{
		"WITH c AS (SELECT 1) DELETE FROM t", "with c as (select 1) update t set a = 1",
		"WITH c AS (SELECT 1) INSERT INTO t SELECT * FROM c", "WITH c AS (SELECT 1) REPLACE INTO t VALUES (1)",
		"WITH c AS (SELECT 1) INSERT OR REPLACE INTO t VALUES (1)", "with/**/c as (select 1)/**/delete/**/from t",
		"WITH RECURSIVE c(x) AS (SELECT 1) DELETE FROM t WHERE a IN c", "EXPLAIN WITH c AS (SELECT 1) DELETE FROM t",
	} {
		if err := readOnlyGuard(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q: got %v, want ErrReadOnly", text, err)
		}
	}
	for _, text := range []string{
		"WITH c AS (SELECT 'delete' AS \"update\") SELECT * FROM c", "select replace(name, 'a', 'b') from t",
		"select replace (name, 'a', 'b') from t", "select updated_at, deleted from t", "select 1 -- delete\n",
		"select /* update */ 1",
	} {
		if err := readOnlyGuard(text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
}

func TestCheckSingle(t *testing.T) {
	tests := []struct {
		text string
		ok   bool
	}{
		{"select 1; select 2", false},
		{"select 1;", true},
		{"select 1; -- trailing", true},
		{"select 1; /* c */ ", true},
		{"select ';' ; ", true},
		{`select ";", 'a;b' as [;]`, true},
		{"select 1 /* ; select 2 */", true},
		{"select 1 -- ; select\n", true},
		{"select 1;\nselect 2", false},
		{"select 1; ;", false},
		{"select 1;\vPRAGMA user_version=5", false},
	}
	for _, tt := range tests {
		err := checkSingle(tt.text)
		if tt.ok != (err == nil) {
			t.Errorf("%q: got %v", tt.text, err)
		}
		if err != nil && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%q: kind %v, want ErrSyntax", tt.text, err)
		}
	}
}

func TestIsTxControl(t *testing.T) {
	for text, want := range map[string]bool{
		"COMMIT": true, "end transaction": true, "BEGIN IMMEDIATE": true, "rollback": true,
		"ROLLBACK TRANSACTION": true, "ROLLBACK TO s": false, "ROLLBACK TRANSACTION TO SAVEPOINT s": false,
		"; ;commit": true, "/* c */ begin": true, "select 1": false, "release s": false,
	} {
		if got := isTxControl(text); got != want {
			t.Errorf("isTxControl(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCheckExplain(t *testing.T) {
	for _, text := range []string{"select 1", "insert into t values (1)", "update t set a = 1", "delete from t",
		"replace into t values (1)", "with c as (select 1) delete from t", "PRAGMA table_info(t)"} {
		if err := checkExplain(text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
	for _, text := range []string{"PRAGMA user_version=5", "ATTACH 'x' AS x", "create table t(a)", "drop table t", "COMMIT", "VACUUM"} {
		if err := checkExplain(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q: got %v, want ErrReadOnly", text, err)
		}
	}
}

func TestLoadExtensionRefused(t *testing.T) {
	for _, text := range []string{
		"select load_extension('x')", "SELECT LOAD_EXTENSION ('x', 'init')", `select "load_extension"('x')`,
		"select [load_extension]('x')", "with c as (select load_extension('x')) select * from c",
		"select load_extension/**/('x')", "EXPLAIN SELECT load_extension('x')",
	} {
		if err := readOnlyGuard(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q: got %v, want ErrReadOnly", text, err)
		}
	}
	for _, text := range []string{"select 'load_extension(x)'", "select 1 -- load_extension('x')\n", "select load_extension_count from t"} {
		if err := readOnlyGuard(text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
}
