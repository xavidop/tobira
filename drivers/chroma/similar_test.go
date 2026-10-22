package chroma

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestConvertDistance(t *testing.T) {
	for _, tt := range []struct {
		space string
		in    float64
		want  float64
	}{
		{"l2", 0.1, math.Sqrt(0.1)},
		{"l2", 16, 4},
		{"l2", -1e-9, 0}, // float noise below zero
		{"cosine", 0.25, 0.25},
		{"ip", -5, -6}, // Chroma's 1 - dot; the contract is -dot
		{"ip", 1, 0},
	} {
		if got := convertDistance(tt.space, tt.in); math.Abs(got-tt.want) > 1e-12 {
			t.Errorf("convertDistance(%s, %g) = %g, want %g", tt.space, tt.in, got, tt.want)
		}
	}
}

func TestMetricOfSpace(t *testing.T) {
	for space, want := range map[string]driver.VectorMetric{"l2": driver.MetricL2, "cosine": driver.MetricCosine, "ip": driver.MetricInnerProduct, "": driver.MetricL2} {
		if got, ok := metricOf(space); !ok || got != want {
			t.Errorf("metricOf(%q) = %q %v, want %q", space, got, ok, want)
		}
	}
	if _, ok := metricOf("hamming"); ok {
		t.Error("unknown space accepted")
	}
}

// queryBodies returns the bodies of the /query requests the fake saw.
func queryBodies(f *fakeChroma) []map[string]any {
	var out []map[string]any
	for _, r := range f.requests() {
		if strings.HasSuffix(r.path, "/query") {
			out = append(out, r.body)
		}
	}
	return out
}

func TestSimilarAgainstFake(t *testing.T) {
	f := newFake(t)
	coll := f.addColl("default_database", "vecs", "l2", 2)
	coll.add("1", []float64{0.9, 0.3}, "near", map[string]any{"n": 10})
	coll.add("2", []float64{5, 0.1}, "", map[string]any{"n": 20})
	coll.add("3", []float64{6, 6}, "", nil)
	emb := &countingEmbedder{dims: 2}
	c := openFake(t, f, driver.ConnConfig{Embedder: emb, ReadOnly: true})
	ref := driver.CollectionRef{Name: "vecs"}
	ctx := t.Context()

	rs, err := c.Similar(ctx, ref, driver.VectorQuery{Vector: []float32{1, 0}, K: 2})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	di, ii := colIdx(cols, "_distance"), colIdx(cols, "id")
	if len(rows) != 2 || di < 0 || ii < 0 {
		t.Fatalf("Similar: %d rows, columns %v", len(rows), cols)
	}
	if d, _ := rows[0][di].AsFloat(); math.Abs(d-math.Sqrt(0.1)) > 1e-6 {
		t.Errorf("_distance = %g, want the true L2 %g", d, math.Sqrt(0.1))
	}
	if id, _ := rows[0][ii].AsInt(); id != 1 {
		t.Errorf("top id = %s", rows[0][ii])
	}

	// Text goes through the host embedder; the request carries the
	// embedding, never text for the server to embed.
	if _, err := c.Similar(ctx, ref, driver.VectorQuery{Text: "hello", K: 1}); err != nil {
		t.Fatal(err)
	}
	if emb.calls != 1 || emb.texts[0] != "hello" {
		t.Errorf("embedder calls = %d %q", emb.calls, emb.texts)
	}
	for _, b := range queryBodies(f) {
		if _, ok := b["query_texts"]; ok {
			t.Errorf("a query sent query_texts: %v", b)
		}
		if _, ok := b["query_embeddings"]; !ok {
			t.Errorf("a query has no query_embeddings: %v", b)
		}
	}

	sent := len(queryBodies(f))
	for name, tc := range map[string]struct {
		ref  driver.CollectionRef
		q    driver.VectorQuery
		want error
	}{
		"metric mismatch":   {ref, driver.VectorQuery{Vector: []float32{1, 0}, Metric: driver.MetricCosine}, driver.ErrUnsupported},
		"unknown metric":    {ref, driver.VectorQuery{Vector: []float32{1, 0}, Metric: "hamming"}, driver.ErrUnsupported},
		"wrong dimension":   {ref, driver.VectorQuery{Vector: []float32{1, 0, 0}}, driver.ErrSyntax},
		"non-finite":        {ref, driver.VectorQuery{Vector: []float32{float32(math.Inf(1)), 0}}, driver.ErrSyntax},
		"other field":       {ref, driver.VectorQuery{Vector: []float32{1, 0}, Field: "other"}, driver.ErrNotFound},
		"nothing to search": {ref, driver.VectorQuery{}, driver.ErrSyntax},
		"missing coll text": {driver.CollectionRef{Name: "nope"}, driver.VectorQuery{Text: "x"}, driver.ErrNotFound},
		"bad filter text":   {ref, driver.VectorQuery{Text: "x", Filter: driver.Filter{Raw: "[]"}}, driver.ErrSyntax},
		"metric text":       {ref, driver.VectorQuery{Text: "x", Metric: driver.MetricInnerProduct}, driver.ErrUnsupported},
	} {
		if _, err := c.Similar(ctx, tc.ref, tc.q); !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, err)
		}
	}
	if emb.calls != 1 {
		t.Errorf("refused text searches called the embedder: %d calls", emb.calls)
	}
	if n := len(queryBodies(f)); n != sent {
		t.Errorf("refused searches reached the server: %d", n-sent)
	}

	// Embedder output of the wrong size is refused.
	c2 := openFake(t, f, driver.ConnConfig{Embedder: &countingEmbedder{dims: 3}})
	if _, err := c2.Similar(ctx, ref, driver.VectorQuery{Text: "x"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("embedder dimension mismatch: want ErrSyntax, got %v", err)
	}
	// No embedder: the hint.
	c3 := openFake(t, f, driver.ConnConfig{})
	_, err = c3.Similar(ctx, ref, driver.VectorQuery{Text: "x"})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		t.Errorf("no embedder: want ErrUnsupported with the hint, got %v", err)
	}
	// An embedder error is passed on.
	c4 := openFake(t, f, driver.ConnConfig{Embedder: &countingEmbedder{dims: 2, err: errors.New("quota")}})
	if _, err := c4.Similar(ctx, ref, driver.VectorQuery{Text: "x"}); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("embedder error: got %v", err)
	}
}

