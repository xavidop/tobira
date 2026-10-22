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

func TestInfoCapabilities(t *testing.T) {
	got := New().Info().Capabilities
	for _, want := range []driver.Capability{driver.CapMutator, driver.CapTransactor, driver.CapExporter, driver.CapWatcher} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("Info().Capabilities lacks %s: %v", want, got)
		}
	}
}

// keyed creates a private table with a serial primary key, a NOT NULL text
// column and a few typed columns.
func keyed(t *testing.T, c *conn) (string, driver.CollectionRef) {
	t.Helper()
	name := fmt.Sprintf("tobira_keyed_%d", scratchSeq.Add(1))
	mustExec(t, c, "CREATE TABLE "+name+" (id serial PRIMARY KEY, name text NOT NULL, doc jsonb, at timestamptz, amount numeric, ok boolean)")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS "+name) })
	return name, driver.CollectionRef{Namespace: "public", Name: name}
}

// keyless creates a private table without a key holding two identical-key rows.
func keyless(t *testing.T, c *conn) (string, driver.CollectionRef) {
	t.Helper()
	name := fmt.Sprintf("tobira_keyless_%d", scratchSeq.Add(1))
	mustExec(t, c, "CREATE TABLE "+name+" (a text, b text)")
	mustExec(t, c, "INSERT INTO "+name+" VALUES ('x','1'), ('x','2'), (NULL,'n')")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS "+name) })
	return name, driver.CollectionRef{Namespace: "public", Name: name}
}

func idKey(id int64) driver.Object { return driver.Object{{Key: "id", Value: driver.Int(id)}} }

func str(s string) driver.Value { return driver.String(s) }

func nameOf(t *testing.T, q driver.Querier, table string, id int64) (string, bool) {
	t.Helper()
	rs, err := q.Query(ctxT(t), driver.Query{Text: "select name from " + table + " where id = $1", Params: []driver.Value{driver.Int(id)}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return "", false
	}
	s, _ := rows[0][0].AsText()
	return s, true
}

func TestMutatorRoundTrip(t *testing.T) {
	c := openPG(t, false)
	table, ref := keyed(t, c)
	ctx := ctxT(t)
	evil := "Bobby'); drop table " + table + ";--"
	key, err := c.Insert(ctx, ref, driver.Object{
		{Key: "name", Value: str(evil)},
		{Key: "doc", Value: driver.JSON([]byte(`{"a":1}`))},
		{Key: "at", Value: driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))},
		{Key: "amount", Value: driver.Decimal("1.50")},
		{Key: "ok", Value: driver.Bool(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	idv, ok := key.Get("id")
	if !ok {
		t.Fatalf("Insert did not return the generated key: %v", key)
	}
	id, _ := idv.AsInt()
	if got, ok := nameOf(t, c, table, id); !ok || got != evil {
		t.Fatalf("inserted name = %q (present %v)", got, ok)
	}
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "name", Value: str("Rob")}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := nameOf(t, c, table, id); got != "Rob" {
		t.Fatalf("after Update name = %q", got)
	}
	if err := c.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}
	if _, ok := nameOf(t, c, table, id); ok {
		t.Fatal("row still present after Delete")
	}
}

func TestMutatorInsertDefaultValuesAndSuppliedKey(t *testing.T) {
	c := openPG(t, false)
	name := fmt.Sprintf("tobira_dflt_%d", scratchSeq.Add(1))
	mustExec(t, c, "CREATE TABLE "+name+" (id integer PRIMARY KEY DEFAULT 7, name text DEFAULT 'd')")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS "+name) })
	ref := driver.CollectionRef{Name: name}
	key, err := c.Insert(ctxT(t), ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := key.Get("id"); v.String() != "7" {
		t.Fatalf("default key = %v", key)
	}
	key, err = c.Insert(ctxT(t), ref, driver.Object{{Key: "id", Value: driver.Int(9)}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := key.Get("id"); v.String() != "9" {
		t.Fatalf("supplied key = %v", key)
	}
}

func TestMutatorErrors(t *testing.T) {
	c := openPG(t, false)
	table, ref := keyed(t, c)
	kl, klRef := keyless(t, c)
	ctx := ctxT(t)
	one := driver.Object{{Key: "name", Value: str("y")}}
	xKey := driver.Object{{Key: "a", Value: str("x")}}

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"update missing row", c.Update(ctx, ref, idKey(999999), one), driver.ErrNotFound},
		{"delete missing row", c.Delete(ctx, ref, idKey(999999)), driver.ErrNotFound},
		{"unknown collection", c.Update(ctx, driver.CollectionRef{Name: "nope"}, idKey(1), one), driver.ErrNotFound},
		{"unknown key field", c.Delete(ctx, ref, driver.Object{{Key: `id" or 1=1 --`, Value: driver.Int(1)}}), driver.ErrNotFound},
		{"unknown change field", c.Update(ctx, ref, idKey(1), driver.Object{{Key: `x" = 1 --`, Value: driver.Int(1)}}), driver.ErrNotFound},
		{"empty key", c.Delete(ctx, ref, nil), driver.ErrSyntax},
		{"empty changes", c.Update(ctx, ref, idKey(1), nil), driver.ErrSyntax},
		{"multi-row update", c.Update(ctx, klRef, xKey, driver.Object{{Key: "b", Value: str("z")}}), driver.ErrConflict},
		{"multi-row delete", c.Delete(ctx, klRef, xKey), driver.ErrConflict},
	}
	_, ierr := c.Insert(ctx, ref, driver.Object{{Key: "doc", Value: driver.Null()}})
	tests = append(tests, struct {
		name string
		err  error
		want error
	}{"not-null constraint", ierr, driver.ErrConstraint})
	for _, tc := range tests {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.err, tc.want)
		}
	}
	if n := count(t, c, kl+" where b in ('1','2')"); n != 2 {
		t.Errorf("multi-row conflict must roll back: rows = %d, want 2", n)
	}
	if err := c.Update(ctx, klRef, driver.Object{{Key: "a", Value: driver.Null()}}, driver.Object{{Key: "b", Value: str("changed")}}); err != nil {
		t.Errorf("null key must match with IS NULL: %v", err)
	}
	_ = table
}

