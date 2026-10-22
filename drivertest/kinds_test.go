package drivertest

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func kindFake(t *testing.T, cfg driver.ConnConfig) (*Fake, driver.Conn) {
	t.Helper()
	f := NewFake(WithDocuments(), WithKeyValues(), WithVectors(), WithTimeSeries())
	c, err := f.Driver().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return f, c
}

func colNames(cols []driver.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// Each kind fixture seeds one collection, listed with its kind's type.
func TestKindFixturesSeedFake(t *testing.T) {
	_, c := kindFake(t, driver.ConnConfig{})
	colls, err := c.Collections(context.Background(), driver.NamespaceRef{Name: "main"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]driver.CollectionType{}
	for _, cl := range colls {
		got[cl.Ref.Name] = cl.Type
	}
	want := map[string]driver.CollectionType{
		DocumentsCollection:  driver.CollectionCollection,
		KeyValueCollection:   driver.CollectionKeyPattern,
		VectorCollection:     driver.CollectionCollection,
		TimeSeriesCollection: driver.CollectionMeasurement,
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("collection %s type = %q, want %q (all: %v)", name, got[name], typ, got)
		}
	}
	if n := len(TimeSeriesDataset().Rows); n != 120 {
		t.Errorf("time series has %d points, want 120 (1-minute points over 2 hours)", n)
	}
	for _, r := range VectorDataset().Rows {
		if v, ok := r[1].AsVector(); !ok || len(v) != 4 {
			t.Fatalf("vector row %v: want a 4-dim vector", r)
		}
	}
	types := map[string]bool{}
	for _, r := range KeyValueDataset().Rows {
		s, _ := r[1].AsText()
		types[s] = true
	}
	for _, typ := range []string{"string", "hash", "list", "set", "zset"} {
		if !types[typ] {
			t.Errorf("key-value fixture has no %s key", typ)
		}
	}
}

// The document fixture is schemaless: nested objects, lists, mixed types,
// nulls and missing fields, keyed by _id.
func TestDocumentsFixtureShape(t *testing.T) {
	docs := Documents()
	if len(docs) < 4 {
		t.Fatalf("%d documents", len(docs))
	}
	var nested, list, mixed, missing bool
	ageTypes := map[driver.Type]bool{}
	for _, d := range docs {
		if _, ok := d.Get("_id"); !ok {
			t.Fatalf("document without _id: %v", d)
		}
		if _, ok := d.Get("name"); !ok {
			missing = true
		}
		for _, m := range d {
			switch m.Value.Type() {
			case driver.TypeObject:
				nested = true
			case driver.TypeList:
				list = true
			}
		}
		if v, ok := d.Get("age"); ok {
			ageTypes[v.Type()] = true
		}
	}
	mixed = len(ageTypes) > 1
	if !nested || !list || !mixed || !missing {
		t.Fatalf("nested=%v list=%v mixed=%v missing=%v", nested, list, mixed, missing)
	}
	// Documents returns a copy.
	docs[0][0].Key = "changed"
	if Documents()[0][0].Key != "_id" {
		t.Fatal("Documents shares its data")
	}
}

// A schemaless collection lists, per page, only the fields its rows have,
// in the collection's first-seen order; Describe infers the fields.
func TestFakeSchemalessPages(t *testing.T) {
	_, c := kindFake(t, driver.ConnConfig{})
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: DocumentsCollection}
	br := c.(driver.Browser)
	var pages [][]string
	cursor := ""
	for {
		rs, err := br.Browse(ctx, ref, driver.BrowseOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		rows, st, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		cols := colNames(rs.Columns())
		for _, r := range rows {
			if len(r) != len(cols) {
				t.Fatalf("row width %d for columns %v", len(r), cols)
			}
		}
		pages = append(pages, cols)
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if len(pages) < 2 || slices.Equal(pages[0], pages[1]) {
		t.Fatalf("page columns %v: want them to differ between pages", pages)
	}
	if pages[0][0] != "_id" {
		t.Fatalf("page 1 columns %v: _id first", pages[0])
	}
	sch, err := c.Describe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !sch.Schemaless || sch.Type != driver.CollectionCollection || len(sch.Sample) == 0 {
		t.Fatalf("schema = %+v", sch)
	}
	i := slices.IndexFunc(sch.Fields, func(f driver.Field) bool { return f.Name == "address" })
	if i < 0 || sch.Fields[i].Type != driver.TypeObject || !slices.ContainsFunc(sch.Fields[i].Fields, func(f driver.Field) bool { return f.Name == "city" }) {
		t.Fatalf("address field = %+v", sch.Fields)
	}
	j := slices.IndexFunc(sch.Fields, func(f driver.Field) bool { return f.Name == "age" })
	if j < 0 || !strings.Contains(sch.Fields[j].NativeType, "|") {
		t.Fatalf("age field %+v: want a type union for mixed values", sch.Fields)
	}
}

func similarIDs(t *testing.T, c driver.Conn, q driver.VectorQuery) ([]int64, []float64) {
	t.Helper()
	ctx := context.Background()
	rs, err := c.(driver.VectorSearcher).Similar(ctx, driver.CollectionRef{Namespace: "main", Name: VectorCollection}, q)
	if err != nil {
		t.Fatalf("Similar(%s): %v", q.Metric, err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	di := len(rs.Columns()) - 1
	var ids []int64
	var ds []float64
	for _, r := range rows {
		id, _ := r[0].AsInt()
		d, _ := r[di].AsFloat()
		ids, ds = append(ids, id), append(ds, d)
	}
	return ids, ds
}

// The Fake ranks by exact L2, cosine and inner product, and each metric has
// a different nearest neighbour on the vector fixture.
func TestFakeSimilarMetrics(t *testing.T) {
	_, c := kindFake(t, driver.ConnConfig{})
	q := KindVectorFixture(driver.CollectionRef{}).Query.Vector
	for _, tc := range []struct {
		metric driver.VectorMetric
		top    int64
		dist   float64
	}{
		{driver.MetricL2, 1, math.Sqrt(0.1*0.1 + 0.3*0.3)},
		{driver.MetricCosine, 2, 1 - 5/math.Sqrt(25+0.01)},
		{driver.MetricInnerProduct, 3, -6},
		{driver.MetricDefault, 2, 1 - 5/math.Sqrt(25+0.01)}, // the Fake's default is cosine
	} {
		ids, ds := similarIDs(t, c, driver.VectorQuery{Vector: q, K: 3, Metric: tc.metric})
		if len(ids) != 3 || ids[0] != tc.top {
			t.Errorf("%q: ids %v, want %d first and 3 rows", tc.metric, ids, tc.top)
			continue
		}
		if math.Abs(ds[0]-tc.dist) > 1e-6 {
			t.Errorf("%q: distance %g, want %g", tc.metric, ds[0], tc.dist)
		}
	}
	_, err := c.(driver.VectorSearcher).Similar(context.Background(), driver.CollectionRef{Namespace: "main", Name: VectorCollection},
		driver.VectorQuery{Vector: q, Metric: "hamming"})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("unknown metric: %v", err)
	}
}

// A text query needs ConnConfig.Embedder: without one it is ErrUnsupported
// with the hint; with one the text is embedded and searched.
func TestFakeSimilarText(t *testing.T) {
	ref := driver.CollectionRef{Namespace: "main", Name: VectorCollection}
	_, plain := kindFake(t, driver.ConnConfig{})
	_, err := plain.(driver.VectorSearcher).Similar(context.Background(), ref, driver.VectorQuery{Text: "hello", K: 2})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		t.Fatalf("no embedder: %v", err)
	}
	_, c := kindFake(t, driver.ConnConfig{Embedder: HashEmbedder{Dims: 4}})
	rs, err := c.(driver.VectorSearcher).Similar(context.Background(), ref, driver.VectorQuery{Text: "hello", K: 2})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(context.Background(), rs, 0)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	// A wrong-sized embedding is refused, not silently matched against nothing.
	_, c8 := kindFake(t, driver.ConnConfig{Embedder: HashEmbedder{Dims: 8}})
	if _, err := c8.(driver.VectorSearcher).Similar(context.Background(), ref, driver.VectorQuery{Text: "hello"}); !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("8-dim embedding on 4-dim vectors: %v", err)
	}
}

func TestHashEmbedderDeterministic(t *testing.T) {
	e := HashEmbedder{Dims: 6}
	a, err := e.Embed(context.Background(), []string{"alpha", "beta", "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 || len(a[0]) != 6 || e.Dimensions() != 6 {
		t.Fatalf("shape %d x %d", len(a), len(a[0]))
	}
	if !slices.Equal(a[0], a[2]) || slices.Equal(a[0], a[1]) {
		t.Fatalf("not deterministic per text: %v", a)
	}
	if (HashEmbedder{}).Dimensions() == 0 {
		t.Fatal("zero Dims must default to a fixed size")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Embed(ctx, []string{"x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// The kind fixtures for the Strict checks agree with the datasets.
func TestKindFixtureExpectations(t *testing.T) {
	v := KindVectorFixture(driver.CollectionRef{Namespace: "n", Name: "v"})
	if v.Collection.Name != "v" || len(v.Query.Vector) != 4 || !EqualValue(driver.Int(1), v.WantTopID) || v.Query.Metric != driver.MetricL2 {
		t.Fatalf("vector fixture = %+v", v)
	}
	if !EqualValue(driver.Int(2), v.WantTopByMetric[driver.MetricCosine]) || !EqualValue(driver.Int(3), v.WantTopByMetric[driver.MetricInnerProduct]) {
		t.Fatalf("per-metric tops = %v", v.WantTopByMetric)
	}
	ts := KindTimeSeriesFixture(driver.CollectionRef{Name: "cpu"})
	if ts.Query.Window != 30*time.Minute || ts.Query.Field != "usage_user" {
		t.Fatalf("time-series query = %+v", ts.Query)
	}
	for _, agg := range []string{"mean", "min", "max", "count"} {
		pts := ts.WantPoints[agg]
		if len(pts) != 4 {
			t.Fatalf("%s: %d windows, want 4", agg, len(pts))
		}
		for i, p := range pts {
			if want := TimeSeriesStart().Add(time.Duration(i) * 30 * time.Minute); !p.Time.Equal(want) {
				t.Errorf("%s window %d at %v, want %v", agg, i, p.Time, want)
			}
		}
	}
	for i := range 4 {
		lo, mean, hi := ts.WantPoints["min"][i].Value, ts.WantPoints["mean"][i].Value, ts.WantPoints["max"][i].Value
		if ts.WantPoints["count"][i].Value != 30 || lo > mean || mean > hi || lo == hi {
			t.Errorf("window %d: count %g min %g mean %g max %g", i, ts.WantPoints["count"][i].Value, lo, mean, hi)
		}
	}
}

// ---- Strict checks catch wrong answers ----

// brokenDriver wraps the Fake's conns with one wrong capability.
type brokenDriver struct {
	f    *Fake
	wrap func(driver.Conn) driver.Conn
}

func (d brokenDriver) Info() driver.Info { return d.f.Driver().Info() }

func (d brokenDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.f.Driver().Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return d.wrap(c), nil
}

// ignoresMetric always ranks by cosine.
type ignoresMetric struct{ driver.Conn }

func (c ignoresMetric) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	q.Metric = driver.MetricCosine
	return c.Conn.(driver.VectorSearcher).Similar(ctx, ref, q)
}

// ignoresK returns every row.
type ignoresK struct{ driver.Conn }

func (c ignoresK) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	q.K = 1000
	return c.Conn.(driver.VectorSearcher).Similar(ctx, ref, q)
}

// alwaysMean ignores the requested aggregate.
type alwaysMean struct{ driver.Conn }

func (c alwaysMean) Range(ctx context.Context, ref driver.CollectionRef, q driver.RangeQuery) (driver.ResultSet, error) {
	q.Aggregate = "mean"
	return c.Conn.(driver.TimeSeriesReader).Range(ctx, ref, q)
}

// shiftedWindows drops the first window.
type shiftedWindows struct{ driver.Conn }

func (c shiftedWindows) Range(ctx context.Context, ref driver.CollectionRef, q driver.RangeQuery) (driver.ResultSet, error) {
	q.Start = q.Start.Add(q.Window)
	return c.Conn.(driver.TimeSeriesReader).Range(ctx, ref, q)
}

// textColumn rewrites column i of every row as its String text.
func textColumn(ctx context.Context, rs driver.ResultSet, err error, i int) (driver.ResultSet, error) {
	if err != nil {
		return nil, err
	}
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	cols := rs.Columns()
	cols[i].Type = driver.TypeString
	for _, r := range rows {
		r[i] = driver.String(r[i].String())
	}
	return driver.NewSliceResultSet(cols, rows, st), nil
}

// stringIDs returns the right rows with ids as text.
type stringIDs struct{ driver.Conn }

func (c stringIDs) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	rs, err := c.Conn.(driver.VectorSearcher).Similar(ctx, ref, q)
	return textColumn(ctx, rs, err, 0)
}

// stringTimes returns the right windows with _time as text.
type stringTimes struct{ driver.Conn }

func (c stringTimes) Range(ctx context.Context, ref driver.CollectionRef, q driver.RangeQuery) (driver.ResultSet, error) {
	rs, err := c.Conn.(driver.TimeSeriesReader).Range(ctx, ref, q)
	return textColumn(ctx, rs, err, 0)
}

func kindConfig(d driver.Driver) Config {
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
			Vector:     KindVectorFixture(driver.CollectionRef{Namespace: "main", Name: VectorCollection}),
			TimeSeries: KindTimeSeriesFixture(driver.CollectionRef{Namespace: "main", Name: TimeSeriesCollection}),
		},
	}
}

func TestHelperBrokenKinds(t *testing.T) {
	f := NewFake(WithVectors(), WithTimeSeries())
	wraps := map[string]func(driver.Conn) driver.Conn{
		"ignores-metric":  func(c driver.Conn) driver.Conn { return ignoresMetric{c} },
		"ignores-k":       func(c driver.Conn) driver.Conn { return ignoresK{c} },
		"always-mean":     func(c driver.Conn) driver.Conn { return alwaysMean{c} },
		"shifted-windows": func(c driver.Conn) driver.Conn { return shiftedWindows{c} },
		"string-ids":      func(c driver.Conn) driver.Conn { return stringIDs{c} },
		"string-times":    func(c driver.Conn) driver.Conn { return stringTimes{c} },
	}
	w, ok := wraps[os.Getenv("TOBIRA_DRIVERTEST_HELPER")]
	if !ok {
		t.Skip("helper process only")
	}
	c := kindConfig(brokenDriver{f: f, wrap: w})
	if _, ok := w(nil).(driver.VectorSearcher); ok {
		testVectorSearcher(t, c)
	} else {
		testTimeSeriesReader(t, c)
	}
}

// The Strict checks fail a driver that ignores the metric or k, or that
// gets the window aggregates wrong.
func TestStrictKindChecksCatchWrongAnswers(t *testing.T) {
	for variant, want := range map[string]string{
		"ignores-metric":  "metric",
		"ignores-k":       "K = 1",
		"always-mean":     "aggregate",
		"shifted-windows": "windows",
		"string-ids":      "type",
		"string-times":    "not a time",
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperBrokenKinds$", "-test.count=1", "-test.v")
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

// The honest Fake passes the kind checks, and a zero fixture value skips
// the new assertions (third-party drivers keep passing).
func TestStrictKindChecksHonest(t *testing.T) {
	f := NewFake(WithVectors(), WithTimeSeries())
	c := kindConfig(f.Driver())
	t.Run("vector", func(t *testing.T) { testVectorSearcher(t, c) })
	t.Run("timeseries", func(t *testing.T) { testTimeSeriesReader(t, c) })
	legacy := c
	legacy.Fixture.Vector = &VectorFixture{Collection: c.Fixture.Vector.Collection, Query: driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, K: 2}}
	legacy.Fixture.TimeSeries = &TimeSeriesFixture{Collection: c.Fixture.TimeSeries.Collection, Query: driver.RangeQuery{Field: "usage_user"}}
	t.Run("legacy-vector", func(t *testing.T) { testVectorSearcher(t, legacy) })
	t.Run("legacy-timeseries", func(t *testing.T) { testTimeSeriesReader(t, legacy) })
}

// Windows align to the Unix epoch (as date_bin's default origin does), so a
// window that does not divide a day starts on epoch multiples, not on
// multiples counted from year 1.
func TestFakeRangeWindowsAlignToEpoch(t *testing.T) {
	_, c := kindFake(t, driver.ConnConfig{})
	const w = 7 * time.Minute
	ctx := context.Background()
	rs, err := c.(driver.TimeSeriesReader).Range(ctx, driver.CollectionRef{Namespace: "main", Name: TimeSeriesCollection},
		driver.RangeQuery{Field: "usage_user", Window: w, Aggregate: "count"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(ctx, rs, 0)
	for _, r := range rows {
		ts, _ := r[0].AsTime()
		if ts.UnixNano()%int64(w) != 0 {
			t.Fatalf("window starts at %v, not a multiple of %v since the epoch", ts, w)
		}
	}
}
