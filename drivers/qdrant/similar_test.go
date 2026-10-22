package qdrant

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

func TestEffectiveK(t *testing.T) {
	for _, tt := range []struct {
		in, want int
		warn     bool
	}{
		{0, defaultK, false}, {-3, defaultK, false}, {1, 1, false}, {maxK, maxK, false},
		{maxK + 1, maxK, true}, {math.MaxInt32, maxK, true},
	} {
		got, warn := effectiveK(tt.in)
		if got != tt.want || (warn != "") != tt.warn {
			t.Errorf("effectiveK(%d) = %d, %q; want %d, warning %v", tt.in, got, warn, tt.want, tt.warn)
		}
	}
}

func TestDistanceFromScore(t *testing.T) {
	for _, tt := range []struct {
		d     pb.Distance
		score float32
		want  float64
	}{
		{pb.Distance_Euclid, 5, 5},
		{pb.Distance_Cosine, 0.75, 0.25},
		{pb.Distance_Dot, 6, -6},
		{pb.Distance_Manhattan, 3, 3},
	} {
		if got := distanceFromScore(tt.d, tt.score); math.Abs(got-tt.want) > 1e-6 {
			t.Errorf("distanceFromScore(%v, %v) = %v, want %v", tt.d, tt.score, got, tt.want)
		}
	}
}

func TestPickVector(t *testing.T) {
	single := &collSchema{vectors: []vectorInfo{{name: "", column: "vector", size: 4, distance: pb.Distance_Cosine}}}
	named := &collSchema{vectors: []vectorInfo{
		{name: "vec_cos", column: "vec_cos", size: 4, distance: pb.Distance_Cosine},
		{name: "vec_ip", column: "vec_ip", size: 4, distance: pb.Distance_Dot},
		{name: "vec_l2", column: "vec_l2", size: 4, distance: pb.Distance_Euclid},
		{name: "sparse", column: "sparse", sparse: true},
	}}
	for _, tt := range []struct {
		name   string
		s      *collSchema
		field  string
		metric driver.VectorMetric
		want   string
		kind   error
	}{
		{"default", single, "", driver.MetricDefault, "vector", nil},
		{"default by column name", single, "vector", driver.MetricCosine, "vector", nil},
		{"default wrong metric", single, "", driver.MetricL2, "", driver.ErrUnsupported},
		{"named by field", named, "vec_ip", driver.MetricDefault, "vec_ip", nil},
		{"named by field and metric", named, "vec_l2", driver.MetricL2, "vec_l2", nil},
		{"named field wrong metric", named, "vec_l2", driver.MetricCosine, "", driver.ErrUnsupported},
		{"named by metric", named, "", driver.MetricInnerProduct, "vec_ip", nil},
		{"named ambiguous", named, "", driver.MetricDefault, "", driver.ErrSyntax},
		{"missing field", named, "nope", driver.MetricDefault, "", driver.ErrNotFound},
		{"sparse field", named, "sparse", driver.MetricDefault, "", driver.ErrUnsupported},
		{"unknown metric", single, "", driver.VectorMetric("hamming"), "", driver.ErrUnsupported},
		{"no vectors", &collSchema{}, "", driver.MetricDefault, "", driver.ErrUnsupported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := pickVector(tt.s, tt.field, tt.metric)
			if tt.kind != nil {
				if !errors.Is(err, tt.kind) {
					t.Fatalf("err = %v, want %v", err, tt.kind)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if v.column != tt.want {
				t.Errorf("picked %q, want %q", v.column, tt.want)
			}
		})
	}
	// Manhattan has no tobira metric: it is reachable only by default.
	man := &collSchema{vectors: []vectorInfo{{name: "", column: "vector", size: 2, distance: pb.Distance_Manhattan}}}
	if _, err := pickVector(man, "", driver.MetricDefault); err != nil {
		t.Errorf("manhattan default: %v", err)
	}
	if _, err := pickVector(man, "", driver.MetricL2); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("manhattan as l2: err = %v, want ErrUnsupported", err)
	}
}

