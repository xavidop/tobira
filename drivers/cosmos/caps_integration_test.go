//go:build integration

package cosmos

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func ctx30(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func runRows(t *testing.T, q driver.Querier, query driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := q.Query(ctx30(t), query)
	if err != nil {
		t.Fatalf("Query(%q): %v", query.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx30(t), rs, 0)
	if err != nil {
		t.Fatalf("Query(%q): reading rows: %v", query.Text, err)
	}
	return cols, rows, st
}

func colIndex(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no column %q in %v", name, cols)
	}
	return i
}

func TestQueryForms(t *testing.T) {
	c := openTest(t, testDB, false)
	t.Run("ValueCount", func(t *testing.T) {
		cols, rows, st := runRows(t, c, driver.Query{Text: "SELECT VALUE COUNT(1) FROM " + bigName})
		if len(cols) != 1 || cols[0].Name != valueColumn || len(rows) != 1 || !drivertest.EqualValue(driver.Int(bigSize), rows[0][0]) {
			t.Errorf("SELECT VALUE COUNT(1): cols %v rows %v", cols, rows)
		}
		if len(st.Notices) == 0 || !strings.Contains(st.Notices[0], "RU") {
			t.Errorf("Stats.Notices = %v, want the request charge", st.Notices)
		}
	})
	t.Run("EnvelopeWithPartitionKey", func(t *testing.T) {
		cols, rows, _ := runRows(t, c, driver.Query{Text: `{"container":"orders","query":"SELECT * FROM c WHERE c.total > @min","partitionKey":"acme","parameters":{"@min":20}}`})
		if len(rows) != 1 || !drivertest.EqualValue(driver.String("o2"), rows[0][colIndex(t, cols, "id")]) {
			t.Errorf("envelope query: %v", rows)
		}
		_, rows, _ = runRows(t, c, driver.Query{Text: `{"container":"orders","query":"SELECT * FROM c","partitionKey":"globex"}`})
		if len(rows) != 1 {
			t.Errorf("partition-scoped query returned %d rows, want 1", len(rows))
		}
	})
	t.Run("PositionalParams", func(t *testing.T) {
		_, rows, _ := runRows(t, c, driver.Query{Text: "SELECT * FROM orders o WHERE o.tenant = @p1 AND o.total >= @p2", Params: []driver.Value{driver.String("acme"), driver.Float(12.5)}})
		if len(rows) != 2 {
			t.Errorf("got %d rows, want 2", len(rows))
		}
	})
	t.Run("JoinAndNested", func(t *testing.T) {
		cols, rows, _ := runRows(t, c, driver.Query{Text: "SELECT o.id, l.sku, o.ship.city FROM orders o JOIN l IN o.lines WHERE o.tenant = 'acme'"})
		if len(rows) != 1 || !drivertest.EqualValue(driver.String("x"), rows[0][colIndex(t, cols, "sku")]) ||
			!drivertest.EqualValue(driver.String("Lyon"), rows[0][colIndex(t, cols, "city")]) {
			t.Errorf("join: cols %v rows %v", cols, rows)
		}
		if cols[0].Source != nil {
			t.Errorf("a projection must not be editable in place: %+v", cols[0])
		}
	})
	t.Run("StarSelectIsEditable", func(t *testing.T) {
		cols, _, _ := runRows(t, c, driver.Query{Text: "SELECT * FROM orders o", Limit: 1})
		for _, name := range []string{"tenant", "id"} {
			col := cols[colIndex(t, cols, name)]
			if col.Source == nil || col.Source.Name != ordersName || col.Source.Namespace != testDB || !col.Key {
				t.Errorf("column %s = %+v, want an editable key column of tobira.orders", name, col)
			}
		}
		if col := cols[colIndex(t, cols, "total")]; col.Key {
			t.Errorf("total must not be a key column")
		}
	})
	t.Run("LimitTruncates", func(t *testing.T) {
		_, rows, st := runRows(t, c, driver.Query{Text: "SELECT * FROM " + bigName + " b", Limit: 30})
		if len(rows) != 30 || !st.Truncated {
			t.Errorf("Limit 30 over %d items: %d rows, Truncated %v", bigSize, len(rows), st.Truncated)
		}
	})
	t.Run("OtherNamespace", func(t *testing.T) {
		_, rows, _ := runRows(t, c, driver.Query{Text: "SELECT * FROM things t", Namespace: otherDB})
		if len(rows) != 0 {
			t.Errorf("got %d rows, want 0", len(rows))
		}
	})
	t.Run("Errors", func(t *testing.T) {
		for _, tc := range []struct {
			q    driver.Query
			kind error
		}{
			{driver.Query{Text: "SELECT * FROM nope n"}, driver.ErrNotFound},
			{driver.Query{Text: "SELECT * FROM things t", Namespace: "no_such_db"}, driver.ErrNotFound},
			{driver.Query{Text: "SELECT * FROM orders o WHERE"}, driver.ErrSyntax},
			{driver.Query{Text: "SELECT * FROM orders o", Params: []driver.Value{driver.Unknown("x", nil, "x")}}, driver.ErrUnsupported},
		} {
			rs, err := c.Query(ctx30(t), tc.q)
			if err == nil {
				_, _, err = driver.Collect(ctx30(t), rs, 0)
			}
			if !errors.Is(err, tc.kind) {
				t.Errorf("Query(%q in %q): want %v, got %v", tc.q.Text, tc.q.Namespace, tc.kind, err)
			}
			if err != nil && strings.Contains(err.Error(), "\n") {
				t.Errorf("Query(%q): multi-line error %q", tc.q.Text, err)
			}
		}
	})
}

// With a smaller read-ahead budget the columns come from fewer items and
// the result says which fields were left out.
func TestReadAheadBudget(t *testing.T) {
	old := maxResultBytes
	maxResultBytes = 2 << 10
	t.Cleanup(func() { maxResultBytes = old })
	c := openTest(t, testDB, false)
	cols, rows, st := runRows(t, c, driver.Query{Text: "SELECT b.id, b.n, b.late FROM " + bigName + " b ORDER BY b.n"})
	if len(rows) != bigSize {
		t.Fatalf("got %d rows, want %d", len(rows), bigSize)
	}
	if slices.ContainsFunc(cols, func(c driver.Column) bool { return c.Name == "late" }) {
		t.Errorf("columns %v: the budget should have stopped the read-ahead before the late items", cols)
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "no column") || !strings.Contains(st.Warnings[0], "read-ahead") {
		t.Errorf("Warnings = %v", st.Warnings)
	}
}

func TestTypes(t *testing.T) {
	c := openTest(t, testDB, false)
	cols, rows, _ := runRows(t, c, driver.Query{Text: `SELECT 1 AS i, 1.5 AS f, 9007199254740993 AS big, "s" AS s, true AS b, null AS n, [1,"a"] AS l, {"k":{"z":1}} AS o FROM orders o WHERE o.id = 'o1' AND o.tenant = 'acme'`})
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	want := map[string]driver.Value{
		"i": driver.Int(1), "f": driver.Float(1.5), "big": driver.Int(9007199254740993), "s": driver.String("s"),
		"b": driver.Bool(true), "n": driver.Null(),
		"l": driver.List(driver.Int(1), driver.String("a")),
		"o": driver.ObjectValue(driver.Member{Key: "k", Value: driver.ObjectValue(driver.Member{Key: "z", Value: driver.Int(1)})}),
	}
	for name, w := range want {
		got := rows[0][colIndex(t, cols, name)]
		if got.Type() != w.Type() || !drivertest.EqualValue(w, got) {
			t.Errorf("%s = %s (%s), want %s (%s)", name, got, got.Type(), w, w.Type())
		}
	}
}

func TestCatalog(t *testing.T) {
	c := openTest(t, testDB, false)
	ctx := ctx30(t)
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(nss, func(n driver.Namespace) bool { return n.Name == testDB })
	if i < 0 || !nss[i].Default || nss[i].Label != "database" {
		t.Errorf("Namespaces = %+v, want %q as the default database", nss, testDB)
	}
	if !slices.ContainsFunc(nss, func(n driver.Namespace) bool { return n.Name == otherDB && !n.Default }) {
		t.Errorf("Namespaces = %+v, want %q", nss, otherDB)
	}
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: testDB})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, col := range cols {
		names = append(names, col.Ref.Name)
	}
	if !slices.IsSorted(names) || !slices.Contains(names, ordersName) || !slices.Contains(names, vectorsName) {
		t.Errorf("Collections = %v", names)
	}
	if _, err := c.Collections(ctx, driver.NamespaceRef{Name: "no_such_db"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections of a missing database: want ErrNotFound, got %v", err)
	}

	sch, err := c.Describe(ctx, driver.CollectionRef{Name: ordersName})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sch.PrimaryKey, []string{"tenant", "id"}) || sch.Ref.Namespace != testDB || !sch.Schemaless {
		t.Errorf("Describe(orders) = key %v ref %+v schemaless %v", sch.PrimaryKey, sch.Ref, sch.Schemaless)
	}
	byName := map[string]driver.Field{}
	for _, f := range sch.Fields {
		byName[f.Name] = f
	}
	if f := byName["total"]; f.Type != driver.TypeFloat || f.NativeType != "number" {
		t.Errorf("total = %+v, want a float number (12.5, 40, 7)", f)
	}
	if f := byName["ship"]; f.Type != driver.TypeObject || !f.Nullable || len(f.Fields) != 2 {
		t.Errorf("ship = %+v, want a nullable object with city and zip", f)
	}
	if f := byName["lines"]; f.Type != driver.TypeList || len(f.Fields) != 2 {
		t.Errorf("lines = %+v, want a list of objects with sku and qty", f)
	}
	if f := byName["_etag"]; !f.Generated {
		t.Errorf("_etag = %+v, want Generated", f)
	}
	var methods []string
	for _, ix := range sch.Indexes {
		methods = append(methods, ix.Method)
	}
	if !slices.Contains(methods, "composite") || !slices.Contains(methods, "partition key + id") {
		t.Errorf("Indexes = %+v", sch.Indexes)
	}
	if !strings.Contains(sch.Comment, "partition key /tenant") || !strings.Contains(sch.DDL, `"partitionKey"`) || len(sch.Sample) != 3 {
		t.Errorf("Comment %q, DDL %d bytes, %d sample rows", sch.Comment, len(sch.DDL), len(sch.Sample))
	}

	hier, err := c.Describe(ctx, driver.CollectionRef{Name: hierName})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hier.PrimaryKey, []string{"tenant", "user.name", "id"}) || len(hier.Fields) != 1 || hier.Fields[0].Name != "id" {
		t.Errorf("Describe(events) = key %v fields %+v", hier.PrimaryKey, hier.Fields)
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe(nope): want ErrNotFound, got %v", err)
	}
}

