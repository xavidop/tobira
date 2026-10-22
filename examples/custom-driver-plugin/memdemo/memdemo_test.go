package memdemo

// The conformance kit (in this module's main package tests) covers the
// driver contract. These tests cover what the kit does not: the query
// language's edges, the filter operators and transaction conflicts.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func openConn(t *testing.T, d *Driver) *Conn {
	t.Helper()
	c, err := d.Open(context.Background(), driver.ConnConfig{DSN: "memdemo://"})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Conn)
}

func rows(t *testing.T, rs driver.ResultSet, err error) []driver.Row {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// count runs a query on q and returns how many rows it gave.
func count(t *testing.T, q driver.Querier, text string) int {
	t.Helper()
	rs, err := q.Query(context.Background(), driver.Query{Text: text})
	return len(rows(t, rs, err))
}

func TestParse(t *testing.T) {
	params := []driver.Value{driver.Int(7)}
	for _, tc := range []struct {
		text  string
		verb  string
		field string
		value driver.Value
	}{
		{"select * from people", "select", "", driver.Value{}},
		{"SELECT * FROM people;", "select", "", driver.Value{}},
		{"select * from people where name = 'O''Brien'", "select", "name", driver.String("O'Brien")},
		{"select * from people where id=$1", "select", "id", driver.Int(7)},
		{"select * from people where score = 1.50", "select", "score", driver.Decimal("1.50")},
		{"select * from people where active = TRUE", "select", "active", driver.Bool(true)},
		{"delete from people where id = -3", "delete", "id", driver.Int(-3)},
		{"reset people", "reset", "", driver.Value{}},
	} {
		st, err := parse(tc.text, params)
		if err != nil {
			t.Errorf("parse(%q): %v", tc.text, err)
			continue
		}
		if st.verb != tc.verb || st.coll != "people" {
			t.Errorf("parse(%q) = %+v", tc.text, st)
		}
		if tc.field == "" {
			if st.where != nil {
				t.Errorf("parse(%q): unexpected where %+v", tc.text, st.where)
			}
			continue
		}
		if st.where == nil || st.where.field != tc.field || st.where.value.Type() != tc.value.Type() ||
			st.where.value.String() != tc.value.String() {
			t.Errorf("parse(%q): where %+v, want %s = %s", tc.text, st.where, tc.field, tc.value)
		}
	}
	for _, text := range []string{
		"", "   ", ";", "update people", "select name from people", "select * people",
		"select * from", "select * from people where", "select * from people where id",
		"select * from people where id = ", "select * from people where id = $2",
		"select * from people where name = 'open", "select * from people where name = bare",
		"select * from people; delete from people", "reset people where id = 1",
		"sleep", "sleep 61s", "sleep -1s", "sleep soon", "sleep 1s 2s", "sleep 'x'",
	} {
		if _, err := parse(text, params); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parse(%q): want ErrSyntax, got %v", text, err)
		}
	}
}

func TestBrowseOperators(t *testing.T) {
	c := openConn(t, New())
	ref := driver.CollectionRef{Namespace: Namespace, Name: "tobira_people"}
	browse := func(conds ...driver.Condition) []driver.Row {
		rs, err := c.Browse(context.Background(), ref, driver.BrowseOptions{
			Limit: 1000, Filter: driver.Filter{Conditions: conds},
		})
		return rows(t, rs, err)
	}
	for name, tc := range map[string]struct {
		cond driver.Condition
		want int
	}{
		"lt":       {driver.Condition{Field: "id", Op: driver.OpLt, Value: driver.Int(4)}, 3},
		"ge":       {driver.Condition{Field: "id", Op: driver.OpGe, Value: driver.Int(55)}, 3},
		"in":       {driver.Condition{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(9), driver.Int(99))}, 2},
		"prefix":   {driver.Condition{Field: "email", Op: driver.OpPrefix, Value: driver.String("user0")}, 9},
		"ne":       {driver.Condition{Field: "id", Op: driver.OpNe, Value: driver.Int(1)}, 56},
		"decimal":  {driver.Condition{Field: "score", Op: driver.OpGt, Value: driver.Decimal("0")}, 57},
		"not null": {driver.Condition{Field: "age", Op: driver.OpNotNull}, 57 - len(browse(driver.Condition{Field: "age", Op: driver.OpIsNull}))},
	} {
		if got := len(browse(tc.cond)); got != tc.want {
			t.Errorf("%s: %d rows, want %d", name, got, tc.want)
		}
	}
	if len(browse(driver.Condition{Field: "age", Op: driver.OpIsNull})) == 0 {
		t.Error("is_null: People has NULL ages, got none")
	}
	_, err := c.Browse(context.Background(), ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "id > 1"}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("raw filter: want ErrUnsupported, got %v", err)
	}
	_, err = c.Browse(context.Background(), ref, driver.BrowseOptions{Cursor: "nope"})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad cursor: want ErrSyntax, got %v", err)
	}
}

