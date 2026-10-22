package duckdb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestReadOnlyTx(t *testing.T) {
	path := newFile(t, "create table t (a int primary key)", "insert into t values (1)")
	c := open(t, path, false)
	ctx := context.Background()
	tx, err := c.(driver.Transactor).Begin(ctx, driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if v := one(t, tx, "select count(*) from t"); v.String() != "1" {
		t.Errorf("count = %v", v)
	}
	if _, err := queryAll(t, tx, driver.Query{Text: "insert into t values (2)"}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("insert in a read-only Tx = %v, want ErrReadOnly", err)
	}
	if err := tx.(driver.Mutator).Delete(ctx, driver.CollectionRef{Namespace: "main", Name: "t"}, driver.Object{{Key: "a", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Delete in a read-only Tx = %v, want ErrReadOnly", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("Commit of a read-only Tx: %v", err)
	}
	ro := open(t, path, true)
	if _, err := ro.(driver.Transactor).Begin(ctx, driver.TxOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-write Begin on a read-only conn = %v, want ErrReadOnly", err)
	}
}

func TestTxAbortedByError(t *testing.T) {
	path := newFile(t, "create table t (a int primary key)")
	c := open(t, path, false)
	ctx := context.Background()
	tx, err := c.(driver.Transactor).Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queryAll(t, tx, driver.Query{Text: "insert into t values (1)"}); err != nil {
		t.Fatal(err)
	}
	// A statement that fails to bind leaves the transaction alone...
	if _, err := queryAll(t, tx, driver.Query{Text: "select * from nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("missing table = %v, want ErrNotFound", err)
	}
	if v := one(t, tx, "select count(*) from t"); v.String() != "1" {
		t.Fatalf("count after a bind error = %v, want 1", v)
	}
	// ...but one that fails running aborts it.
	if _, err := queryAll(t, tx, driver.Query{Text: "insert into t values (1)"}); !errors.Is(err, driver.ErrConstraint) {
		t.Fatalf("duplicate key = %v, want ErrConstraint", err)
	}
	_, err = queryAll(t, tx, driver.Query{Text: "select 1"})
	if !errors.Is(err, driver.ErrConnection) || !strings.Contains(err.Error(), "roll back") {
		t.Errorf("a statement after DuckDB aborted the Tx = %v, want ErrConnection asking for a rollback", err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Error("Commit of an aborted Tx succeeded")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("Rollback after a failed Commit: %v", err)
	}
	if v := one(t, c, "select count(*) from t"); v.String() != "0" {
		t.Errorf("count = %v, want 0 after the aborted Tx", v)
	}
}

func TestTxRefusesControlStatements(t *testing.T) {
	path := newFile(t, "create table t (a int)")
	c := open(t, path, false)
	ctx := context.Background()
	tx, err := c.(driver.Transactor).Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{"commit", "rollback", "end", "begin"} {
		if _, err := queryAll(t, tx, driver.Query{Text: q}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q in a Tx = %v, want ErrUnsupported", q, err)
		}
	}
	if _, err := queryAll(t, tx, driver.Query{Text: "insert into t values (1)"}); err != nil {
		t.Fatalf("the Tx is unusable after refusals: %v", err)
	}
}

func TestNamespaceQuery(t *testing.T) {
	path := newFile(t, "create schema s", "create table s.t (a int)", "insert into s.t values (5)")
	rw := open(t, path, false)
	for _, q := range []driver.Querier{rw, roRunner{rw.(*conn)}} {
		rows, err := queryAll(t, q, driver.Query{Text: "select a from t", Namespace: "s"})
		if err != nil || len(rows) != 1 || rows[0][0].String() != "5" {
			t.Errorf("%T: select in namespace s = %v, %v", q, rows, err)
		}
	}
	// The search_path does not leak to the next statement.
	if _, err := queryAll(t, rw, driver.Query{Text: "select a from t"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("select without the namespace = %v, want ErrNotFound", err)
	}
	ro := open(t, newFile(t, "create schema s", "create table s.t (a int)"), true)
	if _, err := queryAll(t, ro, driver.Query{Text: "select a from t", Namespace: "s"}); err != nil {
		t.Errorf("namespace on a read-only instance: %v", err)
	}
}

func TestExplain(t *testing.T) {
	path := newFile(t, "create table t (a int)", "insert into t values (1)")
	rw := open(t, path, false)
	x := rw.(driver.Explainer)
	ctx := context.Background()
	plan, err := x.Explain(ctx, driver.Query{Text: "select * from t"}, driver.ExplainOptions{})
	if err != nil || !strings.Contains(plan.Text, "SCAN") {
		t.Fatalf("Explain = %+v, %v", plan, err)
	}
	if _, err := x.Explain(ctx, driver.Query{Text: "select * from t", ReadOnly: true}, driver.ExplainOptions{Analyze: true}); err != nil {
		t.Errorf("EXPLAIN ANALYZE of a read with ReadOnly: %v", err)
	}
	if _, err := x.Explain(ctx, driver.Query{Text: "delete from t", ReadOnly: true}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("EXPLAIN ANALYZE of a delete with ReadOnly = %v, want ErrReadOnly", err)
	}
	if v := one(t, rw, "select count(*) from t"); v.String() != "1" {
		t.Errorf("count = %v", v)
	}
	ro := open(t, newFile(t, "create table t (a int)"), true)
	if _, err := ro.(driver.Explainer).Explain(ctx, driver.Query{Text: "select * from read_csv('x.csv')"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("explaining read_csv on a read-only conn = %v, want ErrReadOnly (binding reads the file)", err)
	}
}

func TestBrowseReadOnlyConn(t *testing.T) {
	path := newFile(t, "create table t (a int, b varchar)", "insert into t select range, 'x' || range from range(25)")
	ro := open(t, path, true)
	b := ro.(driver.Browser)
	ctx := context.Background()
	rs, err := b.Browse(ctx, driver.CollectionRef{Namespace: "main", Name: "t"}, driver.BrowseOptions{Limit: 10,
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "b", Op: driver.OpPrefix, Value: driver.String("x1")}}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != 10 || st.NextCursor != "10" {
		t.Errorf("first page: %d rows, cursor %q, %v", len(rows), st.NextCursor, err)
	}
	_, err = b.Browse(ctx, driver.CollectionRef{Namespace: "main", Name: "t"}, driver.BrowseOptions{Filter: driver.Filter{Raw: "a in (select 1 from read_csv('x.csv'))"}})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("a raw filter calling read_csv = %v, want ErrReadOnly", err)
	}
}
