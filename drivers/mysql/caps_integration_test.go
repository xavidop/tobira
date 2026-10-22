//go:build integration

package mysql

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"fmt"
	"time"

	"github.com/xavidop/tobira/driver"
)

var peopleRef = driver.CollectionRef{Namespace: "tobira", Name: "tobira_people"}

func browseAll(t *testing.T, c *conn, ref driver.CollectionRef, opts driver.BrowseOptions) ([]driver.Column, []driver.Row, []string) {
	t.Helper()
	var all []driver.Row
	var cols []driver.Column
	var cursors []string
	for range 1000 {
		rs, err := c.Browse(ctxT(t), ref, opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		cols = rs.Columns()
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
		if st.NextCursor == "" {
			return cols, all, cursors
		}
		cursors = append(cursors, st.NextCursor)
		opts.Cursor = st.NextCursor
	}
	t.Fatal("Browse never ended")
	return nil, nil, nil
}

func TestBrowseKeysetAndOffset(t *testing.T) {
	c := openMy(t, false)
	_, rows, cursors := browseAll(t, c, driver.CollectionRef{Name: "tobira_big"}, driver.BrowseOptions{Limit: 300})
	if len(rows) != 1000 || len(cursors) != 3 || !strings.HasPrefix(cursors[0], keysetPrefix) {
		t.Fatalf("keyset: %d rows, cursors %v", len(rows), cursors)
	}
	for i, r := range rows {
		if n, _ := r[0].AsInt(); n != int64(i+1) {
			t.Fatalf("keyset row %d = %s", i, r[0])
		}
	}
	_, rows, cursors = browseAll(t, c, driver.CollectionRef{Name: "tobira_big"}, driver.BrowseOptions{Limit: 400, Sort: []driver.SortKey{{Field: "n", Desc: true}}})
	if len(rows) != 1000 || !strings.HasPrefix(cursors[0], offsetPrefix) {
		t.Fatalf("offset: %d rows, cursors %v", len(rows), cursors)
	}
	if n, _ := rows[0][0].AsInt(); n != 1000 {
		t.Errorf("desc first = %s", rows[0][0])
	}
}

func TestBrowseWithoutPrimaryKeyCoversEveryRow(t *testing.T) {
	db := admin(t)
	mustExec(t, db, "CREATE TABLE tobira_nopk (a INT, b VARCHAR(5))")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_nopk") })
	mustExec(t, db, "INSERT INTO tobira_nopk VALUES (1,'x'),(1,'y'),(2,'x'),(NULL,'z'),(3,NULL)")
	c := openMy(t, false)
	_, rows, _ := browseAll(t, c, driver.CollectionRef{Name: "tobira_nopk"}, driver.BrowseOptions{Limit: 2})
	var got []string
	for _, r := range rows {
		got = append(got, r[0].String()+r[1].String())
	}
	slices.Sort(got)
	if want := []string{"1x", "1y", "2x", "3NULL", "NULLz"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// TestBrowseTinyint1IsBool: Browse types columns from Describe, so a
// TINYINT(1) column is Bool, while Query keeps it Int.
func TestBrowseTinyint1IsBool(t *testing.T) {
	c := openMy(t, false)
	rs, err := c.Browse(ctxT(t), peopleRef, driver.BrowseOptions{Limit: 5, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}}}})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	ai := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == "active" })
	if cols[ai].Type != driver.TypeBool {
		t.Errorf("active column type = %s, want bool", cols[ai].Type)
	}
	rows, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil || len(rows) == 0 {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	for _, r := range rows {
		if b, ok := r[ai].AsBool(); !ok || r[ai].Type() != driver.TypeBool || !b {
			t.Errorf("active = %s (%s), want bool true", r[ai], r[ai].Type())
		}
	}
	_, _, qcols := collect(t, c, driver.Query{Text: "SELECT active FROM tobira_people LIMIT 1"})
	if qcols[0].Type != driver.TypeInt {
		t.Errorf("Query keeps TINYINT(1) as Int, got %s", qcols[0].Type)
	}
}

