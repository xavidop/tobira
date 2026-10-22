//go:build integration

package mssql

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestMutatorIdentityAndTriggers(t *testing.T) {
	c := openMS(t, false)
	ctx := ctx30(t)
	orders := driver.CollectionRef{Namespace: "sales", Name: "orders"}
	key, err := c.Insert(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(1)}, {Key: "id", Value: driver.Null()}, {Key: "note", Value: driver.String("first")}})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := key[0].Value.AsInt()
	if len(key) != 1 || key[0].Key != "id" || !ok || id < 1 {
		t.Fatalf("identity key = %+v", key)
	}
	key, err = c.Insert(ctx, orders, driver.Object{{Key: "id", Value: driver.Int(500)}, {Key: "person_id", Value: driver.Int(2)}, {Key: "note", Value: driver.String("explicit")}})
	if err != nil || key[0].Value.String() != "500" {
		t.Fatalf("explicit identity insert: %+v %v", key, err)
	}
	if err := c.Update(ctx, orders, driver.Object{{Key: "id", Value: driver.Int(500)}}, driver.Object{{Key: "total", Value: driver.Decimal("2.50")}}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, "SELECT CAST(doubled * 100 AS bigint) FROM sales.orders WHERE id = 500"); n != 500 {
		t.Errorf("doubled after update = %d/100, want 5.00", n)
	}
	// Unchanged values still count as one matched row.
	if err := c.Update(ctx, orders, driver.Object{{Key: "id", Value: driver.Int(500)}}, driver.Object{{Key: "total", Value: driver.Decimal("2.50")}}); err != nil {
		t.Errorf("no-op update: %v", err)
	}
	_, err = c.Insert(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(999)}})
	if !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("foreign key violation: want ErrConstraint, got %v", err)
	}
	if err := c.Delete(ctx, orders, driver.Object{{Key: "note", Value: driver.Null()}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("delete by a NULL note: %v", err)
	}
	if err := c.Delete(ctx, orders, driver.Object{{Key: "id", Value: driver.Int(500)}}); err != nil {
		t.Fatal(err)
	}
	// Several matches: nothing changes.
	mustExec(t, admin(t), "INSERT INTO sales.orders (person_id, note) VALUES (3, N'dup1'), (3, N'dup2')")
	if err := c.Delete(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(3)}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("delete matching two rows: want ErrConflict, got %v", err)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM sales.orders WHERE person_id = 3"); n != 2 {
		t.Errorf("rows after a refused delete = %d, want 2", n)
	}
	// OUTPUT … INTO works on a table with a trigger.
	key, err = c.Insert(ctx, driver.CollectionRef{Name: "tobira_trig"}, driver.Object{{Key: "v", Value: driver.Int(7)}})
	if err != nil || len(key) != 1 {
		t.Fatalf("insert into a table with a trigger: %+v %v", key, err)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM tobira_trig_log WHERE n = 7"); n != 1 {
		t.Errorf("trigger ran %d times", n)
	}
	if _, err := openMS(t, true).Insert(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("insert on a read-only conn: %v", err)
	}
}

func TestTransactions(t *testing.T) {
	c := openMS(t, false)
	ctx := ctx30(t)
	db := admin(t)
	mustExec(t, db, "CREATE TABLE tobira_tx (n INT PRIMARY KEY)")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_tx") })

	tx, err := c.Begin(ctx, driver.TxOptions{Isolation: "serializable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := collect(t, tx, driver.Query{Text: "INSERT INTO tobira_tx VALUES (1)"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"COMMIT", "INSERT INTO tobira_tx VALUES (2) COMMIT", "EXEC('COMMIT')", "sp_who", "BEGIN TRAN"} {
		if _, _, _, err := collect(t, tx, driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q in a Tx: want ErrUnsupported, got %v", text, err)
		}
	}
	m, ok := driver.As[driver.Mutator](tx)
	if !ok {
		t.Fatal("Tx is not a Mutator")
	}
	if _, err := m.Insert(ctx, driver.CollectionRef{Name: "tobira_tx"}, driver.Object{{Key: "n", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("duplicate insert in a Tx: %v", err)
	}
	// The failed mutation left the transaction usable.
	if _, err := m.Insert(ctx, driver.CollectionRef{Name: "tobira_tx"}, driver.Object{{Key: "n", Value: driver.Int(3)}}); err != nil {
		t.Errorf("insert after a failed one: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM tobira_tx"); n != 2 {
		t.Errorf("rows after commit = %d, want 2", n)
	}

	// A statement that ends the transaction (past the check) kills the Tx.
	tx, err = c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mt := tx.(*msTx)
	rs, err := mt.query(ctx, driver.Query{Text: "INSERT INTO tobira_tx VALUES (10); ROLLBACK"}, nil)
	if err == nil {
		_, _, err = driver.Collect(ctx, rs, 0)
	}
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a statement ending the Tx: want ErrUnsupported, got %v", err)
	}
	if _, _, _, err := collect(t, tx, driver.Query{Text: "SELECT 1 AS x"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("a dead Tx: want ErrConnection, got %v", err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("commit of a dead Tx: %v", err)
	}

	// A read-only Tx refuses writes and never commits.
	tx, err = c.Begin(ctx, driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := collect(t, tx, driver.Query{Text: "DELETE FROM tobira_tx"}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("delete in a read-only Tx: %v", err)
	}
	rs, err = tx.(*msTx).query(ctx, driver.Query{Text: "DELETE FROM tobira_tx; SELECT 1 AS x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver.Collect(ctx, rs, 0); err != nil {
		t.Fatal(err)
	}
	if x, ok := driver.As[driver.Explainer](tx); !ok {
		t.Error("Tx is not an Explainer")
	} else if _, err := x.Explain(ctx, driver.Query{Text: "SELECT * FROM tobira_tx"}, driver.ExplainOptions{}); err != nil {
		t.Errorf("Tx.Explain: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM tobira_tx"); n != 2 {
		t.Errorf("a read-only Tx kept a write: %d rows, want 2", n)
	}

	// Rollback with a result set still open.
	tx, err = c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rs, err = tx.Query(ctx, driver.Query{Text: "SELECT a.n FROM tobira_big a CROSS JOIN tobira_big b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rs.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	if _, err := c.Begin(ctx, driver.TxOptions{Isolation: "chaos"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown isolation: %v", err)
	}
}
