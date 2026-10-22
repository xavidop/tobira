package drivertest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

var (
	wideRef   = driver.CollectionRef{Namespace: "main", Name: WideColumnCollection}
	searchRef = driver.CollectionRef{Namespace: "main", Name: SearchCollection}
)

// The Fake's own queries for the two fixtures.
var (
	widePartitionQuery = driver.Query{Text: "select * from " + WideColumnCollection + " where tenant = '" + WideColumnPartition + "'"}
	searchQuery        = driver.Query{Text: "search " + SearchCollection + " for '" + SearchTerm + "'"}
)

func wideSearchFake(t *testing.T, opts ...FakeOption) (*Fake, driver.Conn) {
	t.Helper()
	f := NewFake(append([]FakeOption{WithWideColumns(), WithSearch()}, opts...)...)
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return f, c
}

// The wide-column fixture: a composite key, NULLs, map and set columns,
// and more than one row per partition in an order that is not the
// clustering order.
func TestWideColumnDatasetShape(t *testing.T) {
	d := WideColumnDataset()
	if want := []string{"tenant", "ts", "seq"}; !slices.Equal(d.Key, want) {
		t.Errorf("Key = %v, want %v", d.Key, want)
	}
	if want := slices.Concat(WideColumnPartitionKey(), sortFields(WideColumnClusteringKey())); !slices.Equal(d.Key, want) {
		t.Errorf("Key %v is not the partition key then the clustering columns %v", d.Key, want)
	}
	types := map[string]driver.Type{}
	for _, c := range d.Columns {
		types[c.Name] = c.Type
	}
	for name, typ := range map[string]driver.Type{
		"tenant": driver.TypeString, "ts": driver.TypeTime, "seq": driver.TypeInt,
		"value": driver.TypeFloat, "attrs": driver.TypeObject, "tags": driver.TypeList,
	} {
		if types[name] != typ {
			t.Errorf("column %s has type %s, want %s", name, types[name], typ)
		}
	}
	nulls, acme := 0, 0
	for _, r := range d.Rows {
		for _, v := range r {
			if v.IsNull() {
				nulls++
			}
		}
		if s, _ := r[0].AsText(); s == WideColumnPartition {
			acme++
		}
	}
	if nulls == 0 {
		t.Error("the fixture has no NULL: it must cover an unset column")
	}
	if acme < 3 {
		t.Errorf("partition %s has %d rows, want at least 3", WideColumnPartition, acme)
	}
	want := KindWideColumnFixture(wideRef, widePartitionQuery).WantRows
	var stored []driver.Row
	for _, r := range d.Rows {
		if s, _ := r[0].AsText(); s == WideColumnPartition {
			stored = append(stored, r)
		}
	}
	if slices.EqualFunc(stored, want, func(a, b driver.Row) bool { return slices.EqualFunc(a, b, EqualValue) }) {
		t.Error("the dataset already lists the partition in clustering order: a driver that ignores clustering would pass")
	}
}

// The expected partition is in clustering order: ts descending, then seq
// ascending.
func TestKindWideColumnFixtureOrder(t *testing.T) {
	fx := KindWideColumnFixture(wideRef, widePartitionQuery)
	if len(fx.WantRows) < 3 {
		t.Fatalf("WantRows has %d rows", len(fx.WantRows))
	}
	ti, si := slices.IndexFunc(fx.WantColumns, func(c driver.Column) bool { return c.Name == "ts" }),
		slices.IndexFunc(fx.WantColumns, func(c driver.Column) bool { return c.Name == "seq" })
	for i := 1; i < len(fx.WantRows); i++ {
		a, _ := fx.WantRows[i-1][ti].AsTime()
		b, _ := fx.WantRows[i][ti].AsTime()
		x, _ := fx.WantRows[i-1][si].AsInt()
		y, _ := fx.WantRows[i][si].AsInt()
		if a.Before(b) || (a.Equal(b) && x > y) {
			t.Errorf("row %d is out of clustering order (ts desc, seq asc)", i)
		}
	}
	if !slices.Equal(fx.PartitionKey, []string{"tenant"}) || len(fx.ClusteringKey) != 2 || !fx.ClusteringKey[0].Desc || fx.ClusteringKey[1].Desc {
		t.Errorf("keys: partition %v, clustering %v", fx.PartitionKey, fx.ClusteringKey)
	}
}

