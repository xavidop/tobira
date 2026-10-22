package drivertest

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func testExporter(t *testing.T, c Config) {
	e := needCap[driver.Exporter](t, c, driver.CapExporter)
	formats := e.ExportFormats()
	if len(formats) == 0 {
		t.Fatal("ExportFormats is empty: list at least one native format")
	}
	var buf bytes.Buffer
	if err := e.Export(ctxTimeout(t), c.Fixture.SelectAll, formats[0], &buf); err != nil {
		t.Fatalf("Export(SelectAll, %q): %v", formats[0], err)
	}
	if buf.Len() == 0 {
		t.Errorf("Export(SelectAll, %q) wrote no bytes", formats[0])
	}
}

func testWatcher(t *testing.T, c Config) {
	w := needCap[driver.Watcher](t, c, driver.CapWatcher)
	if c.Fixture.Watch == nil {
		missingFixture(t, c, "Watch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := w.Watch(ctx, *c.Fixture.Watch)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if ch == nil {
		t.Fatal("Watch returned a nil channel without an error")
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Watch channel still open 2s after its context was cancelled: close it when ctx ends")
		}
	}
}

func testVectorSearcher(t *testing.T, c Config) {
	f := c.Fixture.Vector
	v := needCap[driver.VectorSearcher](t, c, driver.CapVectorSearcher)
	if f == nil {
		missingFixture(t, c, "Vector")
	}
	ctx := ctxTimeout(t)
	cols, rows := rankSimilar(ctx, t, v, f.Collection, f.Query)
	if len(rows) < atLeastOne(f.MinRows) {
		t.Errorf("Similar: got %d rows, want at least %d", len(rows), atLeastOne(f.MinRows))
	}
	if f.Query.K > 0 && len(rows) > f.Query.K {
		t.Errorf("Similar: got %d rows for K = %d", len(rows), f.Query.K)
	}
	if f.WantTopID.IsNull() && len(f.WantTopByMetric) == 0 {
		return
	}
	idField := f.IDField
	if idField == "" {
		idField = "id"
	}
	if !f.WantTopID.IsNull() {
		checkTop(t, "Similar", rows, columnIndex(t, cols, idField), f.WantTopID)
		q := f.Query
		q.K = 1
		cols1, rows1 := rankSimilar(ctx, t, v, f.Collection, q)
		if len(rows1) != 1 {
			t.Errorf("Similar with K = 1 returned %d rows, want exactly 1: honour k", len(rows1))
		} else {
			checkTop(t, "Similar with K = 1", rows1, columnIndex(t, cols1, idField), f.WantTopID)
		}
	}
	metrics := make([]driver.VectorMetric, 0, len(f.WantTopByMetric))
	for m := range f.WantTopByMetric {
		metrics = append(metrics, m)
	}
	slices.Sort(metrics)
	for _, m := range metrics {
		q := f.Query
		q.Metric = m
		mc, mr := rankSimilar(ctx, t, v, f.Collection, q)
		checkTop(t, fmt.Sprintf("Similar with metric %q", m), mr, columnIndex(t, mc, idField), f.WantTopByMetric[m])
	}
}

// rankSimilar runs Similar and checks the result has a numeric _distance
// column ranked nearest first.
func rankSimilar(ctx context.Context, t *testing.T, v driver.VectorSearcher, ref driver.CollectionRef, q driver.VectorQuery) ([]driver.Column, []driver.Row) {
	t.Helper()
	rs, err := v.Similar(ctx, ref, q)
	if err != nil {
		t.Fatalf("Similar (metric %q, K = %d): %v", q.Metric, q.K, err)
	}
	cols := rs.Columns()
	di := columnIndex(t, cols, "_distance")
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatalf("Similar: reading rows: %v", err)
	}
	prev := math.Inf(-1)
	for i, r := range rows {
		d, ok := r[di].AsFloat()
		if !ok {
			t.Fatalf("Similar: row %d _distance is %s, want a number", i, r[di])
		}
		if d < prev {
			t.Errorf("Similar (metric %q): row %d _distance %g is below the previous %g: rank nearest first", q.Metric, i, d, prev)
		}
		prev = d
	}
	return cols, rows
}

