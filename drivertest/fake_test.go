package drivertest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func openFake(t *testing.T, cfg driver.ConnConfig, opts ...FakeOption) (*Fake, driver.Conn) {
	t.Helper()
	f := NewFake(append([]FakeOption{WithPeople()}, opts...)...)
	c, err := f.Driver().Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return f, c
}

func run(t *testing.T, q driver.Querier, query driver.Query) ([]driver.Row, driver.Stats, error) {
	t.Helper()
	rs, err := q.Query(context.Background(), query)
	if err != nil {
		return nil, driver.Stats{}, err
	}
	return driver.Collect(context.Background(), rs, 0)
}

func TestFakeBasics(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	if info := f.Driver().Info(); info.Name != "fake" || len(info.Schemes) != 1 || info.Schemes[0] != "fake" {
		t.Fatalf("Info = %+v", info)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	nss, err := c.Namespaces(ctx)
	if err != nil || len(nss) != 1 || nss[0].Name != "main" || !nss[0].Default {
		t.Fatalf("Namespaces = %v, %v", nss, err)
	}
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: "main"})
	if err != nil || len(cols) != 1 || cols[0].Ref.Name != "tobira_people" || cols[0].Type != driver.CollectionTable {
		t.Fatalf("Collections = %v, %v", cols, err)
	}
	sch, err := c.Describe(ctx, driver.CollectionRef{Namespace: "main", Name: "tobira_people"})
	if err != nil || len(sch.PrimaryKey) != 1 || sch.PrimaryKey[0] != "id" || len(sch.Fields) != 8 {
		t.Fatalf("Describe = %+v, %v", sch, err)
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Namespace: "main", Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Describe missing = %v", err)
	}
	rows, st, err := run(t, c, driver.Query{Text: "select * from tobira_people limit 5"})
	if err != nil || len(rows) != 5 || !st.Truncated {
		t.Fatalf("limit 5: %d rows, %+v, %v", len(rows), st, err)
	}
	rows, st, err = run(t, c, driver.Query{Text: "SELECT * FROM tobira_people", Limit: 10})
	if err != nil || len(rows) != 10 || !st.Truncated {
		t.Fatalf("Query.Limit: %d rows, %+v, %v", len(rows), st, err)
	}
	rows, st, _ = run(t, c, driver.Query{Text: "select * from tobira_people limit 3", Limit: 10})
	if len(rows) != 3 || !st.Truncated {
		t.Fatalf("smaller limit wins: %d rows, %+v", len(rows), st)
	}
	rows, _, _ = run(t, c, driver.Query{Text: "select * from tobira_people"})
	if len(rows) != 57 {
		t.Fatalf("all rows = %d", len(rows))
	}
}

func TestFakeQueryLanguage(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	tests := []struct {
		name    string
		q       driver.Query
		wantN   int
		firstID int64
		wantErr error
	}{
		{"where int", driver.Query{Text: "select * from tobira_people where id = 7"}, 1, 7, nil},
		{"where string with quote", driver.Query{Text: "select * from tobira_people where name = 'Conan O''Brien'"}, 4, 2, nil},
		{"where bool", driver.Query{Text: "select * from tobira_people where active = false"}, 19, 3, nil},
		{"where null", driver.Query{Text: "select * from tobira_people where age = null"}, 5, 11, nil},
		{"param dollar", driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}}, 1, 7, nil},
		{"param question", driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(9)}}, 1, 9, nil},
		{"param missing", driver.Query{Text: "select * from tobira_people where id = $2", Params: []driver.Value{driver.Int(9)}}, 0, 0, driver.ErrSyntax},
		{"order desc", driver.Query{Text: "select * from tobira_people order by id desc limit 2"}, 2, 57, nil},
		{"order nulls last asc", driver.Query{Text: "select * from tobira_people order by age asc", Limit: 57}, 57, 0, nil},
		{"missing collection", driver.Query{Text: "select * from nope"}, 0, 0, driver.ErrNotFound},
		{"missing field", driver.Query{Text: "select * from tobira_people where nope = 1"}, 0, 0, driver.ErrNotFound},
		{"garbage", driver.Query{Text: "frobnicate"}, 0, 0, driver.ErrSyntax},
		{"empty", driver.Query{Text: "  "}, 0, 0, driver.ErrSyntax},
		{"bad limit", driver.Query{Text: "select * from tobira_people limit x"}, 0, 0, driver.ErrSyntax},
		{"unterminated", driver.Query{Text: "select * from tobira_people where name = 'x"}, 0, 0, driver.ErrSyntax},
		{"trailing junk", driver.Query{Text: "select * from tobira_people foo"}, 0, 0, driver.ErrSyntax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, err := run(t, c, tc.q)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				var de *driver.Error
				if !errors.As(err, &de) || de.Driver != "fake" {
					t.Fatalf("not a fake *driver.Error: %v", err)
				}
				return
			}
			if err != nil || len(rows) != tc.wantN {
				t.Fatalf("got %d rows, err %v; want %d", len(rows), err, tc.wantN)
			}
			if tc.firstID != 0 {
				if id, _ := rows[0][0].AsInt(); id != tc.firstID {
					t.Fatalf("first id = %d, want %d", id, tc.firstID)
				}
			}
		})
	}
	// nulls sort last ascending, first descending.
	rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people order by age"})
	if !rows[56][3].IsNull() || rows[0][3].IsNull() {
		t.Fatal("asc: nulls must be last")
	}
	rows, _, _ = run(t, c, driver.Query{Text: "select * from tobira_people order by age desc"})
	if !rows[0][3].IsNull() || rows[56][3].IsNull() {
		t.Fatal("desc: nulls must be first")
	}
}