// The Fake reports the partition and clustering keys and keeps each
// partition in clustering order.
func TestFakeWideColumns(t *testing.T) {
	_, c := wideSearchFake(t)
	sch, err := c.Describe(context.Background(), wideRef)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sch.PartitionKey, WideColumnPartitionKey()) || !slices.Equal(sch.ClusteringKey, WideColumnClusteringKey()) {
		t.Errorf("Describe keys: partition %v, clustering %v", sch.PartitionKey, sch.ClusteringKey)
	}
	if !slices.Equal(sch.PrimaryKey, WideColumnDataset().Key) {
		t.Errorf("PrimaryKey = %v", sch.PrimaryKey)
	}
	cols, rows, _, err := runQuery(context.Background(), c, widePartitionQuery)
	if err != nil {
		t.Fatal(err)
	}
	fx := KindWideColumnFixture(wideRef, widePartitionQuery)
	if len(rows) != len(fx.WantRows) {
		t.Fatalf("partition query: %d rows, want %d", len(rows), len(fx.WantRows))
	}
	ti := columnIndexOf(cols, "ts")
	wi := columnIndexOf(fx.WantColumns, "ts")
	for i := range rows {
		if !EqualValue(fx.WantRows[i][wi], rows[i][ti]) {
			t.Errorf("row %d ts = %s, want %s", i, rows[i][ti], fx.WantRows[i][wi])
		}
	}
	// Other stores report no partition or clustering key.
	f2 := NewFake(WithPeople())
	c2, _ := f2.Driver().Open(context.Background(), driver.ConnConfig{})
	defer func() { _ = c2.Close() }()
	ps, err := c2.Describe(context.Background(), driver.CollectionRef{Namespace: "main", Name: peopleCollection})
	if err != nil {
		t.Fatal(err)
	}
	if ps.PartitionKey != nil || ps.ClusteringKey != nil {
		t.Errorf("People: partition %v, clustering %v, want nil", ps.PartitionKey, ps.ClusteringKey)
	}
}

func columnIndexOf(cols []driver.Column, name string) int {
	return slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
}

// The search fixture: documents with text fields, a keyword list, numbers,
// an object and a missing field; the term matches exactly three, one of
// them clearly best.
func TestSearchDocumentsShape(t *testing.T) {
	docs := SearchDocuments()
	if len(docs) < 5 {
		t.Fatalf("%d documents", len(docs))
	}
	fx := KindSearchFixture(searchRef, searchQuery)
	if len(fx.WantIDs) != 3 || !EqualValue(fx.WantTopID, driver.String("a1")) {
		t.Errorf("WantIDs %v, WantTopID %s", fx.WantIDs, fx.WantTopID)
	}
	if fx.Type != driver.CollectionIndex {
		t.Errorf("Type = %q, want index", fx.Type)
	}
	names := map[string]driver.Field{}
	for _, f := range fx.Fields {
		names[f.Name] = f
	}
	for _, n := range []string{"title", "body", "tags", "views", "rating", "author"} {
		if _, ok := names[n]; !ok {
			t.Errorf("mapping lacks %s", n)
		}
	}
	if a := names["author"]; a.Type != driver.TypeObject || len(a.Fields) != 2 {
		t.Errorf("author = %+v, want an Object with two nested fields", a)
	}
	if names["tags"].Type != driver.TypeUnknown {
		t.Errorf("tags must accept any type (a keyword field holding a list), got %s", names["tags"].Type)
	}
}