func TestTransactionConflict(t *testing.T) {
	d := New()
	c := openConn(t, d)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: Namespace, Name: "tobira_people"}
	tx, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Query(ctx, driver.Query{Text: "delete from tobira_people where id = 1"}); err != nil {
		t.Fatal(err)
	}
	// The Tx's write is invisible outside it.
	if n := count(t, c, "select * from tobira_people where id = 1"); n != 1 {
		t.Fatalf("outside the Tx: %d rows for id 1, want 1", n)
	}
	// Someone else commits first.
	if err := c.Delete(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("Commit after a concurrent write: want ErrConflict, got %v", err)
	}
	// The failed Tx is still open, so it can be rolled back.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, "select * from tobira_people where id = 1"); n != 1 {
		t.Fatalf("after the conflict: %d rows for id 1, want 1", n)
	}
	if _, err := tx.Query(ctx, driver.Query{Text: "select * from tobira_people"}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("query on a finished Tx: want ErrConflict, got %v", err)
	}

	// A Tx without writes commits even though the store moved on.
	ro, err := c.Begin(ctx, driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(3)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Query(ctx, driver.Query{Text: "reset tobira_people"}); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("write in a read-only Tx: want ErrReadOnly, got %v", err)
	}
	if err := ro.Commit(ctx); err != nil {
		t.Fatalf("Commit of a read-only Tx: %v", err)
	}
	if _, err := c.Begin(ctx, driver.TxOptions{Isolation: "serializable"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("serializable: want ErrUnsupported, got %v", err)
	}
}

func TestMutatorChecks(t *testing.T) {
	c := openConn(t, New())
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: Namespace, Name: "tobira_people"}
	for name, values := range map[string]driver.Object{
		"duplicate key": {{Key: "id", Value: driver.Int(1)}},
		"no key":        {{Key: "name", Value: driver.String("x")}},
		"wrong type":    {{Key: "id", Value: driver.String("100")}},
	} {
		if _, err := c.Insert(ctx, ref, values); !errors.Is(err, driver.ErrConstraint) {
			t.Errorf("Insert %s: want ErrConstraint, got %v", name, err)
		}
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(100)}, {Key: "nope", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Insert unknown field: want ErrNotFound, got %v", err)
	}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "id", Value: driver.Int(2)}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("Update to a taken key: want ErrConstraint, got %v", err)
	}
	if err := c.Update(ctx, ref, driver.Object{{Key: "name", Value: driver.String("x")}}, key); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Update by a non-key field: want ErrSyntax, got %v", err)
	}
	// None of the refused writes changed anything.
	if n := count(t, c, "select * from tobira_people"); n != 57 {
		t.Errorf("%d rows after refused writes, want 57", n)
	}
}

func TestComplete(t *testing.T) {
	c := openConn(t, New())
	for _, tc := range []struct {
		text, want, kind string
	}{
		{"sel", "select", "keyword"},
		{"select * from tob", "tobira_people", "collection"},
		{"select * from tobira_people where em", "email", "field"},
		{"reset ", "tobira_people", "collection"},
	} {
		got, err := c.Complete(context.Background(), driver.CompletionRequest{Text: tc.text, Cursor: len(tc.text)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || got[0].Label != tc.want || got[0].Kind != tc.kind {
			t.Errorf("Complete(%q) = %+v, want %s %q first", tc.text, got, tc.kind, tc.want)
		}
	}
}

func TestSleep(t *testing.T) {
	st, err := parse("SLEEP 1.5s;", nil)
	if err != nil || st.verb != "sleep" || st.sleep != 1500*time.Millisecond || st.writes() {
		t.Fatalf("parse(sleep 1.5s) = %+v, %v", st, err)
	}
	c := openConn(t, New())
	ctx := context.Background()

	start := time.Now()
	if n := count(t, c, "sleep 50ms"); n != 0 {
		t.Errorf("sleep returned %d rows, want none", n)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("sleep 50ms returned after %v", d)
	}

	// Cancelling ends the sleep at once, on the Conn and in a Tx.
	tx, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]driver.Querier{"conn": c, "tx": tx} {
		cctx, cancel := context.WithCancel(ctx)
		time.AfterFunc(20*time.Millisecond, cancel)
		start := time.Now()
		_, err := q.Query(cctx, driver.Query{Text: "sleep 10s"})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: cancelled sleep: want context.Canceled, got %v", name, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: cancelled sleep took %v", name, d)
		}
	}
	// The sleeping Tx did not lock anything up: it still works.
	if n := count(t, tx, "select * from tobira_people where id = 1"); n != 1 {
		t.Errorf("Tx after a cancelled sleep: %d rows, want 1", n)
	}

	plan, err := c.Explain(ctx, driver.Query{Text: "sleep 2s"}, driver.ExplainOptions{})
	if err != nil || plan.Root == nil || plan.Root.Label != "sleep" {
		t.Errorf("Explain(sleep 2s) = %+v, %v", plan, err)
	}
}
