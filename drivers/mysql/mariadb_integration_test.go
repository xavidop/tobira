//go:build integration

package mysql

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// MariaDB stores JSON as LONGTEXT with a json_valid check, so the People
// dataset's tags column reads as a String: a declared storage difference.
var mariaDBOverrides = map[string]driver.Type{"tags": driver.TypeString}

func TestMariaDBConformance(t *testing.T) {
	// Through the mariadb driver, which forces the flavor.
	runConformance(t, newFlavored(flavorMariaDB), seedConformanceDB(t, mariaDSN, mariaHostPort), mariaDBOverrides)
}

func TestMariaDBDetected(t *testing.T) {
	c := openMaria(t, false)
	if c.flavor != flavorMariaDB || c.detected != flavorMariaDB || !strings.Contains(c.version, "MariaDB") {
		t.Fatalf("flavor %v, detected %v, version %q", c.flavor, c.detected, c.version)
	}
	ro := openMaria(t, true)
	if err := ro.readOnlyRefusal(); err != nil {
		t.Errorf("read-only refused on %s: %v", ro.version, err)
	}
	u := openDSN(t, "mariadb://root:tobira@"+mariaHostPort+"/tobira", true)
	rows, _, _ := collect(t, u, driver.Query{Text: "SELECT COUNT(*) FROM tobira_people"})
	if len(rows) != 1 || rows[0][0].String() != fmt.Sprint(len(drivertest.People.Rows)) {
		t.Errorf("mariadb:// read-only count = %v", rows)
	}
	// flavor=mysql on MariaDB: writable works, read-only is off.
	if _, err := New().Open(ctxT(t), driver.ConnConfig{DSN: mariaDSN + "?flavor=mysql", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("flavor=mysql read-only on MariaDB: want ErrUnsupported, got %v", err)
	}
	// mariadb:// on MySQL is a forced flavor too.
	if _, err := New().Open(ctxT(t), driver.ConnConfig{DSN: "mariadb://root:tobira@" + testHostPort + "/tobira", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("mariadb:// read-only on MySQL: want ErrUnsupported, got %v", err)
	}
}

// mariaScratch copies tobira_people into a private MariaDB table.
func mariaScratch(t *testing.T, db *sql.DB) string {
	t.Helper()
	name := fmt.Sprintf("tobira_mscratch_%d", scratchSeq.Add(1))
	mustExec(t, db, "CREATE TABLE "+name+" (PRIMARY KEY (id)) AS SELECT * FROM tobira_people")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE IF EXISTS "+name) })
	return name
}

// seqState reads the sequence's next value without consuming one.
func seqState(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow("SELECT next_not_cached_value FROM tobira_seq").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMariaDBReadOnlyProbes runs the MySQL probes and MariaDB's own ways to
// write (sequences, RETURNING, compound statements, SET STATEMENT, EXECUTE
// IMMEDIATE, /*M! … */ comments) on a read-only connection and with the
// per-call flag. Each must be ErrReadOnly and change nothing.
func TestMariaDBReadOnlyProbes(t *testing.T) {
	db := mariaAdmin(t)
	tbl := mariaScratch(t, db)
	seq := seqState(t, db)
	probes := append(readOnlyProbeTexts(tbl),
		"SELECT NEXTVAL(tobira_seq)",
		"SELECT NEXT VALUE FOR tobira_seq",
		"SELECT SETVAL(tobira_seq, 1000)",
		"SELECT * FROM "+tbl+" FOR UPDATE",
		"SELECT * FROM "+tbl+" FOR UPDATE SKIP LOCKED",
		"DELETE FROM "+tbl+" RETURNING id",
		"INSERT INTO "+tbl+" (id, name, email) VALUES (1001, 'x', 'y') RETURNING id",
		"/*M! DELETE FROM "+tbl+" */",
		"SELECT 1 /*M!100000 , (SELECT COUNT(*) FROM "+tbl+") */",
		"ANALYZE SELECT * FROM "+tbl,
		"ANALYZE FORMAT=JSON DELETE FROM "+tbl,
		"BEGIN NOT ATOMIC DELETE FROM "+tbl+"; END",
		"IF 1 THEN DELETE FROM "+tbl+"; END IF",
		"SET STATEMENT max_statement_time=0 FOR DELETE FROM "+tbl,
		"EXECUTE IMMEDIATE 'DELETE FROM "+tbl+"'",
		"CREATE OR REPLACE TABLE "+tbl+" (a INT)",
		"CREATE SEQUENCE tobira_probe_seq",
		"BACKUP STAGE START",
	)
	requireProbesRefused(t, db, openMaria(t, true), openMaria(t, false), tbl, probes)
	if got := seqState(t, db); got != seq {
		t.Errorf("a probe moved the sequence: %d, was %d", got, seq)
	}
}

// TestMariaDBBehindTheAllowList skips the allow-list: MariaDB's read-only
// transaction and the post-check alone must refuse writes and escapes.
func TestMariaDBBehindTheAllowList(t *testing.T) {
	db := mariaAdmin(t)
	tbl := mariaScratch(t, db)
	before, seq := tableState(t, db, tbl), seqState(t, db)
	for _, readOnlyConn := range []bool{true, false} {
		c := openMaria(t, readOnlyConn)
		for _, text := range []string{
			"DELETE FROM " + tbl,
			"CREATE TABLE tobira_probe_new (a INT)",
			"DROP TABLE " + tbl,
			"COMMIT",
			"ROLLBACK",
			"SET SESSION TRANSACTION READ WRITE",
			"SELECT NEXTVAL(tobira_seq)",
			"SELECT * FROM " + tbl + " FOR UPDATE",
		} {
			rs, err := c.run(ctxT(t), driver.Query{Text: text}, true, nil)
			if err == nil {
				_, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("read-only conn %v: unguarded %q: want ErrReadOnly, got %v", readOnlyConn, text, err)
			}
			if got := tableState(t, db, tbl); got != before {
				t.Fatalf("unguarded %q changed the table: %s, was %s", text, got, before)
			}
			if got := tableState(t, db, "tobira_probe_new"); got != "missing" {
				t.Fatalf("unguarded %q created a table", text)
			}
		}
	}
	if got := seqState(t, db); got != seq {
		t.Errorf("the sequence moved: %d, was %d", got, seq)
	}
}

func TestMariaDBReadOnlyPostCheckFailsClosed(t *testing.T) {
	c := openMaria(t, true)
	c.sessionCheckSQL = "SELECT no_such_column"
	if err := queryErr(t, c, driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("want ErrReadOnly when the post-check cannot read the session, got %v", err)
	}
}

// TestMariaDBSQLModes: MSSQL mode reads [ ] as identifier quotes, so the
// guard's lexer would see a string where the server sees a name. Such
// sql_modes are refused on every checked path; ANSI and NO_BACKSLASH_ESCAPES
// are read by the lexer and work.
func TestMariaDBSQLModes(t *testing.T) {
	// Precondition: the mismatch is real. With no guard, MSSQL mode takes
	// [a'] as a name, where the lexer sees the start of a string.
	rw := openDSN(t, mariaDSN+"?sql_mode=%27MSSQL%27", false)
	rows, _, _ := collect(t, rw, driver.Query{Text: "SELECT 1 AS [a']"})
	if len(rows) != 1 {
		t.Fatalf("MSSQL mode precondition: %v", rows)
	}
	for _, mode := range []string{"MSSQL", "ORACLE", "POSTGRESQL"} {
		dsn := mariaDSN + "?sql_mode=%27" + mode + "%27"
		ro := openDSN(t, dsn, true)
		if err := queryErr(t, ro, driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), mode) {
			t.Errorf("%s: read-only query: want ErrUnsupported naming the mode, got %v", mode, err)
		}
		w := openDSN(t, dsn, false)
		if err := queryErr(t, w, driver.Query{Text: "SELECT 1", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s: per-call read-only: want ErrUnsupported, got %v", mode, err)
		}
		for _, readOnly := range []bool{true, false} {
			tx, err := w.Begin(ctxT(t), driver.TxOptions{ReadOnly: readOnly})
			if err == nil {
				_ = tx.Rollback(ctxT(t))
			}
			if !errors.Is(err, driver.ErrUnsupported) {
				t.Errorf("%s: Begin(read-only %v): want ErrUnsupported, got %v", mode, readOnly, err)
			}
		}
	}
	for _, mode := range []string{"ANSI", "NO_BACKSLASH_ESCAPES", "TRADITIONAL"} {
		ro := openDSN(t, mariaDSN+"?sql_mode=%27"+mode+"%27", true)
		if rows, _, _ := collect(t, ro, driver.Query{Text: `SELECT 'a\\'`}); len(rows) != 1 {
			t.Errorf("%s: %v", mode, rows)
		}
	}
	// Under NO_BACKSLASH_ESCAPES the backslash ends the string: the
	// statement after it is seen and refused.
	ro := openDSN(t, mariaDSN+"?sql_mode=%27NO_BACKSLASH_ESCAPES%27", true)
	if err := queryErr(t, ro, driver.Query{Text: `SELECT 'a\'; DELETE FROM tobira_people; -- '`}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("NO_BACKSLASH_ESCAPES: want ErrReadOnly, got %v", err)
	}
}

func TestMariaDBDescribe(t *testing.T) {
	c := openMaria(t, false)
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_orders"})
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]driver.Field{}
	for _, f := range s.Fields {
		fields[f.Name] = f
	}
	for name, want := range map[string]string{"total": "0.00", "note": "'n/a'", "created": "current_timestamp()", "doubled": "`total` * 2", "person_id": "", "ref": "", "id": ""} {
		if got := fields[name].Default; got != want {
			t.Errorf("%s default = %q, want %q", name, got, want)
		}
	}
	if !fields["id"].AutoIncrement || !fields["doubled"].Generated || fields["note"].Comment != "free text" {
		t.Errorf("fields = %+v", s.Fields)
	}
	for _, name := range []string{"ref", "ip"} {
		if fields[name].Type != driver.TypeString {
			t.Errorf("%s type = %v, want String", name, fields[name].Type)
		}
	}
	if len(s.PrimaryKey) != 1 || s.PrimaryKey[0] != "id" || s.Comment != "orders table" {
		t.Errorf("pk %v, comment %q", s.PrimaryKey, s.Comment)
	}
	idx := map[string]driver.Index{}
	for _, ix := range s.Indexes {
		idx[ix.Name] = ix
	}
	if ix := idx["tobira_orders_person_total"]; strings.Join(ix.Fields, ",") != "person_id,total" || ix.Unique {
		t.Errorf("composite index = %+v", ix)
	}
	if ix := idx["tobira_orders_note"]; !ix.Unique || ix.Method != "btree" {
		t.Errorf("unique index = %+v", ix)
	}
	if len(s.ForeignKeys) != 1 || s.ForeignKeys[0].Name != "fk_orders_person" || s.ForeignKeys[0].References.Name != "tobira_people" {
		t.Errorf("foreign keys = %+v", s.ForeignKeys)
	}
	rows, _, cols := collect(t, c, driver.Query{Text: "SELECT ref, ip FROM tobira_orders"})
	if len(rows) != 1 || rows[0][0].String() != "123e4567-e89b-12d3-a456-426614174000" || rows[0][1].String() != "::1" || cols[0].Type != driver.TypeString {
		t.Errorf("uuid/inet6 row = %v, cols %+v", rows, cols)
	}
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range nss {
		switch ns.Name {
		case "mysql", "information_schema", "performance_schema", "sys":
			t.Errorf("system database %q listed", ns.Name)
		}
	}
}

func TestMariaDBExplain(t *testing.T) {
	db := mariaAdmin(t)
	name := mariaScratch(t, db)
	before := tableState(t, db, name)
	c := openMaria(t, false)
	plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM " + name + " WHERE id = ?", Params: []driver.Value{driver.Int(3)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Root == nil || plan.Root.Label != "query_block" || !strings.Contains(plan.Text, "select_type") {
		t.Errorf("plan = %+v", plan)
	}
	if plan, err = c.Explain(ctxT(t), driver.Query{Text: "DELETE FROM " + name + " WHERE id = 1"}, driver.ExplainOptions{}); err != nil || plan.Root == nil {
		t.Errorf("DELETE plan = %+v, %v", plan, err)
	}
	for _, readOnlyConn := range []bool{true, false} {
		c := openMaria(t, readOnlyConn)
		if plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM " + name, ReadOnly: true}, driver.ExplainOptions{}); err != nil || plan.Root == nil {
			t.Errorf("read-only conn %v: Explain(SELECT) = %+v, %v", readOnlyConn, plan, err)
		}
		_, err := c.Explain(ctxT(t), driver.Query{Text: "DELETE FROM " + name, ReadOnly: true}, driver.ExplainOptions{})
		var de *driver.Error
		if !errors.Is(err, driver.ErrReadOnly) || !errors.As(err, &de) || de.Code != "1792" {
			t.Errorf("read-only conn %v: Explain(DELETE): want ErrReadOnly from the server (1792), got %v", readOnlyConn, err)
		}
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("Explain changed the table: %s, was %s", got, before)
	}
}

