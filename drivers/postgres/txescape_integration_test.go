//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// escapeTexts end or detach a transaction, hidden behind empty statements.
var escapeTexts = []string{";commit", "/**/;end", " ; commit", ";prepare transaction 'p1'", ";;rollback", "commit", "prepare transaction 'p2'"}

func TestEmptyStatementPrefixCannotLeaveTx(t *testing.T) {
	w := openPG(t, false)
	table, _ := keyed(t, w)
	mustExec(t, w, "INSERT INTO "+table+" (name) VALUES ('seed')")
	ro := openPG(t, true)
	write := "update " + table + " set name = 'pwned' where id = 1"
	lift := "set default_transaction_read_only = off"

	check := func(t *testing.T, label string, want error, run func(text string) error) {
		for _, text := range escapeTexts {
			err := run(text)
			if err == nil || !errors.Is(err, want) {
				t.Errorf("%s %q: want %v, got %v", label, text, want, err)
			}
		}
		if got, _ := nameOf(t, w, table, 1); got != "seed" {
			t.Errorf("%s: write persisted, name = %q", label, got)
		}
		if n := count(t, w, "pg_prepared_xacts"); n != 0 {
			t.Errorf("%s: %d prepared transactions left behind", label, n)
		}
	}

	check(t, "plain call on a read-only conn", driver.ErrReadOnly, func(text string) error {
		return queryErr(t, ro, driver.Query{Text: text})
	})
	for _, tc := range []struct {
		name string
		conn *conn
		opts driver.TxOptions
		want error
	}{
		{"read-only Tx on read-only conn", ro, driver.TxOptions{ReadOnly: true}, driver.ErrReadOnly},
		{"read-only Tx on writable conn", w, driver.TxOptions{ReadOnly: true}, driver.ErrReadOnly},
		{"read-write Tx", w, driver.TxOptions{}, driver.ErrUnsupported},
	} {
		check(t, tc.name, tc.want, func(text string) error {
			tx := begin(t, tc.conn, tc.opts)
			defer func() { _ = tx.Rollback(ctxT(t)) }()
			err := queryErr(t, tx, driver.Query{Text: text})
			if tc.opts.ReadOnly {
				// whatever happened, the Tx must not accept a write afterwards
				_ = queryErr(t, tx, driver.Query{Text: lift})
				if werr := queryErr(t, tx, driver.Query{Text: write}); werr == nil {
					t.Errorf("%s %q: a write succeeded afterwards", tc.name, text)
				}
			}
			return err
		})
	}
}

// TestFailClosedWhenTxEnds bypasses the keyword refusal to prove the
// transaction-status check catches a statement that ended the Tx.
func TestFailClosedWhenTxEnds(t *testing.T) {
	w := openPG(t, false)
	table, _ := keyed(t, w)
	mustExec(t, w, "INSERT INTO "+table+" (name) VALUES ('seed')")
	for _, readOnly := range []bool{true, false} {
		tx := begin(t, w, driver.TxOptions{ReadOnly: readOnly}).(*pgTx)
		scope := &txScope{tx: tx.tx, readOnly: readOnly, onEscape: tx.kill}
		rs, err := w.query(ctxT(t), driver.Query{Text: "commit"}, false, scope)
		if err == nil {
			_, _, err = driver.Collect(ctxT(t), rs, 0)
		}
		if err == nil {
			t.Errorf("readOnly=%v: a statement that ended the Tx was accepted", readOnly)
		}
		if _, err := tx.Query(ctxT(t), driver.Query{Text: "update " + table + " set name = 'pwned'"}); err == nil {
			t.Errorf("readOnly=%v: the Tx accepted a statement after it escaped", readOnly)
		}
		_ = tx.Rollback(ctxT(t))
	}
	if got, _ := nameOf(t, w, table, 1); got != "seed" {
		t.Errorf("name = %q", got)
	}
}

