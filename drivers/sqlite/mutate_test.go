package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func idKey(id int64) driver.Object { return driver.Object{{Key: "id", Value: driver.Int(id)}} }

func mutator(t *testing.T, c driver.Conn) driver.Mutator {
	t.Helper()
	m, ok := driver.As[driver.Mutator](c)
	if !ok {
		t.Fatal("conn is not a Mutator")
	}
	return m
}

func nameOf(t *testing.T, q driver.Querier, id int64) (string, bool) {
	t.Helper()
	rs, err := q.Query(context.Background(), driver.Query{Text: "select name from tobira_people where id = ?", Params: []driver.Value{driver.Int(id)}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return "", false
	}
	s, _ := rows[0][0].AsText()
	return s, true
}

func TestInfoCapabilities(t *testing.T) {
	got := New().Info().Capabilities
	for _, want := range []driver.Capability{driver.CapMutator, driver.CapTransactor} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("Info().Capabilities lacks %s: %v", want, got)
		}
	}
}

func TestMutatorRoundTrip(t *testing.T) {
	c := openTemp(t, false)
	m, ctx := mutator(t, c), context.Background()
	key, err := m.Insert(ctx, peopleRef, driver.Object{
		{Key: "name", Value: driver.String("Bobby'); drop table tobira_people;--")},
		{Key: "email", Value: driver.String("bobby@example.com")},
		{Key: "score", Value: driver.Decimal("1.50")},
		{Key: "active", Value: driver.Bool(true)},
		{Key: "created_at", Value: driver.String("2026-01-01 00:00:00")},
		{Key: "tags", Value: driver.JSON([]byte(`[]`))},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := key.Get("id")
	if !ok {
		t.Fatalf("Insert did not return the generated id: %v", key)
	}
	n, _ := id.AsInt()
	if got, ok := nameOf(t, c, n); !ok || got != "Bobby'); drop table tobira_people;--" {
		t.Fatalf("inserted name = %q (present %v)", got, ok)
	}
	if err := m.Update(ctx, peopleRef, key, driver.Object{{Key: "name", Value: driver.String("Rob")}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := nameOf(t, c, n); got != "Rob" {
		t.Fatalf("after Update name = %q", got)
	}
	if err := m.Delete(ctx, peopleRef, key); err != nil {
		t.Fatal(err)
	}
	if _, ok := nameOf(t, c, n); ok {
		t.Fatal("row still present after Delete")
	}
}

func TestMutatorInsertReturnsSuppliedKey(t *testing.T) {
	c := openTemp(t, false)
	m := mutator(t, c)
	key, err := m.Insert(context.Background(), peopleRef, driver.Object{
		{Key: "id", Value: driver.Int(5000)}, {Key: "name", Value: driver.String("x")}, {Key: "email", Value: driver.String("x@example.com")},
		{Key: "score", Value: driver.Decimal("1")}, {Key: "active", Value: driver.Bool(true)},
		{Key: "created_at", Value: driver.String("2026-01-01")}, {Key: "tags", Value: driver.JSON([]byte(`[]`))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := key.Get("id"); v.String() != "5000" {
		t.Fatalf("key = %v", key)
	}
}

func TestMutatorErrors(t *testing.T) {
	c := openTemp(t, false)
	m, ctx := mutator(t, c), context.Background()
	run(t, c, driver.Query{Text: "create table keyless (a text, b text)"})
	run(t, c, driver.Query{Text: "insert into keyless values ('x','1'), ('x','2')"})
	keyless := driver.CollectionRef{Namespace: "main", Name: "keyless"}
	one := driver.Object{{Key: "name", Value: driver.String("y")}}

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"update missing row", m.Update(ctx, peopleRef, idKey(999999), one), driver.ErrNotFound},
		{"delete missing row", m.Delete(ctx, peopleRef, idKey(999999)), driver.ErrNotFound},
		{"unknown collection", m.Update(ctx, driver.CollectionRef{Name: "nope"}, idKey(1), one), driver.ErrNotFound},
		{"unknown key field", m.Delete(ctx, peopleRef, driver.Object{{Key: `id" or 1=1 --`, Value: driver.Int(1)}}), driver.ErrNotFound},
		{"unknown change field", m.Update(ctx, peopleRef, idKey(1), driver.Object{{Key: `x" = 1 --`, Value: driver.Int(1)}}), driver.ErrNotFound},
		{"empty key", m.Delete(ctx, peopleRef, nil), driver.ErrSyntax},
		{"empty changes", m.Update(ctx, peopleRef, idKey(1), nil), driver.ErrSyntax},
		{"multi-row update", m.Update(ctx, keyless, driver.Object{{Key: "a", Value: driver.String("x")}}, driver.Object{{Key: "b", Value: driver.String("z")}}), driver.ErrConflict},
		{"multi-row delete", m.Delete(ctx, keyless, driver.Object{{Key: "a", Value: driver.String("x")}}), driver.ErrConflict},
		{"constraint", m.Update(ctx, peopleRef, idKey(1), driver.Object{{Key: "name", Value: driver.Null()}}), driver.ErrConstraint},
	}
	for _, tc := range tests {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.err, tc.want)
		}
	}
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from keyless where b in ('1','2')"})
	if n, _ := rows[0][0].AsInt(); n != 2 {
		t.Errorf("multi-row conflict must roll back: keyless rows = %d, want 2", n)
	}
	rows, _, _ = run(t, c, driver.Query{Text: "select count(*) from tobira_people"})
	if n, _ := rows[0][0].AsInt(); n != 57 {
		t.Errorf("people rows = %d, want 57", n)
	}
}

func TestMutatorNullKey(t *testing.T) {
	c := openTemp(t, false)
	m, ctx := mutator(t, c), context.Background()
	run(t, c, driver.Query{Text: "create table keyless (a text, b text)"})
	run(t, c, driver.Query{Text: "insert into keyless values (null, 'only')"})
	keyless := driver.CollectionRef{Namespace: "main", Name: "keyless"}
	if err := m.Update(ctx, keyless, driver.Object{{Key: "a", Value: driver.Null()}}, driver.Object{{Key: "b", Value: driver.String("changed")}}); err != nil {
		t.Fatalf("null key must match with IS NULL: %v", err)
	}
}

func TestMutatorReadOnlyConn(t *testing.T) {
	c := openTemp(t, true)
	m, ctx := mutator(t, c), context.Background()
	_, ierr := m.Insert(ctx, peopleRef, driver.Object{{Key: "name", Value: driver.String("x")}})
	for name, err := range map[string]error{
		"insert": ierr,
		"update": m.Update(ctx, peopleRef, idKey(1), driver.Object{{Key: "name", Value: driver.String("x")}}),
		"delete": m.Delete(ctx, peopleRef, idKey(1)),
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
	tx, err := tr.Begin(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestTxCommitAndRollback(t *testing.T) {
	c := openTemp(t, false)
	ctx := context.Background()
	for _, commit := range []bool{false, true} {
		tx := begin(t, c, driver.TxOptions{})
		m, ok := driver.As[driver.Mutator](tx)
		if !ok {
			t.Fatal("Tx is not a Mutator")
		}
		if err := m.Update(ctx, peopleRef, idKey(2), driver.Object{{Key: "name", Value: driver.String("in-tx")}}); err != nil {
			t.Fatal(err)
		}
		runTx(t, tx, driver.Query{Text: "update tobira_people set age = 99 where id = 2"})
		if got, _ := nameOf(t, tx, 2); got != "in-tx" {
			t.Errorf("Tx must see its own writes, got %q", got)
		}
		if got, _ := nameOf(t, c, 2); got == "in-tx" {
			t.Error("uncommitted write visible outside the Tx")
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		} else if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("Rollback after finish must be a no-op: %v", err)
		}
		got, _ := nameOf(t, c, 2)
		if (got == "in-tx") != commit {
			t.Errorf("commit=%v: name = %q", commit, got)
		}
	}
}

func TestTxMultiRowKeepsTxUsable(t *testing.T) {
	c := openTemp(t, false)
	ctx := context.Background()
	run(t, c, driver.Query{Text: "create table keyless (a text, b text)"})
	run(t, c, driver.Query{Text: "insert into keyless values ('x','1'), ('x','2')"})
	tx := begin(t, c, driver.TxOptions{})
	m, _ := driver.As[driver.Mutator](tx)
	if err := m.Update(ctx, peopleRef, idKey(3), driver.Object{{Key: "name", Value: driver.String("kept")}}); err != nil {
		t.Fatal(err)
	}
	err := m.Delete(ctx, driver.CollectionRef{Name: "keyless"}, driver.Object{{Key: "a", Value: driver.String("x")}})
	if !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Tx must survive a conflicting statement: %v", err)
	}
	if got, _ := nameOf(t, c, 3); got != "kept" {
		t.Errorf("earlier write lost: %q", got)
	}
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from keyless"})
	if n, _ := rows[0][0].AsInt(); n != 2 {
		t.Errorf("conflicting delete leaked: %d rows", n)
	}
}

func TestTxReadOnly(t *testing.T) {
	ctx := context.Background()
	for _, readOnlyConn := range []bool{false, true} {
		c := openTemp(t, readOnlyConn)
		tx := begin(t, c, driver.TxOptions{ReadOnly: true})
		if got, ok := nameOf(t, tx, 1); !ok || got == "" {
			t.Errorf("read-only Tx cannot read: %q %v", got, ok)
		}
		for _, text := range []string{
			"update tobira_people set name = 'x'", "delete from tobira_people", "commit", "end", "rollback", "pragma user_version = 5",
			"attach database ':memory:' as m", "insert into tobira_people select * from tobira_people",
		} {
			if _, err := tx.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("readOnlyConn=%v: %q in a read-only Tx: want ErrReadOnly, got %v", readOnlyConn, text, err)
			}
		}
		m, ok := driver.As[driver.Mutator](tx)
		if ok {
			if err := m.Delete(ctx, peopleRef, idKey(1)); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("Mutator on a read-only Tx: %v", err)
			}
		}
		if _, err := tx.Query(ctx, driver.Query{Text: "select 1; select 2"}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("multi-statement in Tx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Error(err)
		}
	}
}

func TestTxQuerySingleStatement(t *testing.T) {
	c := openTemp(t, false)
	tx := begin(t, c, driver.TxOptions{})
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Query(context.Background(), driver.Query{Text: "update tobira_people set name='a'; delete from tobira_people"}); !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("want ErrSyntax, got %v", err)
	}
}

func TestBeginOnReadOnlyConnRefusesWrites(t *testing.T) {
	c := openTemp(t, true)
	tr, _ := driver.As[driver.Transactor](c)
	if _, err := tr.Begin(context.Background(), driver.TxOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
}

func TestBeginIsolation(t *testing.T) {
	c := openTemp(t, false)
	tr, _ := driver.As[driver.Transactor](c)
	if _, err := tr.Begin(context.Background(), driver.TxOptions{Isolation: "read committed"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	tx, err := tr.Begin(context.Background(), driver.TxOptions{Isolation: "serializable"})
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(context.Background())
}

func TestCloseRollsBackOpenTx(t *testing.T) {
	path := seedFile(t)
	open := func() driver.Conn {
		c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + path})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	tx := begin(t, c, driver.TxOptions{})
	runTx(t, tx, driver.Query{Text: "update tobira_people set name = 'lost' where id = 4"})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Query(context.Background(), driver.Query{Text: "select 1"}); err == nil {
		t.Error("Tx must be unusable after Conn.Close")
	}
	if err := tx.Commit(context.Background()); err == nil {
		t.Error("Commit after Conn.Close must fail")
	}
	c2 := open()
	defer func() { _ = c2.Close() }()
	if got, _ := nameOf(t, c2, 4); got == "lost" {
		t.Error("Close must roll back the open Tx")
	}
	// The write lock must be free again.
	if err := mutator(t, c2).Update(context.Background(), peopleRef, idKey(4), driver.Object{{Key: "name", Value: driver.String("ok")}}); err != nil {
		t.Fatal(err)
	}
}

func TestTxHoldsWriteLock(t *testing.T) {
	path := seedFile(t)
	cfg := driver.ConnConfig{DSN: "sqlite:" + path, Options: map[string]string{"busy_timeout": "0"}}
	a, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	tx := begin(t, a, driver.TxOptions{})
	defer func() { _ = tx.Rollback(context.Background()) }()
	trB, _ := driver.As[driver.Transactor](b)
	if _, err := trB.Begin(context.Background(), driver.TxOptions{}); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("second writer: want ErrConflict (BEGIN IMMEDIATE), got %v", err)
	}
}

func runTx(t *testing.T, q driver.Querier, query driver.Query) {
	t.Helper()
	rs, err := q.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query.Text, err)
	}
	if _, _, err := driver.Collect(context.Background(), rs, 0); err != nil {
		t.Fatal(err)
	}
}

func TestTxControlRefused(t *testing.T) {
	texts := []string{"commit", ";commit", "/**/;end", " ; commit", "begin", ";;rollback", "rollback transaction", "END TRANSACTION"}
	for _, ro := range []bool{false, true} {
		c := openTemp(t, false)
		want := driver.ErrUnsupported
		if ro {
			want = driver.ErrReadOnly
		}
		tx := begin(t, c, driver.TxOptions{ReadOnly: ro})
		if !ro {
			runTx(t, tx, driver.Query{Text: "update tobira_people set name = 'gone' where id = 5"})
		}
		for _, text := range texts {
			if _, err := tx.Query(context.Background(), driver.Query{Text: text}); !errors.Is(err, want) {
				t.Errorf("readOnly=%v %q: want %v, got %v", ro, text, want, err)
			}
		}
		if !ro { // nested savepoints are still allowed
			runTx(t, tx, driver.Query{Text: "savepoint a"})
			runTx(t, tx, driver.Query{Text: "rollback to a"})
			runTx(t, tx, driver.Query{Text: "release a"})
		}
		if err := tx.Rollback(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got, _ := nameOf(t, c, 5); got == "gone" {
			t.Errorf("readOnly=%v: write persisted despite Rollback", ro)
		}
	}
}

func TestTxEscapeFailsClosed(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openTemp(t, false)
		tx := begin(t, c, driver.TxOptions{ReadOnly: ro}).(*tx)
		// Bypass the keyword refusal: a statement that ends the transaction.
		rs, err := execOn(context.Background(), tx.sc, false, true, driver.Query{Text: "commit"}, time.Now())
		if err == nil {
			_ = rs.Close()
		}
		if _, err := tx.Query(context.Background(), driver.Query{Text: "update tobira_people set name = 'pwned'"}); err == nil {
			t.Errorf("readOnly=%v: a statement ran after the Tx escaped", ro)
		}
		if err := tx.Commit(context.Background()); err == nil {
			t.Errorf("readOnly=%v: Commit must fail after the Tx escaped", ro)
		}
		_ = tx.Rollback(context.Background())
		if got, _ := nameOf(t, c, 1); got == "pwned" {
			t.Error("write persisted")
		}
	}
}

func TestTxOpenResultSetsDoNotBlock(t *testing.T) {
	within := func(name string, f func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { f(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s hung with an open Tx ResultSet", name)
		}
	}
	for _, how := range []string{"close", "commit", "rollback"} {
		c := openTemp(t, false)
		tx := begin(t, c, driver.TxOptions{})
		if _, err := tx.Query(context.Background(), driver.Query{Text: "select * from tobira_people"}); err != nil {
			t.Fatal(err)
		}
		within(how, func() {
			switch how {
			case "close":
				_ = c.Close()
			case "commit":
				if err := tx.Commit(context.Background()); err != nil {
					t.Error(err)
				}
			case "rollback":
				_ = tx.Rollback(context.Background())
			}
		})
		_ = c.Close()
	}
}

func TestTxMutateTableCreatedInTx(t *testing.T) {
	c := openTemp(t, false)
	ctx := context.Background()
	tx := begin(t, c, driver.TxOptions{})
	defer func() { _ = tx.Rollback(ctx) }()
	runTx(t, tx, driver.Query{Text: "create table t2 (id integer primary key, v text)"})
	m, _ := driver.As[driver.Mutator](tx)
	ref := driver.CollectionRef{Name: "t2"}
	key, err := m.Insert(ctx, ref, driver.Object{{Key: "v", Value: driver.String("x")}})
	if err != nil {
		t.Fatalf("insert into a table created in the Tx: %v", err)
	}
	if err := m.Update(ctx, ref, key, driver.Object{{Key: "v", Value: driver.String("y")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}
}

func TestTxReadOnlyQueryInReadWriteTx(t *testing.T) {
	c := openTemp(t, false)
	tx := begin(t, c, driver.TxOptions{})
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Query(context.Background(), driver.Query{Text: "select 1", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestInsertReturnsExactKey(t *testing.T) {
	c := openTemp(t, false)
	m, ctx := mutator(t, c), context.Background()
	run(t, c, driver.Query{Text: "create table k (code text primary key default 'dflt', v text)"})
	ref := driver.CollectionRef{Name: "k"}
	key, err := m.Insert(ctx, ref, driver.Object{{Key: "v", Value: driver.String("a")}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := key.Get("code"); v.String() != "dflt" {
		t.Fatalf("defaulted key = %v", key)
	}
	key, err = m.Insert(ctx, ref, driver.Object{{Key: "CODE", Value: driver.String("up")}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := key.Get("code"); v.String() != "up" {
		t.Fatalf("key for a differently-cased field = %v", key)
	}
}

func TestTxConnectionIsDiscarded(t *testing.T) {
	for _, commit := range []bool{true, false} {
		c := openTemp(t, false)
		ctx := context.Background()
		tx := begin(t, c, driver.TxOptions{})
		runTx(t, tx, driver.Query{Text: "create temp table zz_tmp (a)"})
		runTx(t, tx, driver.Query{Text: "pragma query_only = 1"})
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		} else if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		// A fresh write must neither see the temp table nor be read-only.
		if err := mutator(t, c).Update(ctx, peopleRef, idKey(6), driver.Object{{Key: "name", Value: driver.String("after")}}); err != nil {
			t.Fatalf("commit=%v: the Tx's connection state leaked: %v", commit, err)
		}
		if _, err := c.Query(ctx, driver.Query{Text: "select * from zz_tmp"}); err == nil {
			t.Errorf("commit=%v: temp table survived the Tx", commit)
		}
	}
}