func TestMariaDBTx(t *testing.T) {
	db := mariaAdmin(t)
	name := mariaScratch(t, db)
	c := openMaria(t, false)
	tx := begin(t, c, driver.TxOptions{})
	for _, text := range []string{
		"SET STATEMENT max_statement_time=0 FOR CREATE TABLE tobira_tx_escape (a INT)",
		"BEGIN NOT ATOMIC COMMIT; END",
		"lbl: BEGIN NOT ATOMIC COMMIT; END",
		"CREATE OR REPLACE TABLE tobira_tx_escape (a INT)",
		"EXECUTE IMMEDIATE 'COMMIT'",
	} {
		if err := queryErr(t, tx, driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q in a Tx: want ErrUnsupported, got %v", text, err)
		}
	}
	if err := tx.Update(ctxT(t), driver.CollectionRef{Name: name}, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "age", Value: driver.Int(77)}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	var age int
	if err := db.QueryRow("SELECT age FROM " + name + " WHERE id = 1").Scan(&age); err != nil || age != 77 {
		t.Errorf("committed age = %d, %v", age, err)
	}
	// The post-check: a statement that ends the transaction kills the Tx.
	for _, tc := range []struct {
		readOnly bool
		text     string
		kind     error
	}{
		{false, "COMMIT", driver.ErrUnsupported},
		{false, "CREATE TABLE tobira_tx_escape (a INT)", driver.ErrUnsupported},
		{true, "COMMIT", driver.ErrReadOnly},
		{true, "SET SESSION TRANSACTION READ WRITE", driver.ErrReadOnly},
	} {
		tx := begin(t, c, driver.TxOptions{ReadOnly: tc.readOnly})
		rs, err := tx.query(ctxT(t), driver.Query{Text: tc.text}, nil)
		if err == nil {
			_, _, err = driver.Collect(ctxT(t), rs, 0)
		}
		if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), "roll back") {
			t.Errorf("ro %v %q: want %v (transaction ended), got %v", tc.readOnly, tc.text, tc.kind, err)
		}
		if err := queryErr(t, tx, driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrConnection) {
			t.Errorf("ro %v %q: a dead Tx must refuse later statements, got %v", tc.readOnly, tc.text, err)
		}
		_ = tx.Rollback(ctxT(t))
	}
	mustExec(t, db, "DROP TABLE IF EXISTS tobira_tx_escape")
	// Early close of a Tx result set uses KILL QUERY on MariaDB too.
	tx = begin(t, c, driver.TxOptions{ReadOnly: true})
	rs, err := tx.Query(ctxT(t), driver.Query{Text: "SELECT seq FROM seq_1_to_100000"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rs.Next(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := rs.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}
	if rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT 1"}); len(rows) != 1 {
		t.Errorf("Tx after early close: %v", rows)
	}
	_ = tx.Rollback(ctxT(t))
}

