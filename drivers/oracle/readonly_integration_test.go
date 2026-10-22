//go:build integration

package oracle

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// The server reads the constructs the guard relies on the way the lexer
// does. These run on the writable path, so the guard is not involved.
func TestServerLexing(t *testing.T) {
	c := openOra(t, false)
	for _, tc := range []struct{ name, q, cols, val string }{
		// q-quoting hides quotes and ';' in a string.
		{"q quote", "select q'[a';b]' v from dual", "V", "a';b"},
		{"nq quote", "select nq'{x}' v from dual", "V", "x"},
		// 1delete is the number 1d followed by the alias elete, which is
		// why the guard refuses any malformed number.
		{"number swallows a letter", "select 1delete from dual", "ELETE", "1"},
		// A lone CR does not end a "--" comment: the rest of the line is
		// still comment. The guard refuses a CR in a line comment anyway.
		{"cr does not end a line comment", "select 1 x -- c\r, 2 y from dual", "X", "1"},
		// Block comments do not nest.
		{"block comments do not nest", "select 1 /* /* */ x from dual", "X", "1"},
		// A trailing ';' is accepted.
		{"trailing semicolon", "select 1 x from dual;", "X", "1"},
		// '#' and '$' continue an identifier.
		{"hash and dollar in words", `select 1 a#b$ from dual`, "A#B$", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, cols := collect(t, c, driver.Query{Text: tc.q})
			if got := names(cols); got != tc.cols {
				t.Fatalf("%q: columns %s, want %s", tc.q, got, tc.cols)
			}
			if len(rows) != 1 || rows[0][0].String() != tc.val {
				t.Errorf("%q: rows %v, want %s", tc.q, rows, tc.val)
			}
		})
	}
	// A '"' cannot be doubled inside a quoted identifier.
	if err := queryErr(t, c, driver.Query{Text: `select 1 "a""b" from dual`}); err == nil {
		t.Error(`"a""b" was accepted; the lexer reads it as two identifiers`)
	}
}

// The read-only transaction stops what passes the allow-list, which the
// allow-list normally refuses first: with the allow-list bypassed, a DML
// statement fails inside SET TRANSACTION READ ONLY (ORA-01456).
func TestReadOnlyTransactionLayer(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	c := openOra(t, false)
	before := count(t, db, `"`+tbl+`"`)
	for _, q := range []string{
		`delete from "` + tbl + `" where "id" = 1`,
		`update "` + tbl + `" set "name" = 'x'`,
		`insert into "` + tbl + `" ("id", "name", "email") values (999, 'x', 'x@x')`,
	} {
		_, err := c.run(ctxT(t), driver.Query{Text: q}, true, nil)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s without the allow-list: want ErrReadOnly, got %v", q, err)
		}
	}
	if after := count(t, db, `"`+tbl+`"`); after != before {
		t.Errorf("rows changed from %d to %d", before, after)
	}
}

// DDL commits implicitly and so leaves the read-only transaction: Oracle
// runs it. The allow-list refuses DDL; were it to slip through, the
// transaction id check reports ErrReadOnly afterwards and discards the
// session. This pins that the check catches it.
func TestReadOnlyTransactionIDCheck(t *testing.T) {
	db := admin(t)
	c := openOra(t, false)
	t.Cleanup(func() {
		if tableExists(t, db, "TOBIRA_ESCAPE") {
			mustExec(t, db, "DROP TABLE tobira_escape PURGE")
		}
	})
	rs, err := c.run(ctxT(t), driver.Query{Text: "create table tobira_escape (a number)"}, true, nil)
	if err == nil {
		_, _, err = driver.Collect(ctxT(t), rs, 0)
	}
	if !errors.Is(err, driver.ErrReadOnly) || !strings.Contains(err.Error(), "ended the read-only transaction") {
		t.Fatalf("DDL without the allow-list: want the transaction check's ErrReadOnly, got %v", err)
	}
	// With the allow-list, nothing reaches the server.
	if err := queryErr(t, c, driver.Query{Text: "create table tobira_escape2 (a number)", ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("DDL with ReadOnly: want ErrReadOnly, got %v", err)
	}
	if tableExists(t, db, "TOBIRA_ESCAPE2") {
		t.Error("the refused DDL ran")
	}
}

func TestReadOnlyRefusals(t *testing.T) {
	db := admin(t)
	ro := openOra(t, true)
	var before int64
	if err := db.QueryRow("SELECT last_number FROM user_sequences WHERE sequence_name = 'TOBIRA_SEQ'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"select tobira_seq.nextval from dual",
		`select tobira_seq."NEXTVAL" from dual`,
		"alter session set current_schema = tobira_ns",
		"select * from dual@nowhere",
		"select utl_inaddr.get_host_name('127.0.0.1') from dual",
		"select httpuritype('http://127.0.0.1:1/').getclob() from dual",
		"lock table tobira_big in exclusive mode",
		"savepoint x",
		"set transaction read write",
	} {
		if err := queryErr(t, ro, driver.Query{Text: q}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s on a read-only conn: want ErrReadOnly, got %v", q, err)
		}
	}
	var after int64
	if err := db.QueryRow("SELECT last_number FROM user_sequences WHERE sequence_name = 'TOBIRA_SEQ'").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("the sequence advanced from %d to %d", before, after)
	}
	// Reads work, namespaced reads too, and the session comes back clean.
	rows, _, _ := collect(t, ro, driver.Query{Text: "select label from things", Namespace: "TOBIRA_NS"})
	if len(rows) != 1 || rows[0][0].String() != "in-ns" {
		t.Errorf("namespaced read-only query: %v", rows)
	}
	ro.db.SetMaxOpenConns(1)
	if got := currentSchema(t, ro); got != "TOBIRA" {
		t.Errorf("CURRENT_SCHEMA after a namespaced call = %s, want TOBIRA", got)
	}
	rs, err := ro.run(ctxT(t), driver.Query{Text: txIDSQL}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil || len(rr) != 1 || rr[0][0].IsNull() {
		t.Errorf("a read-only query runs in a transaction: %v %v", rr, err)
	}
}

// A read-only conn needs a verified server version; a writable one does not.
func TestReadOnlyUnverifiedVersion(t *testing.T) {
	c := openOra(t, false)
	c.version = "26.1.0.0.0"
	if err := queryErr(t, c, driver.Query{Text: "select 1 from dual", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("read-only query on an unverified version: want ErrUnsupported, got %v", err)
	}
	if err := queryErr(t, c, driver.Query{Text: "select 1 from dual"}); err != nil {
		t.Errorf("writable query on an unverified version: %v", err)
	}
}

// Cancelling a read-only query mid-stream ends it quickly, and the pool
// stays usable.
func TestReadOnlyCancelMidStream(t *testing.T) {
	c := openOra(t, true)
	ctx, cancel := contextWithCancel(t)
	rs, err := c.Query(ctx, driver.Query{Text: "select a.n from tobira_big a, tobira_big b, tobira_big c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(ctx); !ok || err != nil {
		t.Fatalf("first row: %v %v", ok, err)
	}
	cancel()
	_, _, err = rs.Next(ctx)
	if err == nil {
		t.Error("Next after cancel: want an error")
	}
	_ = rs.Close()
	rows, _, _ := collect(t, c, driver.Query{Text: "select count(*) from tobira_big"})
	if rows[0][0].String() != "1000" {
		t.Errorf("after cancel: %v", rows)
	}
}
