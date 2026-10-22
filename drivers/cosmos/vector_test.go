package cosmos

import (
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

func TestVectorFunction(t *testing.T) {
	for _, tc := range []struct {
		m      driver.VectorMetric
		policy string
		want   string
	}{
		{driver.MetricDefault, "cosine", "cosine"},
		{driver.MetricDefault, "euclidean", "euclidean"},
		{driver.MetricDefault, "", "cosine"}, // the account reports no policy: Cosmos DB's default
		{driver.MetricL2, "cosine", "euclidean"},
		{driver.MetricCosine, "euclidean", "cosine"},
		{driver.MetricInnerProduct, "cosine", "dotproduct"},
	} {
		got, err := vectorFunction(tc.m, tc.policy)
		if err != nil || got != tc.want {
			t.Errorf("vectorFunction(%q, %q) = %q, %v; want %q", tc.m, tc.policy, got, err, tc.want)
		}
	}
	if _, err := vectorFunction("hamming", "cosine"); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("an unknown metric: want ErrUnsupported, got %v", err)
	}
	if _, err := vectorFunction(driver.MetricDefault, "manhattan"); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("an unknown policy function: want ErrUnsupported, got %v", err)
	}
}

func TestDistanceOf(t *testing.T) {
	for _, tc := range []struct {
		fn          string
		score, want float64
	}{
		{"euclidean", 0.5, 0.5},
		{"cosine", 0.75, 0.25},
		{"dotproduct", 6, -6},
	} {
		if got := distanceOf(tc.fn, tc.score); got != tc.want {
			t.Errorf("distanceOf(%q, %g) = %g, want %g", tc.fn, tc.score, got, tc.want)
		}
	}
}

func TestVectorSQL(t *testing.T) {
	got := vectorSQL(`c["vec"]`, "euclidean", true, 3, `c["n"] > @w1`)
	want := `SELECT TOP 3 c AS doc, VectorDistance(c["vec"], @qv, true, {'distanceFunction':'euclidean'}) AS score FROM c` +
		` WHERE c["n"] > @w1 ORDER BY VectorDistance(c["vec"], @qv, true, {'distanceFunction':'euclidean'})`
	if got != want {
		t.Errorf("vectorSQL:\n got %s\nwant %s", got, want)
	}
	got = vectorSQL(`c["vec"]`, "cosine", false, 10, "")
	want = `SELECT TOP 10 c AS doc, VectorDistance(c["vec"], @qv, false, {'distanceFunction':'cosine'}) AS score FROM c` +
		` ORDER BY VectorDistance(c["vec"], @qv, false, {'distanceFunction':'cosine'})`
	if got != want {
		t.Errorf("vectorSQL without a filter:\n got %s\nwant %s", got, want)
	}
}

func TestVectorPathField(t *testing.T) {
	for _, tc := range []struct{ path, want string }{{"/vec", "vec"}, {"/emb/text", "emb.text"}} {
		if got := pkFieldName(tc.path); got != tc.want {
			t.Errorf("pkFieldName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestVectorEmbeddingFallback(t *testing.T) {
	withPolicy := &azcosmos.ContainerProperties{ID: "v", VectorEmbeddingPolicy: &azcosmos.VectorEmbeddingPolicy{VectorEmbeddings: []azcosmos.VectorEmbedding{
		{Path: "/a", DistanceFunction: "euclidean", Dimensions: 3}, {Path: "/b/c", DistanceFunction: "cosine", Dimensions: 2},
	}}}
	if e, err := vectorEmbedding(withPolicy, "b.c"); err != nil || e.Path != "/b/c" || e.Dimensions != 2 {
		t.Errorf("vectorEmbedding(b.c) = %+v, %v", e, err)
	}
	if _, err := vectorEmbedding(withPolicy, ""); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("two vector fields and no Field: want ErrSyntax, got %v", err)
	}
	if _, err := vectorEmbedding(withPolicy, "zz"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("an unknown vector field: want ErrNotFound, got %v", err)
	}
	// The emulator does not return the vector embedding policy: the field
	// then comes from the vector indexes, or from VectorQuery.Field.
	indexed := &azcosmos.ContainerProperties{ID: "v", IndexingPolicy: &azcosmos.IndexingPolicy{VectorIndexes: []azcosmos.VectorIndex{{Path: "/vec", Type: "diskANN"}}}}
	if e, err := vectorEmbedding(indexed, ""); err != nil || e.Path != "/vec" || e.DistanceFunction != "" || e.Dimensions != 0 {
		t.Errorf("vectorEmbedding from the vector index = %+v, %v", e, err)
	}
	bare := &azcosmos.ContainerProperties{ID: "v"}
	if e, err := vectorEmbedding(bare, "emb"); err != nil || e.Path != "/emb" {
		t.Errorf("vectorEmbedding(emb) without a reported policy = %+v, %v", e, err)
	}
	if _, err := vectorEmbedding(bare, ""); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("no policy, no index, no Field: want ErrUnsupported, got %v", err)
	}
}
