//go:build integration

package mssql

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// An audit trigger's own row counts must not make a one-row Update or
// Delete look like several, nor inflate Query's rows affected.
func TestRowCountsIgnoreTriggers(t *testing.T) {
	db := admin(t)
	mustExec(t, db, "CREATE TABLE tobira_audited (id INT PRIMARY KEY, v INT)")
	mustExec(t, db, "CREATE TABLE tobira_audit (op NVARCHAR(10), id INT)")
	t.Cleanup(func() {
		mustExec(t, db, "DROP TABLE tobira_audited")
		mustExec(t, db, "DROP TABLE tobira_audit")
	})
	mustExec(t, db, "CREATE TRIGGER tr_tobira_audited ON tobira_audited AFTER UPDATE, DELETE AS "+
		"INSERT INTO tobira_audit SELECT N'old', id FROM deleted; INSERT INTO tobira_audit SELECT N'old2', id FROM deleted")
	mustExec(t, db, "INSERT INTO tobira_audited VALUES (1, 1), (2, 2), (3, 3)")
	c := openMS(t, false)
	ctx := ctx30(t)
	ref := driver.CollectionRef{Name: "tobira_audited"}
	if err := c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "v", Value: driver.Int(10)}}); err != nil {
		t.Errorf("Update of an audited row: %v", err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}); err != nil {
		t.Errorf("Delete of an audited row: %v", err)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM tobira_audited WHERE id = 1"); n != 0 {
		t.Errorf("row 1 still there: %d", n)
	}
	_, _, st, err := collect(t, c, driver.Query{Text: "DELETE FROM tobira_audited WHERE id = @p1", Params: []driver.Value{driver.Int(2)}})
	if err != nil || st.RowsAffected != 1 {
		t.Errorf("Query DELETE of one audited row: rows affected %d, %v; want 1", st.RowsAffected, err)
	}
	_, _, st, err = collect(t, c, driver.Query{Text: "UPDATE tobira_audited SET v = 0 -- trailing comment"})
	if err != nil || st.RowsAffected != 1 {
		t.Errorf("Query UPDATE with a trailing comment: rows affected %d, %v; want 1", st.RowsAffected, err)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM tobira_audit"); n != 8 {
		t.Errorf("audit rows = %d, want 8 (the trigger ran twice per changed row)", n)
	}
}

// A failed explicit-identity insert leaves IDENTITY_INSERT off, so the next
// insert in the same transaction can let the server assign the identity.
func TestIdentityInsertResetAfterFailure(t *testing.T) {
	c := openMS(t, false)
	ctx := ctx30(t)
	tx, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	m, _ := driver.As[driver.Mutator](tx)
	orders := driver.CollectionRef{Namespace: "sales", Name: "orders"}
	_, err = m.Insert(ctx, orders, driver.Object{{Key: "id", Value: driver.Int(7000)}, {Key: "person_id", Value: driver.String("not a number")}})
	if err == nil {
		t.Fatal("an insert with a bad person_id succeeded")
	}
	// A conversion error aborts the batch, which in SQL Server 2022 also
	// ends the transaction: the Tx reports itself dead. Otherwise the next
	// insert must work with IDENTITY_INSERT off.
	_, err = m.Insert(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(1)}, {Key: "note", Value: driver.String("idreset1")}})
	if errors.Is(err, driver.ErrConnection) {
		_ = tx.Rollback(ctx)
		_, err = c.Insert(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(1)}, {Key: "note", Value: driver.String("idreset2")}})
	}
	if err != nil {
		t.Errorf("identity insert after a failed explicit one: %v", err)
	}
	// A statement-level failure (duplicate key) keeps the transaction:
	// the next insert in it lets the server assign the identity.
	tx2, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(context.Background()) }()
	m2, _ := driver.As[driver.Mutator](tx2)
	if _, err := m2.Insert(ctx, orders, driver.Object{{Key: "id", Value: driver.Int(90001)}, {Key: "person_id", Value: driver.Int(1)}, {Key: "note", Value: driver.String("idreset3")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Insert(ctx, orders, driver.Object{{Key: "id", Value: driver.Int(90001)}, {Key: "person_id", Value: driver.Int(1)}, {Key: "note", Value: driver.String("idreset4")}}); !errors.Is(err, driver.ErrConstraint) {
		t.Fatalf("duplicate explicit id: %v", err)
	}
	if _, err := m2.Insert(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(1)}, {Key: "note", Value: driver.String("idreset5")}}); err != nil {
		t.Errorf("identity insert after a duplicate explicit one: %v", err)
	}
}

// The server's user options can turn implicit transactions on for every
// session; the read-only and writable paths still work.
func TestImplicitTransactionsUserOption(t *testing.T) {
	db := admin(t)
	mustExec(t, db, "EXEC sp_configure 'user options', 2; RECONFIGURE")
	t.Cleanup(func() { mustExec(t, db, "EXEC sp_configure 'user options', 0; RECONFIGURE") })
	c := openMS(t, false)
	for _, ro := range []bool{true, false} {
		if _, rows, _, err := collect(t, c, driver.Query{Text: "SELECT COUNT(*) AS n FROM tobira_people", ReadOnly: ro}); err != nil || len(rows) != 1 {
			t.Errorf("query (read-only %v) under implicit transactions: %v", ro, err)
		}
	}
	if _, err := c.Explain(ctx30(t), driver.Query{Text: "SELECT * FROM tobira_people"}, driver.ExplainOptions{}); err != nil {
		t.Errorf("explain under implicit transactions: %v", err)
	}
	tx, err := c.Begin(ctx30(t), driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := collect(t, tx, driver.Query{Text: "SELECT 1 AS x"}); err != nil {
		t.Errorf("read-only Tx under implicit transactions: %v", err)
	}
	_ = tx.Rollback(ctx30(t))
}