func TestBrowseRawFilterOnReadOnlyPath(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	ref := driver.CollectionRef{Name: name}
	for _, readOnlyConn := range []bool{true, false} {
		c := openMy(t, readOnlyConn)
		rs, err := c.Browse(ctxT(t), ref, driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: "age > 40 -- trailing comment"}})
		if err != nil {
			t.Fatalf("raw filter with a comment: %v", err)
		}
		if _, _, err := driver.Collect(ctxT(t), rs, 0); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{
			"1=1) UNION SELECT * FROM " + name + " INTO OUTFILE '/tmp/x' -- ",
			"1=1; DELETE FROM " + name,
			"1=1 /*!50000 ) ; DELETE FROM " + name + " -- */",
		} {
			rs, err := c.Browse(ctxT(t), ref, driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: raw}})
			if err == nil {
				_, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if !errors.Is(err, driver.ErrReadOnly) && !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
				t.Errorf("read-only conn %v: raw %q: want a refusal, got %v", readOnlyConn, raw, err)
			}
		}
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("table changed: %s, was %s", got, before)
	}
}

func TestExplain(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	c := openMy(t, false)
	plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM " + name + " WHERE id = ?", Params: []driver.Value{driver.Int(3)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Format != "text" || !strings.Contains(plan.Text, "->") || plan.Root == nil || plan.Root.Label != "query_block" {
		t.Errorf("plan = %+v", plan)
	}
	// A single-table DELETE has no tree plan: the traditional table is shown.
	plan, err = c.Explain(ctxT(t), driver.Query{Text: "DELETE FROM " + name + " WHERE id = 1"}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Text, "select_type") || !strings.Contains(plan.Text, "DELETE") || plan.Root == nil {
		t.Errorf("DELETE plan = %+v", plan)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Analyze: want ErrUnsupported, got %v", err)
	}
	for _, text := range []string{"ANALYZE SELECT 1", "DROP TABLE " + name, "SELECT 1; DELETE FROM " + name, "SELECT /*! 1 */"} {
		if _, err := c.Explain(ctxT(t), driver.Query{Text: text}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Explain(%q): want ErrUnsupported, got %v", text, err)
		}
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("Explain changed the table: %s, was %s", got, before)
	}
}

