//go:build integration

package cassandra

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func collect(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	return cols, rows, st
}

func colIndex(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no column %q", name)
	}
	return i
}

func TestNamespacesAndCollections(t *testing.T) {
	c := openTest(t, testKS, false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
		if systemKeyspace(ns.Name) {
			t.Errorf("Namespaces lists the system keyspace %q", ns.Name)
		}
		if ns.Label != "keyspace" || ns.Default != (ns.Name == testKS) {
			t.Errorf("namespace %+v", ns)
		}
	}
	if !slices.Contains(names, testKS) || !slices.Contains(names, otherKS) {
		t.Errorf("Namespaces = %q", names)
	}
	sys, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsnFor(testKS, testPassword, "system_keyspaces=true")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sys.Close() }()
	all, err := sys.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(all, func(n driver.Namespace) bool { return n.Name == "system_schema" }) {
		t.Error("system_keyspaces=true does not list system_schema")
	}

	colls, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: testKS})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]driver.CollectionType{}
	for _, col := range colls {
		types[col.Ref.Name] = col.Type
		if col.Ref.Namespace != testKS || col.RowEstimate != -1 {
			t.Errorf("collection %+v", col)
		}
	}
	if types["tobira_people"] != driver.CollectionTable || types["people_by_email"] != driver.CollectionMaterializedView {
		t.Errorf("collection types = %v", types)
	}
	if _, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "no_such_keyspace"}); err != nil {
		t.Errorf("Collections of a missing keyspace: %v (want an empty list)", err)
	}
}

func TestDescribeDetails(t *testing.T) {
	c := openTest(t, testKS, false)
	sch, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	if sch.Ref.Namespace != testKS || sch.Type != driver.CollectionTable || !slices.Equal(sch.PartitionKey, []string{"id"}) || len(sch.ClusteringKey) != 0 {
		t.Errorf("schema %+v", sch)
	}
	if !slices.ContainsFunc(sch.Indexes, func(ix driver.Index) bool {
		return ix.Name == "people_email" && slices.Equal(ix.Fields, []string{"email"})
	}) {
		t.Errorf("indexes = %+v, want people_email on email", sch.Indexes)
	}
	if sch.Fields[0].Name != "id" || sch.Fields[0].Nullable {
		t.Errorf("the key column comes first and is not nullable: %+v", sch.Fields[0])
	}
	view, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "people_by_email"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Type != driver.CollectionMaterializedView || !slices.Equal(view.PrimaryKey, []string{"email", "id"}) {
		t.Errorf("view schema %+v", view)
	}
	typ, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "types"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]driver.Type{
		"ti": driver.TypeInt, "vi": driver.TypeDecimal, "f": driver.TypeFloat, "bl": driver.TypeBytes, "dt": driver.TypeTime,
		"tm": driver.TypeString, "u": driver.TypeString, "du": driver.TypeString, "l": driver.TypeList, "m": driver.TypeObject,
		"tp": driver.TypeList, "addr": driver.TypeObject, "vec": driver.TypeVector, "nested": driver.TypeObject,
	}
	for _, f := range typ.Fields {
		if w, ok := want[f.Name]; ok && f.Type != w {
			t.Errorf("types.%s (%s): Type %s, want %s", f.Name, f.NativeType, f.Type, w)
		}
	}
	if _, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: "no_such_keyspace", Name: "x"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe in a missing keyspace: %v, want ErrNotFound", err)
	}
}