func TestMutatorReadOnlyConn(t *testing.T) {
	c := openPG(t, true)
	ctx := ctxT(t)
	ref := driver.CollectionRef{Name: "tobira_people"}
	_, ierr := c.Insert(ctx, ref, driver.Object{{Key: "name", Value: str("x")}})
	for name, err := range map[string]error{
		"insert": ierr,
		"update": c.Update(ctx, ref, idKey(1), driver.Object{{Key: "name", Value: str("x")}}),
		"delete": c.Delete(ctx, ref, idKey(1)),
	} {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s on read-only conn: %v", name, err)
		}
	}
}

func begin(t *testing.T, c driver.Conn, opts driver.TxOptions) driver.Tx {
	t.Helper()
	tr, ok := driver.As[driver.Transactor](c)
	if !ok {
		t.Fatal("conn is not a Transactor")
	}
	tx, err := tr.Begin(ctxT(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func runTx(t *testing.T, q driver.Querier, text string) {
	t.Helper()
	rs, err := q.Query(ctxT(t), driver.Query{Text: text})
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	if _, _, err := driver.Collect(ctxT(t), rs, 0); err != nil {
		t.Fatalf("%s: %v", text, err)
	}
}

func TestTxCommitAndRollback(t *testing.T) {
	c := openPG(t, false)
	table, ref := keyed(t, c)
	mustExec(t, c, "INSERT INTO "+table+" (name) VALUES ('seed')")
	for _, commit := range []bool{false, true} {
		tx := begin(t, c, driver.TxOptions{})
		m, ok := driver.As[driver.Mutator](tx)
		if !ok {
			t.Fatal("Tx is not a Mutator")
		}
		if err := m.Update(ctxT(t), ref, idKey(1), driver.Object{{Key: "name", Value: str("in-tx")}}); err != nil {
			t.Fatal(err)
		}
		runTx(t, tx, "update "+table+" set ok = true where id = 1")
		if got, _ := nameOf(t, tx, table, 1); got != "in-tx" {
			t.Errorf("Tx must see its own writes, got %q", got)
		}
		if got, _ := nameOf(t, c, table, 1); got == "in-tx" {
			t.Error("uncommitted write visible outside the Tx")
		}
		if commit {
			if err := tx.Commit(ctxT(t)); err != nil {
				t.Fatal(err)
			}
		} else if err := tx.Rollback(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctxT(t)); err != nil {
			t.Errorf("Rollback after finish must be a no-op: %v", err)
		}
		got, _ := nameOf(t, c, table, 1)
		if (got == "in-tx") != commit {
			t.Errorf("commit=%v: name = %q", commit, got)
		}
	}
}

func TestTxMultiRowKeepsTxUsable(t *testing.T) {
	c := openPG(t, false)
	table, ref := keyed(t, c)
	kl, klRef := keyless(t, c)
	mustExec(t, c, "INSERT INTO "+table+" (name) VALUES ('seed')")
	tx := begin(t, c, driver.TxOptions{})
	m, _ := driver.As[driver.Mutator](tx)
	if err := m.Update(ctxT(t), ref, idKey(1), driver.Object{{Key: "name", Value: str("kept")}}); err != nil {
		t.Fatal(err)
	}
	err := m.Delete(ctxT(t), klRef, driver.Object{{Key: "a", Value: str("x")}})
	if !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	// A failing statement must not poison the Tx either.
	if err := m.Update(ctxT(t), ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "name", Value: driver.Null()}}); !errors.Is(err, driver.ErrConstraint) {
		t.Fatalf("want ErrConstraint, got %v", err)
	}
	if err := tx.Commit(ctxT(t)); err != nil {
		t.Fatalf("Tx must survive a failed mutation: %v", err)
	}
	if got, _ := nameOf(t, c, table, 1); got != "kept" {
		t.Errorf("earlier write lost: %q", got)
	}
	if n := count(t, c, kl); n != 3 {
		t.Errorf("conflicting delete leaked: %d rows", n)
	}
}

func TestTxReadOnly(t *testing.T) {
	for _, readOnlyConn := range []bool{false, true} {
		c := openPG(t, readOnlyConn)
		total := count(t, c, "tobira_people")
		tx := begin(t, c, driver.TxOptions{ReadOnly: true})
		if got, ok := nameOf(t, tx, "tobira_people", 1); !ok || got == "" {
			t.Errorf("read-only Tx cannot read: %q %v", got, ok)
		}
		for _, text := range []string{
			"update tobira_people set name = 'x'", "delete from tobira_people", "create table tobira_nope (a int)",
			"commit", "end", "rollback", "abort", "begin", "start transaction read write", "prepare transaction 'px'",
			"set transaction read write",
		} {
			stx := begin(t, c, driver.TxOptions{ReadOnly: true}) // a failed statement aborts its Tx
			if err := queryErr(t, stx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("readOnlyConn=%v: %q in a read-only Tx: want ErrReadOnly, got %v", readOnlyConn, text, err)
			}
			_ = stx.Rollback(ctxT(t))
		}
		// The Tx must still be read-only and usable after the refused statements.
		m, _ := driver.As[driver.Mutator](tx)
		if err := m.Delete(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, idKey(1)); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Mutator on a read-only Tx: %v", err)
		}
		_ = tx.Rollback(ctxT(t))
		if got := count(t, c, "tobira_people"); got != total {
			t.Errorf("rows = %d, want %d", got, total)
		}
	}
}