// TestExplainOnReadOnlyPath: a SELECT is explained inside the read-only
// transaction. MySQL refuses to explain DML in a read-only transaction
// (1792), so EXPLAIN of a DELETE is ErrReadOnly and deletes nothing; a
// user-supplied EXPLAIN ANALYZE stays refused by the allow-list.
func TestExplainOnReadOnlyPath(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	for _, readOnlyConn := range []bool{true, false} {
		c := openMy(t, readOnlyConn)
		plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM " + name, ReadOnly: true}, driver.ExplainOptions{})
		if err != nil || plan.Text == "" {
			t.Fatalf("read-only conn %v: Explain(SELECT) = %+v, %v", readOnlyConn, plan, err)
		}
		for _, text := range []string{"DELETE FROM " + name, "UPDATE " + name + " SET age = 1", "INSERT INTO " + name + " (id, name, email) VALUES (9999, 'x', 'y')"} {
			_, err := c.Explain(ctxT(t), driver.Query{Text: text, ReadOnly: true}, driver.ExplainOptions{})
			var de *driver.Error
			if !errors.Is(err, driver.ErrReadOnly) || !errors.As(err, &de) || de.Code != "1792" {
				t.Errorf("read-only conn %v: Explain(%q): want ErrReadOnly from the server (1792), got %v", readOnlyConn, text, err)
			}
		}
		for _, text := range []string{"ANALYZE DELETE FROM " + name, "DROP TABLE " + name} {
			if _, err := c.Explain(ctxT(t), driver.Query{Text: text, ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("read-only conn %v: Explain(%q): want ErrReadOnly, got %v", readOnlyConn, text, err)
			}
		}
		if err := queryErr(t, c, driver.Query{Text: "EXPLAIN ANALYZE DELETE FROM " + name, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only conn %v: user EXPLAIN ANALYZE DELETE: want ErrReadOnly, got %v", readOnlyConn, err)
		}
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("table changed: %s, was %s", got, before)
	}
}

func personRow(id int64, email string) driver.Object {
	return driver.Object{
		{Key: "id", Value: driver.Int(id)}, {Key: "name", Value: driver.String("Probe")},
		{Key: "email", Value: driver.String(email)}, {Key: "active", Value: driver.Bool(true)},
	}
}

func TestMutator(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	ref := driver.CollectionRef{Name: name}
	c := openMy(t, false)
	ctx := ctxT(t)

	key, err := c.Insert(ctx, ref, personRow(5000, "p5000@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 1 || key[0].Key != "id" || key[0].Value.String() != "5000" {
		t.Errorf("supplied key = %v", key)
	}
	k := driver.Object{{Key: "id", Value: driver.Int(5000)}}
	if err := c.Update(ctx, ref, k, driver.Object{{Key: "name", Value: driver.String("Renamed")}}); err != nil {
		t.Fatal(err)
	}
	// Same values again: MySQL reports 0 rows affected, but one row matched.
	if err := c.Update(ctx, ref, k, driver.Object{{Key: "name", Value: driver.String("Renamed")}}); err != nil {
		t.Errorf("Update with unchanged values: %v", err)
	}
	var got string
	if err := db.QueryRow("SELECT name FROM " + name + " WHERE id = 5000").Scan(&got); err != nil || got != "Renamed" {
		t.Errorf("name = %q, %v", got, err)
	}
	missing := driver.Object{{Key: "id", Value: driver.Int(999999)}}
	if err := c.Update(ctx, ref, missing, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Update(missing): %v", err)
	}
	if err := c.Delete(ctx, ref, missing); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Delete(missing): %v", err)
	}

	// A key that matches several rows changes nothing.
	mustExec(t, db, "UPDATE "+name+" SET age = 77 WHERE id IN (2, 3)")
	before := tableState(t, db, name)
	many := driver.Object{{Key: "age", Value: driver.Int(77)}}
	if err := c.Update(ctx, ref, many, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Update(two rows): want ErrConflict, got %v", err)
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("a conflicting update changed the table: %s, was %s", got, before)
	}
	mustExec(t, db, "UPDATE "+name+" SET name = 'same' WHERE id IN (2, 3)")
	before = tableState(t, db, name)
	if err := c.Update(ctx, ref, many, driver.Object{{Key: "name", Value: driver.String("same")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Update(two unchanged rows): want ErrConflict, got %v", err)
	}
	if err := c.Delete(ctx, ref, many); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Delete(two rows): want ErrConflict, got %v", err)
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("a conflicting mutation changed the table: %s, was %s", got, before)
	}

	if err := c.Delete(ctx, ref, k); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Insert(ctx, ref, personRow(1, "dup@example.com")); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("Insert(duplicate key): want ErrConstraint, got %v", err)
	}
	if _, err := c.Insert(ctx, driver.CollectionRef{Name: "tobira_people_view"}, personRow(1, "x")); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Insert into a view: want ErrUnsupported, got %v", err)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "nope", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Insert(unknown field): want ErrNotFound, got %v", err)
	}
}

func TestMutatorAutoIncrementKey(t *testing.T) {
	db := admin(t)
	c := openMy(t, false)
	ctx := ctxT(t)
	ref := driver.CollectionRef{Name: "tobira_orders"}
	key, err := c.Insert(ctx, ref, driver.Object{{Key: "person_id", Value: driver.Int(1)}, {Key: "total", Value: driver.Decimal("9.99")}, {Key: "note", Value: driver.String("t5-a")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(t, db, "DELETE FROM tobira_orders WHERE person_id = 1 AND total IN (9.99, 1.25)") })
	var max int64
	if err := db.QueryRow("SELECT MAX(id) FROM tobira_orders").Scan(&max); err != nil {
		t.Fatal(err)
	}
	if len(key) != 1 || key[0].Key != "id" {
		t.Fatalf("key = %v", key)
	}
	if n, _ := key[0].Value.AsInt(); n != max || n == 0 {
		t.Errorf("auto-increment key = %s, want LAST_INSERT_ID %d", key[0].Value, max)
	}
	key, err = c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Null()}, {Key: "person_id", Value: driver.Int(1)}, {Key: "total", Value: driver.Decimal("1.25")}, {Key: "note", Value: driver.String("t5-b")}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := key[0].Value.AsInt(); n != max+1 {
		t.Errorf("NULL id: key = %v, want %d", key, max+1)
	}
}

