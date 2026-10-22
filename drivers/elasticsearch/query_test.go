package elasticsearch

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func collect(t *testing.T, rs driver.ResultSet, err error) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return cols, rows, st
}

// collectQuery runs q on c and drains it.
func collectQuery(t *testing.T, c *conn, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(context.Background(), q)
	return collect(t, rs, err)
}

func colNames(cols []driver.Column) []string {
	var out []string
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out
}

// Without a size or a limit every hit streams, page by page, through one
// point in time that is closed at the end.
func TestQueryStreamsAllPages(t *testing.T) {
	f := &fakeES{n: 25, product: true}
	c := newFakeConn(t, f, flavorElastic, false)
	c.pageSize = 10
	cols, rows, st := collectQuery(t, c, driver.Query{Text: `{"index":"docs","sort":[{"id":"asc"}]}`})
	if len(rows) != 25 || st.Truncated {
		t.Fatalf("got %d rows (truncated %v), want 25", len(rows), st.Truncated)
	}
	if got := colNames(cols); !slices.Equal(got, []string{"_id", "_score", "id", "name"}) {
		t.Errorf("columns = %v", got)
	}
	if cols[0].Source == nil || cols[0].Source.Name != "docs" || !cols[0].Key || cols[0].Type != driver.TypeString {
		t.Errorf("_id column = %+v", cols[0])
	}
	if cols[2].Type != driver.TypeInt {
		t.Errorf("id column type = %s", cols[2].Type)
	}
	for i, r := range rows {
		if n, _ := r[2].AsInt(); n != int64(i+1) {
			t.Fatalf("row %d id = %s", i, r[2])
		}
		if !r[1].IsNull() {
			t.Errorf("row %d _score = %s, want NULL for a field sort", i, r[1])
		}
	}
	if opened, closed, _ := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("points in time opened %d, closed %d; want 1 and 1", opened, closed)
	}
}

// A Query.Limit within one page needs no point in time: one request of
// limit+1 hits decides truncation.
func TestQueryLimitSinglePage(t *testing.T) {
	f := &fakeES{n: 25, product: true}
	c := newFakeConn(t, f, flavorElastic, false)
	c.pageSize = 10
	_, rows, st := collectQuery(t, c, driver.Query{Text: `{"index":"docs"}`, Limit: 5})
	if len(rows) != 5 || !st.Truncated {
		t.Fatalf("got %d rows (truncated %v), want 5 truncated", len(rows), st.Truncated)
	}
	_, rows, st = collectQuery(t, c, driver.Query{Text: `{"index":"docs","size":3}`, Limit: 5})
	if len(rows) != 3 || st.Truncated {
		t.Fatalf("size 3: got %d rows (truncated %v), want 3 not truncated", len(rows), st.Truncated)
	}
	if opened, _, _ := f.counts(); opened != 0 {
		t.Errorf("opened %d points in time for single-page queries", opened)
	}
}

// A limit beyond one page pages through a point in time and stops at
// limit+1 hits.
func TestQueryLimitAcrossPages(t *testing.T) {
	f := &fakeES{n: 25, product: true}
	c := newFakeConn(t, f, flavorElastic, false)
	c.pageSize = 10
	_, rows, st := collectQuery(t, c, driver.Query{Text: `{"index":"docs"}`, Limit: 15})
	if len(rows) != 15 || !st.Truncated {
		t.Fatalf("got %d rows (truncated %v), want 15 truncated", len(rows), st.Truncated)
	}
	if opened, closed, _ := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("points in time opened %d, closed %d", opened, closed)
	}
}

// Closing a result early releases its point in time.
func TestQueryCloseReleasesPIT(t *testing.T) {
	f := &fakeES{n: 25, product: true}
	c := newFakeConn(t, f, flavorElastic, false)
	c.pageSize = 10
	rs, err := c.Query(context.Background(), driver.Query{Text: `{"index":"docs"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(context.Background()); !ok || err != nil {
		t.Fatalf("Next: %v %v", ok, err)
	}
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rs.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if opened, closed, _ := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("points in time opened %d, closed %d", opened, closed)
	}
}

// A refused read-only request sends nothing.
func TestQueryReadOnlySendsNothing(t *testing.T) {
	f := &fakeES{n: 3, product: true}
	c := newFakeConn(t, f, flavorElastic, true)
	_, _, before := f.counts()
	_, err := c.Query(context.Background(), driver.Query{Text: `{"method":"POST","path":"/docs/_delete_by_query","body":{"query":{"match_all":{}}}}`})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
	if _, _, after := f.counts(); len(after) != len(before) {
		t.Errorf("a refused write sent %v", after[len(before):])
	}
	if _, err := c.Query(context.Background(), driver.Query{Text: `{"index":"docs"}`, Params: []driver.Value{driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("params: want ErrUnsupported, got %v", err)
	}
}

// The raw form returns a response object as one row of its keys, and the
// rows of a _cat array.
func TestRawQueryShapes(t *testing.T) {
	f := &fakeES{n: 3, product: true}
	c := newFakeConn(t, f, flavorElastic, false)
	cols, rows, _ := collectQuery(t, c, driver.Query{Text: `{"method":"GET","path":"/"}`})
	if len(rows) != 1 || !slices.Equal(colNames(cols), []string{"version"}) {
		t.Fatalf("GET /: columns %v rows %v", colNames(cols), rows)
	}
	cols, rows, _ = collectQuery(t, c, driver.Query{Text: `{"method":"POST","path":"/docs/_search","body":{"size":2}}`})
	if len(rows) != 2 || !slices.Contains(colNames(cols), "_id") || !slices.Contains(colNames(cols), "name") {
		t.Fatalf("raw _search: columns %v rows %d", colNames(cols), len(rows))
	}
}

// OpenSearch sends no X-Elastic-Product header: the opensearch driver
// accepts it, the elasticsearch driver refuses with a hint.
func TestProductCheck(t *testing.T) {
	f := &fakeES{n: 3}
	c := newFakeConn(t, f, flavorOpenSearch, false)
	_, rows, _ := collectQuery(t, c, driver.Query{Text: `{"index":"docs"}`, Limit: 5})
	if len(rows) != 3 {
		t.Errorf("opensearch: got %d rows", len(rows))
	}
	f2 := &fakeES{n: 3}
	f2.t = t
	srv := newServer(t, f2)
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "elasticsearch://" + srv + "?tls=false"})
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("elasticsearch against a server without the product header: want ErrConnection, got %v", err)
	}
}

// Closing the connection releases the points in time its results
// still hold.
func TestCloseReleasesOpenPITs(t *testing.T) {
	f := &fakeES{n: 25, product: true}
	c := newFakeConn(t, f, flavorElastic, false)
	c.pageSize = 10
	rs, err := c.Query(context.Background(), driver.Query{Text: `{"index":"docs"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(context.Background()); !ok || err != nil {
		t.Fatalf("Next: %v %v", ok, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, closed, _ := f.counts(); opened != 1 || closed != 1 {
		t.Errorf("points in time opened %d, closed %d; want 1 and 1", opened, closed)
	}
	_ = rs.Close() // nothing left to release, and no request on a closed conn
	if _, closed, _ := f.counts(); closed != 1 {
		t.Errorf("the result's Close released the point in time again: %d closes", closed)
	}
}