func TestQueryTextsUseHostEmbedder(t *testing.T) {
	f := newFake(t)
	coll := f.addColl("default_database", "vecs", "l2", 2)
	coll.add("1", []float64{1, 0}, "", nil)
	coll.add("2", []float64{0, 3}, "", nil)
	emb := &countingEmbedder{dims: 2}
	c := openFake(t, f, driver.ConnConfig{Embedder: emb, ReadOnly: true})
	ctx := t.Context()
	rs, err := c.Query(ctx, driver.Query{Text: `{"collection":"vecs","queryTexts":["a","b"],"nResults":2}`, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if emb.calls != 1 || len(emb.texts) != 2 {
		t.Errorf("embedder: %d calls for %q", emb.calls, emb.texts)
	}
	if len(rows) != 4 || colIdx(cols, "_query") != 0 {
		t.Fatalf("two queries of 2: %d rows, columns %v", len(rows), cols)
	}
	// Query shows Chroma's own distance (squared L2 here).
	if d, _ := rows[1][colIdx(cols, "_distance")].AsFloat(); math.Abs(d-10) > 1e-6 {
		t.Errorf("raw distance = %g, want 10", d)
	}
	for _, b := range queryBodies(f) {
		if _, ok := b["query_texts"]; ok {
			t.Errorf("Query sent query_texts to the server: %v", b)
		}
	}
	c2 := openFake(t, f, driver.ConnConfig{})
	if _, err := c2.Query(ctx, driver.Query{Text: `{"collection":"vecs","queryTexts":["a"]}`}); !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		t.Errorf("queryTexts without an embedder: %v", err)
	}
	if _, err := c2.Query(ctx, driver.Query{Text: `{"collection":"vecs","queryEmbeddings":[[1,0,0]]}`}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("wrong query dimension: want ErrSyntax, got %v", err)
	}
}

func TestQueryGetAgainstFake(t *testing.T) {
	f := newFake(t)
	coll := f.addColl("default_database", "big", "l2", 1)
	for i := range 700 {
		coll.add("r"+itoa3(i), []float64{float64(i)}, "", map[string]any{"i": i})
	}
	c := openFake(t, f, driver.ConnConfig{})
	ctx := t.Context()
	count := func(q driver.Query) (int, driver.Stats) {
		t.Helper()
		rs, err := c.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		rows, st, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows), st
	}
	if n, st := count(driver.Query{Text: `{"collection":"big"}`}); n != 700 || st.Truncated {
		t.Errorf("whole collection: %d rows, truncated %v", n, st.Truncated)
	}
	gets := 0
	for _, r := range f.requests() {
		if strings.HasSuffix(r.path, "/get") {
			gets++
		}
	}
	if gets < 3 {
		t.Errorf("700 records came in %d get requests, want pages", gets)
	}
	if n, st := count(driver.Query{Text: `{"collection":"big"}`, Limit: 10}); n != 10 || !st.Truncated {
		t.Errorf("Query.Limit 10: %d rows, truncated %v", n, st.Truncated)
	}
	if n, _ := count(driver.Query{Text: `{"collection":"big","limit":5,"offset":698}`}); n != 2 {
		t.Errorf("limit 5 offset 698: %d rows, want 2", n)
	}
	if n, _ := count(driver.Query{Text: `{"collection":"big","limit":450}`}); n != 450 {
		t.Errorf("limit 450: %d rows", n)
	}
	rs, err := c.Query(ctx, driver.Query{Text: `{"op":"count","collection":"big"}`})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(ctx, rs, 0)
	if len(rows) != 1 || rows[0][0].String() != "700" {
		t.Errorf("count = %v", rows)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"collection":"nope"}`}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing collection: want ErrNotFound, got %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"collection":"big"}`, Namespace: "other_db"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing database: want ErrNotFound, got %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"collection":"big"}`, Params: []driver.Value{driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("params: want ErrUnsupported, got %v", err)
	}
	for _, name := range []string{"..", "a/b", "x"} {
		if _, err := c.Query(ctx, driver.Query{Text: `{"collection":"` + name + `"}`}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("collection %q: want ErrNotFound, got %v", name, err)
		}
	}
}

func itoa3(i int) string {
	return string([]byte{byte('0' + i/100%10), byte('0' + i/10%10), byte('0' + i%10)})
}

// An empty id selection answers before the embedder is called.
func TestSimilarEmptySelectionSkipsEmbedder(t *testing.T) {
	f := newFake(t)
	coll := f.addColl("default_database", "vecs", "l2", 2)
	coll.add("1", []float64{1, 0}, "", nil)
	emb := &countingEmbedder{dims: 2}
	c := openFake(t, f, driver.ConnConfig{Embedder: emb})
	rs, err := c.Similar(t.Context(), driver.CollectionRef{Name: "vecs"}, driver.VectorQuery{Text: "x", Filter: driver.Filter{Conditions: []driver.Condition{
		cond("id", driver.OpEq, driver.Int(1)), cond("id", driver.OpEq, driver.Int(2)),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(t.Context(), rs, 0)
	if len(rows) != 0 || emb.calls != 0 || len(queryBodies(f)) != 0 {
		t.Errorf("empty selection: %d rows, %d embedder calls, %d queries", len(rows), emb.calls, len(queryBodies(f)))
	}
}

func TestQueryRecordsBadDistance(t *testing.T) {
	bad := json.Number("1e999")
	_, err := queryRecords(queryResponse{IDs: [][]string{{"1"}}, Distances: [][]*json.Number{{&bad}}}, func(d float64) float64 { return d })
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("unparsable distance: want an unexpected-reply error, got %v", err)
	}
}