// TestMutatorReadOnlySendsNothing: on a read-only connection the Mutator
// refuses before taking a connection. The pool is closed underneath, so any
// statement attempt would fail with a different error.
func TestMutatorReadOnlySendsNothing(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	mc := c.(*conn)
	_ = mc.db.Close()
	defer func() { _ = c.Close() }()
	ctx := ctxT(t)
	k := driver.Object{{Key: "id", Value: driver.Int(1)}}
	_, insErr := mc.Insert(ctx, peopleRef, personRow(1, "x"))
	for name, err := range map[string]error{
		"Insert": insErr,
		"Update": mc.Update(ctx, peopleRef, k, driver.Object{{Key: "name", Value: driver.String("x")}}),
		"Delete": mc.Delete(ctx, peopleRef, k),
	} {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s: want ErrReadOnly, got %v", name, err)
		}
	}
	if _, err := mc.Begin(ctx, driver.TxOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Begin(read-write) on a read-only conn: want ErrReadOnly, got %v", err)
	}
}

func begin(t *testing.T, c *conn, opts driver.TxOptions) *myTx {
	t.Helper()
	tx, err := c.Begin(ctxT(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx.(*myTx)
}

// TestTxRefusesImplicitCommits: on a writable Tx, statements that would
// commit implicitly are refused before they are sent, and the Tx stays open.
func TestTxRefusesImplicitCommits(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	if _, st, _ := collect(t, tx, driver.Query{Text: "UPDATE " + name + " SET age = 1 WHERE id = 1"}); st.RowsAffected != 1 {
		t.Fatalf("rows affected = %d", st.RowsAffected)
	}
	for _, text := range []string{
		"CREATE TABLE tobira_tx_probe (a INT)", "COMMIT", "commit", "/* x */ COMMIT", "ROLLBACK", "START TRANSACTION",
		"BEGIN", "SAVEPOINT s", "SET autocommit = 1", "LOCK TABLES " + name + " WRITE", "TRUNCATE TABLE " + name,
		"DROP TABLE " + name, "CALL nope()", "/*!50000 COMMIT */",
	} {
		if err := queryErr(t, tx, driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Tx.Query(%q): want ErrUnsupported, got %v", text, err)
		}
	}
	// Still the same transaction: the update is visible inside, invisible outside.
	rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT age FROM " + name + " WHERE id = 1"})
	if n, _ := rows[0][0].AsInt(); n != 1 {
		t.Errorf("age inside the Tx = %s, want 1", rows[0][0])
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("uncommitted change visible outside the Tx: %s, was %s", got, before)
	}
	if err := tx.Rollback(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("after Rollback: %s, was %s", got, before)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_NAME = 'tobira_tx_probe'").Scan(&n); err != nil || n != 0 {
		t.Errorf("CREATE TABLE ran: %d, %v", n, err)
	}
}

// TestTxPostCheckFailsClosed skips the statement check to prove the layer
// behind it: a statement that ends the transaction kills the Tx.
func TestTxPostCheckFailsClosed(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	c := openMy(t, false)
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
		if !tc.readOnly {
			if err := tx.Update(ctxT(t), driver.CollectionRef{Name: name}, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "age", Value: driver.Int(2)}}); !errors.Is(err, driver.ErrConnection) {
				t.Errorf("%q: a dead Tx must refuse mutations, got %v", tc.text, err)
			}
		}
		if err := tx.Commit(ctxT(t)); err == nil || !strings.Contains(err.Error(), "the transaction was rolled back") {
			t.Errorf("ro %v %q: Commit of a dead Tx: want an error saying it was rolled back, got %v", tc.readOnly, tc.text, err)
		}
	}
	mustExec(t, db, "DROP TABLE IF EXISTS tobira_tx_escape")
}

