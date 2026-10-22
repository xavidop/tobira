package drivertest

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// kindSubtests check the kind-specific shapes that need no capability:
// they run when their fixture is set, and under Config.Strict a driver of
// that kind must set it.
var kindSubtests = []subtest{
	{"WideColumn", testWideColumn},
	{"Search", testSearch},
}

// missingKindFixture ends a kind subtest whose fixture is nil: a failure
// for a Strict driver of that kind, a skip otherwise.
func missingKindFixture(t *testing.T, c Config, kind driver.Kind, field, builder string) {
	t.Helper()
	if c.Strict && c.Driver.Info().Kind == kind {
		t.Fatalf("Config.Strict: a %s driver must set Fixture.%s (see %s)", kind, field, builder)
	}
	t.Skipf("no Fixture.%s", field)
}

func testWideColumn(t *testing.T, c Config) {
	f := c.Fixture.WideColumn
	if f == nil {
		missingKindFixture(t, c, driver.KindWideColumn, "WideColumn", "KindWideColumnFixture")
	}
	conn := openConn(t, c.Open)
	ctx := ctxTimeout(t)
	sch, err := conn.Describe(ctx, f.Collection)
	if err != nil {
		t.Fatalf("Describe(%s): %v", f.Collection.Name, err)
	}
	if !slices.Equal(sch.PartitionKey, f.PartitionKey) {
		t.Errorf("Describe(%s): PartitionKey %q, want %q", f.Collection.Name, sch.PartitionKey, f.PartitionKey)
	}
	if !slices.Equal(sch.ClusteringKey, f.ClusteringKey) {
		t.Errorf("Describe(%s): ClusteringKey %v, want %v (field and order)", f.Collection.Name, sch.ClusteringKey, f.ClusteringKey)
	}
	if want := slices.Concat(f.PartitionKey, sortFields(f.ClusteringKey)); !slices.Equal(sch.PrimaryKey, want) {
		t.Errorf("Describe(%s): PrimaryKey %q, want the partition key then the clustering columns %q", f.Collection.Name, sch.PrimaryKey, want)
	}
	for _, want := range f.WantColumns {
		i := slices.IndexFunc(sch.Fields, func(fd driver.Field) bool { return fd.Name == want.Name })
		switch {
		case i < 0:
			t.Errorf("Describe(%s): field %q is missing", f.Collection.Name, want.Name)
		case sch.Fields[i].Type != want.Type:
			t.Errorf("Describe(%s): field %q: want Type %s, got %s (native type %q)",
				f.Collection.Name, want.Name, want.Type, sch.Fields[i].Type, sch.Fields[i].NativeType)
		}
	}
	if f.Partition.Text == "" {
		if c.Strict {
			t.Fatalf("Config.Strict: Fixture.WideColumn.Partition is empty: give the native query that reads partition %q (see KindWideColumnFixture)", WideColumnPartition)
		}
		return
	}
	cols, rows, _, err := runQuery(ctx, conn, f.Partition)
	if err != nil {
		t.Fatalf("Query(Partition): %v", err)
	}
	if len(rows) != len(f.WantRows) {
		t.Fatalf("Query(Partition): got %d rows, want %d", len(rows), len(f.WantRows))
	}
	for wi, wc := range f.WantColumns {
		ci := columnIndex(t, cols, wc.Name)
		for r, want := range f.WantRows {
			if got := rows[r][ci]; !EqualValue(want[wi], got) {
				t.Errorf("Query(Partition): row %d column %q is %s (%s), want %s (%s): return the partition in clustering order",
					r, wc.Name, got, got.Type(), want[wi], want[wi].Type())
			}
		}
	}
}

