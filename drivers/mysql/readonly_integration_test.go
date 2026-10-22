//go:build integration

package mysql

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"context"

	"github.com/xavidop/tobira/driver"
)

// TestReadOnlyProbes runs statements that would write, end the READ ONLY
// transaction or escape it, on a read-only connection and with the per-call
// ReadOnly flag on a writable one. Each must be ErrReadOnly and leave the
// table (rows, checksum and columns) and the schema unchanged.
func TestReadOnlyProbes(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	requireProbesRefused(t, db, openMy(t, true), openMy(t, false), tbl, readOnlyProbeTexts(tbl))
}

// readOnlyProbeTexts are statements that write, or try to leave the
// read-only transaction, against the scratch table tbl.
func readOnlyProbeTexts(tbl string) []string {
	return []string{
		"CREATE TABLE tobira_probe_new (a INT)",
		"DROP TABLE " + tbl,
		"ALTER TABLE " + tbl + " ADD COLUMN z INT",
		"TRUNCATE TABLE " + tbl,
		"SET autocommit = 1",
		"LOCK TABLES " + tbl + " WRITE",
		"SELECT 1; DELETE FROM " + tbl,
		"/* x */ DELETE FROM " + tbl,
		"# x\nDELETE FROM " + tbl,
		"-- x\nDELETE FROM " + tbl,
		"/*!50000 DELETE FROM " + tbl + " */",
		"SELECT 1 /*!50000 , (SELECT COUNT(*) FROM " + tbl + ") */",
		"WITH c AS (SELECT 1) DELETE FROM " + tbl,
		"WITH c AS (SELECT 1) UPDATE " + tbl + " SET name = 'x'",
		"EXPLAIN ANALYZE DELETE FROM " + tbl,
		"DESC DELETE FROM " + tbl,
		"SELECT * FROM " + tbl + " INTO OUTFILE '/tmp/tobira_probe_out'",
		"SELECT * FROM " + tbl + " INTO DUMPFILE '/tmp/tobira_probe_dump'",
		"COMMIT",
		"ROLLBACK",
		"START TRANSACTION",
		"BEGIN",
		"SET SESSION TRANSACTION READ WRITE",
		"SET TRANSACTION READ WRITE",
		"INSERT INTO " + tbl + " (id, name, email) VALUES (1000, 'x', 'x@x')",
		"UPDATE " + tbl + " SET name = 'x'",
		"DELETE FROM " + tbl,
		"REPLACE INTO " + tbl + " (id, name, email) VALUES (1, 'x', 'x@x')",
		"CALL nope()",
		"DO RELEASE_ALL_LOCKS()",
		"PREPARE s FROM 'DELETE FROM " + tbl + "'",
		"HANDLER " + tbl + " OPEN",
	}
}

// requireProbesRefused runs every probe through a read-only connection (ro)
// and through Query.ReadOnly on a writable one (rw), and fails unless each
// is ErrReadOnly and the table, its columns and the database are unchanged.
func requireProbesRefused(t *testing.T, db *sql.DB, ro, rw *conn, tbl string, probes []string) {
	t.Helper()
	before, cols := tableState(t, db, tbl), columnNames(t, db, tbl)
	for _, p := range probes {
		for _, path := range []struct {
			name string
			c    *conn
			flag bool
		}{{"read-only conn", ro, false}, {"ReadOnly flag", rw, true}} {
			err := queryErr(t, path.c, driver.Query{Text: p, ReadOnly: path.flag})
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s: %q: want ErrReadOnly, got %v", path.name, p, err)
			}
			if got := tableState(t, db, tbl); got != before {
				t.Fatalf("%s: %q changed the table: %s, was %s", path.name, p, got, before)
			}
			if got := columnNames(t, db, tbl); got != cols {
				t.Fatalf("%s: %q changed the columns: %s, was %s", path.name, p, got, cols)
			}
			if got := tableState(t, db, "tobira_probe_new"); got != "missing" {
				t.Fatalf("%s: %q created a table", path.name, p)
			}
		}
	}
}

// TestReadOnlyAllowedStatements runs each allow-listed kind on a read-only
// connection.
func TestReadOnlyAllowedStatements(t *testing.T) {
	ro := openMy(t, true)
	for _, q := range []string{
		"SELECT COUNT(*) FROM tobira_people",
		"SELECT 1;",
		"WITH c AS (SELECT id FROM tobira_people) SELECT COUNT(*) FROM c",
		"VALUES ROW(1, 2)",
		"TABLE tobira_big",
		"SHOW TABLES",
		"DESCRIBE tobira_people",
		"DESC tobira_people name",
		"EXPLAIN SELECT * FROM tobira_people",
		"EXPLAIN ANALYZE SELECT * FROM tobira_people",
		"EXPLAIN FORMAT=JSON SELECT * FROM tobira_people",
		"SELECT /*+ MAX_EXECUTION_TIME(5000) */ COUNT(*) FROM tobira_people",
		"SELECT name INTO @tobira_v FROM tobira_people WHERE id = 1",
		"USE tobira_ns",
	} {
		if err := queryErr(t, ro, driver.Query{Text: q}); err != nil {
			t.Errorf("%q on a read-only connection: %v", q, err)
		}
	}
}