func TestTxReadOnly(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	for _, readOnlyConn := range []bool{true, false} {
		c := openMy(t, readOnlyConn)
		tx := begin(t, c, driver.TxOptions{ReadOnly: true})
		rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT COUNT(*) FROM " + name})
		if n, _ := rows[0][0].AsInt(); n != 57 {
			t.Errorf("count = %s", rows[0][0])
		}
		for _, text := range []string{"DELETE FROM " + name, "CREATE TABLE x (a INT)", "COMMIT", "SET SESSION TRANSACTION READ WRITE", "SELECT 1 /*!, 2 */"} {
			if err := queryErr(t, tx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("read-only Tx %q: want ErrReadOnly, got %v", text, err)
			}
		}
		m, ok := driver.As[driver.Mutator](driver.Tx(tx))
		if !ok {
			t.Fatal("Tx is not a Mutator")
		}
		if err := m.Delete(ctxT(t), driver.CollectionRef{Name: name}, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only Tx Delete: want ErrReadOnly, got %v", err)
		}
		if err := tx.Commit(ctxT(t)); err != nil {
			t.Errorf("Commit of a read-only Tx: %v", err)
		}
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("table changed: %s, was %s", got, before)
	}
}

func TestTxMutatorAndCommit(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	ref := driver.CollectionRef{Name: name}
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{Isolation: "serializable"})
	m, ok := driver.As[driver.Mutator](driver.Tx(tx))
	if !ok {
		t.Fatal("Tx is not a Mutator")
	}
	ctx := ctxT(t)
	if _, err := m.Insert(ctx, ref, personRow(6000, "p6000@example.com")); err != nil {
		t.Fatal(err)
	}
	// A failed mutation inside the Tx leaves it usable.
	mustExec(t, db, "UPDATE "+name+" SET age = 88 WHERE id IN (4, 5)")
	if err := m.Delete(ctx, ref, driver.Object{{Key: "age", Value: driver.Int(88)}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Delete(two rows) in Tx: want ErrConflict, got %v", err)
	}
	if err := m.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(6000)}}, driver.Object{{Key: "name", Value: driver.String("InTx")}}); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT (SELECT TRX_ISOLATION_LEVEL FROM information_schema.INNODB_TRX WHERE TRX_MYSQL_THREAD_ID = CONNECTION_ID()), name FROM " + name + " WHERE id = 6000"})
	if rows[0][0].String() != "SERIALIZABLE" || rows[0][1].String() != "InTx" {
		t.Errorf("inside the Tx: %v", rows[0])
	}
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM " + name + " WHERE id = 6000").Scan(&n)
	if n != 0 {
		t.Error("uncommitted insert visible outside the Tx")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("Rollback after Commit: %v", err)
	}
	if _, err := tx.Query(ctx, driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Query after Commit: %v", err)
	}
	var got string
	if err := db.QueryRow("SELECT name FROM " + name + " WHERE id = 6000").Scan(&got); err != nil || got != "InTx" {
		t.Errorf("after Commit: %q, %v", got, err)
	}
	if _, err := c.Begin(ctx, driver.TxOptions{Isolation: "snapshot"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown isolation: %v", err)
	}
}