// The Fake answers "search <coll> for '<term>'" with a _score column,
// best first, and only the matching documents.
func TestFakeSearch(t *testing.T) {
	_, c := wideSearchFake(t)
	sch, err := c.Describe(context.Background(), searchRef)
	if err != nil {
		t.Fatal(err)
	}
	if sch.Type != driver.CollectionIndex {
		t.Errorf("Describe type %q, want index", sch.Type)
	}
	cols, rows, _, err := runQuery(context.Background(), c, searchQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 || cols[0].Name != "_score" || cols[0].Type != driver.TypeFloat {
		t.Fatalf("columns %v: want _score (float) first", colNames(cols))
	}
	idi := columnIndexOf(cols, "_id")
	var ids []string
	for _, r := range rows {
		s, _ := r[idi].AsText()
		ids = append(ids, s)
	}
	if !slices.Equal(ids, []string{"a1", "a2", "a3"}) {
		t.Errorf("hits %v, want a1 first then a2, a3", ids)
	}
	if _, _, _, err := runQuery(context.Background(), c, driver.Query{Text: "search nope for 'x'"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("search on a missing collection: %v", err)
	}
	if _, _, _, err := runQuery(context.Background(), c, driver.Query{Text: "search " + SearchCollection + " 'x'"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("search without for: %v", err)
	}
	// A search is a read: a read-only query may run it.
	if _, _, _, err := runQuery(context.Background(), c, driver.Query{Text: searchQuery.Text, ReadOnly: true}); err != nil {
		t.Errorf("read-only search: %v", err)
	}
}

// WithKind sets the kind the Fake reports.
func TestFakeWithKind(t *testing.T) {
	if k := NewFake(WithKind(driver.KindSearch)).Driver().Info().Kind; k != driver.KindSearch {
		t.Errorf("Kind = %q", k)
	}
	if k := NewFake().Driver().Info().Kind; k != driver.KindOther {
		t.Errorf("default Kind = %q", k)
	}
}

// ---- Strict checks ----

func wideSearchConfig(d driver.Driver) Config {
	open := func(tb testing.TB) driver.Conn {
		c, err := d.Open(context.Background(), driver.ConnConfig{})
		if err != nil {
			tb.Fatal(err)
		}
		return c
	}
	return Config{
		Driver: d, Open: open, OpenReadOnly: open, Strict: true,
		Fixture: Fixture{
			WideColumn: KindWideColumnFixture(wideRef, widePartitionQuery),
			Search:     KindSearchFixture(searchRef, searchQuery),
		},
	}
}

// Outside Strict a fixture without a partition query skips the read; the
// mapping check allows the IDField beside the fixture's fields.
func TestWideColumnPartitionOptionalOutsideStrict(t *testing.T) {
	f := NewFake(WithWideColumns(), WithSearch(), WithKind(driver.KindWideColumn))
	c := wideSearchConfig(f.Driver())
	c.Strict = false
	c.Fixture.WideColumn.Partition = driver.Query{}
	if !t.Run("widecolumn", func(t *testing.T) { testWideColumn(t, c) }) {
		t.Error("a non-Strict run without a partition query must pass")
	}
}

// The honest Fake passes both checks, whatever kind it reports.
func TestStrictWideSearchHonest(t *testing.T) {
	for _, k := range []driver.Kind{driver.KindOther, driver.KindWideColumn, driver.KindSearch} {
		f := NewFake(WithWideColumns(), WithSearch(), WithKind(k))
		c := wideSearchConfig(f.Driver())
		t.Run(string(k)+"/widecolumn", func(t *testing.T) { testWideColumn(t, c) })
		t.Run(string(k)+"/search", func(t *testing.T) { testSearch(t, c) })
	}
}

// Without a fixture the checks skip, unless the driver is of that kind
// and runs Strict.
func TestStrictWideSearchMissingFixture(t *testing.T) {
	f := NewFake(WithWideColumns(), WithSearch())
	c := wideSearchConfig(f.Driver())
	c.Fixture.WideColumn, c.Fixture.Search = nil, nil
	t.Run("other kind skips", func(t *testing.T) {
		ok := t.Run("widecolumn", func(t *testing.T) { testWideColumn(t, c) })
		ok = t.Run("search", func(t *testing.T) { testSearch(t, c) }) && ok
		if !ok {
			t.Error("a driver of another kind must skip, not fail")
		}
	})
}

// wrongWide answers Describe without clustering order and reads the
// partition in stored order.
type wrongWide struct{ driver.Conn }

func (c wrongWide) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	s, err := c.Conn.Describe(ctx, ref)
	if err == nil && s.ClusteringKey != nil {
		for i := range s.ClusteringKey {
			s.ClusteringKey[i].Desc = false
		}
	}
	return s, err
}

// noPartition reports no partition key.
type noPartition struct{ driver.Conn }

func (c noPartition) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	s, err := c.Conn.Describe(ctx, ref)
	if err == nil {
		s.PartitionKey = nil
	}
	return s, err
}

// unorderedPartition returns the partition in reverse.
type unorderedPartition struct{ driver.Conn }

func (c unorderedPartition) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	rs, err := c.Conn.Query(ctx, q)
	if err != nil || !strings.Contains(q.Text, "tenant") {
		return rs, err
	}
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows)
	return driver.NewSliceResultSet(rs.Columns(), rows, st), nil
}

// ascendingScore returns hits worst first.
type ascendingScore struct{ driver.Conn }

func (c ascendingScore) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	rs, err := c.Conn.Query(ctx, q)
	if err != nil || !strings.HasPrefix(q.Text, "search") {
		return rs, err
	}
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows)
	return driver.NewSliceResultSet(rs.Columns(), rows, st), nil
}

// wrongHits answers another term.
type wrongHits struct{ driver.Conn }

func (c wrongHits) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if strings.HasPrefix(q.Text, "search") {
		return c.Conn.Query(ctx, driver.Query{Text: "search " + SearchCollection + " for 'water'"})
	}
	return c.Conn.Query(ctx, q)
}

