package tui

import (
	"reflect"
	"testing"
)

// Each driver dialect splits with its own lexical rules, and JSON bodies are
// split into their top-level values, never at a semicolon.
func TestSplitStatementsPerDialect(t *testing.T) {
	for _, tc := range []struct {
		dialect, text string
		want          []string
	}{
		{"googlesql", "select 'a\\';b'; select \"x;y\"; select `c;d` from t # ;\n", []string{"select 'a\\';b'", `select "x;y"`, "select `c;d` from t # ;"}},
		{"googlesql", "select 1 --;\n; select 2", []string{"select 1 --;", "select 2"}},
		{"tsql", "select [a]];b] from t; select 2", []string{"select [a]];b] from t", "select 2"}},
		{"oracle", "select q'[it's;]' from dual; select 1 from dual", []string{"select q'[it's;]' from dual", "select 1 from dual"}},
		{"oracle", "begin\n  x := 1;\n  y := 2;\nend;", []string{"begin\n  x := 1;\n  y := 2;\nend;"}},
		{"cql", "select $$a;b$$ from t; select 2", []string{"select $$a;b$$ from t", "select 2"}},
		{"partiql", "insert into t value {'a': 'x;y'}; select * from `t;u`", []string{"insert into t value {'a': 'x;y'}", "select * from `t;u`"}},
		{"snowflake", "select 'a\\';b'; select $$x;y$$", []string{"select 'a\\';b'", "select $$x;y$$"}},
		{"vertica", "select E'\\';x'; select $$a;b$$", []string{"select E'\\';x'", "select $$a;b$$"}},
		{"duckdb", "select $$a;b$$; select E'\\';'", []string{"select $$a;b$$", "select E'\\';'"}},
		{"elasticsearch", "{\"query\": \"a;b\"}\n\n{\"q\": \"c\\\"};\"}", []string{`{"query": "a;b"}`, `{"q": "c\"};"}`}},
		{"opensearch", "  [1, {\"a\": \"]\"}] ", []string{`[1, {"a": "]"}]`}},
		{"firestore-json", "{\"collection\":\"users\"}; ", []string{`{"collection":"users"}`, ";"}},
		{"weaviate", "{ Get { Article { title } } }", []string{"{ Get { Article { title } } }"}},
		{"pinecone", "", nil},
		{"chroma", "{\"op\":\"get\"", []string{`{"op":"get"`}},
		{"milvus", "{\"a\":1}{\"b\":2}", []string{`{"a":1}`, `{"b":2}`}},
		{"mongodb", "{\"find\":\"a;b\",\"filter\":{}}\n{\"count\":\"c\"}", []string{`{"find":"a;b","filter":{}}`, `{"count":"c"}`}},
		{"qdrant", "{\"op\":\"scroll\",\"collection\":\"x;y\"}", []string{`{"op":"scroll","collection":"x;y"}`}},
		{"cosmos-sql", "SELECT * FROM c WHERE c.a = 'x;y'; SELECT 1", []string{"SELECT * FROM c WHERE c.a = 'x;y'", "SELECT 1"}},
		{"cosmos-sql", " {\"query\": \"select 1; select 2\"}", []string{`{"query": "select 1; select 2"}`}},
	} {
		spans := splitStatements(tc.dialect, tc.text)
		var got []string
		for _, s := range spans {
			got = append(got, tc.text[s.Start:s.End])
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: splitStatements(%q) = %q, want %q", tc.dialect, tc.text, got, tc.want)
		}
	}
}

// GoogleSQL reads "…" as a string, like MySQL, for the completion token.
func TestGoogleSQLDoubleQuoteString(t *testing.T) {
	if !doubleQuoteString("googlesql") {
		t.Error(`doubleQuoteString("googlesql") = false, want true`)
	}
}
