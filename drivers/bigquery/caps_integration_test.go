//go:build integration

package bigquery

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestNamespacesAndCollections(t *testing.T) {
	c := open(t, "", false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
		if ns.Label != "dataset" {
			t.Errorf("namespace %s label %q", ns.Name, ns.Label)
		}
		if ns.Default != (ns.Name == testDataset) {
			t.Errorf("namespace %s Default = %v", ns.Name, ns.Default)
		}
	}
	for _, want := range []string{testDataset, conformanceDataset, nsDataset} {
		if !slices.Contains(names, want) {
			t.Errorf("namespaces %v lack %s", names, want)
		}
	}
	colls, err := c.Collections(ctxT(t), driver.NamespaceRef{})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]driver.CollectionType{}
	for _, col := range colls {
		types[col.Ref.Name] = col.Type
		if col.Ref.Namespace != testDataset || col.RowEstimate != -1 {
			t.Errorf("collection %+v", col)
		}
	}
	if types["tobira_people"] != driver.CollectionTable || types["tobira_people_view"] != driver.CollectionView || types["tobira_kinds"] != driver.CollectionTable {
		t.Errorf("collection types = %v", types)
	}
	other, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: nsDataset})
	if err != nil || len(other) != 1 || other[0].Ref.Name != "things" {
		t.Errorf("Collections(%s) = %v, %v", nsDataset, other, err)
	}
	if _, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(nope): want ErrNotFound, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	c := open(t, "", false)
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.PrimaryKey, []string{"id"}) || s.Comment != "the conformance People" || s.Type != driver.CollectionTable {
		t.Errorf("schema = %+v", s)
	}
	if s.Fields[0].Nullable || !s.Fields[1].Nullable {
		t.Errorf("id nullable %v, name nullable %v", s.Fields[0].Nullable, s.Fields[1].Nullable)
	}
	if s.RowEstimate != 57 {
		t.Errorf("RowEstimate = %d", s.RowEstimate)
	}
	k, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: testDataset, Name: "tobira_kinds"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range k.Fields {
		got[f.Name] = f.NativeType + "/" + f.Type.String()
	}
	want := map[string]string{
		"id": "INT64/int", "d": "DATE/time", "dt": "DATETIME/time", "tm": "TIME/string", "b": "BYTES/bytes",
		"f": "FLOAT64/float", "big": "BIGNUMERIC/decimal", "arr": "ARRAY<INT64>/list",
		"rec": "STRUCT<label STRING, tags ARRAY<STRING>>/object",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("field %s = %s, want %s", name, got[name], w)
		}
	}
	if k.Fields[0].Comment != "row id" {
		t.Errorf("id comment %q", k.Fields[0].Comment)
	}
	rec := k.Fields[len(k.Fields)-1]
	if len(rec.Fields) != 2 || rec.Fields[1].Name != "tags" || rec.Fields[1].Type != driver.TypeList {
		t.Errorf("rec nested fields = %+v", rec.Fields)
	}
	v, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people_view"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != driver.CollectionView || !strings.Contains(v.DDL, "SELECT id, name") {
		t.Errorf("view schema = %+v", v)
	}
}

func TestKindsValues(t *testing.T) {
	c := open(t, "", true)
	cols, rows, _ := queryAll(t, c, driver.Query{Text: "SELECT * FROM tobira_kinds ORDER BY id"})
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	byName := map[string]driver.Value{}
	for i, col := range cols {
		byName[col.Name] = rows[0][i]
	}
	check := func(name string, typ driver.Type, text string) {
		t.Helper()
		v := byName[name]
		if v.Type() != typ || v.String() != text {
			t.Errorf("%s = %s (%s), want %s (%s)", name, v, v.Type(), text, typ)
		}
	}
	d, _ := byName["d"].AsTime()
	if !d.Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("d = %v", d)
	}
	dt, _ := byName["dt"].AsTime()
	if !dt.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) {
		t.Errorf("dt = %v", dt)
	}
	check("tm", driver.TypeString, "05:06:07.123456")
	if b, _ := byName["b"].AsBytes(); string(b) != "\x00\x01" {
		t.Errorf("b = %q", b)
	}
	check("f", driver.TypeFloat, "1.5")
	check("big", driver.TypeDecimal, "123456789012345678901234567890.5")
	if l, ok := byName["arr"].AsList(); !ok || len(l) != 3 {
		t.Errorf("arr = %s (%s)", byName["arr"], byName["arr"].Type())
	}
	obj, ok := byName["rec"].AsObject()
	if !ok || len(obj) != 2 || obj[0].Key != "label" || obj[1].Key != "tags" {
		t.Fatalf("rec = %s (%s)", byName["rec"], byName["rec"].Type())
	}
	if l, ok := obj[1].Value.AsList(); !ok || len(l) != 2 {
		t.Errorf("rec.tags = %s", obj[1].Value)
	}
	for i, col := range cols {
		v := rows[1][i]
		switch col.Name {
		case "id":
		case "arr":
			if l, ok := v.AsList(); !ok || len(l) != 0 {
				t.Errorf("empty arr = %s (%s)", v, v.Type())
			}
		default:
			if !v.IsNull() {
				t.Errorf("row 2 %s = %s, want NULL", col.Name, v)
			}
		}
	}
}