// TestReadOnlyLockingReads: the allow-list accepts locking reads, but MySQL
// refuses FOR UPDATE inside a READ ONLY transaction (1792), so it is
// ErrReadOnly; shared locks are allowed and end with the ROLLBACK.
func TestReadOnlyLockingReads(t *testing.T) {
	ro := openMy(t, true)
	err := queryErr(t, ro, driver.Query{Text: "SELECT * FROM tobira_people WHERE id = 1 FOR UPDATE"})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("FOR UPDATE: want ErrReadOnly, got %v", err)
	}
	for _, q := range []string{
		"SELECT * FROM tobira_people WHERE id = 1 FOR SHARE",
	} {
		if err := queryErr(t, ro, driver.Query{Text: q}); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
}

// TestReadOnlyTransactionLayer reaches the server through a statement the
// allow-list accepts: a stored function that deletes. The READ ONLY
// transaction must refuse it with 1792, mapped to ErrReadOnly.
func TestReadOnlyTransactionLayer(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	mustExec(t, db, "CREATE FUNCTION tobira_write_fn() RETURNS INT DETERMINISTIC MODIFIES SQL DATA BEGIN DELETE FROM "+tbl+"; RETURN 1; END")
	t.Cleanup(func() { mustExec(t, db, "DROP FUNCTION IF EXISTS tobira_write_fn") })
	before := tableState(t, db, tbl)
	for _, path := range []struct {
		name string
		c    *conn
		flag bool
	}{{"read-only conn", openMy(t, true), false}, {"ReadOnly flag", openMy(t, false), true}} {
		err := queryErr(t, path.c, driver.Query{Text: "SELECT tobira_write_fn()", ReadOnly: path.flag})
		var de *driver.Error
		if !errors.Is(err, driver.ErrReadOnly) || !errors.As(err, &de) || de.Code != "1792" {
			t.Errorf("%s: want ErrReadOnly with code 1792, got %v", path.name, err)
		}
		if got := tableState(t, db, tbl); got != before {
			t.Fatalf("%s: the function wrote: %s, was %s", path.name, got, before)
		}
	}
}