// noScore drops the _score column.
type noScore struct{ driver.Conn }

func (c noScore) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	rs, err := c.Conn.Query(ctx, q)
	if err != nil || !strings.HasPrefix(q.Text, "search") {
		return rs, err
	}
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i] = rows[i][1:]
	}
	return driver.NewSliceResultSet(rs.Columns()[1:], rows, st), nil
}

// noMapping describes the index without its nested author fields.
type noMapping struct{ driver.Conn }

func (c noMapping) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	s, err := c.Conn.Describe(ctx, ref)
	if err == nil {
		for i := range s.Fields {
			s.Fields[i].Fields = nil
		}
	}
	return s, err
}

// extraMapping describes the index with a field the fixture does not
// define, at the top level and inside author.
type extraMapping struct{ driver.Conn }

func (c extraMapping) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	s, err := c.Conn.Describe(ctx, ref)
	if err == nil {
		for i := range s.Fields {
			if s.Fields[i].Name == "author" {
				s.Fields[i].Fields = append(s.Fields[i].Fields, driver.Field{Name: "age", Type: driver.TypeInt})
			}
		}
		s.Fields = append(s.Fields, driver.Field{Name: "leaked", Type: driver.TypeString})
	}
	return s, err
}

func TestHelperBrokenWideSearch(t *testing.T) {
	wraps := map[string]func(driver.Conn) driver.Conn{
		"clustering-order":    func(c driver.Conn) driver.Conn { return wrongWide{c} },
		"no-partition":        func(c driver.Conn) driver.Conn { return noPartition{c} },
		"unordered-partition": func(c driver.Conn) driver.Conn { return unorderedPartition{c} },
		"ascending-score":     func(c driver.Conn) driver.Conn { return ascendingScore{c} },
		"wrong-hits":          func(c driver.Conn) driver.Conn { return wrongHits{c} },
		"no-score":            func(c driver.Conn) driver.Conn { return noScore{c} },
		"no-mapping":          func(c driver.Conn) driver.Conn { return noMapping{c} },
		"extra-mapping":       func(c driver.Conn) driver.Conn { return extraMapping{c} },
	}
	variant := os.Getenv("TOBIRA_DRIVERTEST_HELPER")
	if variant == "missing-wide" || variant == "missing-search" {
		kind := driver.KindWideColumn
		if variant == "missing-search" {
			kind = driver.KindSearch
		}
		f := NewFake(WithWideColumns(), WithSearch(), WithKind(kind))
		c := wideSearchConfig(f.Driver())
		c.Fixture.WideColumn, c.Fixture.Search = nil, nil
		t.Run("widecolumn", func(t *testing.T) { testWideColumn(t, c) })
		t.Run("search", func(t *testing.T) { testSearch(t, c) })
		return
	}
	if variant == "no-partition-query" {
		f := NewFake(WithWideColumns(), WithSearch(), WithKind(driver.KindWideColumn))
		c := wideSearchConfig(f.Driver())
		c.Fixture.WideColumn.Partition = driver.Query{}
		testWideColumn(t, c)
		return
	}
	w, ok := wraps[variant]
	if !ok {
		t.Skip("helper process only")
	}
	f := NewFake(WithWideColumns(), WithSearch())
	c := wideSearchConfig(brokenDriver{f: f, wrap: w})
	switch variant {
	case "clustering-order", "no-partition", "unordered-partition":
		testWideColumn(t, c)
	default:
		testSearch(t, c)
	}
}

// The Strict checks fail a driver that gets the keys, the clustering
// order, the hits, the ranking or the mapping wrong, and a driver of the
// kind that gives no fixture.
func TestStrictWideSearchCatchWrongAnswers(t *testing.T) {
	for variant, want := range map[string]string{
		"clustering-order":    "ClusteringKey",
		"no-partition":        "PartitionKey",
		"unordered-partition": "clustering order",
		"ascending-score":     "_score",
		"wrong-hits":          "hits",
		"no-score":            "_score",
		"no-mapping":          "author.name",
		"extra-mapping":       "author.age",
		"no-partition-query":  "Partition",
		"missing-wide":        "Fixture.WideColumn",
		"missing-search":      "Fixture.Search",
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperBrokenWideSearch$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "TOBIRA_DRIVERTEST_HELPER="+variant)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Errorf("%s: the Strict checks passed a wrong driver (err=%v):\n%s", variant, err, out)
			continue
		}
		if !strings.Contains(string(out), want) {
			t.Errorf("%s: failure output does not mention %q:\n%s", variant, want, out)
		}
	}
}
