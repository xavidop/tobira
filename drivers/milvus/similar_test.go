package milvus

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"

	"github.com/xavidop/tobira/driver"
)

// vecsProto: three float vectors (one per metric), a BM25 sparse output of
// "body" and a TEXTEMBEDDING output of "doc".
func vecsProto() *schemapb.CollectionSchema {
	dim := func(n string) []*commonpb.KeyValuePair { return []*commonpb.KeyValuePair{{Key: "dim", Value: n}} }
	return &schemapb.CollectionSchema{
		Name: "vecs",
		Fields: []*schemapb.FieldSchema{
			{FieldID: 100, Name: "id", DataType: schemapb.DataType_Int64, IsPrimaryKey: true},
			{FieldID: 101, Name: "vec_l2", DataType: schemapb.DataType_FloatVector, TypeParams: dim("4")},
			{FieldID: 102, Name: "vec_cos", DataType: schemapb.DataType_FloatVector, TypeParams: dim("4")},
			{FieldID: 103, Name: "vec_ip", DataType: schemapb.DataType_FloatVector, TypeParams: dim("4")},
			{FieldID: 104, Name: "body", DataType: schemapb.DataType_VarChar},
			{FieldID: 105, Name: "sparse", DataType: schemapb.DataType_SparseFloatVector, IsFunctionOutput: true},
			{FieldID: 106, Name: "doc", DataType: schemapb.DataType_VarChar},
			{FieldID: 107, Name: "doc_vec", DataType: schemapb.DataType_FloatVector, TypeParams: dim("4"), IsFunctionOutput: true},
		},
		Functions: []*schemapb.FunctionSchema{
			{Name: "bm25", Type: schemapb.FunctionType_BM25, InputFieldNames: []string{"body"}, OutputFieldNames: []string{"sparse"}},
			{Name: "emb", Type: schemapb.FunctionType_TextEmbedding, InputFieldNames: []string{"doc"}, OutputFieldNames: []string{"doc_vec"},
				Params: []*commonpb.KeyValuePair{{Key: "provider", Value: "openai"}, {Key: "model_name", Value: "text-embedding-3-small"}, {Key: "api_key", Value: "sk-should-never-show"},
					{Key: "endpoint", Value: "https://bob:hunter2@tei.example/embed?token=t0ps3cret&x=1"}}},
		},
	}
}

func vecsMetrics() map[string]string {
	return map[string]string{"vec_l2": "L2", "vec_cos": "COSINE", "vec_ip": "IP", "sparse": "BM25", "doc_vec": "COSINE"}
}

func TestPickVector(t *testing.T) {
	s := schemaFromProtoSchema("default", "vecs", vecsProto())
	m := vecsMetrics()
	for _, tc := range []struct {
		field  string
		metric driver.VectorMetric
		text   bool
		want   string
		kind   error
	}{
		{"", driver.MetricL2, false, "vec_l2", nil},
		{"", driver.MetricCosine, false, "", driver.ErrSyntax}, // vec_cos and doc_vec
		{"", driver.MetricInnerProduct, false, "vec_ip", nil},
		{"", driver.MetricDefault, false, "", driver.ErrSyntax},
		{"vec_cos", driver.MetricDefault, false, "vec_cos", nil},
		{"vec_cos", driver.MetricL2, false, "", driver.ErrUnsupported},
		{"nope", driver.MetricDefault, false, "", driver.ErrNotFound},
		{"body", driver.MetricDefault, false, "", driver.ErrUnsupported},
		{"sparse", driver.MetricDefault, false, "", driver.ErrUnsupported}, // a vector cannot search BM25
		{"sparse", driver.MetricDefault, true, "sparse", nil},
		{"", driver.MetricDefault, true, "", driver.ErrSyntax}, // sparse and doc_vec both embed text
		{"", "manhattan", false, "", driver.ErrUnsupported},
	} {
		f, err := pickVector(s, m, tc.field, tc.metric, tc.text)
		if tc.kind != nil {
			if !errors.Is(err, tc.kind) {
				t.Errorf("pickVector(%q, %q, text=%v) = %v, want %v", tc.field, tc.metric, tc.text, err, tc.kind)
			}
			continue
		}
		if err != nil || f.name != tc.want {
			t.Errorf("pickVector(%q, %q, text=%v) = %v, %v; want %s", tc.field, tc.metric, tc.text, f, err, tc.want)
		}
	}
}