func TestMariaDBBrowseCompositeKeyset(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		if got := browsePairs(t, openMaria(t, readOnly)); fmt.Sprint(got) != fmt.Sprint(pairsKeys) {
			t.Errorf("read-only %v: browse = %v, want %v", readOnly, got, pairsKeys)
		}
	}
}

// TestMariaDBDiscardsSession: a read-only call's GET_LOCK lock and user
// variables do not reach the next call (its connection is discarded).
func TestMariaDBDiscardsSession(t *testing.T) {
	c := openMaria(t, true)
	collect(t, c, driver.Query{Text: "SELECT GET_LOCK('tobira_maria_lock', 0), @v := 1"})
	rows, _, _ := collect(t, c, driver.Query{Text: "SELECT IS_FREE_LOCK('tobira_maria_lock'), @v"})
	if len(rows) != 1 || rows[0][0].String() != "1" || !rows[0][1].IsNull() {
		t.Errorf("session state leaked: %v", rows)
	}
}

// TestForcedSingleStoreOnMariaDB runs the singlestore flavor's dialect
// (folded index rows, expanded keyset comparisons, START TRANSACTION for
// mutations, no Transactor, no read-only) against MariaDB.
func TestForcedSingleStoreOnMariaDB(t *testing.T) {
	dsn := mariaDSN + "?flavor=singlestore"
	if _, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsn, ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("read-only open: want ErrUnsupported, got %v", err)
	}
	c := openDSN(t, dsn, false)
	if c.flavor != flavorSingleStore || c.detected != flavorMariaDB {
		t.Fatalf("flavor %v, detected %v", c.flavor, c.detected)
	}
	if _, err := c.Begin(ctxT(t), driver.TxOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Begin: want ErrUnsupported, got %v", err)
	}
	if got := browsePairs(t, c); fmt.Sprint(got) != fmt.Sprint(pairsKeys) {
		t.Errorf("browse = %v, want %v", got, pairsKeys)
	}
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_orders"})
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]driver.Index{}
	for _, ix := range s.Indexes {
		idx[ix.Name] = ix
	}
	if strings.Join(s.PrimaryKey, ",") != "id" || strings.Join(idx["tobira_orders_person_total"].Fields, ",") != "person_id,total" || !idx["tobira_orders_note"].Unique {
		t.Errorf("pk %v, indexes %+v", s.PrimaryKey, s.Indexes)
	}
	db := mariaAdmin(t)
	ref := driver.CollectionRef{Name: mariaScratch(t, db)}
	if _, err := c.Insert(ctxT(t), ref, personRow(500, "forced@x")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	k := driver.Object{{Key: "id", Value: driver.Int(500)}}
	if err := c.Update(ctxT(t), ref, k, driver.Object{{Key: "age", Value: driver.Int(9)}}); err != nil {
		t.Errorf("Update: %v", err)
	}
	if err := c.Delete(ctxT(t), ref, k); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := c.Delete(ctxT(t), ref, k); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Delete again: want ErrNotFound, got %v", err)
	}
}

