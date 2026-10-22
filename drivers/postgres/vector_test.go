package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestSupportsVectorOnlyWithExtension(t *testing.T) {
	without := &conn{}
	if _, ok := driver.As[driver.VectorSearcher](without); ok {
		t.Error("VectorSearcher offered without the vector extension")
	}
	for _, cp := range driver.CapabilitiesOf(without) {
		if cp == driver.CapVectorSearcher {
			t.Error("CapabilitiesOf lists vector_searcher without the extension")
		}
	}
	if !without.Supports(driver.CapBrowser) || !without.Supports(driver.CapWatcher) {
		t.Error("other capabilities must stay supported")
	}
	with := &conn{vectorSchema: "public"}
	if _, ok := driver.As[driver.VectorSearcher](with); !ok {
		t.Error("VectorSearcher not offered with the extension")
	}
}

func TestSimilarWithoutExtensionIsUnsupported(t *testing.T) {
	c := &conn{}
	_, err := c.Similar(context.Background(), driver.CollectionRef{Name: "t"}, driver.VectorQuery{Vector: []float32{1}})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "vector extension") {
		t.Fatalf("err = %v, want ErrUnsupported naming the extension", err)
	}
}

func TestOpenKeepsEmbedderAndSkipsProbeWithoutOne(t *testing.T) {
	emb := drivertest.HashEmbedder{Dims: 4}
	c, err := openWith(t, &fakeConnect{}, driver.ConnConfig{DSN: "postgres://u@127.0.0.1:1/db", Embedder: emb})
	if err != nil {
		t.Fatal(err)
	}
	pc := c.(*conn)
	if pc.embedder != emb {
		t.Errorf("embedder = %v, want the ConnConfig's", pc.embedder)
	}
	if pc.vectorSchema != "" {
		t.Errorf("vectorSchema = %q without a probe", pc.vectorSchema)
	}
}

type failingEmbedder struct{ err error }

func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, f.err }
func (failingEmbedder) Dimensions() int                                        { return 0 }

type countEmbedder struct{ n int }

func (c countEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return make([][]float32, c.n), nil
}
func (countEmbedder) Dimensions() int { return 0 }