func TestFakeOnQuery(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	f.OnQuery("ping!", func(driver.Query) (driver.ResultSet, error) {
		return driver.NewSliceResultSet([]driver.Column{{Name: "x", Type: driver.TypeInt}}, []driver.Row{{driver.Int(1)}}, driver.Stats{}), nil
	})
	f.OnQuery(`re:^boom\d$`, func(driver.Query) (driver.ResultSet, error) {
		return nil, driver.Wrap(driver.ErrPermission, "fake", "query", errors.New("scripted"))
	})
	f.OnQuery("select * from tobira_people", func(driver.Query) (driver.ResultSet, error) {
		return driver.NewSliceResultSet(nil, nil, driver.Stats{}), nil
	})
	if rows, _, err := run(t, c, driver.Query{Text: "ping!"}); err != nil || len(rows) != 1 {
		t.Fatalf("exact: %v %v", rows, err)
	}
	if _, _, err := run(t, c, driver.Query{Text: "boom7"}); !errors.Is(err, driver.ErrPermission) {
		t.Fatalf("regexp: %v", err)
	}
	if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); len(rows) != 0 {
		t.Fatal("OnQuery must take precedence over the built-in language")
	}
}

func TestFakeReadOnly(t *testing.T) {
	ctx := context.Background()
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	delQ := driver.Query{Text: "delete from tobira_people where id = 1"}

	t.Run("conn read-only", func(t *testing.T) {
		_, c := openFake(t, driver.ConnConfig{ReadOnly: true})
		for _, q := range []string{"insert into tobira_people values (1)", "UPDATE tobira_people set x = 1", delQ.Text} {
			if _, err := c.Query(ctx, driver.Query{Text: q}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%q: err = %v", q, err)
			}
		}
		m, ok := driver.As[driver.Mutator](c)
		if !ok {
			t.Fatal("no Mutator")
		}
		muts := map[string]func() error{
			"Insert": func() error {
				_, err := m.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(100)}})
				return err
			},
			"Update": func() error {
				return m.Update(ctx, ref, key, driver.Object{{Key: "name", Value: driver.String("x")}})
			},
			"Delete": func() error { return m.Delete(ctx, ref, key) },
		}
		for name, fn := range muts {
			if err := fn(); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); len(rows) != 57 {
			t.Fatalf("data changed: %d rows", len(rows))
		}
		tr, _ := driver.As[driver.Transactor](c)
		tx, err := tr.Begin(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Query(ctx, delQ); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("tx write on read-only conn: %v", err)
		}
	})
	t.Run("query read-only", func(t *testing.T) {
		_, c := openFake(t, driver.ConnConfig{})
		q := delQ
		q.ReadOnly = true
		if _, err := c.Query(ctx, q); !errors.Is(err, driver.ErrReadOnly) {
			t.Fatalf("err = %v", err)
		}
		if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); len(rows) != 57 {
			t.Fatalf("data changed: %d rows", len(rows))
		}
		// reads still work with ReadOnly
		if rows, _, err := run(t, c, driver.Query{Text: "select * from tobira_people limit 1", ReadOnly: true}); err != nil || len(rows) != 1 {
			t.Fatalf("read with ReadOnly: %v", err)
		}
	})
	t.Run("write returns RowsAffected 1", func(t *testing.T) {
		_, c := openFake(t, driver.ConnConfig{})
		rows, st, err := run(t, c, driver.Query{Text: "insert into tobira_people values (1)"})
		if err != nil || len(rows) != 0 || st.RowsAffected != 1 {
			t.Fatalf("%v %v %+v", rows, err, st)
		}
		_, st, err = run(t, c, delQ)
		if err != nil || st.RowsAffected != 1 {
			t.Fatalf("%v %+v", err, st)
		}
		if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); len(rows) != 56 {
			t.Fatalf("delete did not delete: %d", len(rows))
		}
		_, st, err = run(t, c, driver.Query{Text: "delete from tobira_people where id = -1"})
		if err != nil || st.RowsAffected != 0 {
			t.Fatalf("delete of nothing: %v %+v", err, st)
		}
	})
	t.Run("column-less non-write returns RowsAffected 0", func(t *testing.T) {
		// Like sqlite and postgres for DDL: no columns, zero rows affected.
		_, c := openFake(t, driver.ConnConfig{})
		rs, err := c.Query(ctx, driver.Query{Text: "sleep 1ms"})
		if err != nil {
			t.Fatal(err)
		}
		cols := rs.Columns()
		_ = rs.Close()
		if len(cols) != 0 {
			t.Fatalf("columns = %v", cols)
		}
		_, st, err := run(t, c, driver.Query{Text: "sleep 1ms"})
		if err != nil || st.RowsAffected != 0 {
			t.Fatalf("%v %+v", err, st)
		}
		if _, st, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); st.RowsAffected != -1 {
			t.Fatalf("select RowsAffected = %d, want -1", st.RowsAffected)
		}
	})
}