func TestBrowse(t *testing.T) {
	c := openTest(t, testDB, false)
	ref := driver.CollectionRef{Name: bigName}
	page := func(opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats) {
		t.Helper()
		rs, err := c.Browse(ctx30(t), ref, opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		rows, st, err := driver.Collect(ctx30(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		return rs.Columns(), rows, st
	}
	for _, sort := range [][]driver.SortKey{nil, {{Field: "n", Desc: true}}} {
		seen := map[string]bool{}
		var sizes []int
		cursor := ""
		var prev int64 = 1 << 62
		for range 10 {
			cols, rows, st := page(driver.BrowseOptions{Limit: 100, Sort: sort, Cursor: cursor})
			sizes = append(sizes, len(rows))
			id, n := colIndex(t, cols, "id"), colIndex(t, cols, "n")
			for _, r := range rows {
				s, _ := r[id].AsText()
				if seen[s] {
					t.Errorf("sort %v: id %s twice", sort, s)
				}
				seen[s] = true
				if v, _ := r[n].AsInt(); sort != nil && v > prev {
					t.Errorf("sort n desc: %d after %d", v, prev)
				} else {
					prev = v
				}
			}
			if cursor = st.NextCursor; cursor == "" {
				break
			}
		}
		if len(seen) != bigSize || !slices.Equal(sizes, []int{100, 100, 50}) {
			t.Errorf("sort %v: saw %d ids in pages %v, want %d in [100 100 50]", sort, len(seen), sizes, bigSize)
		}
	}
	_, rows, _ := page(driver.BrowseOptions{Filter: driver.Filter{
		Conditions: []driver.Condition{{Field: "n", Op: driver.OpGt, Value: driver.Int(240)}},
		Raw:        "NOT IS_DEFINED(c.missing)",
	}})
	if len(rows) != 10 {
		t.Errorf("n > 240: %d rows, want 10", len(rows))
	}
	_, rows, _ = page(driver.BrowseOptions{Limit: 1000, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "late", Op: driver.OpIsNull}}}})
	if len(rows) != bigLateFrom-1 {
		t.Errorf("late is null: %d rows, want %d", len(rows), bigLateFrom-1)
	}
	_, _, st := page(driver.BrowseOptions{Limit: 10})
	if _, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Limit: 10, Sort: []driver.SortKey{{Field: "n"}}, Cursor: st.NextCursor}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("a cursor reused with another sort: want ErrSyntax, got %v", err)
	}
	cols, _, _ := page(driver.BrowseOptions{Limit: 1})
	if col := cols[colIndex(t, cols, "id")]; !col.Key || col.Source == nil || col.Source.Name != bigName {
		t.Errorf("Browse id column = %+v, want an editable key", col)
	}
	if _, err := c.Browse(ctx30(t), driver.CollectionRef{Name: "nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Browse(nope): want ErrNotFound, got %v", err)
	}
}