// TestMariaDBDetectionProbes: the MariaDB-only variable is what identifies a
// MariaDB whose version string was overridden; MySQL has neither probe
// variable.
func TestMariaDBDetectionProbes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		db            *sql.DB
		maria, memsql bool
	}{{"mariadb", mariaAdmin(t), true, false}, {"mysql", admin(t), false, false}} {
		probe := serverProbe(ctxT(t), tc.db)
		for v, want := range map[string]bool{mariaDBProbeVar: tc.maria, memsqlProbeVar: tc.memsql} {
			if got, err := probe(v); err != nil || got != want {
				t.Errorf("%s: @@%s exists = %v, %v; want %v", tc.name, v, got, err, want)
			}
		}
		// As if the version said MySQL 8.0: the probe still finds MariaDB.
		f, err := classifyFlavor(ctxT(t), "8.0.35", "", probe, true, nil)
		if want := map[bool]flavor{true: flavorMariaDB, false: flavorMySQL}[tc.maria]; err != nil || f != want {
			t.Errorf("%s with an overridden version: %v, %v; want %v", tc.name, f, err, want)
		}
	}
}

// TestMariaDBSessionVariableLayer checks the read-only session variable on
// its own: on a connection opened read-only, outside any transaction and
// with the lexer bypassed (a raw session, autocommit), MariaDB itself refuses
// writes with 1792.
func TestMariaDBSessionVariableLayer(t *testing.T) {
	db := mariaAdmin(t)
	tbl := mariaScratch(t, db)
	before, seq := tableState(t, db, tbl), seqState(t, db)
	c := openMaria(t, true)
	sc, err := c.db.Conn(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close() }()
	if _, err := sc.ExecContext(ctxT(t), "COMMIT"); err != nil { // no transaction is open
		t.Fatal(err)
	}
	var ro int64
	if err := sc.QueryRowContext(ctxT(t), "SELECT @@SESSION.transaction_read_only").Scan(&ro); err != nil || ro != 1 {
		t.Fatalf("session transaction_read_only = %d, %v", ro, err)
	}
	for _, text := range []string{
		"DELETE FROM " + tbl,
		"INSERT INTO " + tbl + " (id, name, email) VALUES (1002, 'x', 'y')",
		"SELECT NEXTVAL(tobira_seq)",
		"SELECT SETVAL(tobira_seq, 5000)",
		"CREATE TEMPORARY TABLE tobira_tmp (a INT)",
	} {
		_, err := sc.ExecContext(ctxT(t), text)
		var me *gomysql.MySQLError
		if !errors.As(err, &me) || me.Number != 1792 {
			t.Errorf("%q on the raw read-only session: want error 1792, got %v", text, err)
		}
	}
	discardConn(sc)
	if got := tableState(t, db, tbl); got != before {
		t.Errorf("the table changed: %s, was %s", got, before)
	}
	if got := seqState(t, db); got != seq {
		t.Errorf("the sequence moved: %d, was %d", got, seq)
	}
}

