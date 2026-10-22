//go:build integration

package influxdb

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestNamespaces(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
		if ns.Default != (ns.Name == testDB) || ns.Label != "database" {
			t.Errorf("namespace %+v: want Default only for %s and Label database", ns, testDB)
		}
	}
	if !slices.Contains(names, testDB) || !slices.Contains(names, otherDB) || slices.Contains(names, internalDatabase) {
		t.Errorf("Namespaces = %v, want %s and %s without %s", names, testDB, otherDB, internalDatabase)
	}
}

func TestCollections(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	cols, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: testDB})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, col := range cols {
		names = append(names, col.Ref.Name)
		if col.Type != driver.CollectionMeasurement || col.Ref.Namespace != testDB || col.RowEstimate != -1 {
			t.Errorf("collection %+v", col)
		}
	}
	if want := []string{"cpu", "sparse", "tobira_people", "types"}; !slices.Equal(names, want) {
		t.Errorf("Collections = %v, want %v", names, want)
	}
	if _, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(nope) = %v, want ErrNotFound", err)
	}
}

func TestDescribe(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: testDB, Name: "types"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		typ  driver.Type
		role string
	}{
		"site": {driver.TypeString, "tag"}, "time": {driver.TypeTime, "timestamp"},
		"f": {driver.TypeFloat, "field"}, "i": {driver.TypeInt, "field"}, "u": {driver.TypeDecimal, "field"},
		"s": {driver.TypeString, "field"}, "b": {driver.TypeBool, "field"},
	}
	if len(s.Fields) != len(want) {
		t.Errorf("Describe(types) has %d fields, want %d: %+v", len(s.Fields), len(want), s.Fields)
	}
	for _, f := range s.Fields {
		w, ok := want[f.Name]
		if !ok || f.Type != w.typ || f.Comment != w.role || f.NativeType == "" {
			t.Errorf("field %+v, want type %s role %s", f, w.typ, w.role)
		}
	}
	if !slices.Equal(s.PrimaryKey, []string{"site", "time"}) || s.Type != driver.CollectionMeasurement || s.Schemaless {
		t.Errorf("Describe(types) = pk %v type %s schemaless %v", s.PrimaryKey, s.Type, s.Schemaless)
	}
	_, err = c.Describe(ctxT(t), driver.CollectionRef{Namespace: testDB, Name: "nope"})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe(nope) = %v, want ErrNotFound", err)
	}
}

func TestQueryValues(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	cols, rows, st := query(t, c, driver.Query{Text: "SELECT * FROM types"})
	if len(rows) != 1 || st.RowsAffected != -1 || st.Truncated {
		t.Fatalf("rows %v stats %+v", rows, st)
	}
	r := rows[0]
	check := func(name string, want driver.Value, native string) {
		t.Helper()
		i := colIndex(t, cols, name)
		if got := r[i]; got.Type() != want.Type() || !drivertest.EqualValue(want, got) {
			t.Errorf("%s = %s (%s), want %s (%s)", name, got, got.Type(), want, want.Type())
		}
		if cols[i].NativeType != native {
			t.Errorf("%s NativeType = %q, want %q", name, cols[i].NativeType, native)
		}
	}
	check("site", driver.String("o'hare"), "tag")
	check("f", driver.Float(1.5), "field::float")
	check("i", driver.Int(-3), "field::integer")
	check("u", driver.Decimal("7"), "field::uinteger")
	check("s", driver.String("x"), "field::string")
	check("b", driver.Bool(true), "field::boolean")
	check("time", driver.Time(time.Unix(0, 1767225600000000000).UTC()), "timestamp")
	if ts, _ := r[colIndex(t, cols, "time")].AsTime(); ts.Location() != time.UTC {
		t.Errorf("time is in %v, want UTC", ts.Location())
	}
}

func TestQueryNamespaceAndDatabase(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	_, rows, _ := query(t, c, driver.Query{Text: "SELECT label FROM things", Namespace: otherDB})
	if len(rows) != 1 || rows[0][0].String() != "in-other" {
		t.Errorf("rows from %s = %v", otherDB, rows)
	}
	_, err := c.Query(ctxT(t), driver.Query{Text: "SELECT 1", Namespace: "nope"})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Query in an unknown database = %v, want ErrNotFound", err)
	}

	nodb := open(t, driver.ConnConfig{DSN: "influxdb3://:" + testToken + "@" + testHostPort})
	if _, err := nodb.Query(ctxT(t), driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Query with no database = %v, want ErrSyntax", err)
	}
	_, rows, _ = query(t, c, driver.Query{Text: "WITH w AS (SELECT host, usage_user FROM cpu WHERE host = 'web-1') SELECT count(*) FROM w"})
	if n, _ := rows[0][0].AsInt(); n != 60 {
		t.Errorf("WITH … SELECT count = %s, want 60", rows[0][0])
	}
	if _, rows, _ := query(t, nodb, driver.Query{Text: "SELECT 1", Namespace: testDB}); len(rows) != 1 {
		t.Errorf("Query with Namespace on a connection without a database: %v", rows)
	}
	if nss, err := nodb.Namespaces(ctxT(t)); err != nil || len(nss) < 2 {
		t.Errorf("Namespaces without a database = %v, %v", nss, err)
	}
}