// Every CQL type survives a Mutator insert and a query, and the column
// types match Describe's.
func TestTypesRoundTrip(t *testing.T) {
	c := openTest(t, testKS, false)
	m, _ := driver.As[driver.Mutator](c)
	ref := driver.CollectionRef{Name: "types"}
	ts := time.Date(2026, 3, 4, 5, 6, 7, 8e6, time.UTC)
	str := driver.String
	mem := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	row := driver.Object{
		mem("id", driver.Int(1)), mem("ti", driver.Int(-8)), mem("si", driver.Int(300)), mem("bi", driver.Int(1<<40)),
		mem("vi", driver.Decimal("123456789012345678901234567890")), mem("f", driver.Float(1.5)), mem("d", driver.Float(0.1)),
		mem("dec", driver.Decimal("-12.340")), mem("b", driver.Bool(true)), mem("t", str("Zoë")), mem("a", str("plain")),
		mem("bl", driver.Bytes([]byte{0, 1, 2})), mem("ts", driver.Time(ts)), mem("dt", str("2026-03-04")),
		mem("tm", str("05:06:07.008")), mem("u", str("123e4567-e89b-12d3-a456-426614174000")),
		mem("tu", str("50554d6e-29bb-11e5-b345-feff819cdc9f")), mem("ip", str("::1")), mem("du", str("1mo2d3h")),
		mem("l", driver.List(driver.Int(3), driver.Int(1))), mem("s", driver.JSON([]byte(`["b","a"]`))),
		mem("m", driver.ObjectValue(mem("x", driver.Int(1)), mem("y", driver.Int(2)))),
		mem("addr", driver.ObjectValue(mem("street", str("Main")))),
		mem("vec", driver.Vector([]float32{1, 2, 3})),
		mem("nested", driver.ObjectValue(mem("k", driver.List(driver.Int(1), driver.Int(2))))),
	}
	key, err := m.Insert(ctxT(t), ref, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 1 || key[0].Key != "id" {
		t.Errorf("Insert key = %v", key)
	}
	t.Cleanup(func() { _ = m.Delete(context.Background(), ref, key) })
	// gocql cannot bind a top-level tuple: the Mutator refuses it, and
	// the test writes it in CQL.
	if err := m.Update(ctxT(t), ref, key, driver.Object{mem("tp", driver.List(driver.Int(7), driver.Null()))}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Update of a tuple column: %v, want ErrUnsupported", err)
	}
	if err := exec(ctxT(t), "UPDATE "+testKS+".types SET tp = (7, null) WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]driver.Value{
		"ti": driver.Int(-8), "si": driver.Int(300), "bi": driver.Int(1 << 40), "vi": driver.Decimal("123456789012345678901234567890"),
		"f": driver.Float(1.5), "d": driver.Float(0.1), "dec": driver.Decimal("-12.340"), "b": driver.Bool(true),
		"t": str("Zoë"), "a": str("plain"), "bl": driver.Bytes([]byte{0, 1, 2}), "ts": driver.Time(ts),
		"dt": driver.Time(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)), "tm": str("05:06:07.008"),
		"u": str("123e4567-e89b-12d3-a456-426614174000"), "tu": str("50554d6e-29bb-11e5-b345-feff819cdc9f"),
		"ip": str("::1"), "du": str("1mo2d3h"), "l": driver.List(driver.Int(3), driver.Int(1)),
		"s":      driver.List(str("a"), str("b")), // a set comes back sorted
		"m":      driver.ObjectValue(mem("x", driver.Int(1)), mem("y", driver.Int(2))),
		"tp":     driver.List(driver.Int(7), driver.Null()),
		"addr":   driver.ObjectValue(mem("street", str("Main")), mem("zip", driver.Null())),
		"vec":    driver.Vector([]float32{1, 2, 3}),
		"nested": driver.ObjectValue(mem("k", driver.List(driver.Int(1), driver.Int(2)))),
	}
	cols, rows, _ := collect(t, c, driver.Query{Text: "SELECT * FROM types WHERE id = 1"})
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	sch, _ := c.Describe(ctxT(t), ref)
	for name, w := range want {
		i := colIndex(t, cols, name)
		got := rows[0][i]
		if got.Type() != w.Type() || !drivertest.EqualValue(w, got) {
			t.Errorf("%s (%s) = %s (%s), want %s (%s)", name, cols[i].NativeType, got, got.Type(), w, w.Type())
		}
		if f := sch.Fields[slices.IndexFunc(sch.Fields, func(f driver.Field) bool { return f.Name == name })]; f.Type != cols[i].Type {
			t.Errorf("%s: Describe says %s, the result column %s", name, f.Type, cols[i].Type)
		}
	}
	// A time keeps its milliseconds.
	if gt, _ := rows[0][colIndex(t, cols, "ts")].AsTime(); !gt.Equal(ts) {
		t.Errorf("ts = %v, want %v", gt, ts)
	}
	// A counter reads as an Int.
	_, crow, _ := collect(t, c, driver.Query{Text: "SELECT hits FROM counters WHERE id = 1"})
	if len(crow) != 1 || !drivertest.EqualValue(driver.Int(5), crow[0][0]) || crow[0][0].Type() != driver.TypeInt {
		t.Errorf("counter = %v", crow)
	}
}