// TestForcedMySQLOnMariaDB: flavor=mysql on a MariaDB server still sends
// MariaDB's syntax for Explain (no FORMAT=TREE) and the catalog (no
// STATISTICS.EXPRESSION), since the server must parse it.
func TestForcedMySQLOnMariaDB(t *testing.T) {
	c := openDSN(t, mariaDSN+"?flavor=mysql", false)
	if c.flavor != flavorMySQL || c.detected != flavorMariaDB {
		t.Fatalf("flavor %v, detected %v", c.flavor, c.detected)
	}
	plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM tobira_people WHERE id = 1"}, driver.ExplainOptions{})
	if err != nil || plan.Root == nil || !strings.Contains(plan.Text, "select_type") {
		t.Errorf("Explain = %+v, %v", plan, err)
	}
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_orders"})
	if err != nil || len(s.Indexes) == 0 {
		t.Fatalf("Describe = %+v, %v", s, err)
	}
	for _, f := range s.Fields {
		if f.Name == "note" && f.Default != "'n/a'" {
			t.Errorf("note default = %q, want MariaDB's own quoting kept", f.Default)
		}
	}
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range nss {
		if ns.Name == "mysql" || ns.Name == "sys" {
			t.Errorf("system database %q listed", ns.Name)
		}
	}
}