// TestReadOnlyBehindTheAllowList skips the allow-list (run with
// check=nil) to prove the layers behind it: the session and transaction
// refuse writes and DDL, and the post-check catches a statement that left the
// transaction or made the session read-write.
func TestReadOnlyBehindTheAllowList(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	before := tableState(t, db, tbl)
	for _, readOnlyConn := range []bool{true, false} {
		c := openMy(t, readOnlyConn)
		for _, text := range []string{
			"DELETE FROM " + tbl,
			"CREATE TABLE tobira_probe_new (a INT)",
			"DROP TABLE " + tbl,
			"COMMIT",
			"ROLLBACK",
			"SET SESSION TRANSACTION READ WRITE",
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
}

// TestReadOnlyDiscardsSession checks that nothing a read-only call does to
// its session reaches the next call: the default database, user variables
// and GET_LOCK locks.
func TestReadOnlyDiscardsSession(t *testing.T) {
	db := admin(t)
	for _, path := range []struct {
		name string
		c    *conn
		flag bool
	}{{"read-only conn", openMy(t, true), false}, {"ReadOnly flag", openMy(t, false), true}} {
		c := path.c
		c.db.SetMaxOpenConns(1) // a reused session would be the same one
		run := func(text string) []driver.Row {
			rows, _, _ := collect(t, c, driver.Query{Text: text, ReadOnly: path.flag})
			return rows
		}
		run("USE tobira_ns")
		if got := run("SELECT DATABASE()"); got[0][0].String() != "tobira" {
			t.Errorf("%s: USE leaked into the next call: DATABASE() = %s", path.name, got[0][0])
		}
		run("SELECT 5 INTO @tobira_leak")
		if got := run("SELECT @tobira_leak"); !got[0][0].IsNull() {
			t.Errorf("%s: user variable leaked into the next call: %s", path.name, got[0][0])
		}
		if got := run("SELECT GET_LOCK('tobira_probe_lock', 0)"); got[0][0].String() != "1" {
			t.Fatalf("%s: GET_LOCK = %s", path.name, got[0][0])
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			var holder sql.NullInt64
			if err := db.QueryRow("SELECT IS_USED_LOCK('tobira_probe_lock')").Scan(&holder); err != nil {
				t.Fatal(err)
			}
			if !holder.Valid {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the GET_LOCK lock is still held by connection %d after the call", path.name, holder.Int64)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestReadOnlySessionParam checks the second layer on a read-only
// connection: every pooled session is read-only, even if the DSN said
// otherwise, while a writable connection's sessions are not.
func TestReadOnlySessionParam(t *testing.T) {
	ro := openDSN(t, testDSN+"?transaction_read_only=0", true)
	rw := openMy(t, false)
	for _, tc := range []struct {
		c    *conn
		want int64
	}{{ro, 1}, {rw, 0}} {
		var got int64
		if err := tc.c.db.QueryRow("SELECT @@SESSION.transaction_read_only").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("read-only %v: @@transaction_read_only = %d, want %d", tc.c.readOnly, got, tc.want)
		}
	}
}

// TestReadOnlyHonoursSQLMode runs a text that hides INTO OUTFILE inside what
// the default lexer reads as a string. Under NO_BACKSLASH_ESCAPES the server
// reads it differently, and the guard must too.
func TestReadOnlyHonoursSQLMode(t *testing.T) {
	c := openDSN(t, testDSN+"?sql_mode=%27NO_BACKSLASH_ESCAPES%27", true)
	text := `SELECT 'a\' INTO OUTFILE '/tmp/tobira_mode_probe' -- '`
	if err := checkReadOnly(text, lexMode{}); err != nil {
		t.Fatalf("precondition: the default lexer refuses the probe: %v", err)
	}
	if err := queryErr(t, c, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("want ErrReadOnly under NO_BACKSLASH_ESCAPES, got %v", err)
	}
	if rows, _, _ := collect(t, c, driver.Query{Text: `SELECT 'a\b'`}); rows[0][0].String() != `a\b` {
		t.Errorf(`SELECT 'a\b' = %s`, rows[0][0])
	}
}

func TestReadOnlyMultipleReadsUnsupported(t *testing.T) {
	ro := openMy(t, true)
	err := queryErr(t, ro, driver.Query{Text: "SELECT 1; SELECT 2"})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("want ErrUnsupported, got %v", err)
	}
}

// TestReadOnlyRefusesUnsafeClientCharset forces a pooled session to gbk,
// where 0xbf5c is one character and its 0x5c does not escape the next quote.
// The guard's lexer reads that backslash as an escape and would see a
// harmless string; the server would see INTO OUTFILE. The read-only call must
// refuse before the statement runs.
func TestReadOnlyRefusesUnsafeClientCharset(t *testing.T) {
	c := openMy(t, true)
	c.db.SetMaxOpenConns(1)
	ctx := ctxT(t)
	if _, err := c.db.ExecContext(ctx, "SET NAMES gbk"); err != nil { // driver-internal: the pooled session keeps it
		t.Fatal(err)
	}
	text := "SELECT '\xbf\\' INTO OUTFILE '/tmp/tobira_gbk_probe' -- '"
	if err := checkReadOnly(text, lexMode{}); err != nil {
		t.Fatalf("precondition: the utf8 lexer refuses the probe: %v", err)
	}
	err := queryErr(t, c, driver.Query{Text: text})
	var de *driver.Error
	if !errors.Is(err, driver.ErrUnsupported) || (errors.As(err, &de) && de.Code != "") {
		t.Fatalf("gbk session: want ErrUnsupported from the guard (no server code), got %v", err)
	}
	if _, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN + "?charset=gbk", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Open with charset=gbk = %v, want ErrUnsupported", err)
	}
	var cs string
	w := openMy(t, false)
	if err := w.db.QueryRowContext(ctx, "SELECT @@SESSION.character_set_client").Scan(&cs); err != nil || cs != "utf8mb4" {
		t.Errorf("character_set_client = %q, %v; want utf8mb4", cs, err)
	}
}

// TestReadOnlySetupCancelled cancels the call before its READ ONLY
// transaction starts: the error must be the context's, not a false ErrReadOnly
// from a post-check that ran outside any transaction.
func TestReadOnlySetupCancelled(t *testing.T) {
	c := openMy(t, true)
	sc, err := c.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.execute(ctx, sc, driver.Query{Text: "SELECT 1"}, nil, true, checkReadOnly)
	if !errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("want context.Canceled and not ErrReadOnly, got %v", err)
	}
}

// TestReadOnlyPostCheckFailsClosed: when the session's read-only state cannot
// be read back, the call fails instead of passing.
func TestReadOnlyPostCheckFailsClosed(t *testing.T) {
	c := openMy(t, true)
	c.sessionCheckSQL = "SELECT no_such_column"
	err := queryErr(t, c, driver.Query{Text: "SELECT 1"})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("want ErrReadOnly when the post-check cannot read the session, got %v", err)
	}
}