func TestTxOpenResultSet(t *testing.T) {
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	rs, err := tx.Query(ctxT(t), driver.Query{Text: "SELECT n FROM tobira_big"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Query(ctxT(t), driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("second Query with a result set open: want ErrUnsupported, got %v", err)
	}
	if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
		t.Fatalf("Next: %v %v", ok, err)
	}
	if err := rs.Close(); err != nil { // drains: the Tx connection survives
		t.Fatal(err)
	}
	rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT 1"})
	if len(rows) != 1 {
		t.Errorf("after Close: %v", rows)
	}
	rs, err = tx.Query(ctxT(t), driver.Query{Text: "SELECT n FROM tobira_big"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctxT(t)); err != nil { // closes the open result set (bounded), then commits
		t.Errorf("Commit with a result set open: %v", err)
	}
	_ = rs.Close()
}

func TestCloseRollsBackOpenTx(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.(driver.Transactor).Begin(ctxT(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queryErr(t, tx, driver.Query{Text: "DELETE FROM " + name}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("Close did not roll back: %s, was %s", got, before)
	}
}

func TestComplete(t *testing.T) {
	db := admin(t)
	c := openMy(t, false)
	get := func(text string) []driver.Completion {
		t.Helper()
		got, err := c.Complete(ctxT(t), driver.CompletionRequest{Text: text, Cursor: len(text)})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	has := func(cs []driver.Completion, label string) *driver.Completion {
		for i := range cs {
			if cs[i].Label == label {
				return &cs[i]
			}
		}
		return nil
	}
	if has(get("select * from tobira_"), "tobira_people") == nil {
		t.Error("no tobira_people after from")
	}
	if has(get("select * from tobira_people where na"), "name") == nil {
		t.Error("no name field")
	}
	if has(get("select * from tobira_ns."), "things") == nil {
		t.Error("no things in tobira_ns")
	}
	nss := get("select * from ")
	for _, sys := range []string{"mysql", "sys", "information_schema", "performance_schema"} {
		if has(nss, sys) != nil {
			t.Errorf("system database %s offered", sys)
		}
	}
	_ = get("select * from tobira") // fills the cache
	mustExec(t, db, "CREATE TABLE `tobira weird` (`select` INT)")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE `tobira weird`") })
	if has(get("select * from tobira"), "tobira weird") != nil {
		t.Error("the cached catalog already shows the new table")
	}
	c.Invalidate()
	w := has(get("select * from tobira"), "tobira weird")
	if w == nil || w.Insert != "`tobira weird`" {
		t.Errorf("after Invalidate: %+v", w)
	}
	f := has(get("select * from `tobira weird` where "), "select")
	if f == nil || f.Insert != "`select`" {
		t.Errorf("reserved column: %+v", f)
	}
}

// TestDeleteStopsAtTwoRows: a key matching many rows is detected after two.
func TestDeleteStopsAtTwoRows(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	mustExec(t, db, "UPDATE "+name+" SET age = 99 WHERE id <= 6")
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	if err := tx.Delete(ctxT(t), driver.CollectionRef{Name: name}, driver.Object{{Key: "age", Value: driver.Int(99)}}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("Delete(six rows): want ErrConflict, got %v", err)
	}
	rows, _, _ := collect(t, tx, driver.Query{Text: "SHOW SESSION STATUS LIKE 'Handler_delete'"})
	if n := rows[0][1].String(); n != "2" {
		t.Errorf("rows deleted before the conflict was found: %s, want 2", n)
	}
}

// keysetTables are primary-key shapes keyset paging must page through
// without skipping or repeating rows.
func TestBrowseKeysetShapes(t *testing.T) {
	db := admin(t)
	c := openMy(t, false)
	for _, tc := range []struct {
		name, create, insert string
		keyset               bool
	}{
		{"composite", "a INT, b VARCHAR(10), v INT, PRIMARY KEY (a, b)",
			"SELECT n % 7, CONCAT('k', n % 13), n FROM tobira_big WHERE n <= 91", true},
		{"string", "s VARCHAR(20) COLLATE utf8mb4_bin PRIMARY KEY, v INT",
			"SELECT CONCAT(IF(n % 2 = 0, 'B', 'a'), LPAD(n, 4, '0')), n FROM tobira_big WHERE n <= 95", true},
		{"datetime", "d DATETIME(6) PRIMARY KEY, v INT",
			"SELECT TIMESTAMP('2026-01-01') + INTERVAL n * 1001 MICROSECOND, n FROM tobira_big WHERE n <= 95", true},
		{"float", "f FLOAT PRIMARY KEY, v INT",
			"SELECT n / 10, n FROM tobira_big WHERE n <= 95", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := "tobira_keyset_" + tc.name
			mustExec(t, db, "CREATE TABLE "+table+" ("+tc.create+")")
			t.Cleanup(func() { mustExec(t, db, "DROP TABLE "+table) })
			mustExec(t, db, "INSERT INTO "+table+" "+tc.insert)
			var want int
			if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&want); err != nil {
				t.Fatal(err)
			}
			cols, rows, cursors := browseAll(t, c, driver.CollectionRef{Name: table}, driver.BrowseOptions{Limit: 10})
			vi := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == "v" })
			seen := map[string]bool{}
			for _, r := range rows {
				if seen[r[vi].String()] {
					t.Errorf("row v=%s returned twice", r[vi])
				}
				seen[r[vi].String()] = true
			}
			if len(seen) != want || len(rows) != want {
				t.Errorf("saw %d distinct of %d rows (%d returned)", len(seen), want, len(rows))
			}
			if len(cursors) > 0 && (cursors[0][:2] == keysetPrefix) != tc.keyset {
				t.Errorf("cursor %q: keyset paging = %v, want %v", cursors[0], !tc.keyset, tc.keyset)
			}
		})
	}
}

// TestBrowseSortOnNullableTies: OFFSET paging sorted on a column full of
// NULLs and ties still returns every row exactly once.
func TestBrowseSortOnNullableTies(t *testing.T) {
	db := admin(t)
	mustExec(t, db, "CREATE TABLE tobira_ties (id INT PRIMARY KEY, g INT NULL)")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_ties") })
	mustExec(t, db, "INSERT INTO tobira_ties SELECT n, IF(n % 3 = 0, NULL, n % 4) FROM tobira_big WHERE n <= 97")
	c := openMy(t, false)
	for _, desc := range []bool{false, true} {
		_, rows, _ := browseAll(t, c, driver.CollectionRef{Name: "tobira_ties"}, driver.BrowseOptions{Limit: 7, Sort: []driver.SortKey{{Field: "g", Desc: desc}}})
		ids := map[string]bool{}
		for _, r := range rows {
			ids[r[0].String()] = true
		}
		if len(rows) != 97 || len(ids) != 97 {
			t.Errorf("desc %v: %d rows, %d distinct, want 97", desc, len(rows), len(ids))
		}
		firstNull := rows[0][1].IsNull()
		if firstNull != desc {
			t.Errorf("desc %v: NULLs first = %v", desc, firstNull)
		}
	}
}

func TestNamespacesExcludeSystemDatabases(t *testing.T) {
	c := openMy(t, false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range nss {
		names = append(names, n.Name)
	}
	for _, sys := range []string{"information_schema", "mysql", "performance_schema", "sys"} {
		if slices.Contains(names, sys) {
			t.Errorf("system database %s listed: %v", sys, names)
		}
	}
	if !slices.Contains(names, "tobira") {
		t.Errorf("tobira missing: %v", names)
	}
}

// TestBrowseRawFilterUsesSessionMode: under NO_BACKSLASH_ESCAPES a raw
// filter that only balances under the default mode is refused.
func TestBrowseRawFilterUsesSessionMode(t *testing.T) {
	raw := `name = 'x\') OR (1=1 -- '`
	for _, ro := range []bool{false, true} {
		c := openDSN(t, testDSN+"?sql_mode=%27NO_BACKSLASH_ESCAPES%27", ro)
		rs, err := c.Browse(ctxT(t), peopleRef, driver.BrowseOptions{Filter: driver.Filter{Raw: raw}})
		if err == nil {
			_ = rs.Close()
		}
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("read-only %v: want ErrSyntax, got %v", ro, err)
		}
	}
}

// TestStatsDuringBlockedNext: Stats answers while Next waits on the network.
func TestStatsDuringBlockedNext(t *testing.T) {
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	rs, err := tx.Query(ctxT(t), driver.Query{Text: "SELECT n, REPEAT('x', 40000), SLEEP(IF(n = 3, 2, 0)) FROM tobira_big WHERE n <= 3"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	started := make(chan struct{})
	early := make(chan error, 1) // Next ended before the third row
	go func() {
		for i := 0; ; i++ {
			if i == 2 {
				close(started) // two rows read; the third waits two seconds on the server
			}
			if _, ok, err := rs.Next(context.Background()); !ok || err != nil {
				if i < 2 {
					early <- fmt.Errorf("Next ended at row %d: ok=%v err=%v", i+1, ok, err)
				}
				return
			}
		}
	}()
	select {
	case <-started:
	case err := <-early:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first two rows did not arrive in 10s")
	}
	time.Sleep(200 * time.Millisecond)
	within(t, "Stats during a blocked Next", 100*time.Millisecond, func() { _ = rs.Stats() })
	_ = rs.Close()
}
