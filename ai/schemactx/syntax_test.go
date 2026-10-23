package schemactx

import (
	"strings"
	"testing"
)

// Every dialect whose query is a tobira request shape, not a language the
// model knows, has a note that ends the sentence "A query for this database
// is ..." and shows examples; SQL dialects have none.
func TestQuerySyntax(t *testing.T) {
	for dialect, want := range map[string]string{
		"mongodb":        `{"count":"orders"`,
		"redis":          "HGETALL",
		"elasticsearch":  `"index"`,
		"firestore-json": `"collection"`,
		"pinecone":       `"topK"`,
		"qdrant":         `"op":"scroll"`,
		"milvus":         `"outputFields"`,
		"weaviate":       `"bm25"`,
		"chroma":         `"queryTexts"`,
		"cosmos-sql":     "SELECT VALUE COUNT(1)",
		"partiql":        `FROM "orders"`,
		"influxdb3-sql":  "date_bin",
		"cypher":         "db.relationshipTypes()",
	} {
		got := QuerySyntax(dialect)
		if !strings.HasPrefix(got, "one ") || !strings.Contains(got, "Examples: ") || !strings.Contains(got, want) {
			t.Errorf("%s: %q", dialect, got)
		}
	}
	if got := QuerySyntax("mongodb"); !strings.Contains(got, "never mongosh") {
		t.Errorf("mongodb note does not warn off mongosh: %q", got)
	}
	for _, sql := range []string{"postgresql", "sqlite", "mysql", "tsql", "fake", ""} {
		if got := QuerySyntax(sql); got != "" {
			t.Errorf("%s: unexpected note %q", sql, got)
		}
	}
}
