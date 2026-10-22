package pinecone

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestDistanceFromScore(t *testing.T) {
	for _, tc := range []struct {
		metric string
		score  float32
		want   float64
	}{
		{metricEuclidean, 2.5, 2.5},
		{metricCosine, 0.75, 0.25},
		{metricDot, 3, -3},
	} {
		if got := distanceFromScore(tc.metric, tc.score); got != tc.want {
			t.Errorf("%s(%v) = %v, want %v", tc.metric, tc.score, got, tc.want)
		}
	}
}

func TestCheckMetric(t *testing.T) {
	for _, tc := range []struct {
		index string
		req   driver.VectorMetric
		ok    bool
	}{
		{metricEuclidean, driver.MetricDefault, true},
		{metricEuclidean, driver.MetricL2, true},
		{metricCosine, driver.MetricCosine, true},
		{metricDot, driver.MetricInnerProduct, true},
		{metricEuclidean, driver.MetricCosine, false},
		{metricCosine, driver.MetricInnerProduct, false},
		{"", driver.MetricDefault, false},
		{metricCosine, "hamming", false},
	} {
		err := checkMetric("idx", tc.index, tc.req)
		if (err == nil) != tc.ok {
			t.Errorf("checkMetric(%q, %q) = %v, want ok %v", tc.index, tc.req, err, tc.ok)
		}
		if err != nil && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("checkMetric(%q, %q) kind = %v", tc.index, tc.req, err)
		}
	}
}

type failingEmbedder struct{ err error }

func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, f.err }
func (failingEmbedder) Dimensions() int                                        { return 4 }

func TestQueryVector(t *testing.T) {
	ctx := context.Background()
	c := newTestConn(t, false)
	if v, err := c.queryVector(ctx, driver.VectorQuery{Vector: []float32{1, 2}}, 2); err != nil || len(v) != 2 {
		t.Errorf("vector: %v %v", v, err)
	}
	if _, err := c.queryVector(ctx, driver.VectorQuery{Vector: []float32{1}}, 2); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("wrong size: %v", err)
	}
	if _, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"}, 4); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("no embedder: %v", err)
	}
	if _, err := c.queryVector(ctx, driver.VectorQuery{}, 4); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("nothing: %v", err)
	}
	e := newTestConnCfg(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 4}})
	v, err := e.queryVector(ctx, driver.VectorQuery{Text: "hello"}, 4)
	if err != nil || len(v) != 4 {
		t.Errorf("embedded: %v %v", v, err)
	}
	if _, err := e.queryVector(ctx, driver.VectorQuery{Text: "hello"}, 8); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("embedding size: %v", err)
	}
	f := newTestConnCfg(t, driver.ConnConfig{Embedder: failingEmbedder{err: driver.Wrap(driver.ErrAuth, "ai", "embed", errors.New("no key"))}})
	if _, err := f.queryVector(ctx, driver.VectorQuery{Text: "x"}, 4); !errors.Is(err, driver.ErrAuth) {
		t.Errorf("embedder error kind: %v", err)
	}
}

// Similar refuses what it can before contacting the server.
func TestSimilarRefusesBeforeNetwork(t *testing.T) {
	c := newTestConn(t, false)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "default", Name: "vectors"}
	for name, q := range map[string]driver.VectorQuery{
		"nothing":   {},
		"field":     {Vector: []float32{1}, Field: "other"},
		"metric":    {Vector: []float32{1}, Metric: driver.MetricCosine},
		"id filter": {Vector: []float32{1}, Filter: driver.Filter{Conditions: []driver.Condition{{Field: idColumn, Op: driver.OpEq, Value: driver.String("x")}}}},
		"bad raw":   {Vector: []float32{1}, Filter: driver.Filter{Raw: "nope"}},
	} {
		_, err := c.Similar(ctx, ref, q)
		if !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
		if errors.Is(err, driver.ErrConnection) {
			t.Errorf("%s reached the network: %v", name, err)
		}
	}
	if _, err := c.Similar(ctx, driver.CollectionRef{Namespace: "other", Name: "v"}, driver.VectorQuery{Vector: []float32{1}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown index: %v", err)
	}
}