func testSearch(t *testing.T, c Config) {
	f := c.Fixture.Search
	if f == nil {
		missingKindFixture(t, c, driver.KindSearch, "Search", "KindSearchFixture")
	}
	conn := openConn(t, c.Open)
	ctx := ctxTimeout(t)
	sch, err := conn.Describe(ctx, f.Collection)
	if err != nil {
		t.Fatalf("Describe(%s): %v", f.Collection.Name, err)
	}
	if f.Type != "" && sch.Type != f.Type {
		t.Errorf("Describe(%s): Type %q, want %q", f.Collection.Name, sch.Type, f.Type)
	}
	idField := f.IDField
	if idField == "" {
		idField = "_id"
	}
	checkMapping(t, f.Collection.Name, "", f.Fields, sch.Fields, idField)

	cols, rows, _, err := runQuery(ctx, conn, f.Query)
	if err != nil {
		t.Fatalf("Query(Search): %v", err)
	}
	si := slices.IndexFunc(cols, func(col driver.Column) bool { return col.Name == "_score" })
	if si < 0 {
		t.Fatalf("Query(Search): result has no _score column (columns %v)", columnNames(cols))
	}
	prev := math.Inf(1)
	for r, row := range rows {
		x, ok := numeric(row[si])
		if !ok {
			t.Fatalf("Query(Search): row %d _score is %s, want a number", r, row[si])
		}
		s, _ := x.Float64()
		if s <= 0 {
			t.Errorf("Query(Search): row %d _score %g, want a positive relevance score", r, s)
		}
		if s > prev {
			t.Errorf("Query(Search): row %d _score %g is above the previous %g: rank the best hit first", r, s, prev)
		}
		prev = s
	}
	ii := columnIndex(t, cols, idField)
	got := make([]driver.Value, len(rows))
	for r, row := range rows {
		got[r] = row[ii]
	}
	if !sameValueSet(got, f.WantIDs) {
		t.Errorf("Query(Search): hits %v, want exactly %v", got, f.WantIDs)
	}
	if !f.WantTopID.IsNull() && len(rows) > 0 {
		if top := rows[0][ii]; top.Type() != f.WantTopID.Type() || !EqualValue(f.WantTopID, top) {
			t.Errorf("Query(Search): best hit is %s (%s), want %s (%s): rank by relevance, best first", top, top.Type(), f.WantTopID, f.WantTopID.Type())
		}
	}
}

// checkMapping reports the want fields missing from got, or of another
// Type (TypeUnknown accepts any), recursing into nested fields. Where want
// defines the fields of a level, got may hold no other field there: the
// allowed names (the id field, at the top level) are the only extras. A
// want field with no nested Fields leaves its sub-fields unchecked.
func checkMapping(t *testing.T, coll, prefix string, want, got []driver.Field, allowed ...string) {
	t.Helper()
	for _, w := range want {
		path := prefix + w.Name
		i := slices.IndexFunc(got, func(fd driver.Field) bool { return fd.Name == w.Name })
		if i < 0 {
			t.Errorf("Describe(%s): mapping lacks field %q", coll, path)
			continue
		}
		if w.Type != driver.TypeUnknown && got[i].Type != w.Type {
			t.Errorf("Describe(%s): field %q: want Type %s, got %s (native type %q)", coll, path, w.Type, got[i].Type, got[i].NativeType)
		}
		if len(w.Fields) > 0 {
			checkMapping(t, coll, path+".", w.Fields, got[i].Fields)
		}
	}
	for _, g := range got {
		known := slices.ContainsFunc(want, func(w driver.Field) bool { return w.Name == g.Name })
		if !known && !slices.Contains(allowed, g.Name) {
			t.Errorf("Describe(%s): mapping has unexpected field %q (the fixture does not define it)", coll, prefix+g.Name)
		}
	}
}

// sameValueSet reports whether got and want hold the same values, each
// with the same Type, ignoring order.
func sameValueSet(got, want []driver.Value) bool {
	if len(got) != len(want) {
		return false
	}
	used := make([]bool, len(want))
	for _, g := range got {
		k := nextMatch(want, 0, g)
		for k >= 0 && used[k] {
			k = nextMatch(want, k+1, g)
		}
		if k < 0 {
			return false
		}
		used[k] = true
	}
	return true
}

// nextMatch is the index of the first value of want from i on that equals
// g with the same Type, or -1.
func nextMatch(want []driver.Value, i int, g driver.Value) int {
	for ; i < len(want); i++ {
		if want[i].Type() == g.Type() && EqualValue(want[i], g) {
			return i
		}
	}
	return -1
}

func columnNames(cols []driver.Column) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return fmt.Sprint(out)
}
