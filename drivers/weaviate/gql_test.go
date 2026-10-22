package weaviate

import (
	"errors"
	"strings"
	"testing"

	"github.com/weaviate/weaviate/entities/models"
	"github.com/xavidop/tobira/driver"
)

func TestLexGraphQL(t *testing.T) {
	toks, err := lexGraphQL("# comment\n{ Get { A(where: {valueText: \"a \\\"}\\u00e9\", n: -1.5e3}) { b ...on X { c } } } }\ufeff")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range toks {
		got = append(got, tk.text)
	}
	want := `{ Get { A ( where : { valueText : "a \"}\u00e9" , n : -1.5e3 } ) { b ... on X { c } } } }`
	// Commas are insignificant in GraphQL and dropped by the lexer.
	want = strings.ReplaceAll(want, " , ", " ")
	if strings.Join(got, " ") != want {
		t.Errorf("tokens:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	for _, bad := range []string{`{ "unterminated }`, `{ """block`, "{ \x01 }", `{ a % b }`, "{ \"line\nbreak\" }", `{ "\q" }`, `{ 1.e }`} {
		if _, err := lexGraphQL(bad); err == nil {
			t.Errorf("lexGraphQL(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseGraphQLAccepts(t *testing.T) {
	for _, tt := range []struct {
		text, op, class string
	}{
		{`{ Get { Article { title } } }`, "Get", "Article"},
		{`query { Get { Article(limit: 3, where: {path: ["n"], operator: GreaterThan, valueInt: 1}) { title _additional { id distance } } } }`, "Get", "Article"},
		{`query Named { Aggregate { Article { meta { count } } } }`, "Aggregate", "Article"},
		{`{Get{Article(nearVector:{vector:[1,0]}){title author{...on Person{name}}}}}`, "Get", "Article"},
		{`{ Get { a: Article { t: title } } }`, "Get", "Article"},
		{"{ Get { Article(bm25: {query: \"\"\"block \"quoted\" text\"\"\"}) { title } } }", "Get", "Article"},
	} {
		doc, err := parseGraphQL(tt.text)
		if err != nil {
			t.Fatalf("parseGraphQL(%s): %v", tt.text, err)
		}
		if doc.op != tt.op || doc.class.name != tt.class {
			t.Errorf("parseGraphQL(%s) = op %q class %q", tt.text, doc.op, doc.class.name)
		}
	}
}

func TestParseGraphQLRefusals(t *testing.T) {
	for _, text := range []string{
		`mutation { Delete { Article } }`,
		`subscription { Get { Article { title } } }`,
		`{ Get { Article { title } } } mutation { x }`,
		`{ Get { Article { title } } } { Get { Article { title } } }`,
		`query { Get { Article { title } } } query { Get { B { c } } }`,
		`fragment F on Article { title } { Get { Article { ...F } } }`,
		`{ Get { Article { ...F } } }`,
		`query($n: Int) { Get { Article(limit: $n) { title } } }`,
		`{ Get { Article(limit: $n) { title } } }`,
		`{ Get { Article @include(if: true) { title } } }`,
		`{ Explore(concepts: ["x"]) { beacon } }`,
		`{ __schema { types { name } } }`,
		`{ Get { Article { title } } Aggregate { Article { meta { count } } } }`,
		`{ Get { Article { title } B { c } } }`,
		`{ Get(x: 1) { Article { title } } }`,
		`{ g: Get { Article { title } } }`,
		`{ Get { Article } }`,
		`{ Get { ...on X { a } } }`,
		`{ Get { } }`,
		`{ Get { Article { title } }`,
		`{ Get { Article { title } } } trailing`,
		`{ Get { Article(where: {path: ["a"]} { title } } }`,
		``,
		`Get { Article { title } }`,
		`{ Get { ArticleWithA_Very${} { title } } }`,
	} {
		if _, err := parseGraphQL(text); err == nil {
			t.Errorf("parseGraphQL(%q) succeeded, want a refusal", text)
		}
	}
}

// remote reports every vectorizer a class uses, for the guard.
type fakeClasses map[string][]string

func (f fakeClasses) vectorizers(class string) ([]string, error) {
	v, ok := f[class]
	if !ok {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "query", errors.New("no class"))
	}
	return v, nil
}

func TestGuardGraphQLReadOnly(t *testing.T) {
	classes := fakeClasses{"Local": {"none", "text2vec-transformers"}, "Remote": {"none", "text2vec-openai"}, "Unset": {""}}
	allowed := []string{
		`{ Get { Local { title _additional { id distance certainty score explainScore creationTimeUnix lastUpdateTimeUnix vector vectors { a } isConsistent } } } }`,
		`{ Get { Remote(nearVector: {vector: [1, 0]}, where: {path: ["a"], operator: Equal, valueText: "x"}, limit: 2, offset: 1) { title } } }`,
		`{ Get { Remote(nearObject: {id: "00000000-0000-0000-0000-000000000001"}) { title } } }`,
		`{ Get { Remote(bm25: {query: "x"}, autocut: 1) { title } } }`,
		`{ Get { Remote(after: "00000000-0000-0000-0000-000000000001", limit: 5) { title } } }`,
		`{ Get { Remote(sort: [{path: ["a"], order: asc}]) { title } } }`,
		`{ Get { Local(nearText: {concepts: ["x"]}) { title } } }`,
		`{ Get { Local(hybrid: {query: "x"}) { title } } }`,
		`{ Aggregate { Remote(where: {path: ["a"], operator: Equal, valueInt: 1}, groupBy: ["a"], objectLimit: 3) { meta { count } } } }`,
		`{ Get { Remote { author { ... on Person { name } } } } }`,
		`{ Get { Remote(groupBy: {path: ["a"], groups: 2, objectsPerGroup: 2}) { _additional { group { id count } } } } }`,
		`{ Aggregate { Remote { title { topOccurrences(limit: 3) { value occurs } } } } }`,
	}
	for _, text := range allowed {
		doc, err := parseGraphQL(text)
		if err != nil {
			t.Fatalf("parseGraphQL(%s): %v", text, err)
		}
		if err := guardGraphQL(doc, true, classes); err != nil {
			t.Errorf("guardGraphQL(%s, ro) = %v, want allowed", text, err)
		}
	}
	refused := []string{
		`{ Get { Remote(nearText: {concepts: ["x"]}) { title } } }`,
		`{ Get { Unset(nearText: {concepts: ["x"]}) { title } } }`,
		`{ Get { Remote(hybrid: {query: "x"}) { title } } }`,
		`{ Aggregate { Remote(nearText: {concepts: ["x"]}) { meta { count } } } }`,
		`{ Get { Local(ask: {question: "x"}) { title } } }`,
		`{ Get { Local(nearImage: {image: "aGk="}) { title } } }`,
		`{ Get { Local(nearMedia: {media: "aGk="}) { title } } }`,
		`{ Get { Local(nearAudio: {audio: "aGk="}) { title } } }`,
		`{ Get { Local { title _additional { generate(singleResult: {prompt: "x"}) { singleResult } } } } }`,
		`{ Get { Local { title _additional { rerank(property: "title", query: "x") { score } } } } }`,
		`{ Get { Local { title _additional { answer { result } } } } }`,
		`{ Get { Local { title _additional { summary(properties: ["title"]) { result } } } } }`,
		`{ Get { Local { title _additional { spellCheck { changes } } } } }`,
		`{ Get { Local { title _additional { featureProjection { vector } } } } }`,
		`{ Get { Local { title(x: 1) } } }`,
		`{ Get { Local { author { ... on Person { _additional { generate(singleResult: {prompt: "x"}) { singleResult } } } } } } }`,
		`{ Get { Missing { title } } }`,
	}
	for _, text := range refused {
		doc, err := parseGraphQL(text)
		if err != nil {
			t.Fatalf("parseGraphQL(%s): %v", text, err)
		}
		err = guardGraphQL(doc, true, classes)
		if text == `{ Get { Missing { title } } }` {
			if !errors.Is(err, driver.ErrNotFound) {
				t.Errorf("guardGraphQL(%s) = %v, want ErrNotFound", text, err)
			}
			continue
		}
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("guardGraphQL(%s, ro) = %v, want ErrReadOnly", text, err)
		}
		// A writable connection runs them: they read, at a cost the user chose.
		if err := guardGraphQL(doc, false, classes); err != nil {
			t.Errorf("guardGraphQL(%s, rw) = %v, want allowed", text, err)
		}
	}
}

func TestIsLocalVectorizer(t *testing.T) {
	for _, v := range []string{"none", "text2vec-transformers", "text2vec-contextionary", "text2vec-model2vec", "text2vec-gpt4all", "multi2vec-clip", "img2vec-neural", "ref2vec-centroid"} {
		if !isLocalVectorizer(v) {
			t.Errorf("%s should be local", v)
		}
	}
	for _, v := range []string{"", "text2vec-openai", "text2vec-cohere", "text2vec-ollama", "text2vec-weaviate", "multi2vec-google", "text2vec-huggingface", "Text2vec-transformers", "something-new"} {
		if isLocalVectorizer(v) {
			t.Errorf("%s should count as remote", v)
		}
	}
}

// A class that declares no vectorizer at all must not let nearText or
// hybrid through on a read-only connection (fail-closed).
func TestGuardEmptyVectorizerListRefused(t *testing.T) {
	bare := schemaFromClass(&models.Class{Class: "Bare"})
	if vz := bare.vectorizerNames(); len(vz) != 0 {
		t.Fatalf("vectorizers = %v", vz)
	}
	for _, text := range []string{
		`{ Get { Bare(nearText: {concepts: ["x"]}) { a } } }`,
		`{ Get { Bare(hybrid: {query: "x"}) { a } } }`,
	} {
		doc, err := parseGraphQL(text)
		if err != nil {
			t.Fatal(err)
		}
		err = guardGraphQL(doc, true, classFunc(func(string) ([]string, error) { return bare.vectorizerNames(), nil }))
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("guardGraphQL(%s) = %v, want ErrReadOnly", text, err)
		}
	}
	if remoteVectorizer(nil) == "" {
		t.Error("remoteVectorizer(nil) must report a refusal")
	}
}

// class.Vectorizer counts even without a legacy vector index.
func TestClassVectorizerAlwaysCounted(t *testing.T) {
	s := schemaFromClass(&models.Class{Class: "C", Vectorizer: "text2vec-openai", VectorConfig: map[string]models.VectorConfig{
		"v": {Vectorizer: map[string]any{"none": map[string]any{}}, VectorIndexType: "hnsw"},
	}})
	if got := strings.Join(s.vectorizerNames(), ","); got != "text2vec-openai,none" {
		t.Errorf("vectorizers = %s", got)
	}
	if remoteVectorizer(s.vectorizerNames()) == "" {
		t.Error("text2vec-openai must count as remote")
	}
}

// A local module pointed at an inference URL may send the text anywhere:
// it counts as remote.
func TestInferenceURLCountsAsRemote(t *testing.T) {
	for _, tt := range []struct {
		name  string
		class *models.Class
	}{
		{"legacy moduleConfig", &models.Class{Class: "L", Vectorizer: "text2vec-transformers", VectorIndexType: "hnsw",
			ModuleConfig: map[string]any{"text2vec-transformers": map[string]any{"inferenceUrl": "https://example.com"}}}},
		{"legacy query url", &models.Class{Class: "L", Vectorizer: "text2vec-transformers", VectorIndexType: "hnsw",
			ModuleConfig: map[string]any{"text2vec-transformers": map[string]any{"queryInferenceUrl": "https://example.com"}}}},
		{"named vector", &models.Class{Class: "N", VectorConfig: map[string]models.VectorConfig{
			"v": {Vectorizer: map[string]any{"multi2vec-clip": map[string]any{"inferenceUrl": "http://clip.example"}}, VectorIndexType: "hnsw"},
		}}},
	} {
		vz := schemaFromClass(tt.class).vectorizerNames()
		if remoteVectorizer(vz) == "" {
			t.Errorf("%s: vectorizers %v counted as local", tt.name, vz)
		}
	}
	// Without a URL the module stays local.
	local := schemaFromClass(&models.Class{Class: "L", Vectorizer: "text2vec-transformers", VectorIndexType: "hnsw",
		ModuleConfig: map[string]any{"text2vec-transformers": map[string]any{"vectorizeClassName": false}}})
	if r := remoteVectorizer(local.vectorizerNames()); r != "" {
		t.Errorf("plain transformers counted as remote: %s", r)
	}
}