func TestQueryNamespaceAndParams(t *testing.T) {
	c := openTest(t, testKS, false)
	_, rows, _ := collect(t, c, driver.Query{Text: "SELECT label FROM things WHERE id = :id", Namespace: otherKS, Params: []driver.Value{driver.Int(1)}})
	if len(rows) != 1 || !drivertest.EqualValue(driver.String("thing one"), rows[0][0]) {
		t.Errorf("Query in %s = %v", otherKS, rows)
	}
	// A query without a namespace runs in the DSN's keyspace.
	_, rows, _ = collect(t, c, driver.Query{Text: "SELECT id FROM tobira_people WHERE id = 3"})
	if len(rows) != 1 {
		t.Errorf("default keyspace: %v", rows)
	}
	// A write returns an empty result.
	cols, rows, st := collect(t, c, driver.Query{Text: "INSERT INTO tobira_big (id, n) VALUES (99999, 1)"})
	if len(cols) != 0 || len(rows) != 0 || st.RowsAffected != -1 {
		t.Errorf("write result: %v %v %+v", cols, rows, st)
	}
	_ = exec(context.Background(), "DELETE FROM "+testKS+".tobira_big WHERE id = 99999")
	// A parameter of the wrong type is a syntax error, named.
	if _, err := c.Query(ctxT(t), driver.Query{Text: "SELECT * FROM tobira_people WHERE id = ?", Params: []driver.Value{driver.String("seven")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad parameter: %v, want ErrSyntax", err)
	}
}

func TestQueryErrors(t *testing.T) {
	c := openTest(t, testKS, false)
	for _, tc := range []struct {
		text string
		kind error
		code string
	}{
		{"SELEC * FROM t", driver.ErrSyntax, "0x2000"},
		{"SELECT * FROM no_such_keyspace.t", driver.ErrNotFound, "0x2200"},
		{"SELECT nope FROM tobira_people", driver.ErrSyntax, "0x2200"},
		{"USE " + otherKS, driver.ErrUnsupported, ""},
		{"CREATE TABLE tobira_people (id int PRIMARY KEY)", driver.ErrConflict, "0x2400"},
	} {
		rs, err := c.Query(ctxT(t), driver.Query{Text: tc.text})
		if err == nil {
			_, _, err = driver.Collect(ctxT(t), rs, 0)
		}
		if !errors.Is(err, tc.kind) {
			t.Errorf("%q: %v, want %v", tc.text, err, tc.kind)
		}
		var de *driver.Error
		if errors.As(err, &de) && de.Code != tc.code {
			t.Errorf("%q: code %q, want %q", tc.text, de.Code, tc.code)
		}
	}
}

// The read-only guard, not the server, refuses a user-defined function:
// the same statement runs on a writable connection.
func TestReadOnlyRefusesUDF(t *testing.T) {
	const q = "SELECT " + testKS + ".plus_one(id) FROM tobira_people WHERE id = 1"
	rw := openTest(t, testKS, false)
	_, rows, _ := collect(t, rw, driver.Query{Text: q})
	if len(rows) != 1 || !drivertest.EqualValue(driver.Int(2), rows[0][0]) {
		t.Fatalf("the UDF does not run on a writable connection: %v", rows)
	}
	ro := openTest(t, testKS, true)
	for _, text := range []string{q, "SELECT plus_one(id) FROM tobira_people WHERE id = 1"} {
		if _, err := ro.Query(ctxT(t), driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q on a read-only conn: %v, want ErrReadOnly", text, err)
		}
	}
	// Built-ins still work there.
	_, rows, _ = collect(t, ro, driver.Query{Text: "SELECT writetime(name), token(id), toJson(tags) FROM tobira_people WHERE id = 1"})
	if len(rows) != 1 {
		t.Errorf("built-in functions on a read-only conn: %v", rows)
	}
	_, rows, _ = collect(t, ro, driver.Query{Text: "DESCRIBE TABLE tobira_people"})
	if len(rows) == 0 {
		t.Error("DESCRIBE on a read-only conn: no rows")
	}
	b, _ := driver.As[driver.Browser](ro)
	if _, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{
		Filter: driver.Filter{Raw: "id = plus_one(1)"},
	}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Browse raw filter with a UDF on a read-only conn: %v, want ErrReadOnly", err)
	}
}

func TestBrowseFilters(t *testing.T) {
	c := openTest(t, testKS, false)
	b, _ := driver.As[driver.Browser](c)
	ref := driver.CollectionRef{Name: "tobira_people"}
	page := func(opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats) {
		t.Helper()
		rs, err := b.Browse(ctxT(t), ref, opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		cols := rs.Columns()
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		return cols, rows, st
	}
	cols, rows, _ := page(driver.BrowseOptions{Filter: driver.Filter{
		Conditions: []driver.Condition{{Field: "age", Op: driver.OpGe, Value: driver.Int(60)}},
		Raw:        "active = true",
	}})
	age, active := colIndex(t, cols, "age"), colIndex(t, cols, "active")
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if n, _ := r[age].AsInt(); n < 60 || !drivertest.EqualValue(driver.Bool(true), r[active]) {
			t.Errorf("row %v breaks the filter", r)
		}
	}
	if !cols[colIndex(t, cols, "id")].Key || cols[age].Key || cols[age].Source == nil || cols[age].Source.Namespace != testKS {
		t.Errorf("browse columns must carry the source and key: %+v", cols)
	}
	_, rows, _ = page(driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(2), driver.Int(4))},
	}}})
	if len(rows) != 2 {
		t.Errorf("id in (2, 4): %d rows", len(rows))
	}
	_, rows, _ = page(driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "tags", Op: driver.OpContains, Value: driver.String("eu")},
	}}})
	if len(rows) == 0 {
		t.Error("tags contains eu: no rows")
	}
	// Paging with a filter follows the paging state to the end.
	seen, cursor := 0, ""
	for range 20 {
		_, rows, st := page(driver.BrowseOptions{Limit: 7, Cursor: cursor, Filter: driver.Filter{
			Conditions: []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}},
		}})
		seen += len(rows)
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if seen != 38 || cursor != "" {
		t.Errorf("paged active rows = %d (cursor %q), want 38", seen, cursor)
	}
	// A sorted page of a sorted browse continues by offset.
	_, first, st := page(driver.BrowseOptions{Limit: 5, Sort: []driver.SortKey{{Field: "id", Desc: true}}})
	_, second, _ := page(driver.BrowseOptions{Limit: 5, Cursor: st.NextCursor, Sort: []driver.SortKey{{Field: "id", Desc: true}}})
	if len(first) != 5 || !drivertest.EqualValue(driver.Int(57), first[0][0]) || !drivertest.EqualValue(driver.Int(52), second[0][0]) {
		t.Errorf("sorted pages: %v / %v", first, second)
	}
	if _, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Cursor: st.NextCursor}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("an offset cursor without a sort: %v, want ErrSyntax", err)
	}
	if _, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Browse of a missing table: %v, want ErrNotFound", err)
	}
}