// stubEmbedder returns vecs or err and counts its calls.
type stubEmbedder struct {
	vecs  [][]float32
	err   error
	calls atomic.Int32
}

func (s *stubEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.vecs, s.err
}

func (s *stubEmbedder) Dimensions() int { return 0 }

func TestSimilarQueryVector(t *testing.T) {
	// The collection is resolved first in Similar, so the vector is checked
	// against its size: queryVector takes the size (2 here) directly.
	const size = 2
	ctx := context.Background()

	t.Run("no embedder", func(t *testing.T) {
		c := newTestConnCfg(t, driver.ConnConfig{})
		_, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"}, size)
		if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
			t.Fatalf("err = %v, want ErrUnsupported with the hint", err)
		}
	})
	t.Run("nothing to search", func(t *testing.T) {
		c := newTestConnCfg(t, driver.ConnConfig{})
		_, err := c.queryVector(ctx, driver.VectorQuery{}, size)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Fatalf("err = %v, want ErrSyntax", err)
		}
	})
	t.Run("embedder error keeps its kind", func(t *testing.T) {
		for _, kind := range []error{driver.ErrAuth, driver.ErrConnection, driver.ErrPermission} {
			e := &stubEmbedder{err: driver.Wrap(kind, "", "embed", fmt.Errorf("provider said no"))}
			c := newTestConnCfg(t, driver.ConnConfig{Embedder: e})
			_, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"}, size)
			if !errors.Is(err, kind) {
				t.Errorf("err = %v, want kind %v", err, kind)
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Driver != Name || de.Op != "similar" {
				t.Errorf("err = %#v, want a qdrant similar *driver.Error", err)
			}
		}
	})
	t.Run("embedder unclassified error", func(t *testing.T) {
		e := &stubEmbedder{err: errors.New("boom")}
		c := newTestConnCfg(t, driver.ConnConfig{Embedder: e})
		_, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"}, size)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("embedder cancelled", func(t *testing.T) {
		e := &stubEmbedder{}
		c := newTestConnCfg(t, driver.ConnConfig{Embedder: e})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := c.queryVector(cctx, driver.VectorQuery{Text: "hello"}, size)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("embedder bad shapes", func(t *testing.T) {
		nan, inf := float32(math.NaN()), float32(math.Inf(-1))
		for _, vecs := range [][][]float32{nil, {}, {{1, 2}, {3, 4}}, {{}}, {{1, nan}}, {{inf, 0}}, {{1, 2, 3}}, {{1}}} {
			e := &stubEmbedder{vecs: vecs}
			c := newTestConnCfg(t, driver.ConnConfig{Embedder: e})
			_, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"}, size)
			if !errors.Is(err, driver.ErrSyntax) {
				t.Errorf("vecs %v: err = %v, want ErrSyntax", vecs, err)
			}
		}
	})
	t.Run("vector wins over text", func(t *testing.T) {
		e := &stubEmbedder{vecs: [][]float32{{1}}}
		c := newTestConnCfg(t, driver.ConnConfig{Embedder: e})
		_, err := c.queryVector(ctx, driver.VectorQuery{Vector: []float32{1, 0}, Text: "ignored"}, size)
		if err != nil {
			t.Fatal(err)
		}
		if n := e.calls.Load(); n != 0 {
			t.Errorf("embedder called %d times for a vector query", n)
		}
	})
	t.Run("vector of the wrong size", func(t *testing.T) {
		c := newTestConnCfg(t, driver.ConnConfig{})
		_, err := c.queryVector(ctx, driver.VectorQuery{Vector: []float32{1, 2, 3}}, size)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Fatalf("err = %v, want ErrSyntax", err)
		}
	})
	t.Run("non-finite vector", func(t *testing.T) {
		c := newTestConnCfg(t, driver.ConnConfig{})
		_, err := c.queryVector(ctx, driver.VectorQuery{Vector: []float32{float32(math.NaN()), 0}}, size)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Fatalf("err = %v, want ErrSyntax", err)
		}
	})
}