// checkTop reports when the first row's id is not want.
func checkTop(t *testing.T, what string, rows []driver.Row, idIdx int, want driver.Value) {
	t.Helper()
	if len(rows) == 0 {
		t.Errorf("%s returned no rows, want %s first", what, want)
		return
	}
	got := rows[0][idIdx]
	if got.Type() != want.Type() {
		t.Errorf("%s: nearest neighbour id %s has type %s, want %s %s: return ids with the type the fixture names", what, got, got.Type(), want.Type(), want)
		return
	}
	if !EqualValue(want, got) {
		t.Errorf("%s: nearest neighbour is %s, want %s: rank by the exact distance under the requested metric", what, got, want)
	}
}

func testTimeSeriesReader(t *testing.T, c Config) {
	f := c.Fixture.TimeSeries
	r := needCap[driver.TimeSeriesReader](t, c, driver.CapTimeSeriesReader)
	if f == nil {
		missingFixture(t, c, "TimeSeries")
	}
	ctx := ctxTimeout(t)
	rows := readRange(ctx, t, r, f.Collection, f.Query)
	if len(rows) < atLeastOne(f.MinRows) {
		t.Errorf("Range: got %d rows, want at least %d", len(rows), atLeastOne(f.MinRows))
	}
	aggs := make([]string, 0, len(f.WantPoints))
	for a := range f.WantPoints {
		aggs = append(aggs, a)
	}
	slices.Sort(aggs)
	for _, agg := range aggs {
		q := f.Query
		q.Aggregate = agg
		got, want := readRange(ctx, t, r, f.Collection, q), f.WantPoints[agg]
		if len(got) != len(want) {
			t.Errorf("Range %s: got %d windows, want %d", agg, len(got), len(want))
		}
		for i := range min(len(got), len(want)) {
			ts, ok := got[i][0].AsTime()
			if !ok {
				t.Errorf("Range %s: window %d _time is %s %s, not a time", agg, i, got[i][0].Type(), got[i][0])
			} else if !ts.Equal(want[i].Time) {
				t.Errorf("Range %s: window %d starts at %v, want %v", agg, i, ts, want[i].Time)
			}
			val := got[i][len(got[i])-1]
			x, ok := numeric(val)
			if !ok {
				t.Errorf("Range %s: window %d value %s is not a number", agg, i, val)
				continue
			}
			g, _ := x.Float64()
			if math.Abs(g-want[i].Value) > 1e-9*max(1, math.Abs(want[i].Value)) {
				t.Errorf("Range %s: window %d aggregate is %g, want %g", agg, i, g, want[i].Value)
			}
		}
	}
}

// readRange runs Range and checks its _time … value column layout.
func readRange(ctx context.Context, t *testing.T, r driver.TimeSeriesReader, ref driver.CollectionRef, q driver.RangeQuery) []driver.Row {
	t.Helper()
	rs, err := r.Range(ctx, ref, q)
	if err != nil {
		t.Fatalf("Range (aggregate %q): %v", q.Aggregate, err)
	}
	cols := rs.Columns()
	if len(cols) < 2 || cols[0].Name != "_time" || cols[len(cols)-1].Name != "value" {
		t.Errorf("Range: columns %v, want _time first and value last", cols)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatalf("Range: reading rows: %v", err)
	}
	return rows
}

func testGraphReader(t *testing.T, c Config) {
	f := c.Fixture.Graph
	g := needCap[driver.GraphReader](t, c, driver.CapGraphReader)
	if f == nil {
		missingFixture(t, c, "Graph")
	}
	graph, err := g.Neighbors(ctxTimeout(t), f.Query)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if graph == nil {
		t.Fatal("Neighbors returned a nil Graph without an error")
	}
	if len(graph.Nodes) < atLeastOne(f.MinNodes) {
		t.Errorf("Neighbors: got %d nodes, want at least %d", len(graph.Nodes), atLeastOne(f.MinNodes))
	}
	ids := map[string]bool{}
	for _, n := range graph.Nodes {
		ids[n.ID] = true
	}
	for _, e := range graph.Edges {
		if !ids[e.From] || !ids[e.To] {
			t.Errorf("Neighbors: edge %s -> %s refers to a node that is not in the result", e.From, e.To)
		}
	}
}