func TestQueryInfluxQL(t *testing.T) {
	c := open(t, driver.ConnConfig{DSN: testDSN("language=influxql")})
	cols, rows, _ := query(t, c, driver.Query{Text: "SELECT usage_user FROM cpu WHERE host = 'web-1' AND time >= '2026-01-01T00:00:00Z' LIMIT 3"})
	if len(rows) != 3 {
		t.Fatalf("InfluxQL SELECT returned %d rows, want 3", len(rows))
	}
	if v, _ := rows[0][colIndex(t, cols, "usage_user")].AsFloat(); v != 0.5 {
		t.Errorf("first usage_user = %v, want 0.5", v)
	}
	_, rows, _ = query(t, c, driver.Query{Text: "SHOW MEASUREMENTS"})
	if len(rows) != 4 {
		t.Errorf("SHOW MEASUREMENTS returned %d rows, want 4", len(rows))
	}
	_, rows, _ = query(t, c, driver.Query{Text: "SELECT usage_user FROM cpu WHERE host =~ /web-2/"})
	if len(rows) != 60 {
		t.Errorf("regex SELECT returned %d rows, want 60", len(rows))
	}
	// Browse and Range build SQL whatever the connection's language.
	b := c.(driver.Browser)
	rs, err := b.Browse(ctxT(t), driver.CollectionRef{Namespace: testDB, Name: "cpu"}, driver.BrowseOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := driver.Collect(ctxT(t), rs, 0); err != nil || len(got) != 5 {
		t.Errorf("Browse on an InfluxQL connection: %d rows, %v", len(got), err)
	}
}

func TestQueryLimitAndEarlyClose(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	_, rows, st := query(t, c, driver.Query{Text: "SELECT * FROM cpu", Limit: 7})
	if len(rows) != 7 || !st.Truncated {
		t.Errorf("Limit 7 over 120 rows: %d rows, truncated %v", len(rows), st.Truncated)
	}
	ctx := ctxT(t)
	rs, err := c.Query(ctx, driver.Query{Text: "SELECT value FROM range(1, 20000000000) AS r(value)"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(ctx); !ok || err != nil {
		t.Fatalf("first row: %v %v", ok, err)
	}
	start := time.Now()
	if err := rs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("closing a long result took %v", d)
	}
	if _, ok, err := rs.Next(ctx); ok || err != nil {
		t.Errorf("Next after Close = %v, %v; want end", ok, err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping after an early close: %v", err)
	}
}

func TestRange(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	r := c.(driver.TimeSeriesReader)
	ref := driver.CollectionRef{Namespace: testDB, Name: "cpu"}
	start := drivertest.TimeSeriesStart()
	read := func(q driver.RangeQuery) ([]driver.Column, []driver.Row) {
		t.Helper()
		rs, err := r.Range(ctxT(t), ref, q)
		if err != nil {
			t.Fatalf("Range(%+v): %v", q, err)
		}
		cols := rs.Columns()
		rows, _, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		return cols, rows
	}

	// Raw points; End is exclusive.
	cols, rows := read(driver.RangeQuery{Field: "usage_user", Start: start, End: start.Add(10 * time.Minute)})
	if len(rows) != 10 || cols[0].Name != "_time" || cols[1].Name != "value" {
		t.Errorf("raw range: %d rows, columns %v", len(rows), cols)
	}

	// Grouped by host: 4 windows of 30 minutes for each of 2 hosts.
	cols, rows = read(driver.RangeQuery{Field: "usage_user", Start: start, End: start.Add(2 * time.Hour),
		Window: 30 * time.Minute, Aggregate: "count", GroupBy: []string{"host"}})
	if len(rows) != 8 || len(cols) != 3 || cols[1].Name != "host" {
		t.Fatalf("grouped range: %d rows, columns %v", len(rows), cols)
	}
	for _, row := range rows {
		if n, _ := row[2].AsInt(); n != 15 {
			t.Errorf("window %v host %v count = %s, want 15", row[0], row[1], row[2])
		}
	}

	// last: the value of the last point of each window.
	ds := drivertest.TimeSeriesDataset()
	_, rows = read(driver.RangeQuery{Field: "usage_user", Start: start, End: start.Add(2 * time.Hour), Window: 30 * time.Minute, Aggregate: "last"})
	if len(rows) != 4 {
		t.Fatalf("last: %d windows, want 4", len(rows))
	}
	for w, row := range rows {
		want := ds.Rows[w*30+29][2]
		if !drivertest.EqualValue(want, row[1]) {
			t.Errorf("last of window %d = %s, want %s", w, row[1], want)
		}
	}

	// A filter on a tag, and windows aligned to the epoch whatever Start is.
	_, rows = read(driver.RangeQuery{Field: "usage_user", Start: start.Add(10 * time.Minute), End: start.Add(2 * time.Hour), Window: time.Hour,
		Aggregate: "count", Filter: driver.Filter{Conditions: []driver.Condition{{Field: "host", Op: driver.OpEq, Value: driver.String("web-1")}}}})
	if len(rows) != 2 {
		t.Fatalf("filtered range: %d windows, want 2", len(rows))
	}
	if ts, _ := rows[0][0].AsTime(); !ts.Equal(start) {
		t.Errorf("first window starts at %v, want %v (epoch-aligned)", ts, start)
	}
	if n, _ := rows[0][1].AsInt(); n != 25 {
		t.Errorf("first window count = %s, want 25 (web-1 points from minute 10 to 59)", rows[0][1])
	}

	if _, err := r.Range(ctxT(t), ref, driver.RangeQuery{Field: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Range on an unknown field = %v, want ErrNotFound", err)
	}
	if _, err := r.Range(ctxT(t), driver.CollectionRef{Namespace: testDB, Name: "nope"}, driver.RangeQuery{Field: "x"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Range on an unknown table = %v, want ErrNotFound", err)
	}
}

func TestBrowse(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	b := c.(driver.Browser)
	ref := driver.CollectionRef{Namespace: testDB, Name: "cpu"}
	var all []driver.Row
	var cols []driver.Column
	cursor := ""
	for page := 0; page < 10; page++ {
		rs, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Limit: 25, Cursor: cursor,
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "HOST", Op: driver.OpEq, Value: driver.String("web-1")}}}})
		if err != nil {
			t.Fatal(err)
		}
		cols = rs.Columns()
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if len(all) != 60 {
		t.Fatalf("browsed %d web-1 rows, want 60", len(all))
	}
	ti := colIndex(t, cols, "time")
	for i := 1; i < len(all); i++ {
		a, _ := all[i-1][ti].AsTime()
		b, _ := all[i][ti].AsTime()
		if !a.After(b) {
			t.Fatalf("rows %d and %d are not newest first: %v then %v", i-1, i, a, b)
		}
	}
	rs, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "usage_user > 20"}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil || len(rows) == 0 {
		t.Errorf("raw filter: %d rows, %v", len(rows), err)
	}
	_, err = b.Browse(ctxT(t), ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "1=1) OR (1=1"}})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("escaping raw filter = %v, want ErrSyntax", err)
	}
}