func TestBrowseSortTooLarge(t *testing.T) {
	c := openTest(t, testKS, false)
	b, _ := driver.As[driver.Browser](c)
	ref := driver.CollectionRef{Name: "tobira_big"}
	sorted := driver.BrowseOptions{Sort: []driver.SortKey{{Field: "n", Desc: true}}}
	rs, err := b.Browse(ctxT(t), ref, sorted)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(ctxT(t), rs, 0)
	if len(rows) != 100 || !drivertest.EqualValue(driver.Int(1500), rows[0][1]) {
		t.Errorf("sorted page of 1500 rows: %d rows, first %v", len(rows), rows[0])
	}
	old := maxSortRows
	maxSortRows = 1000
	t.Cleanup(func() { maxSortRows = old })
	_, err = b.Browse(ctxT(t), ref, sorted)
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("sorting more than maxSortRows rows: %v, want ErrUnsupported", err)
	}
}

func TestMutatorConditions(t *testing.T) {
	c := openTest(t, testKS, false)
	m, _ := driver.As[driver.Mutator](c)
	ref := driver.CollectionRef{Name: "tobira_people"}
	existing := driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "name", Value: driver.String("dup")}}
	if _, err := m.Insert(ctxT(t), ref, existing); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("Insert over an existing row: %v, want ErrConstraint", err)
	}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	if err := m.Update(ctxT(t), ref, key, driver.Object{{Key: "id", Value: driver.Int(2)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Update of the key: %v, want ErrUnsupported", err)
	}
	if err := m.Delete(ctxT(t), driver.CollectionRef{Name: "people_by_email"}, driver.Object{{Key: "email", Value: driver.String("x")}, {Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Delete in a view: %v, want ErrUnsupported", err)
	}
	// The row is unchanged.
	_, rows, _ := collect(t, c, driver.Query{Text: "SELECT name FROM tobira_people WHERE id = 1"})
	if len(rows) != 1 || !drivertest.EqualValue(drivertest.People.Rows[0][1], rows[0][0]) {
		t.Errorf("row 1 changed: %v", rows)
	}
}

func TestCancelMidIteration(t *testing.T) {
	c := openTest(t, testKS, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs, err := c.Query(ctx, driver.Query{Text: "SELECT * FROM tobira_big"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	if _, ok, err := rs.Next(ctx); !ok || err != nil {
		t.Fatalf("first row: %v %v", ok, err)
	}
	cancel()
	if _, _, err := rs.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Next after cancel: %v, want context.Canceled", err)
	}
	if err := c.Ping(ctxT(t)); err != nil {
		t.Errorf("the connection is unusable after a cancel: %v", err)
	}
}

func TestQueryLimitAcrossPages(t *testing.T) {
	c := openTest(t, testKS, false)
	_, rows, st := collect(t, c, driver.Query{Text: "SELECT * FROM tobira_big", Limit: 1200})
	if len(rows) != 1200 || !st.Truncated {
		t.Errorf("Limit 1200 over 1500 rows: %d rows, truncated %v", len(rows), st.Truncated)
	}
	_, rows, st = collect(t, c, driver.Query{Text: "SELECT * FROM tobira_big"})
	if len(rows) != 1500 || st.Truncated {
		t.Errorf("unlimited: %d rows over several pages, truncated %v", len(rows), st.Truncated)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsnFor("no_such_keyspace", testPassword, "")}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Open on a missing keyspace: %v, want ErrNotFound", err)
	}
	_, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsnFor(testKS, "", ""), Password: driver.NewSecret("wrong-password")})
	if !errors.Is(err, driver.ErrAuth) || strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("Open with a wrong password: %v, want ErrAuth without the password", err)
	}
	// A connection without a keyspace reads qualified names, and Describe
	// needs a namespace.
	c, err := New().Open(ctxT(t), driver.ConnConfig{DSN: "cassandra://" + testUser + ":" + testPassword + "@" + testHostPort})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, rows, _ := collect(t, c, driver.Query{Text: "SELECT id FROM " + testKS + ".tobira_people WHERE id = 1"}); len(rows) != 1 {
		t.Errorf("qualified read without a keyspace: %v", rows)
	}
	if _, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Describe without a namespace or keyspace: %v, want ErrSyntax", err)
	}
	// Cancelled before the connect ends: Open returns at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor(testKS, testPassword, "")}); !errors.Is(err, context.Canceled) {
		t.Errorf("Open with a cancelled context: %v, want context.Canceled", err)
	}
}