func TestQueryNamespaceAndParams(t *testing.T) {
	c := open(t, "", true)
	_, rows, _ := queryAll(t, c, driver.Query{Text: "SELECT label FROM things", Namespace: nsDataset})
	if len(rows) != 1 || rows[0][0].String() != "in-ns" {
		t.Errorf("namespace query rows = %v", rows)
	}
	_, rows, _ = queryAll(t, c, driver.Query{Text: "SELECT id FROM tobira_people WHERE name = ? AND active = ? AND score > ? ORDER BY id",
		Params: []driver.Value{driver.String("Conan O'Brien"), driver.Bool(true), driver.Decimal("100.5")}})
	if len(rows) == 0 {
		t.Error("parameter query returned no rows")
	}
	if _, err := c.Query(ctxT(t), driver.Query{Text: "SELECT ?", Params: []driver.Value{driver.List()}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("list parameter: want ErrUnsupported, got %v", err)
	}
}

func TestWritableDML(t *testing.T) {
	c := open(t, "", false)
	queryAll(t, c, driver.Query{Text: "CREATE TABLE `" + testProject + "." + testDataset + ".tobira_scratch` (id INT64, v STRING)"})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rs, err := c.Query(ctx, driver.Query{Text: "DROP TABLE `" + testProject + "." + testDataset + ".tobira_scratch`"}); err == nil {
			_ = rs.Close()
		}
	})
	_, _, st := queryAll(t, c, driver.Query{Text: "INSERT INTO tobira_scratch (id, v) VALUES (1, 'a'), (2, 'b')"})
	if st.RowsAffected != 2 && st.RowsAffected != -1 {
		t.Errorf("INSERT RowsAffected = %d", st.RowsAffected)
	}
	_, rows, _ := queryAll(t, c, driver.Query{Text: "SELECT COUNT(*) FROM tobira_scratch"})
	if n, _ := rows[0][0].AsInt(); n != 2 {
		t.Errorf("count after insert = %d", n)
	}
	t.Logf("emulator INSERT RowsAffected = %d", st.RowsAffected)
}

