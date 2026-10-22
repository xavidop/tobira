package weaviate

import (
	"errors"
	"strings"
	"testing"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
)

func testClass() *models.Class {
	return &models.Class{
		Class: "Article",
		Properties: []*models.Property{
			{Name: "title", DataType: []string{"text"}},
			{Name: "n", DataType: []string{"int"}},
			{Name: "tags", DataType: []string{"text[]"}},
			{Name: "when", DataType: []string{"date"}},
			{Name: "vector", DataType: []string{"text"}},
			{Name: "meta", DataType: []string{"object"}, NestedProperties: []*models.NestedProperty{{Name: "k", DataType: []string{"text"}}}},
			{Name: "author", DataType: []string{"Person"}},
		},
		Vectorizer:        "text2vec-openai",
		VectorIndexType:   "hnsw",
		VectorIndexConfig: map[string]any{"distance": "cosine"},
		VectorConfig: map[string]models.VectorConfig{
			"title_vec": {Vectorizer: map[string]any{"text2vec-transformers": map[string]any{}}, VectorIndexType: "flat", VectorIndexConfig: map[string]any{"distance": "l2-squared"}},
			"n":         {Vectorizer: map[string]any{"none": map[string]any{}}, VectorIndexType: "hnsw", VectorIndexConfig: map[string]any{"distance": "dot"}},
			"colbert":   {Vectorizer: map[string]any{"none": map[string]any{}}, VectorIndexType: "hnsw", VectorIndexConfig: map[string]any{"distance": "cosine", "multivector": map[string]any{"enabled": true}}},
		},
	}
}

func TestSchemaFromClass(t *testing.T) {
	s := schemaFromClass(testClass())
	if s.class != "Article" {
		t.Errorf("class = %q", s.class)
	}
	var props []string
	for _, p := range s.props {
		props = append(props, p.name)
	}
	if strings.Join(props, ",") != "title,n,tags,when,vector,meta,author" {
		t.Errorf("props = %v", props)
	}
	got := map[string]vectorInfo{}
	var cols []string
	for _, v := range s.vectors {
		got[v.name] = v
		cols = append(cols, v.column)
	}
	// The legacy vector first, then named vectors by name; a column that
	// would clash with a property is suffixed.
	if strings.Join(cols, ",") != "_vector,colbert,n_vector,title_vec" {
		t.Errorf("vector columns = %v", cols)
	}
	if v := got[""]; v.distance != "cosine" || v.vectorizer != "text2vec-openai" {
		t.Errorf("legacy vector = %+v", v)
	}
	if v := got["title_vec"]; v.distance != "l2-squared" || v.vectorizer != "text2vec-transformers" || v.indexType != "flat" {
		t.Errorf("title_vec = %+v", v)
	}
	if v := got["colbert"]; !v.multi {
		t.Errorf("colbert should be a multi-vector: %+v", v)
	}
	if v := got["n"]; v.distance != "dot" || v.column != "n_vector" {
		t.Errorf("n = %+v", v)
	}
	vz := s.vectorizerNames()
	if strings.Join(vz, ",") != "text2vec-openai,none,none,text2vec-transformers" {
		t.Errorf("vectorizers = %v", vz)
	}
	if _, ok := s.vectorByColumn("title_vec"); !ok {
		t.Error("vectorByColumn(title_vec) not found")
	}
	if _, ok := s.prop("meta"); !ok {
		t.Error("prop(meta) not found")
	}
	// The default distance is cosine when the config does not say.
	c := &models.Class{Class: "B", VectorIndexType: "hnsw"}
	if v := schemaFromClass(c).vectors; len(v) != 1 || v[0].distance != "cosine" || v[0].column != "vector" {
		t.Errorf("defaults = %+v", v)
	}
}

func TestSelection(t *testing.T) {
	s := schemaFromClass(testClass())
	got := s.selection(nil, true)
	want := `title n tags when vector meta { k } _additional { id vector vectors { colbert n title_vec } }`
	if got != want {
		t.Errorf("selection:\n got %s\nwant %s", got, want)
	}
	if got := s.selection([]string{"n"}, false); got != `n _additional { id }` {
		t.Errorf("fields selection = %s", got)
	}
	geo := schemaFromClass(&models.Class{Class: "G", Properties: []*models.Property{
		{Name: "where", DataType: []string{"geoCoordinates"}},
		{Name: "tel", DataType: []string{"phoneNumber"}},
		{Name: "deep", DataType: []string{"object[]"}, NestedProperties: []*models.NestedProperty{
			{Name: "o", DataType: []string{"object"}, NestedProperties: []*models.NestedProperty{{Name: "x", DataType: []string{"int"}}}},
		}},
	}})
	if got := geo.selection(nil, false); got != `where { latitude longitude } tel { input internationalFormatted countryCode national nationalFormatted valid } deep { o { x } } _additional { id }` {
		t.Errorf("geo selection = %s", got)
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"Article", "tobira_people", "A1_b"} {
		if err := checkName("query", ok); err != nil {
			t.Errorf("checkName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1a", "a b", "a-b", "../objects", "A{", "é", `A"`} {
		if err := checkName("query", bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkName(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}
