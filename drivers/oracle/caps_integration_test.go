//go:build integration

package oracle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestMutatorIdentityAndConflicts(t *testing.T) {
	db := admin(t)
	c := openOra(t, false)
	ref := driver.CollectionRef{Name: "TOBIRA_ORDERS"}
	key, err := c.Insert(ctxT(t), ref, driver.Object{{Key: "person_id", Value: driver.Int(1)}, {Key: "TOTAL", Value: driver.Decimal("10.25")}, {Key: "NOTE", Value: driver.String("first")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM tobira_orders") })
	if len(key) != 1 || key[0].Key != "ID" || key[0].Value.IsNull() {
		t.Fatalf("identity key: %v", key)
	}
	if err := c.Update(ctxT(t), ref, key, driver.Object{{Key: "TOTAL", Value: driver.Decimal("11.00")}}); err != nil {
		t.Fatal(err)
	}
	var total string
	if err := db.QueryRow("SELECT TO_CHAR(total, 'FM990.00') FROM tobira_orders").Scan(&total); err != nil || total != "11.00" {
		t.Errorf("after update: %q %v", total, err)
	}
	// A unique violation is ErrConstraint and changes nothing.
	_, err = c.Insert(ctxT(t), ref, driver.Object{{Key: "PERSON_ID", Value: driver.Int(1)}, {Key: "NOTE", Value: driver.String("first")}})
	if !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("duplicate note: want ErrConstraint, got %v", err)
	}
	if n := count(t, db, "tobira_orders"); n != 1 {
		t.Errorf("rows after the failed insert: %d", n)
	}
	// A key matching several rows is ErrConflict and changes nothing.
	mustExec(t, db, "CREATE TABLE tobira_nokey (a NUMBER, b VARCHAR2(10))")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_nokey PURGE") })
	mustExec(t, db, "INSERT INTO tobira_nokey VALUES (1, 'x')")
	mustExec(t, db, "INSERT INTO tobira_nokey VALUES (1, 'x')")
	nk := driver.CollectionRef{Name: "tobira_nokey"}
	k := driver.Object{{Key: "A", Value: driver.Int(1)}}
	if err := c.Update(ctxT(t), nk, k, driver.Object{{Key: "B", Value: driver.String("y")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("update of two rows: want ErrConflict, got %v", err)
	}
	if err := c.Delete(ctxT(t), nk, k); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("delete of two rows: want ErrConflict, got %v", err)
	}
	if n := count(t, db, "tobira_nokey WHERE b = 'x'"); n != 2 {
		t.Errorf("rows changed by a refused mutation: %d left", n)
	}
	// A view is not a mutation target.
	if _, err := c.Insert(ctxT(t), driver.CollectionRef{Name: "tobira_people_view"}, driver.Object{{Key: "id", Value: driver.Int(5000)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("insert into a view: %v", err)
	}
	if err := c.Update(ctxT(t), ref, key, driver.Object{{Key: "nope", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown field: %v", err)
	}
}

func TestTransactions(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	c := openOra(t, false)
	q := func(s string) driver.Query { return driver.Query{Text: strings.ReplaceAll(s, "T", `"`+tbl+`"`)} }

	tx, err := c.Begin(ctxT(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"commit", "rollback", "create table x (a number)", `begin null; end;`, "call p()", "truncate table T", "set transaction read only"} {
		if err := queryErr(t, tx, q(s)); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s in a Tx: want ErrUnsupported, got %v", s, err)
		}
	}
	if _, st, _ := collect(t, tx, q(`update T set "name" = 'tx' where "id" <= 3`)); st.RowsAffected != 3 {
		t.Errorf("RowsAffected %d", st.RowsAffected)
	}
	m, _ := driver.As[driver.Mutator](tx)
	if err := m.Delete(ctxT(t), driver.CollectionRef{Name: tbl}, driver.Object{{Key: "id", Value: driver.Int(4)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.(driver.Explainer).Explain(ctxT(t), q(`select * from T where "id" = :1`), driver.ExplainOptions{}); err != nil {
		t.Errorf("Tx.Explain: %v", err)
	}
	rows, _, _ := collect(t, tx, q(`select count(*) from T where "name" = 'tx'`))
	if rows[0][0].String() != "3" {
		t.Errorf("explain undid the transaction's changes: %v", rows)
	}
	if err := tx.Rollback(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `"`+tbl+`" WHERE "name" = 'tx' OR "id" = 4`); n != 1 {
		t.Errorf("rollback left changes: %d", n)
	}
	if err := tx.Rollback(ctxT(t)); err != nil {
		t.Errorf("second Rollback: %v", err)
	}

	// A statement that ends the transaction (the check bypassed) kills it.
	tx2, err := c.Begin(ctxT(t), driver.TxOptions{Isolation: "serializable"})
	if err != nil {
		t.Fatal(err)
	}
	ot := tx2.(*oraTx)
	collect(t, tx2, q(`update T set "name" = 'gone' where "id" = 5`))
	if _, err := ot.query(ctxT(t), driver.Query{Text: "commit"}, nil); err == nil {
		t.Error("COMMIT with the check bypassed: want the transaction check to fail")
	}
	if err := queryErr(t, tx2, q("select 1 from dual")); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("a dead Tx: want ErrConnection, got %v", err)
	}
	if err := tx2.Commit(ctxT(t)); err == nil {
		t.Error("Commit of a dead Tx: want an error")
	}
	mustExec(t, db, `UPDATE "`+tbl+`" SET "name" = 'x' WHERE "id" = 5`)

	// Read-only transactions.
	rtx, err := c.Begin(ctxT(t), driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := queryErr(t, rtx, q(`update T set "name" = 'ro'`)); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("update in a read-only Tx: %v", err)
	}
	if rows, _, _ := collect(t, rtx, q("select count(*) from T")); rows[0][0].String() != "57" {
		t.Errorf("read in a read-only Tx: %v", rows)
	}
	if _, err := rtx.(driver.Explainer).Explain(ctxT(t), q("select * from T"), driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("explain in a read-only Tx: %v", err)
	}
	if m, _ := driver.As[driver.Mutator](rtx); m.Delete(ctxT(t), driver.CollectionRef{Name: tbl}, driver.Object{{Key: "id", Value: driver.Int(1)}}) == nil {
		t.Error("delete in a read-only Tx succeeded")
	}
	if err := rtx.Commit(ctxT(t)); err != nil {
		t.Errorf("read-only Commit: %v", err)
	}

	if _, err := c.Begin(ctxT(t), driver.TxOptions{Isolation: "repeatable read"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown isolation: %v", err)
	}
	ro := openOra(t, true)
	if _, err := ro.Begin(ctxT(t), driver.TxOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("writable Begin on a read-only conn: %v", err)
	}

	// Rollback with a result set still open, and Close with a Tx open.
	tx3, err := c.Begin(ctxT(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := tx3.Query(ctxT(t), driver.Query{Text: "select n from tobira_big"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := queryErr(t, tx3, q("select 1 from dual")); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("second statement with a result open: %v", err)
	}
	if err := tx3.Rollback(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	tx4, err := c.Begin(ctxT(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, tx4, q(`update T set "name" = 'closed' where "id" = 6`))
	c2, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN})
	if err != nil {
		t.Fatal(err)
	}
	tx5, err := c2.(driver.Transactor).Begin(ctxT(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, tx5, q(`update T set "name" = 'closed' where "id" = 7`))
	_ = c2.Close() // rolls tx5 back
	if n := count(t, db, `"`+tbl+`" WHERE "id" = 7 AND "name" = 'closed'`); n != 0 {
		t.Error("Close did not roll back an open Tx")
	}
	_ = tx4.Rollback(ctxT(t))
}

func TestExplain(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	c := openOra(t, false)
	p, err := c.Explain(ctxT(t), driver.Query{Text: `select * from "` + tbl + `" where "id" = :1;`, Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "Plan hash value") || p.Root == nil || p.Root.Label != "SELECT STATEMENT" || len(p.Root.Children) == 0 {
		t.Errorf("plan: %+v\n%s", p.Root, p.Text)
	}
	for _, ro := range []bool{false, true} {
		if _, err := c.Explain(ctxT(t), driver.Query{Text: `delete from "` + tbl + `"`, ReadOnly: ro}, driver.ExplainOptions{}); err != nil {
			t.Errorf("explain delete (ro %v): %v", ro, err)
		}
	}
	if n := count(t, db, `"`+tbl+`"`); n != 57 {
		t.Errorf("explain ran the delete: %d rows left", n)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "drop table x", ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("explain DDL read-only: %v", err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "begin null; end;"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("explain PL/SQL: %v", err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "select 1 from dual"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("analyze: %v", err)
	}
	ro := openOra(t, true)
	if _, err := ro.Explain(ctxT(t), driver.Query{Text: "select label from things", Namespace: "TOBIRA_NS"}, driver.ExplainOptions{}); err != nil {
		t.Errorf("namespaced explain on a read-only conn: %v", err)
	}
}

func TestBrowseExtras(t *testing.T) {
	db := admin(t)
	mustExec(t, db, `CREATE TABLE tobira_comp (a NUMBER(5), b VARCHAR2(5), v NUMBER(5), PRIMARY KEY (a, b))`)
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_comp PURGE") })
	mustExec(t, db, `INSERT INTO tobira_comp SELECT MOD(LEVEL, 4), 'k' || LPAD(LEVEL, 3, '0'), LEVEL FROM dual CONNECT BY LEVEL <= 25`)
	settle(t, db, "tobira_comp")
	c := openOra(t, true)
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		rs, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "TOBIRA_COMP"}, driver.BrowseOptions{Limit: 7, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			k := r[0].String() + "/" + r[1].String()
			if seen[k] {
				t.Errorf("key %s twice", k)
			}
			seen[k] = true
		}
		pages++
		if cursor = st.NextCursor; cursor == "" || pages > 10 {
			break
		}
	}
	if len(seen) != 25 || pages != 4 {
		t.Errorf("composite keyset: %d keys over %d pages", len(seen), pages)
	}
	rs, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Filter: driver.Filter{Raw: `"age" > 60 -- comment`}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil || len(rows) == 0 {
		t.Errorf("raw filter: %d rows, %v", len(rows), err)
	}
	if _, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Filter: driver.Filter{Raw: `"id" in (select tobira_seq.nextval from dual)`}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("raw filter with NEXTVAL on a read-only conn: %v", err)
	}
	if _, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Filter: driver.Filter{Raw: `1 = 1) union all (select 1`}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("raw filter breaking out: %v", err)
	}
	if _, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("browse missing: %v", err)
	}
}

func TestCompleteQuoting(t *testing.T) {
	c := openOra(t, false)
	got, err := c.Complete(ctxT(t), driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21})
	if err != nil {
		t.Fatal(err)
	}
	ins := map[string]string{}
	for _, x := range got {
		ins[x.Label] = x.Insert
	}
	if ins["tobira_people"] != `"tobira_people"` {
		t.Errorf("lower-case table must be quoted: %q", ins["tobira_people"])
	}
	if v, ok := ins["TOBIRA_ORDERS"]; !ok || v != "" {
		t.Errorf("upper-case table needs no quoting: %q (present %v)", v, ok)
	}
}