func TestBrowseModes(t *testing.T) {
	c := open(t, "", true)
	b, _ := driver.As[driver.Browser](c)
	ref := driver.CollectionRef{Name: "tobira_people"}
	page := func(opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats) {
		t.Helper()
		rs, err := b.Browse(ctxT(t), ref, opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		return rs.Columns(), rows, st
	}
	cols, _, st := page(driver.BrowseOptions{Limit: 10})
	if !strings.HasPrefix(st.NextCursor, listPrefix) {
		t.Errorf("plain browse cursor %q, want a tabledata.list cursor", st.NextCursor)
	}
	if !cols[0].Key || cols[0].Source == nil || cols[0].Source.Name != "tobira_people" {
		t.Errorf("id column = %+v", cols[0])
	}
	// A filtered, sorted browse pages with OFFSET cursors.
	var ids []int64
	cursor := ""
	for range 10 {
		cols, rows, st := page(driver.BrowseOptions{Limit: 4, Cursor: cursor,
			Sort:   []driver.SortKey{{Field: "id", Desc: true}},
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(3), driver.Int(5), driver.Int(8), driver.Int(13), driver.Int(21), driver.Int(34))}}}})
		id := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == "id" })
		for _, r := range rows {
			n, _ := r[id].AsInt()
			ids = append(ids, n)
		}
		cursor = st.NextCursor
		if cursor == "" {
			break
		}
		if !strings.HasPrefix(cursor, offsetPrefix) {
			t.Fatalf("query browse cursor %q", cursor)
		}
	}
	if !slices.Equal(ids, []int64{34, 21, 13, 8, 5, 3}) {
		t.Errorf("filtered sorted ids = %v", ids)
	}
	for _, tc := range []struct {
		cond driver.Condition
		want int
	}{
		{driver.Condition{Field: "email", Op: driver.OpPrefix, Value: driver.String("user0")}, 9},
		{driver.Condition{Field: "age", Op: driver.OpIsNull}, 5},
		{driver.Condition{Field: "age", Op: driver.OpNotNull}, 52},
		{driver.Condition{Field: "name", Op: driver.OpContains, Value: driver.String("O'B")}, 4},
		{driver.Condition{Field: "name", Op: driver.OpContains, Value: driver.String("%")}, 0},
		{driver.Condition{Field: "id", Op: driver.OpLe, Value: driver.Int(10)}, 10},
		{driver.Condition{Field: "id", Op: driver.OpIn, Value: driver.List()}, 0},
	} {
		_, rows, _ := page(driver.BrowseOptions{Limit: 100, Filter: driver.Filter{Conditions: []driver.Condition{tc.cond}}})
		if len(rows) != tc.want {
			t.Errorf("Browse(%s %s %s): %d rows, want %d", tc.cond.Field, tc.cond.Op, tc.cond.Value, len(rows), tc.want)
		}
	}
	if _, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "tags"}}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("sort by JSON: want ErrUnsupported, got %v", err)
	}
	if _, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "nope"}}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("sort by a missing column: want ErrNotFound, got %v", err)
	}
	if _, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Cursor: "garbage"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("bad cursor: want ErrUnsupported, got %v", err)
	}
	// A view cannot be read with tabledata.list: it pages through a query.
	var seen int
	cursor = ""
	for range 10 {
		rs, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_people_view"}, driver.BrowseOptions{Limit: 25, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		rows, st, _ := driver.Collect(ctxT(t), rs, 0)
		seen += len(rows)
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if seen != 57 {
		t.Errorf("view browse saw %d rows, want 57", seen)
	}
}

// The conformance Slow query really runs for longer than the kit's 5s, so
// its mid-query cancellation is meaningful.
func TestSlowQueryIsSlow(t *testing.T) {
	c := open(t, conformanceDataset, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5500*time.Millisecond)
	defer cancel()
	rs, err := c.Query(ctx, driver.Query{Text: "select count(*) from unnest(generate_array(1, 30000)) a, unnest(generate_array(1, 30000)) b"})
	if err == nil {
		_, _, err = driver.Collect(ctx, rs, 0)
		_ = rs.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("slow query: want it still running after 5.5s (DeadlineExceeded), got %v", err)
	}
}

func TestExplainOnEmulator(t *testing.T) {
	c := open(t, "", true)
	x, _ := driver.As[driver.Explainer](c)
	plan, err := x.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM tobira_people"}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"statement type: SELECT", "bytes processed (estimate):", "maximum bytes billed: 1073741824"} {
		if !strings.Contains(plan.Text, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan.Text)
		}
	}
	t.Logf("emulator plan:\n%s", plan.Text)
}

func TestCompleteColumns(t *testing.T) {
	c := open(t, "", true)
	cp, _ := driver.As[driver.Completer](c)
	text := "select na from tobira_people"
	got, err := cp.Complete(ctxT(t), driver.CompletionRequest{Text: text, Cursor: len("select na")})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got, func(c driver.Completion) bool { return c.Label == "name" }) {
		t.Errorf("completions %v lack name", got)
	}
	got, err = cp.Complete(ctxT(t), driver.CompletionRequest{Text: "select * from tobira_ns.", Cursor: len("select * from tobira_ns.")})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got, func(c driver.Completion) bool { return c.Label == "things" }) {
		t.Errorf("qualified completions %v lack things", got)
	}
}