// A UDF named like a built-in shadows it for unqualified calls: the
// read-only guard refuses the name, and the system.-qualified built-in
// still passes.
func TestReadOnlyRefusesShadowingUDF(t *testing.T) {
	if err := exec(ctxT(t), "CREATE FUNCTION "+testKS+".abs (x text) RETURNS NULL ON NULL INPUT RETURNS text LANGUAGE java AS 'return x;'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec(context.Background(), "DROP FUNCTION "+testKS+".abs (text)") })
	rw := openTest(t, testKS, false)
	if _, rows, _ := collect(t, rw, driver.Query{Text: "SELECT abs(name) FROM tobira_people WHERE id = 1"}); len(rows) != 1 {
		t.Fatalf("the shadowing UDF does not run on a writable connection: %v", rows)
	}
	ro := openTest(t, testKS, true)
	for _, text := range []string{
		"SELECT abs(name) FROM tobira_people WHERE id = 1",
		"SELECT abs(age) FROM tobira_people WHERE id = 1",
		"SELECT abs(age) FROM " + testKS + ".tobira_people WHERE id = 1",
	} {
		if _, err := ro.Query(ctxT(t), driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q on a read-only conn: %v, want ErrReadOnly", text, err)
		}
	}
	if _, rows, _ := collect(t, ro, driver.Query{Text: "SELECT system.abs(age) FROM tobira_people WHERE id = 1"}); len(rows) != 1 {
		t.Errorf("system.abs on a read-only conn: %v", rows)
	}
	if _, rows, _ := collect(t, ro, driver.Query{Text: "SELECT floor(ts, 1m), count(*) FROM events WHERE tenant = 'acme' GROUP BY floor(ts, 1m)"}); len(rows) == 0 {
		t.Error("floor() GROUP BY on a read-only conn: no rows")
	}
}

func TestMutatorRefusesCounterTable(t *testing.T) {
	c := openTest(t, testKS, false)
	m, _ := driver.As[driver.Mutator](c)
	ref := driver.CollectionRef{Name: "counters"}
	if _, err := m.Insert(ctxT(t), ref, driver.Object{{Key: "id", Value: driver.Int(2)}, {Key: "hits", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Insert into a counter table: %v, want ErrUnsupported", err)
	}
	if err := m.Delete(ctxT(t), ref, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Delete from a counter table: %v, want ErrUnsupported", err)
	}
}

// Describe is cached for a short while; a DDL statement run through the
// driver, or Invalidate, drops the cache.
func TestDescribeCacheInvalidation(t *testing.T) {
	if err := exec(ctxT(t), "CREATE TABLE "+testKS+".cache_probe (id int PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec(context.Background(), "DROP TABLE "+testKS+".cache_probe") })
	c := openTest(t, testKS, false)
	ref := driver.CollectionRef{Name: "cache_probe"}
	fields := func() int {
		t.Helper()
		s, err := c.Describe(ctxT(t), ref)
		if err != nil {
			t.Fatal(err)
		}
		return len(s.Fields)
	}
	if n := fields(); n != 1 {
		t.Fatalf("fields = %d", n)
	}
	// Outside the driver: the cache still answers until Invalidate.
	if err := exec(ctxT(t), "ALTER TABLE "+testKS+".cache_probe ADD a int"); err != nil {
		t.Fatal(err)
	}
	if n := fields(); n != 1 {
		t.Errorf("within the TTL Describe should come from the cache: %d fields", n)
	}
	inv, _ := driver.As[driver.CatalogInvalidator](c)
	inv.Invalidate()
	if n := fields(); n != 2 {
		t.Errorf("after Invalidate: %d fields, want 2", n)
	}
	// Through the driver: DDL drops the cache by itself.
	collect(t, c, driver.Query{Text: "ALTER TABLE cache_probe ADD b int"})
	if n := fields(); n != 3 {
		t.Errorf("after DDL through the driver: %d fields, want 3", n)
	}
}

// A paging cursor only works for the table and filter it came from.
func TestBrowseCursorBound(t *testing.T) {
	c := openTest(t, testKS, false)
	b, _ := driver.As[driver.Browser](c)
	rs, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	_, st, _ := driver.Collect(ctxT(t), rs, 0)
	if st.NextCursor == "" {
		t.Fatal("no cursor")
	}
	if _, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_big"}, driver.BrowseOptions{Limit: 5, Cursor: st.NextCursor}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("a cursor on another table: %v, want ErrSyntax", err)
	}
	if _, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people"}, driver.BrowseOptions{Limit: 5, Cursor: st.NextCursor,
		Filter: driver.Filter{Raw: "age > 30"}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("a cursor with another filter: %v, want ErrSyntax", err)
	}
}