func TestOpenBadTokenRedacted(t *testing.T) {
	ctx := ctxT(t)
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "influxdb3://:apiv3_wrongwrongwrong@" + testHostPort + "/" + testDB})
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("Open with a wrong token = %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "wrongwrongwrong") {
		t.Errorf("error leaks the token: %v", err)
	}
	// No token at all, on a server with authentication on.
	_, err = New().Open(ctx, driver.ConnConfig{DSN: "influxdb3://" + testHostPort + "/" + testDB})
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("Open without a token on an auth server = %v, want ErrAuth", err)
	}
}

// TestRangeSparse: windows where the field has no point are absent (not
// NULL or 0), and last skips rows without the field.
func TestRangeSparse(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	r := c.(driver.TimeSeriesReader)
	ref := driver.CollectionRef{Namespace: testDB, Name: "sparse"}
	start := drivertest.TimeSeriesStart()
	for agg, want := range map[string][]float64{"mean": {1, 3, 5}, "count": {1, 1, 1}, "last": {1, 3, 5}} {
		rs, err := r.Range(ctxT(t), ref, driver.RangeQuery{Field: "a", Window: time.Hour, Aggregate: agg})
		if err != nil {
			t.Fatalf("Range %s: %v", agg, err)
		}
		rows, _, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(want) {
			t.Errorf("Range %s over sparse a: %d windows %v, want %d", agg, len(rows), rows, len(want))
			continue
		}
		for i, row := range rows {
			ts, _ := row[0].AsTime()
			if wantT := start.Add(time.Duration(2*i) * time.Hour); !ts.Equal(wantT) {
				t.Errorf("Range %s window %d at %v, want %v", agg, i, ts, wantT)
			}
			if !drivertest.EqualValue(driver.Float(want[i]), row[1]) {
				t.Errorf("Range %s window %d = %s, want %v", agg, i, row[1], want[i])
			}
		}
	}
}

// TestRangeCap: a Range stops at the connection's cap and reports it.
func TestRangeCap(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	if got := c.(*conn).rangeCap; got != maxRangePoints {
		t.Fatalf("rangeCap = %d, want %d", got, maxRangePoints)
	}
	c.(*conn).rangeCap = 50
	rs, err := c.(driver.TimeSeriesReader).Range(ctxT(t), driver.CollectionRef{Namespace: testDB, Name: "cpu"}, driver.RangeQuery{Field: "usage_user"})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil || len(rows) != 50 || !st.Truncated {
		t.Errorf("capped Range: %d rows, truncated %v, err %v; want 50, true", len(rows), st.Truncated, err)
	}
}