func TestDistanceFromScore(t *testing.T) {
	for _, tc := range []struct {
		metric string
		score  float32
		want   float64
	}{
		{"L2", 2.5, 2.5},
		{"COSINE", 0.75, 0.25},
		{"IP", 3, -3},
		{"BM25", 1.5, -1.5},
		{"cosine", 1, 0},
	} {
		if got := distanceFromScore(tc.metric, tc.score); math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("distanceFromScore(%s, %g) = %g, want %g", tc.metric, tc.score, got, tc.want)
		}
	}
}

// Text searched through a TEXTEMBEDDING function is sent by Milvus to the
// embedding provider: egress, refused on a read-only request. BM25 runs in
// Milvus and is allowed.
func TestTextEmbeddingEgressRefusedReadOnly(t *testing.T) {
	s := schemaFromProtoSchema("default", "vecs", vecsProto())
	docVec, _ := s.field("doc_vec")
	sparse, _ := s.field("sparse")
	if err := checkTextEgress(s, docVec, true); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("TEXTEMBEDDING read-only = %v, want ErrReadOnly", err)
	}
	if err := checkTextEgress(s, docVec, false); err != nil {
		t.Errorf("TEXTEMBEDDING writable = %v, want nil", err)
	}
	if err := checkTextEgress(s, sparse, true); err != nil {
		t.Errorf("BM25 read-only = %v, want nil", err)
	}
}

func TestSimilarTextEmbeddingReadOnlyNeverSearches(t *testing.T) {
	c, fake := newFakeConn(t, true)
	fake.describe = describeOK(vecsProto())
	fake.index = func(*milvuspb.DescribeIndexRequest) (*milvuspb.DescribeIndexResponse, error) {
		var out []*milvuspb.IndexDescription
		for f, m := range vecsMetrics() {
			out = append(out, &milvuspb.IndexDescription{FieldName: f, IndexName: f, Params: []*commonpb.KeyValuePair{{Key: "metric_type", Value: m}, {Key: "index_type", Value: "AUTOINDEX"}}})
		}
		return &milvuspb.DescribeIndexResponse{Status: okStatus(), IndexDescriptions: out}, nil
	}
	_, err := c.Similar(context.Background(), driver.CollectionRef{Name: "vecs"}, driver.VectorQuery{Text: "hello", Field: "doc_vec"})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("Similar text via TEXTEMBEDDING (ro) = %v, want ErrReadOnly", err)
	}
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"vecs","text":"hello","annsField":"doc_vec"}`})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("search op text via TEXTEMBEDDING (ro) = %v, want ErrReadOnly", err)
	}
	if slices.Contains(fake.Calls(), "Search") {
		t.Errorf("an egress search reached the server: %v", fake.Calls())
	}
}

type fixedEmbedder struct{ vec []float32 }

func (e fixedEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{e.vec}, nil
}
func (e fixedEmbedder) Dimensions() int { return len(e.vec) }

func TestQueryVectorChecks(t *testing.T) {
	c, _ := newFakeConn(t, false)
	if _, err := c.queryVector(context.Background(), driver.VectorQuery{Vector: []float32{1, 2}}, 4); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("wrong dimension: %v", err)
	}
	if _, err := c.queryVector(context.Background(), driver.VectorQuery{Vector: []float32{float32(math.Inf(1)), 0}}, 2); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("non-finite: %v", err)
	}
	if _, err := c.queryVector(context.Background(), driver.VectorQuery{Text: "x"}, 2); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("text without an embedder: %v", err)
	}
	e, _ := newFakeConnCfg(t, driver.ConnConfig{Embedder: fixedEmbedder{[]float32{1, 0}}})
	v, err := e.queryVector(context.Background(), driver.VectorQuery{Text: "x"}, 2)
	if err != nil || len(v) != 2 {
		t.Errorf("embedded text: %v %v", v, err)
	}
}

func TestDescribeRedactsFunctionSecrets(t *testing.T) {
	s := schemaFromProtoSchema("default", "vecs", vecsProto())
	ddl := s.ddl()
	if ddl == "" {
		t.Fatal("no DDL")
	}
	if strings.Contains(ddl, "sk-should-never-show") {
		t.Errorf("the DDL leaks a function credential:\n%s", ddl)
	}
	for _, leak := range []string{"hunter2", "t0ps3cret", "bob:"} {
		if strings.Contains(ddl, leak) {
			t.Errorf("the DDL leaks %q from a URL param:\n%s", leak, ddl)
		}
	}
	if !strings.Contains(ddl, "tei.example/embed") {
		t.Errorf("the DDL lost the endpoint host:\n%s", ddl)
	}
	if !strings.Contains(ddl, "text-embedding-3-small") {
		t.Errorf("the DDL lost harmless params:\n%s", ddl)
	}
}