// TestTxUnlimitedSelectClosedEarlyKeepsTx closes a half-read SELECT in a Tx.
// Each row costs 2 ms of server time, so reading the remaining ~99,500 rows
// would take over 3 minutes, while closing the cursor takes milliseconds. The
// 20 s bound keeps that gap wide on a loaded machine.
func TestTxUnlimitedSelectClosedEarlyKeepsTx(t *testing.T) {
	c := openPG(t, false)
	tx := begin(t, c, driver.TxOptions{})
	rs, err := tx.Query(ctxT(t), driver.Query{Text: "select g, pg_sleep(0.002) from generate_series(1, 100000) g"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(ctxT(t)); err != nil || !ok {
		t.Fatalf("Next: %v %v", ok, err)
	}
	done := make(chan struct{})
	go func() { _ = rs.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second): // below ctxT's 30 s, which would end a drain too
		t.Fatal("closing a half-read unlimited SELECT in a Tx drains the whole result")
	}
	runTx(t, tx, "select 1") // the Tx is still usable
}

func TestSelectIntoInTx(t *testing.T) {
	w := openPG(t, false)
	table, _ := keyed(t, w)
	mustExec(t, w, "INSERT INTO "+table+" (name) VALUES ('seed')")
	into := fmt.Sprintf("tobira_into_%d", scratchSeq.Add(1))
	t.Cleanup(func() { mustExec(t, w, "DROP TABLE IF EXISTS "+into) })

	tx := begin(t, w, driver.TxOptions{})
	if err := queryErr(t, tx, driver.Query{Text: "select 1 as a into " + into}); err != nil {
		t.Fatalf("SELECT INTO in a read-write Tx: %v", err)
	}
	runTx(t, tx, "select 1") // the Tx is still usable
	runTx(t, tx, "select * from "+table+" for update")
	// a failing cursor attempt must not abort the Tx either
	if err := queryErr(t, tx, driver.Query{Text: "select * from nope_missing"}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("missing table: %v", err)
	}
	runTx(t, tx, "select 1")
	_ = tx.Rollback(ctxT(t))

	for _, c := range []*conn{w, openPG(t, true)} {
		tx = begin(t, c, driver.TxOptions{ReadOnly: true})
		if err := queryErr(t, tx, driver.Query{Text: "select 1 as a into " + into}); !errors.Is(err, driver.ErrReadOnly) {
			t.Fatalf("SELECT INTO in a read-only Tx: want ErrReadOnly, got %v", err)
		}
		runTx(t, tx, "select 1") // refused before it ran, so the Tx is usable
		_ = tx.Rollback(ctxT(t))
	}
	if n := count(t, w, "pg_class where relname = '"+into+"'"); n != 0 {
		t.Errorf("table %s was created", into)
	}
}

func TestAndChainRefusedOnReadOnlyPaths(t *testing.T) {
	ro := openPG(t, true)
	for _, text := range []string{"commit and chain", "ROLLBACK /* x */ AND CHAIN", "end and chain", "abort and chain"} {
		if err := queryErr(t, ro, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q on a read-only conn: want ErrReadOnly, got %v", text, err)
		}
		w := openPG(t, false)
		if err := queryErr(t, w, driver.Query{Text: text, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q with Query.ReadOnly: want ErrReadOnly, got %v", text, err)
		}
	}
}

func TestEscapedReadOnlyCallDiscardsConnection(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN + "&pool_max_conns=1", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	pid := func() int64 {
		rs, err := c.Query(ctxT(t), driver.Query{Text: "select pg_backend_pid()"})
		if err != nil {
			t.Fatal(err)
		}
		rows, _, _ := driver.Collect(ctxT(t), rs, 0)
		n, _ := rows[0][0].AsInt()
		return n
	}
	before := pid()
	// Reaches the server: the plain path has no keyword refusal, only the status check.
	if err := queryErr(t, c, driver.Query{Text: "commit"}); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
	if after := pid(); after == before {
		t.Error("the connection whose transaction was ended was returned to the pool")
	}
}

func TestRepeatedCursorFallbacksInTx(t *testing.T) {
	c := openPG(t, false)
	table, _ := keyed(t, c)
	tx := begin(t, c, driver.TxOptions{})
	// A WITH holding a data-modifying CTE cannot be declared: each attempt
	// falls back to direct execution, after a savepoint of its own.
	text := "with d as (insert into " + table + " (name) values ('x') returning id) select id from d"
	for range 100 {
		runTx(t, tx, text)
	}
	// The caller's own savepoint name is free to use.
	runTx(t, tx, "savepoint tobira_sp")
	runTx(t, tx, "rollback to tobira_sp")
	if err := tx.Commit(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, table); n != 100 {
		t.Errorf("rows = %d, want 100", n)
	}
}

func TestRollbackFormsRefusedInReadWriteTx(t *testing.T) {
	c := openPG(t, false)
	table, _ := keyed(t, c)
	for _, text := range []string{"rollback", "rollback and chain", "rollback work and chain", "rollback; to x", "rollback transaction"} {
		tx := begin(t, c, driver.TxOptions{})
		runTx(t, tx, "insert into "+table+" (name) values ('tx')")
		if err := queryErr(t, tx, driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q in a read-write Tx: want ErrUnsupported, got %v", text, err)
		}
		runTx(t, tx, "select 1") // refused before running: the Tx is intact
		_ = tx.Rollback(ctxT(t))
	}
	if n := count(t, c, table); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}