func TestQueryVector(t *testing.T) {
	ctx := context.Background()
	t.Run("vector wins over text", func(t *testing.T) {
		c := &conn{}
		got, err := c.queryVector(ctx, driver.VectorQuery{Vector: []float32{1, 2}, Text: "ignored"})
		if err != nil || len(got) != 2 || got[0] != 1 {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("text without embedder", func(t *testing.T) {
		c := &conn{}
		_, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"})
		if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("text with embedder", func(t *testing.T) {
		emb := drivertest.HashEmbedder{Dims: 4}
		c := &conn{embedder: emb}
		got, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := emb.Embed(ctx, []string{"hello"})
		if len(got) != 4 || got[0] != want[0][0] || got[3] != want[0][3] {
			t.Fatalf("got %v, want %v", got, want[0])
		}
	})
	t.Run("embedder error keeps its kind", func(t *testing.T) {
		host := driver.Wrap(driver.ErrAuth, "ai", "embed", errors.New("bad key"))
		c := &conn{embedder: failingEmbedder{host}}
		_, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"})
		if !errors.Is(err, driver.ErrAuth) || errors.Is(err, driver.ErrUnsupported) {
			t.Fatalf("err = %v, want the host's ErrAuth", err)
		}
	})
	t.Run("embedder context error", func(t *testing.T) {
		c := &conn{embedder: failingEmbedder{context.Canceled}}
		_, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"})
		if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
			t.Fatalf("err = %v, want context.Canceled with no kind", err)
		}
	})
	t.Run("embedder returns the wrong count", func(t *testing.T) {
		c := &conn{embedder: countEmbedder{2}}
		if _, err := c.queryVector(ctx, driver.VectorQuery{Text: "hello"}); err == nil {
			t.Fatal("no error for 2 vectors from 1 text")
		}
	})
	t.Run("neither vector nor text", func(t *testing.T) {
		c := &conn{}
		if _, err := c.queryVector(ctx, driver.VectorQuery{}); !errors.Is(err, driver.ErrSyntax) {
			t.Fatalf("err = %v, want ErrSyntax", err)
		}
	})
}

func TestSimilarK(t *testing.T) {
	for in, want := range map[int]int{-1: 10, 0: 10, 1: 1, 3: 3, 1000: 1000, 1001: 1000, 1 << 30: 1000} {
		if got := similarK(in); got != want {
			t.Errorf("similarK(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestVectorDims(t *testing.T) {
	for in, want := range map[string]int{"vector(4)": 4, "vector(1536)": 1536, "vector": 0, "public.vector(3)": 3, "text": 0, "vector(x)": 0} {
		if got := vectorDims(in); got != want {
			t.Errorf("vectorDims(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestMetricFromOpclass(t *testing.T) {
	for in, want := range map[string]driver.VectorMetric{
		"vector_l2_ops": driver.MetricL2, "vector_cosine_ops": driver.MetricCosine, "vector_ip_ops": driver.MetricInnerProduct,
		"vector_l1_ops": driver.MetricDefault, "": driver.MetricDefault,
	} {
		if got := metricFromOpclass(in); got != want {
			t.Errorf("metricFromOpclass(%q) = %q, want %q", in, got, want)
		}
	}
}

func vecSchema() *driver.CollectionSchema {
	return &driver.CollectionSchema{
		Ref: driver.CollectionRef{Namespace: "public", Name: "Vecs"},
		Fields: []driver.Field{
			{Name: "id", Type: driver.TypeInt, NativeType: "integer"},
			{Name: "Emb", Type: driver.TypeVector, NativeType: "vector(3)"},
			{Name: "label", Type: driver.TypeString, NativeType: "text"},
		},
	}
}

func TestBuildSimilar(t *testing.T) {
	vec := []float32{1, 0, 0}
	t.Run("each metric", func(t *testing.T) {
		for metric, op := range map[driver.VectorMetric]string{driver.MetricL2: "<->", driver.MetricCosine: "<=>", driver.MetricInnerProduct: "<#>"} {
			text, params, err := buildSimilar(vecSchema(), "ext", driver.VectorQuery{Metric: metric, K: 5}, vec, driver.MetricDefault)
			if err != nil {
				t.Fatal(err)
			}
			dist := `"Emb" OPERATOR("ext".` + op + `) CAST($1 AS "ext"."vector")`
			want := `SELECT *, ` + dist + ` AS "_distance" FROM "public"."Vecs" WHERE "Emb" IS NOT NULL ORDER BY ` + dist + ` LIMIT 5`
			if text != want {
				t.Errorf("%s:\n got %s\nwant %s", metric, text, want)
			}
			if len(params) != 1 || params[0].Type() != driver.TypeVector {
				t.Errorf("params = %v", params)
			}
		}
	})
	t.Run("default metric", func(t *testing.T) {
		text, _, err := buildSimilar(vecSchema(), "public", driver.VectorQuery{}, vec, driver.MetricDefault)
		if err != nil || !strings.Contains(text, `OPERATOR("public".<=>)`) || !strings.HasSuffix(text, "LIMIT 10") {
			t.Errorf("no index: got %s, %v; want cosine, k 10", text, err)
		}
		text, _, err = buildSimilar(vecSchema(), "public", driver.VectorQuery{}, vec, driver.MetricInnerProduct)
		if err != nil || !strings.Contains(text, `OPERATOR("public".<#>)`) {
			t.Errorf("ip index: got %s, %v; want the index's metric", text, err)
		}
		text, _, err = buildSimilar(vecSchema(), "public", driver.VectorQuery{Metric: driver.MetricL2}, vec, driver.MetricInnerProduct)
		if err != nil || !strings.Contains(text, `OPERATOR("public".<->)`) {
			t.Errorf("explicit metric: got %s, %v; want l2", text, err)
		}
	})
	t.Run("k is capped", func(t *testing.T) {
		text, _, err := buildSimilar(vecSchema(), "public", driver.VectorQuery{K: 5000}, vec, driver.MetricDefault)
		if err != nil || !strings.HasSuffix(text, "LIMIT 1000") {
			t.Errorf("got %s, %v", text, err)
		}
	})
	t.Run("filter", func(t *testing.T) {
		q := driver.VectorQuery{K: 2, Filter: driver.Filter{
			Conditions: []driver.Condition{{Field: "label", Op: driver.OpEq, Value: driver.String("a")}},
			Raw:        "id > 1",
		}}
		text, params, err := buildSimilar(vecSchema(), "public", q, vec, driver.MetricDefault)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, `WHERE "Emb" IS NOT NULL AND "label" = $2 AND (id > 1`+"\n)") {
			t.Errorf("got %s", text)
		}
		if len(params) != 2 || params[1].String() != "a" {
			t.Errorf("params = %v", params)
		}
	})
	t.Run("named field, case-insensitive", func(t *testing.T) {
		text, _, err := buildSimilar(vecSchema(), "public", driver.VectorQuery{Field: "emb"}, vec, driver.MetricDefault)
		if err != nil || !strings.Contains(text, `"Emb" OPERATOR`) {
			t.Errorf("got %s, %v", text, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		two := vecSchema()
		two.Fields = append(two.Fields, driver.Field{Name: "other", Type: driver.TypeVector, NativeType: "vector(3)"})
		none := vecSchema()
		none.Fields = none.Fields[:1]
		tests := []struct {
			name   string
			schema *driver.CollectionSchema
			q      driver.VectorQuery
			vec    []float32
			kind   error
		}{
			{"unknown metric", vecSchema(), driver.VectorQuery{Metric: "hamming"}, vec, driver.ErrUnsupported},
			{"no such field", vecSchema(), driver.VectorQuery{Field: "nope"}, vec, driver.ErrNotFound},
			{"not a vector field", vecSchema(), driver.VectorQuery{Field: "label"}, vec, driver.ErrUnsupported},
			{"no vector column", none, driver.VectorQuery{}, vec, driver.ErrUnsupported},
			{"several vector columns", two, driver.VectorQuery{}, vec, driver.ErrSyntax},
			{"dimension mismatch", vecSchema(), driver.VectorQuery{}, []float32{1, 2}, driver.ErrSyntax},
			{"empty vector", vecSchema(), driver.VectorQuery{}, nil, driver.ErrSyntax},
			{"bad filter field", vecSchema(), driver.VectorQuery{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "x", Op: driver.OpEq, Value: driver.Int(1)}}}}, vec, driver.ErrNotFound},
		}
		for _, tt := range tests {
			if _, _, err := buildSimilar(tt.schema, "public", tt.q, tt.vec, driver.MetricDefault); !errors.Is(err, tt.kind) {
				t.Errorf("%s: err = %v, want %v", tt.name, err, tt.kind)
			}
		}
	})
	t.Run("several vector columns with a field", func(t *testing.T) {
		two := vecSchema()
		two.Fields = append(two.Fields, driver.Field{Name: "other", Type: driver.TypeVector, NativeType: "vector"})
		text, _, err := buildSimilar(two, "public", driver.VectorQuery{Field: "other"}, []float32{1, 2}, driver.MetricDefault)
		if err != nil || !strings.Contains(text, `"other" OPERATOR`) {
			t.Errorf("got %s, %v (an unsized column skips the dimension check)", text, err)
		}
	})
}

func TestQueryVectorKindlessEmbedderError(t *testing.T) {
	host := errors.New("embedding service exploded")
	c := &conn{embedder: failingEmbedder{host}}
	_, err := c.queryVector(context.Background(), driver.VectorQuery{Text: "hello"})
	if err == nil || !errors.Is(err, host) {
		t.Fatalf("err = %v, want the host error kept", err)
	}
	if k := driver.KindOf(err); k != nil {
		t.Errorf("kind = %v, want none: a kindless host error must not gain a misleading kind", k)
	}
	if !strings.Contains(err.Error(), "embedding service exploded") {
		t.Errorf("message %q lost the host's text", err)
	}
}

func TestBuildSimilarDistanceColumnName(t *testing.T) {
	vec := []float32{1, 0, 0}
	one := vecSchema()
	one.Fields = append(one.Fields, driver.Field{Name: "_distance", Type: driver.TypeFloat, NativeType: "double precision"})
	text, _, err := buildSimilar(one, "public", driver.VectorQuery{}, vec, driver.MetricDefault)
	if err != nil || !strings.Contains(text, `AS "_distance_2" FROM`) {
		t.Errorf("with _distance: got %s, %v; want _distance_2", text, err)
	}
	two := vecSchema()
	two.Fields = append(two.Fields,
		driver.Field{Name: "_distance", Type: driver.TypeFloat}, driver.Field{Name: "_distance_2", Type: driver.TypeFloat})
	text, _, err = buildSimilar(two, "public", driver.VectorQuery{}, vec, driver.MetricDefault)
	if err != nil || !strings.Contains(text, `AS "_distance_3" FROM`) {
		t.Errorf("with _distance and _distance_2: got %s, %v; want _distance_3", text, err)
	}
	if got := distanceColumnName(vecSchema()); got != "_distance" {
		t.Errorf("no clash: %q", got)
	}
}

func TestVectorFieldCaseCollision(t *testing.T) {
	s := vecSchema()
	s.Fields = append(s.Fields, driver.Field{Name: "EMB", Type: driver.TypeVector, NativeType: "vector(3)"})
	_, err := vectorField(s, "emb")
	if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "Emb") || !strings.Contains(err.Error(), "EMB") {
		t.Fatalf("err = %v, want ErrSyntax naming Emb and EMB", err)
	}
	for _, name := range []string{"Emb", "EMB"} {
		f, err := vectorField(s, name)
		if err != nil || f.Name != name {
			t.Errorf("exact %q: got %q, %v", name, f.Name, err)
		}
	}
}

func TestVectorCache(t *testing.T) {
	now := time.Unix(1000, 0)
	vc := &vectorCache{ttl: time.Minute, now: func() time.Time { return now }}
	ref := driver.CollectionRef{Namespace: "public", Name: "vecs"}
	if _, ok := vc.schema(ref); ok {
		t.Fatal("empty cache hit")
	}
	s := vecSchema()
	vc.putSchema(ref, s)
	if got, ok := vc.schema(ref); !ok || got != s {
		t.Fatalf("schema = %v, %v", got, ok)
	}
	if _, ok := vc.metric(ref, "Emb"); ok {
		t.Fatal("metric hit before put")
	}
	vc.putMetric(ref, "Emb", driver.MetricInnerProduct)
	if m, ok := vc.metric(ref, "Emb"); !ok || m != driver.MetricInnerProduct {
		t.Fatalf("metric = %q, %v", m, ok)
	}
	if _, ok := vc.schema(driver.CollectionRef{Namespace: "other", Name: "vecs"}); ok {
		t.Error("another ref hit")
	}
	now = now.Add(time.Minute + time.Second)
	if _, ok := vc.schema(ref); ok {
		t.Error("schema survived the TTL")
	}
	if _, ok := vc.metric(ref, "Emb"); ok {
		t.Error("metric survived the TTL")
	}
	vc.putSchema(ref, s)
	vc.putMetric(ref, "Emb", driver.MetricL2)
	vc.invalidate()
	if _, ok := vc.schema(ref); ok {
		t.Error("schema survived invalidate")
	}
	if _, ok := vc.metric(ref, "Emb"); ok {
		t.Error("metric survived invalidate")
	}
	var zero vectorCache // the zero value works, with the default TTL
	zero.putSchema(ref, s)
	if _, ok := zero.schema(ref); !ok {
		t.Error("zero-value cache missed")
	}
}

func TestConnInvalidateDropsVectorCache(t *testing.T) {
	c, err := openWith(t, &fakeConnect{}, driver.ConnConfig{DSN: "postgres://u@127.0.0.1:1/db"})
	if err != nil {
		t.Fatal(err)
	}
	pc := c.(*conn)
	ref := driver.CollectionRef{Namespace: "public", Name: "vecs"}
	pc.vec.putSchema(ref, vecSchema())
	pc.Invalidate()
	if _, ok := pc.vec.schema(ref); ok {
		t.Error("Invalidate kept the vector cache")
	}
}