func TestMutatorPartitioned(t *testing.T) {
	c := openTest(t, testDB, false)
	ctx := ctx30(t)
	ref := driver.CollectionRef{Name: ordersName}
	key, err := c.Insert(ctx, ref, driver.Object{
		{Key: "id", Value: driver.String("o9")}, {Key: "tenant", Value: driver.String("initech")},
		{Key: "ref", Value: driver.String("I-9")}, {Key: "total", Value: driver.Decimal("3.25")},
		{Key: "at", Value: driver.Time(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), ref, key) })
	if want := (driver.Object{{Key: "tenant", Value: driver.String("initech")}, {Key: "id", Value: driver.String("o9")}}); !drivertest.EqualValue(driver.ObjectValue(want...), driver.ObjectValue(key...)) {
		t.Errorf("Insert key = %v, want %v", key, want)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.String("o9")}, {Key: "tenant", Value: driver.String("initech")}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("Insert of an existing id: want ErrConstraint, got %v", err)
	}
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "total", Value: driver.Float(4)}, {Key: "note", Value: driver.String("rush")}}); err != nil {
		t.Fatal(err)
	}
	cols, rows, _ := runRows(t, c, driver.Query{Text: `{"container":"orders","query":"SELECT * FROM c WHERE c.id = 'o9'","partitionKey":"initech"}`})
	if len(rows) != 1 || !drivertest.EqualValue(driver.Int(4), rows[0][colIndex(t, cols, "total")]) ||
		!drivertest.EqualValue(driver.String("rush"), rows[0][colIndex(t, cols, "note")]) ||
		!drivertest.EqualValue(driver.String("2026-05-01T00:00:00Z"), rows[0][colIndex(t, cols, "at")]) {
		t.Errorf("after Update: cols %v rows %v", cols, rows)
	}
	for _, ch := range []driver.Object{
		{{Key: "tenant", Value: driver.String("other")}},
		{{Key: "id", Value: driver.String("o10")}},
		{{Key: "_etag", Value: driver.String("x")}},
	} {
		if err := c.Update(ctx, ref, key, ch); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Update(%v): want ErrUnsupported, got %v", ch, err)
		}
	}
	wrongPartition := driver.Object{{Key: "tenant", Value: driver.String("acme")}, {Key: "id", Value: driver.String("o9")}}
	if err := c.Update(ctx, ref, wrongPartition, driver.Object{{Key: "x", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Update in the wrong partition: want ErrNotFound, got %v", err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: "id", Value: driver.String("o9")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Delete without the partition key: want ErrSyntax, got %v", err)
	}
	if err := c.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ref, key); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("second Delete: want ErrNotFound, got %v", err)
	}

	gen, err := c.Insert(ctx, ref, driver.Object{{Key: "tenant", Value: driver.String("initech")}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := gen.Get("id")
	if s, _ := id.AsText(); len(s) != 36 {
		t.Errorf("an insert without id got id %s, want a UUID", id)
	}
	if err := c.Delete(ctx, ref, gen); err != nil {
		t.Errorf("Delete(generated): %v", err)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(5)}, {Key: "tenant", Value: driver.String("x")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Insert with a numeric id: want ErrSyntax, got %v", err)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.String("nt")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Insert without the partition key field: want ErrSyntax, got %v", err)
	}

	hier := driver.CollectionRef{Name: hierName}
	hkey, err := c.Insert(ctx, hier, driver.Object{
		{Key: "id", Value: driver.String("e1")}, {Key: "tenant", Value: driver.String("acme")},
		{Key: "user", Value: driver.ObjectValue(driver.Member{Key: "name", Value: driver.String("ada")})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(hkey); got != 3 {
		t.Errorf("hierarchical key = %v, want tenant, user.name and id", hkey)
	}
	if err := c.Update(ctx, hier, hkey, driver.Object{{Key: "kind", Value: driver.String("login")}}); err != nil {
		t.Errorf("Update(hierarchical): %v", err)
	}
	if err := c.Update(ctx, hier, hkey, driver.Object{{Key: "user", Value: driver.ObjectValue()}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Update of the object holding the partition key: want ErrUnsupported, got %v", err)
	}
	if err := c.Delete(ctx, hier, hkey); err != nil {
		t.Errorf("Delete(hierarchical): %v", err)
	}
}

func TestWatch(t *testing.T) {
	ctx := context.Background()
	if _, err := createContainer(ctx, testDB, azcosmos.ContainerProperties{
		ID: "watched", PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{Paths: []string{"/tenant"}},
	}); err != nil {
		t.Fatal(err)
	}
	c := openTest(t, testDB, false)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := c.Watch(wctx, driver.WatchRequest{Channel: "watched"})
	if err != nil {
		t.Fatal(err)
	}
	cc, _ := admin.NewContainer(testDB, "watched")
	if _, err := cc.UpsertItem(ctx, azcosmos.NewPartitionKeyString("acme"), []byte(`{"id":"w1","tenant":"acme","n":1}`), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
		want := driver.ObjectValue(driver.Member{Key: "tenant", Value: driver.String("acme")}, driver.Member{Key: "id", Value: driver.String("w1")})
		n, _ := ev.Payload.AsObject()
		if ev.Op != "upsert" || ev.Channel != "watched" || !drivertest.EqualValue(want, driver.ObjectValue(ev.Key...)) || n == nil || ev.Time.IsZero() {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no change event within 15s")
	}
	cancel()
	for range ch {
	}
	if _, err := c.Watch(ctx, driver.WatchRequest{Channel: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Watch(nope): want ErrNotFound, got %v", err)
	}
}

type fakeEmbedder struct{ vec []float32 }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}
func (f fakeEmbedder) Dimensions() int { return len(f.vec) }

func TestSimilar(t *testing.T) {
	ctx := ctx30(t)
	c, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor(testDB), Password: driver.NewSecret(emulatorKey), Embedder: fakeEmbedder{vec: []float32{1, 0, 0, 0}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	vs := c.(driver.VectorSearcher)
	ref := driver.CollectionRef{Name: vectorsName}
	top := func(q driver.VectorQuery) (string, []driver.Row) {
		t.Helper()
		rs, err := vs.Similar(ctx, ref, q)
		if err != nil {
			t.Fatalf("Similar(%+v): %v", q, err)
		}
		cols := rs.Columns()
		rows, _, err := driver.Collect(ctx, rs, 0)
		if err != nil || len(rows) == 0 {
			t.Fatalf("Similar(%+v): %d rows, %v", q, len(rows), err)
		}
		id, _ := rows[0][colIndex(t, cols, "id")].AsText()
		return id, rows
	}
	if id, rows := top(driver.VectorQuery{Text: "near the x axis", K: 2}); id != "2" || len(rows) != 2 {
		t.Errorf("text query (default metric, cosine): top %s of %d rows, want 2 of 2", id, len(rows))
	}
	if id, _ := top(driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Metric: driver.MetricL2, Field: "vec", Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "id", Op: driver.OpNe, Value: driver.String("1")},
	}}}); id != "4" {
		t.Errorf("L2 without id 1: top %s, want 4", id)
	}
	for _, tc := range []struct {
		ref  driver.CollectionRef
		q    driver.VectorQuery
		kind error
	}{
		{ref, driver.VectorQuery{Vector: []float32{1, 0}}, nil},
		{driver.CollectionRef{Name: ordersName}, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}}, driver.ErrUnsupported},
		{ref, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Metric: "hamming"}, driver.ErrUnsupported},
		{ref, driver.VectorQuery{}, driver.ErrSyntax},
	} {
		_, err := vs.Similar(ctx, tc.ref, tc.q)
		if tc.kind == nil {
			// The emulator does not report the policy's dimensions, so a
			// short vector reaches the server, which returns no rows.
			continue
		}
		if !errors.Is(err, tc.kind) {
			t.Errorf("Similar(%s, %+v): want %v, got %v", tc.ref.Name, tc.q, tc.kind, err)
		}
	}
	plain := openTest(t, testDB, false)
	if _, err := plain.Similar(ctx, ref, driver.VectorQuery{Text: "x"}); !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		t.Errorf("a text query without an embedder: want ErrUnsupported with the hint, got %v", err)
	}
}

func TestCompleteLive(t *testing.T) {
	c := openTest(t, testDB, false)
	got, err := c.Complete(ctx30(t), driver.CompletionRequest{Text: "SELECT * FROM orders o WHERE o.sh", Cursor: len("SELECT * FROM orders o WHERE o.sh")})
	if err != nil || len(got) != 1 || got[0].Label != "ship" {
		t.Errorf("field completion = %+v, %v", got, err)
	}
	got, err = c.Complete(ctx30(t), driver.CompletionRequest{Text: "SELECT * FROM ord", Cursor: len("SELECT * FROM ord")})
	if err != nil || len(got) != 1 || got[0].Label != ordersName {
		t.Errorf("container completion = %+v, %v", got, err)
	}
}