func TestFakeLatencyCancel(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	f.SetLatency(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Query(ctx, driver.Query{Text: "select * from tobira_people"})
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	// already-cancelled context errors even without latency.
	f.SetLatency(0)
	if _, err := c.Query(ctx, driver.Query{Text: "select * from tobira_people"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: %v", err)
	}
}

func TestFakeSleep(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	if _, _, err := run(t, c, driver.Query{Text: "sleep 10ms"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, c, driver.Query{Text: "sleep banana"}); !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("bad duration: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Query(ctx, driver.Query{Text: "sleep 10s"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestFakeFailNext(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	authErr := driver.Wrap(driver.ErrAuth, "fake", "query", errors.New("denied"))
	f.FailNext("query", authErr)
	f.FailNext("query", driver.Wrap(driver.ErrConflict, "fake", "query", errors.New("second")))
	q := driver.Query{Text: "select * from tobira_people limit 1"}
	if _, err := c.Query(context.Background(), q); err != authErr {
		t.Fatalf("first = %v, want the queued error as-is", err)
	}
	if _, err := c.Query(context.Background(), q); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("second (FIFO) = %v", err)
	}
	if _, _, err := run(t, c, q); err != nil {
		t.Fatalf("third = %v", err)
	}
	f.FailNext("ping", authErr)
	if err := c.Ping(context.Background()); err != authErr {
		t.Fatalf("ping = %v", err)
	}
	f.FailNext("open", authErr)
	if _, err := f.Driver().Open(context.Background(), driver.ConnConfig{}); err != authErr {
		t.Fatalf("open = %v", err)
	}
}

func TestFakeCalls(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	_ = c.Ping(ctx)
	_, _ = c.Namespaces(ctx)
	_, _ = c.Collections(ctx, driver.NamespaceRef{Name: "main"})
	_, _ = c.Describe(ctx, ref)
	_, _ = c.Query(ctx, driver.Query{Text: "select * from nope"})
	b, _ := driver.As[driver.Browser](c)
	_, _ = b.Browse(ctx, ref, driver.BrowseOptions{})
	var ops []string
	for _, call := range f.Calls() {
		ops = append(ops, call.Op)
	}
	want := "open,ping,namespaces,collections,describe,query,browse"
	if got := strings.Join(ops, ","); got != want {
		t.Fatalf("ops = %s, want %s", got, want)
	}
	calls := f.Calls()
	if calls[4].Ref != ref || calls[5].Query.Text != "select * from nope" || !errors.Is(calls[5].Err, driver.ErrNotFound) {
		t.Fatalf("call details: %+v / %+v", calls[4], calls[5])
	}
	calls[0].Op = "mutated"
	if f.Calls()[0].Op != "open" {
		t.Fatal("Calls must return a copy")
	}
}

func TestFakeWithoutCapabilities(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{}, WithoutCapabilities(driver.CapBrowser, driver.CapWatcher))
	if _, ok := driver.As[driver.Browser](c); ok {
		t.Fatal("Browser should be disabled")
	}
	if _, ok := driver.As[driver.Mutator](c); !ok {
		t.Fatal("Mutator should remain")
	}
	b := c.(driver.Browser)
	if _, err := b.Browse(context.Background(), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("direct Browse on disabled cap: %v", err)
	}
	caps := f.Driver().Info().Capabilities
	for _, cp := range caps {
		if cp == driver.CapBrowser || cp == driver.CapWatcher {
			t.Fatalf("Info lists disabled %s", cp)
		}
	}
	if len(caps) != 9 { // 8 interfaces plus the declared CapReadOnly
		t.Fatalf("caps = %v", caps)
	}
	if got := driver.CapabilitiesOf(c); len(got) != 8 {
		t.Fatalf("CapabilitiesOf = %v", got)
	}
	full := NewFake()
	if len(full.Driver().Info().Capabilities) != 11 {
		t.Fatal("default Fake must claim all 10 interface capabilities plus CapReadOnly")
	}
}

func TestFakeClosed(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	checks := map[string]error{}
	checks["ping"] = c.Ping(ctx)
	_, checks["query"] = c.Query(ctx, driver.Query{Text: "select * from tobira_people"})
	_, checks["namespaces"] = c.Namespaces(ctx)
	_, checks["collections"] = c.Collections(ctx, driver.NamespaceRef{})
	_, checks["describe"] = c.Describe(ctx, ref)
	_, checks["browse"] = c.(driver.Browser).Browse(ctx, ref, driver.BrowseOptions{})
	_, checks["insert"] = c.(driver.Mutator).Insert(ctx, ref, nil)
	checks["update"] = c.(driver.Mutator).Update(ctx, ref, nil, nil)
	checks["delete"] = c.(driver.Mutator).Delete(ctx, ref, nil)
	_, checks["begin"] = c.(driver.Transactor).Begin(ctx, driver.TxOptions{})
	_, checks["explain"] = c.(driver.Explainer).Explain(ctx, driver.Query{Text: "select * from tobira_people"}, driver.ExplainOptions{})
	_, checks["complete"] = c.(driver.Completer).Complete(ctx, driver.CompletionRequest{})
	checks["export"] = c.(driver.Exporter).Export(ctx, driver.Query{Text: "select * from tobira_people"}, "csv", &bytes.Buffer{})
	_, checks["watch"] = c.(driver.Watcher).Watch(ctx, driver.WatchRequest{Collection: &ref})
	_, checks["similar"] = c.(driver.VectorSearcher).Similar(ctx, ref, driver.VectorQuery{})
	_, checks["range"] = c.(driver.TimeSeriesReader).Range(ctx, ref, driver.RangeQuery{})
	_, checks["neighbors"] = c.(driver.GraphReader).Neighbors(ctx, driver.GraphQuery{})
	for op, err := range checks {
		if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("%s after Close: %v", op, err)
		}
	}
}

func TestFakeBrowse(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	b, _ := driver.As[driver.Browser](c)

	seen := map[int64]bool{}
	cursor, pages := "", 0
	for {
		rs, err := b.Browse(ctx, ref, driver.BrowseOptions{Limit: 20, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		rows, st, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			id, _ := r[0].AsInt()
			if seen[id] {
				t.Fatalf("id %d twice", id)
			}
			seen[id] = true
		}
		pages++
		if st.NextCursor == "" {
			if st.Truncated {
				t.Fatal("last page must not be Truncated")
			}
			break
		}
		if !st.Truncated {
			t.Fatal("non-last page must be Truncated")
		}
		cursor = st.NextCursor
	}
	if len(seen) != 57 || pages != 3 {
		t.Fatalf("seen %d rows in %d pages", len(seen), pages)
	}

	first := func(o driver.BrowseOptions) int64 {
		t.Helper()
		rs, err := b.Browse(ctx, ref, o)
		if err != nil {
			t.Fatal(err)
		}
		rows, _, _ := driver.Collect(ctx, rs, 0)
		id, _ := rows[0][0].AsInt()
		return id
	}
	if got := first(driver.BrowseOptions{Sort: []driver.SortKey{{Field: "id", Desc: true}}}); got != 57 {
		t.Fatalf("desc first = %d", got)
	}
	if got := first(driver.BrowseOptions{Sort: []driver.SortKey{{Field: "id"}}}); got != 1 {
		t.Fatalf("asc first = %d", got)
	}
	f := driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpEq, Value: driver.Int(7)}}}
	rs, _ := b.Browse(ctx, ref, driver.BrowseOptions{Filter: f})
	if rows, _, _ := driver.Collect(ctx, rs, 0); len(rows) != 1 {
		t.Fatalf("eq filter: %d rows", len(rows))
	}
	f = driver.Filter{Conditions: []driver.Condition{{Field: "email", Op: driver.OpPrefix, Value: driver.String("user0")}}}
	rs, _ = b.Browse(ctx, ref, driver.BrowseOptions{Filter: f, Limit: 100})
	if rows, _, _ := driver.Collect(ctx, rs, 0); len(rows) != 9 {
		t.Fatalf("prefix filter: %d rows", len(rows))
	}
	f = driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpIsNull}}}
	rs, _ = b.Browse(ctx, ref, driver.BrowseOptions{Filter: f})
	if rows, _, _ := driver.Collect(ctx, rs, 0); len(rows) != 5 {
		t.Fatalf("is_null filter: %d rows", len(rows))
	}
	f = driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))}}}
	rs, _ = b.Browse(ctx, ref, driver.BrowseOptions{Filter: f})
	if rows, _, _ := driver.Collect(ctx, rs, 0); len(rows) != 2 {
		t.Fatalf("in filter: %d rows", len(rows))
	}
	f = driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.Op("bogus"), Value: driver.Int(1)}}}
	if _, err := b.Browse(ctx, ref, driver.BrowseOptions{Filter: f}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("bogus op: %v", err)
	}
	if _, err := b.Browse(ctx, ref, driver.BrowseOptions{Cursor: "abc"}); !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, err := b.Browse(ctx, driver.CollectionRef{Namespace: "main", Name: "nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestFakeMutator(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	m, _ := driver.As[driver.Mutator](c)
	key, err := m.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(100)}, {Key: "name", Value: driver.String("New")}})
	if err != nil || len(key) != 1 || key[0].Key != "id" {
		t.Fatalf("Insert: %v %v", key, err)
	}
	if _, err := m.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(100)}}); !errors.Is(err, driver.ErrConstraint) {
		t.Fatalf("duplicate key: %v", err)
	}
	if err := m.Update(ctx, ref, key, driver.Object{{Key: "name", Value: driver.String("Renamed")}}); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people where id = 100"})
	if len(rows) != 1 || rows[0][1].String() != "Renamed" {
		t.Fatalf("after update: %v", rows)
	}
	if err := m.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, ref, key); err == nil {
		t.Fatal("setup: delete of a deleted row should fail")
	}
	changes := driver.Object{{Key: "name", Value: driver.String("x")}}
	errCases := []struct {
		name string
		fn   func() error
		want error
	}{
		{"delete missing", func() error { return m.Delete(ctx, ref, key) }, driver.ErrNotFound},
		{"update missing", func() error { return m.Update(ctx, ref, key, changes) }, driver.ErrNotFound},
		{"delete ambiguous", func() error {
			return m.Delete(ctx, ref, driver.Object{{Key: "name", Value: driver.String("Conan O'Brien")}})
		}, driver.ErrConflict},
		{"update ambiguous", func() error {
			return m.Update(ctx, ref, driver.Object{{Key: "name", Value: driver.String("Conan O'Brien")}}, changes)
		}, driver.ErrConflict},
		{"empty key", func() error { return m.Delete(ctx, ref, nil) }, driver.ErrSyntax},
		{"unknown key field", func() error { return m.Delete(ctx, ref, driver.Object{{Key: "nope", Value: driver.Int(1)}}) }, driver.ErrNotFound},
		{"unknown change field", func() error {
			return m.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "nope", Value: driver.Int(1)}})
		}, driver.ErrNotFound},
	}
	for _, tc := range errCases {
		if err := tc.fn(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); len(rows) != 57 {
		t.Fatalf("rows = %d", len(rows))
	}
	if _, err := m.Insert(ctx, ref, driver.Object{{Key: "nope", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestFakeTransactor(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	tr, _ := driver.As[driver.Transactor](c)
	count := func(q driver.Querier) int {
		rows, _, err := run(t, q, driver.Query{Text: "select * from tobira_people"})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	del := driver.Query{Text: "delete from tobira_people where id = 3"}

	tx, err := tr.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, tx, del); err != nil {
		t.Fatal(err)
	}
	if count(tx) != 56 || count(c) != 57 {
		t.Fatal("tx must see its write, conn must not (yet)")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if count(c) != 57 {
		t.Fatal("rollback must discard")
	}
	if _, err := tx.Query(ctx, del); err == nil {
		t.Fatal("query on finished tx must fail")
	}

	tx, _ = tr.Begin(ctx, driver.TxOptions{})
	mu, ok := driver.As[driver.Mutator](tx)
	if !ok {
		t.Fatal("Tx should be a Mutator")
	}
	if err := mu.Delete(ctx, driver.CollectionRef{Namespace: "main", Name: "tobira_people"}, driver.Object{{Key: "id", Value: driver.Int(4)}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if count(c) != 56 {
		t.Fatal("commit must persist")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback after commit must be a no-op: %v", err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("second commit must fail")
	}

	tx, _ = tr.Begin(ctx, driver.TxOptions{ReadOnly: true})
	if _, err := tx.Query(ctx, del); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("read-only tx: %v", err)
	}
}

func TestFakeExplainCompleteExport(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	p, err := c.(driver.Explainer).Explain(ctx, driver.Query{Text: "select * from tobira_people"}, driver.ExplainOptions{})
	if err != nil || p.Format != "text" || p.Text != "FAKE SCAN tobira_people" {
		t.Fatalf("Explain = %+v, %v", p, err)
	}
	if _, err := c.(driver.Explainer).Explain(ctx, driver.Query{Text: "select * from nope"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("explain missing: %v", err)
	}
	if _, err := c.(driver.Explainer).Explain(ctx, driver.Query{Text: "delete from tobira_people where id = 1", ReadOnly: true}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("analyze write read-only: %v", err)
	}

	text := "select  from "
	comps, err := c.(driver.Completer).Complete(ctx, driver.CompletionRequest{Text: text, Cursor: len(text)})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, cp := range comps {
		labels[cp.Label] = cp.Kind
	}
	if labels["tobira_people"] != "collection" || labels["email"] != "field" || labels["select"] != "keyword" {
		t.Fatalf("labels = %v", labels)
	}
	comps, _ = c.(driver.Completer).Complete(ctx, driver.CompletionRequest{Text: "select * from tob", Cursor: 17})
	if len(comps) != 1 || comps[0].Label != "tobira_people" {
		t.Fatalf("prefix completions = %v", comps)
	}

	e := c.(driver.Exporter)
	if got := strings.Join(e.ExportFormats(), ","); got != "csv,ndjson" {
		t.Fatalf("formats = %s", got)
	}
	var buf bytes.Buffer
	if err := e.Export(ctx, driver.Query{Text: "select * from tobira_people limit 2"}, "csv", &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || lines[0] != "id,name,email,age,score,active,created_at,tags" {
		t.Fatalf("csv = %q", buf.String())
	}
	buf.Reset()
	if err := e.Export(ctx, driver.Query{Text: "select * from tobira_people limit 2"}, "ndjson", &buf); err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"id":1,"name":"Ōtani Shōhei"`) {
		t.Fatalf("ndjson = %q", buf.String())
	}
	if err := e.Export(ctx, driver.Query{Text: "select * from tobira_people"}, "parquet", &buf); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("unknown format: %v", err)
	}
}

func TestFakeWatch(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.(driver.Watcher).Watch(ctx, driver.WatchRequest{Collection: &ref})
	if err != nil {
		t.Fatal(err)
	}
	m := c.(driver.Mutator)
	key := driver.Object{{Key: "id", Value: driver.Int(200)}}
	_, _ = m.Insert(ctx, ref, key)
	_ = m.Update(ctx, ref, key, driver.Object{{Key: "name", Value: driver.String("x")}})
	_ = m.Delete(ctx, ref, key)
	for _, want := range []string{"insert", "update", "delete"} {
		select {
		case ev := <-ch:
			if ev.Op != want || ev.Err != nil || len(ev.Key) != 1 {
				t.Fatalf("event = %+v, want %s", ev, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %s event", want)
		}
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected event after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close on cancel")
	}
	if _, err := c.(driver.Watcher).Watch(context.Background(), driver.WatchRequest{}); !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("empty request: %v", err)
	}
}

func TestFakeEmitAndWatchers(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.(driver.Watcher).Watch(ctx, driver.WatchRequest{Channel: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.Watchers(); n != 1 {
		t.Fatalf("Watchers = %d, want 1", n)
	}
	f.Emit(driver.CollectionRef{Name: "other"}, driver.Event{Op: "notify"}) // not watched
	f.Emit(driver.CollectionRef{Name: "orders"}, driver.Event{Op: "notify", Payload: driver.String("hi")})
	select {
	case ev := <-ch:
		if ev.Op != "notify" || ev.Payload.String() != "hi" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	cancel()
	for range ch {
	}
	if n := f.Watchers(); n != 0 {
		t.Fatalf("Watchers after cancel = %d, want 0", n)
	}
}

func TestFakeVector(t *testing.T) {
	f := NewFake()
	f.AddCollection("main", "vecs",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "emb", Type: driver.TypeVector}},
		[]driver.Row{
			{driver.Int(1), driver.Vector([]float32{1, 0})},
			{driver.Int(2), driver.Vector([]float32{0, 1})},
			{driver.Int(3), driver.Vector([]float32{1, 1})},
		}, "id")
	c, _ := f.Driver().Open(context.Background(), driver.ConnConfig{})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "vecs"}
	vs := c.(driver.VectorSearcher)
	rs, err := vs.Similar(ctx, ref, driver.VectorQuery{Vector: []float32{1, 0}, K: 2})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	if cols[len(cols)-1].Name != "_distance" {
		t.Fatalf("cols = %v", cols)
	}
	rows, st, _ := driver.Collect(ctx, rs, 0)
	if len(rows) != 2 || !st.Truncated {
		t.Fatalf("rows=%d st=%+v", len(rows), st)
	}
	if id, _ := rows[0][0].AsInt(); id != 1 {
		t.Fatalf("nearest = %d", id)
	}
	if id, _ := rows[1][0].AsInt(); id != 3 {
		t.Fatalf("second = %d", id)
	}
	d, _ := rows[0][2].AsFloat()
	if d > 1e-6 {
		t.Fatalf("distance = %v", d)
	}
	if _, err := vs.Similar(ctx, ref, driver.VectorQuery{Text: "hello", K: 1}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("text: %v", err)
	}
	if _, err := vs.Similar(ctx, driver.CollectionRef{Namespace: "main", Name: "nope"}, driver.VectorQuery{Vector: []float32{1}}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestFakeTimeSeries(t *testing.T) {
	f := NewFake()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var rows []driver.Row
	for i := 0; i < 6; i++ {
		rows = append(rows, driver.Row{driver.Time(base.Add(time.Duration(i) * 30 * time.Minute)), driver.String("a"), driver.Float(float64(i + 1))})
	}
	f.AddCollection("main", "metrics",
		[]driver.Column{{Name: "ts", Type: driver.TypeTime}, {Name: "host", Type: driver.TypeString}, {Name: "cpu", Type: driver.TypeFloat}}, rows)
	c, _ := f.Driver().Open(context.Background(), driver.ConnConfig{})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "metrics"}
	tr := c.(driver.TimeSeriesReader)

	rs, err := tr.Range(ctx, ref, driver.RangeQuery{Field: "cpu", Window: time.Hour, Aggregate: "sum", GroupBy: []string{"host"}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, col := range rs.Columns() {
		names = append(names, col.Name)
	}
	if strings.Join(names, ",") != "_time,host,value" {
		t.Fatalf("cols = %v", names)
	}
	got, _, _ := driver.Collect(ctx, rs, 0)
	if len(got) != 3 {
		t.Fatalf("buckets = %d", len(got))
	}
	if v, _ := got[0][2].AsFloat(); v != 3 {
		t.Fatalf("first bucket sum = %v", v)
	}
	rs, _ = tr.Range(ctx, ref, driver.RangeQuery{Field: "cpu", Start: base.Add(time.Hour), End: base.Add(2 * time.Hour)})
	got, _, _ = driver.Collect(ctx, rs, 0)
	if len(got) != 2 {
		t.Fatalf("raw points in [1h,2h) = %d", len(got))
	}
	if _, err := tr.Range(ctx, ref, driver.RangeQuery{Field: "cpu", Window: time.Hour, Aggregate: "median"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("aggregate: %v", err)
	}
}

func TestFakeGraph(t *testing.T) {
	f := NewFake()
	f.AddCollection("main", "nodes",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "label", Type: driver.TypeString}, {Name: "name", Type: driver.TypeString}},
		[]driver.Row{
			{driver.Int(1), driver.String("Person"), driver.String("a")},
			{driver.Int(2), driver.String("Person"), driver.String("b")},
			{driver.Int(3), driver.String("City"), driver.String("c")},
			{driver.Int(4), driver.String("Person"), driver.String("d")},
		}, "id")
	f.AddCollection("main", "edges",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "from", Type: driver.TypeInt}, {Name: "to", Type: driver.TypeInt}, {Name: "type", Type: driver.TypeString}},
		[]driver.Row{
			{driver.Int(1), driver.Int(1), driver.Int(2), driver.String("KNOWS")},
			{driver.Int(2), driver.Int(2), driver.Int(3), driver.String("LIVES_IN")},
			{driver.Int(3), driver.Int(3), driver.Int(4), driver.String("HOME_OF")},
		}, "id")
	c, _ := f.Driver().Open(context.Background(), driver.ConnConfig{})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	gr := c.(driver.GraphReader)
	g, err := gr.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(1), Depth: 1})
	if err != nil || len(g.Nodes) != 2 || len(g.Edges) != 1 {
		t.Fatalf("depth 1: %+v %v", g, err)
	}
	if g.Edges[0].From != "1" || g.Edges[0].To != "2" || g.Edges[0].Type != "KNOWS" {
		t.Fatalf("edge = %+v", g.Edges[0])
	}
	if g.Nodes[0].Labels[0] != "Person" {
		t.Fatalf("node = %+v", g.Nodes[0])
	}
	if _, ok := g.Nodes[0].Props.Get("name"); !ok {
		t.Fatalf("props = %v", g.Nodes[0].Props)
	}
	g, _ = gr.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(1), Depth: 3})
	if len(g.Nodes) != 4 || len(g.Edges) != 3 {
		t.Fatalf("depth 3: %d nodes %d edges", len(g.Nodes), len(g.Edges))
	}
	g, _ = gr.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(1), Depth: 3, Limit: 2})
	if len(g.Nodes) != 2 {
		t.Fatalf("limit: %d nodes", len(g.Nodes))
	}
	if _, err := gr.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(99)}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("missing start: %v", err)
	}
	if _, err := newFakeConn(t).(driver.GraphReader).Neighbors(ctx, driver.GraphQuery{Start: driver.Int(1)}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("no graph collections: %v", err)
	}
}

// newFakeConn opens a bare Fake (no data).
func newFakeConn(t *testing.T) driver.Conn {
	t.Helper()
	c, err := NewFake().Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestFakeMulti(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	ctx := context.Background()
	rs, err := c.Query(ctx, driver.Query{Text: "select * from tobira_people limit 2; select * from tobira_people limit 3;"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := rs.(driver.MultiResultSet)
	if !ok {
		t.Fatal("not a MultiResultSet")
	}
	var counts []int
	for {
		n := 0
		for {
			_, ok, err := m.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			n++
		}
		counts = append(counts, n)
		more, err := m.NextResultSet(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if fmt.Sprint(counts) != "[2 3]" {
		t.Fatalf("counts = %v", counts)
	}
	_ = m.Close()
	// a ';' inside a string literal does not split.
	rows, _, err := run(t, c, driver.Query{Text: "select * from tobira_people where name = 'a;b'"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("%v %v", rows, err)
	}
}

func TestFakeConcurrent(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for j := 0; j < 20; j++ {
				rows, _, err := run(t, c, driver.Query{Text: "select * from tobira_people"})
				if err != nil || len(rows) < 1 {
					t.Errorf("query: %v", err)
				}
				f.SetLatency(0)
				_ = c.Ping(ctx)
				f.FailNext("describe", errors.New("x"))
				_ = f.Calls()
			}
		}()
	}
	wg.Wait()
	// a second Conn from the same Fake shares data.
	c2, _ := f.Driver().Open(context.Background(), driver.ConnConfig{})
	defer func() { _ = c2.Close() }()
	_, _, _ = run(t, c, driver.Query{Text: "delete from tobira_people where id = 1"})
	if rows, _, _ := run(t, c2, driver.Query{Text: "select * from tobira_people"}); len(rows) != 56 {
		t.Fatalf("shared data: %d", len(rows))
	}
}

func TestFakeQueryWhitespaceAndWrites(t *testing.T) {
	_, c := openFake(t, driver.ConnConfig{})
	tests := []struct {
		name    string
		text    string
		wantN   int
		wantErr error
	}{
		{"newline after select", "select\n* from tobira_people limit 2", 2, nil},
		{"tab and upper", "SELECT\t*\tFROM\ttobira_people\nLIMIT 3", 3, nil},
		{"leading whitespace", "  \n\t select * from tobira_people limit 1", 1, nil},
		{"delete with tab", "DELETE\tFROM\ttobira_people\tWHERE\tid = 5", 0, nil},
		{"sleep with tab", "sleep\t1ms", 0, nil},
		{"insert unknown collection", "insert into nope values (1)", 0, driver.ErrNotFound},
		{"update unknown collection", "update nope set a = 1", 0, driver.ErrNotFound},
		{"delete unknown collection", "delete from nope where id = 1", 0, driver.ErrNotFound},
		{"delete compound where", "delete from tobira_people where id = 1 and age = 2", 0, driver.ErrSyntax},
		{"delete bad where", "delete from tobira_people where", 0, driver.ErrSyntax},
		{"insert malformed", "insert tobira_people", 0, driver.ErrSyntax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, err := run(t, c, driver.Query{Text: tc.text})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || len(rows) != tc.wantN {
				t.Fatalf("rows=%d err=%v", len(rows), err)
			}
		})
	}
	if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); len(rows) != 56 {
		t.Fatalf("only the id = 5 delete may have applied; rows = %d", len(rows))
	}
}

func TestFakeTxCommitKeepsConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	id := func(n int64) driver.Object { return driver.Object{{Key: "id", Value: driver.Int(n)}} }
	count := func(q driver.Querier) int {
		rows, _, err := run(t, q, driver.Query{Text: "select * from tobira_people"})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	setup := func(t *testing.T) (*Fake, driver.Conn, driver.Conn) {
		f, c1 := openFake(t, driver.ConnConfig{})
		c2, err := f.Driver().Open(ctx, driver.ConnConfig{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c2.Close() })
		return f, c1, c2
	}

	t.Run("empty tx", func(t *testing.T) {
		_, c1, c2 := setup(t)
		tx, _ := c1.(driver.Transactor).Begin(ctx, driver.TxOptions{})
		if _, err := c2.(driver.Mutator).Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(300)}}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if count(c1) != 58 {
			t.Fatal("empty Tx commit must not discard other Conns' writes")
		}
	})
	t.Run("read-only tx", func(t *testing.T) {
		_, c1, c2 := setup(t)
		tx, _ := c1.(driver.Transactor).Begin(ctx, driver.TxOptions{ReadOnly: true})
		_, _ = c2.(driver.Mutator).Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(300)}})
		if err := tx.Commit(ctx); err != nil || count(c1) != 58 {
			t.Fatalf("commit: %v, rows %d", err, count(c1))
		}
	})
	t.Run("writing tx merges", func(t *testing.T) {
		f, c1, c2 := setup(t)
		w, _ := c1.(driver.Watcher).Watch(ctx, driver.WatchRequest{Collection: &ref})
		tx, _ := c1.(driver.Transactor).Begin(ctx, driver.TxOptions{})
		_, _ = c2.(driver.Mutator).Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(300)}})
		<-w // c2's insert event
		if err := tx.(driver.Mutator).Delete(ctx, ref, id(4)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := run(t, tx, driver.Query{Text: "delete from tobira_people where id = 5"}); err != nil {
			t.Fatal(err)
		}
		if count(tx) != 55 || count(c1) != 58 {
			t.Fatalf("tx sees %d, live sees %d", count(tx), count(c1))
		}
		select {
		case ev := <-w:
			t.Fatalf("event before commit: %+v", ev)
		default:
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if count(c2) != 56 {
			t.Fatalf("after commit live rows = %d, want 56 (58 - 2 deletes, 300 kept)", count(c2))
		}
		if rows, _, _ := run(t, c2, driver.Query{Text: "select * from tobira_people where id = 300"}); len(rows) != 1 {
			t.Fatal("row 300 lost")
		}
		for i := 0; i < 2; i++ {
			select {
			case ev := <-w:
				if ev.Op != "delete" {
					t.Fatalf("event = %+v", ev)
				}
			case <-time.After(time.Second):
				t.Fatal("missing commit event")
			}
		}
		_ = f
	})
	t.Run("conflict applies nothing", func(t *testing.T) {
		_, c1, c2 := setup(t)
		tx, _ := c1.(driver.Transactor).Begin(ctx, driver.TxOptions{})
		m := tx.(driver.Mutator)
		if err := m.Delete(ctx, ref, id(1)); err != nil {
			t.Fatal(err)
		}
		if err := m.Update(ctx, ref, id(2), driver.Object{{Key: "name", Value: driver.String("tx")}}); err != nil {
			t.Fatal(err)
		}
		if err := c2.(driver.Mutator).Delete(ctx, ref, id(2)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); !errors.Is(err, driver.ErrConflict) {
			t.Fatalf("commit = %v, want ErrConflict", err)
		}
		if count(c1) != 56 {
			t.Fatalf("a failed commit must apply nothing; rows = %d", count(c1))
		}
		if rows, _, _ := run(t, c1, driver.Query{Text: "select * from tobira_people where id = 1"}); len(rows) != 1 {
			t.Fatal("row 1 deleted by a failed commit")
		}
	})
}

func TestFakeAddCollectionPanicsOnBadRow(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "row 0 has 1 values for 2 columns") {
			t.Fatalf("recover = %v", r)
		}
	}()
	NewFake().AddCollection("main", "x", []driver.Column{{Name: "a"}, {Name: "b"}}, []driver.Row{{driver.Int(1)}})
}

func TestFakeVectorRangeEdgeCases(t *testing.T) {
	f := NewFake()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	f.AddCollection("main", "m",
		[]driver.Column{{Name: "ts", Type: driver.TypeTime}, {Name: "cpu", Type: driver.TypeFloat}, {Name: "emb", Type: driver.TypeVector}},
		[]driver.Row{
			{driver.Time(base), driver.Float(1), driver.Vector([]float32{1})},
			{driver.Time(base.Add(time.Minute)), driver.Null(), driver.Vector([]float32{1})},
		})
	c, _ := f.Driver().Open(context.Background(), driver.ConnConfig{})
	defer func() { _ = c.Close() }()
	ref := driver.CollectionRef{Namespace: "main", Name: "m"}
	ctx := context.Background()
	if _, err := c.(driver.VectorSearcher).Similar(ctx, ref, driver.VectorQuery{Vector: []float32{1}, Field: "cpu"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("non-vector field: %v", err)
	}
	rs, err := c.(driver.TimeSeriesReader).Range(ctx, ref, driver.RangeQuery{Field: "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := driver.Collect(ctx, rs, 0); len(rows) != 1 {
		t.Fatalf("raw range must skip NULL values, got %d rows", len(rows))
	}
}

func TestFakeRaceMixedOperations(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	events, err := c.(driver.Watcher).Watch(ctx, driver.WatchRequest{Collection: &ref})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range events {
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(3)
		go func() { // inserts
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := c.(driver.Mutator).Insert(ctx, ref, nil); err != nil {
					t.Errorf("insert: %v", err)
				}
			}
		}()
		go func() { // transactions
			defer wg.Done()
			c2, _ := f.Driver().Open(ctx, driver.ConnConfig{})
			defer func() { _ = c2.Close() }()
			for i := 0; i < 25; i++ {
				tx, err := c2.(driver.Transactor).Begin(ctx, driver.TxOptions{})
				if err != nil {
					t.Errorf("begin: %v", err)
					return
				}
				_, _, _ = run(t, tx, driver.Query{Text: "delete from tobira_people where id = 1"})
				if i%2 == 0 {
					_ = tx.Commit(ctx)
				} else {
					_ = tx.Rollback(ctx)
				}
			}
		}()
		go func() { // browsing
			defer wg.Done()
			for i := 0; i < 25; i++ {
				rs, err := c.(driver.Browser).Browse(ctx, ref, driver.BrowseOptions{Limit: 20})
				if err != nil {
					t.Errorf("browse: %v", err)
					return
				}
				_, _, _ = driver.Collect(ctx, rs, 0)
			}
		}()
	}
	wg.Wait()
}

func TestFakeDescribeAutoIncrement(t *testing.T) {
	f := NewFake(WithPeople())
	f.AddCollection("main", "named", []driver.Column{{Name: "code", Type: driver.TypeString}}, nil, "code")
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	sch, err := c.Describe(context.Background(), driver.CollectionRef{Namespace: "main", Name: peopleCollection})
	if err != nil {
		t.Fatal(err)
	}
	for _, fl := range sch.Fields {
		if fl.AutoIncrement != (fl.Name == "id") {
			t.Errorf("%s: AutoIncrement = %v", fl.Name, fl.AutoIncrement)
		}
	}
	sch, err = c.Describe(context.Background(), driver.CollectionRef{Namespace: "main", Name: "named"})
	if err != nil {
		t.Fatal(err)
	}
	if sch.Fields[0].AutoIncrement {
		t.Error("a string key is not auto-increment")
	}
}