func TestTxQueryRules(t *testing.T) {
	c := openPG(t, false)
	tx := begin(t, c, driver.TxOptions{})
	if _, err := tx.Query(ctxT(t), driver.Query{Text: "select 1; select 2"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("multi-statement in Tx: %v", err)
	}
	tx = begin(t, c, driver.TxOptions{})
	if _, err := tx.Query(ctxT(t), driver.Query{Text: "select 1", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("ReadOnly query in a read-write Tx: want ErrUnsupported, got %v", err)
	}
	for _, text := range []string{"commit", "rollback", "begin"} {
		if _, err := tx.Query(ctxT(t), driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q in a Tx: want ErrUnsupported, got %v", text, err)
		}
	}
}

func TestTxLimitedQueriesAndNamespace(t *testing.T) {
	c := openPG(t, false)
	tx := begin(t, c, driver.TxOptions{})
	for i := range 2 { // a second limited query would fail on a left-over cursor
		rs, err := tx.Query(ctxT(t), driver.Query{Text: "select id from tobira_people order by id", Limit: 3})
		if err != nil {
			t.Fatalf("limited query %d: %v", i, err)
		}
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil || len(rows) != 3 || !st.Truncated {
			t.Fatalf("limited query %d: rows %d truncated %v err %v", i, len(rows), st.Truncated, err)
		}
	}
	rs, err := tx.Query(ctxT(t), driver.Query{Text: "select label from things", Namespace: "tobira_ns"})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _, err := driver.Collect(ctxT(t), rs, 0); err != nil || len(rows) != 1 {
		t.Fatalf("namespaced query: %d rows, %v", len(rows), err)
	}
	if _, err := tx.Query(ctxT(t), driver.Query{Text: "select label from things"}); err == nil {
		t.Error("search_path leaked out of the namespaced query")
	}
}

func TestBeginRules(t *testing.T) {
	ro := openPG(t, true)
	tr, _ := driver.As[driver.Transactor](ro)
	if _, err := tr.Begin(ctxT(t), driver.TxOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-write Begin on a read-only conn: %v", err)
	}
	c := openPG(t, false)
	tr, _ = driver.As[driver.Transactor](c)
	if _, err := tr.Begin(ctxT(t), driver.TxOptions{Isolation: "bogus"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("bogus isolation: %v", err)
	}
	tx, err := tr.Begin(ctxT(t), driver.TxOptions{Isolation: "Repeatable Read"})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := tx.Query(ctxT(t), driver.Query{Text: "show transaction_isolation"})
	rows, _, _ := driver.Collect(ctxT(t), rs, 0)
	if got, _ := rows[0][0].AsText(); got != "repeatable read" {
		t.Errorf("isolation = %q", got)
	}
	_ = tx.Rollback(ctxT(t))
}

func TestCloseRollsBackOpenTx(t *testing.T) {
	setup := openPG(t, false)
	table, ref := keyed(t, setup)
	mustExec(t, setup, "INSERT INTO "+table+" (name) VALUES ('seed')")
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := driver.As[driver.Transactor](c)
	tx, err := tr.Begin(ctxT(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := driver.As[driver.Mutator](tx)
	if err := m.Update(ctxT(t), ref, idKey(1), driver.Object{{Key: "name", Value: str("lost")}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Query(ctxT(t), driver.Query{Text: "select 1"}); err == nil {
		t.Error("Tx must be unusable after Conn.Close")
	}
	if err := tx.Commit(ctxT(t)); err == nil {
		t.Error("Commit after Conn.Close must fail")
	}
	c2 := openPG(t, false)
	if got, _ := nameOf(t, c2, table, 1); got != "seed" {
		t.Errorf("Close must roll back the open Tx, name = %q", got)
	}
}

func TestSingleConnectionPool(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN + "&pool_max_conns=1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() }) // runs after keyed's DROP
	pc := c.(*conn)
	table, ref := keyed(t, pc)
	mustExec(t, pc, "INSERT INTO "+table+" (name) VALUES ('seed')")
	tx := begin(t, c, driver.TxOptions{})
	m, _ := driver.As[driver.Mutator](tx)
	if err := m.Update(ctxT(t), ref, idKey(1), driver.Object{{Key: "name", Value: str("one-conn")}}); err != nil {
		t.Fatalf("a Tx mutation must not need a second connection: %v", err)
	}
	runTx(t, tx, "set search_path = pg_catalog")
	if err := tx.Commit(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	rs, _ := c.Query(ctxT(t), driver.Query{Text: "show search_path"})
	rows, _, _ := driver.Collect(ctxT(t), rs, 0)
	if got, _ := rows[0][0].AsText(); got == "pg_catalog" {
		t.Error("session state leaked past the Tx: connection was not reset")
	}
}
