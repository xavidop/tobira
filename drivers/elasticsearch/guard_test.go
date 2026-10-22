package elasticsearch

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// Every refusal on a read-only path is ErrReadOnly, whatever the reason:
// the guard fails closed on text it cannot read.
func TestParseRequestReadOnlyRefusals(t *testing.T) {
	for name, text := range map[string]string{
		"delete_by_query":        `{"method":"POST","path":"/tobira_people/_delete_by_query","body":{"query":{"term":{"id":1}}}}`,
		"update_by_query":        `{"method":"POST","path":"/tobira_people/_update_by_query","body":{"query":{"match_all":{}}}}`,
		"reindex":                `{"method":"POST","path":"/_reindex","body":{"source":{"index":"a"},"dest":{"index":"b"}}}`,
		"bulk":                   `{"method":"POST","path":"/_bulk","body":[{"delete":{"_index":"tobira_people","_id":"1"}}]}`,
		"index bulk":             `{"method":"POST","path":"/tobira_people/_bulk","body":[{"delete":{"_id":"1"}}]}`,
		"delete index":           `{"method":"DELETE","path":"/tobira_people"}`,
		"put doc":                `{"method":"PUT","path":"/tobira_people/_doc/1","body":{"id":1}}`,
		"post doc":               `{"method":"POST","path":"/tobira_people/_doc","body":{"id":1}}`,
		"update doc":             `{"method":"POST","path":"/tobira_people/_update/1","body":{"doc":{"name":"x"}}}`,
		"create doc":             `{"method":"PUT","path":"/tobira_people/_create/9","body":{"id":9}}`,
		"put mapping":            `{"method":"PUT","path":"/tobira_people/_mapping","body":{"properties":{"x":{"type":"keyword"}}}}`,
		"post mapping":           `{"method":"POST","path":"/tobira_people/_mapping","body":{"properties":{"x":{"type":"keyword"}}}}`,
		"get on a write path":    `{"method":"GET","path":"/tobira_people/_delete_by_query"}`,
		"head":                   `{"method":"HEAD","path":"/tobira_people"}`,
		"lower-case delete":      `{"method":"delete","path":"/tobira_people"}`,
		"unknown method":         `{"method":"PATCH","path":"/tobira_people/_search"}`,
		"dot-dot traversal":      `{"method":"POST","path":"/tobira_people/_search/../_delete_by_query","body":{"query":{"match_all":{}}}}`,
		"dot segment":            `{"method":"POST","path":"/./_bulk"}`,
		"encoded slash":          `{"method":"POST","path":"/tobira_people%2F_delete_by_query"}`,
		"double slash":           `{"method":"POST","path":"//tobira_people/_delete_by_query"}`,
		"backslash":              `{"method":"POST","path":"/tobira_people\\_delete_by_query"}`,
		"fragment":               `{"method":"GET","path":"/tobira_people/_search#x"}`,
		"space in path":          `{"method":"GET","path":"/tobira_people /_search"}`,
		"search template":        `{"method":"POST","path":"/tobira_people/_search/template","body":{"id":"t"}}`,
		"scroll delete":          `{"method":"DELETE","path":"/_search/scroll","body":{"scroll_id":"x"}}`,
		"open pit":               `{"method":"POST","path":"/tobira_people/_pit?keep_alive=1m"}`,
		"close index":            `{"method":"POST","path":"/tobira_people/_close"}`,
		"forcemerge":             `{"method":"POST","path":"/_forcemerge"}`,
		"cluster settings":       `{"method":"PUT","path":"/_cluster/settings","body":{"persistent":{}}}`,
		"ingest":                 `{"method":"PUT","path":"/_ingest/pipeline/p","body":{}}`,
		"put script":             `{"method":"POST","path":"/_scripts/s","body":{"script":{"lang":"painless","source":"1"}}}`,
		"index segment _bulk":    `{"method":"POST","path":"/_bulk/_search"}`,
		"cat via post":           `{"method":"POST","path":"/_cat/indices"}`,
		"doc via post":           `{"method":"POST","path":"/tobira_people/_doc/1"}`,
		"source param":           `{"method":"GET","path":"/tobira_people/_search?source={\"query\":{\"match_all\":{}}}&source_content_type=application/json"}`,
		"unknown param":          `{"method":"GET","path":"/tobira_people/_search?wait_for_completion=false"}`,
		"refresh param":          `{"method":"GET","path":"/tobira_people/_search?refresh=true"}`,
		"script in search form":  `{"index":"tobira_people","query":{"script":{"script":{"source":"doc['id'].value > 1"}}}}`,
		"script_fields":          `{"index":"tobira_people","script_fields":{"x":{"script":"1"}}}`,
		"script_score":           `{"index":"tobira_people","query":{"script_score":{"query":{"match_all":{}},"script":{"source":"1"}}}}`,
		"runtime mapping script": `{"index":"tobira_people","runtime_mappings":{"x":{"type":"keyword","script":{"source":"emit('a')"}}}}`,
		"scripted_metric":        `{"index":"tobira_people","size":0,"aggs":{"m":{"scripted_metric":{"map_script":"state.x=1"}}}}`,
		"Script in other case":   `{"index":"tobira_people","query":{"SCRIPT":{"source":"1"}}}`,
		"script in raw body":     `{"method":"POST","path":"/tobira_people/_search","body":{"query":{"script":{"script":"1"}}}}`,
		"script in msearch":      `{"method":"POST","path":"/_msearch","body":[{"index":"tobira_people"},{"query":{"script":{"script":"1"}}}]}`,
		"duplicate method":       `{"method":"GET","path":"/tobira_people/_search","method":"DELETE"}`,
		"duplicate path":         `{"method":"POST","path":"/tobira_people/_search","path":"/tobira_people/_delete_by_query"}`,
		"duplicate nested key":   `{"index":"tobira_people","query":{"term":{"id":1},"term":{"id":2}}}`,
		"duplicate index":        `{"index":"tobira_people","index":"other"}`,
		"trailing data":          `{"index":"tobira_people"} {"method":"DELETE","path":"/tobira_people"}`,
		"not an object":          `["DELETE","/tobira_people"]`,
		"malformed":              `{"index":`,
		"empty":                  ``,
		"bad index name":         `{"index":"tobira_people/_delete_by_query"}`,
		"index with query":       `{"index":"tobira_people?x=1"}`,
		"index with space":       `{"index":"tobira people"}`,
		"no index":               `{"query":{"match_all":{}}}`,
		"extra raw key":          `{"method":"GET","path":"/_search","index":"x"}`,
		"string body":            `{"method":"POST","path":"/_search","body":"{\"query\":{}}"}`,
		"relative path":          `{"method":"GET","path":"_search"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRequest(text, true)
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Fatalf("parseRequest(%s, readOnly): want ErrReadOnly, got %v", text, err)
			}
		})
	}
}

func TestParseRequestReadOnlyAllowed(t *testing.T) {
	for _, text := range []string{
		`{"index":"tobira_people","query":{"match_all":{}},"sort":[{"id":"asc"}]}`,
		`{"index":"logs-*,-logs-old","size":0,"aggs":{"n":{"terms":{"field":"tags"}}}}`,
		`{"index":"_all"}`,
		`{"index":"remote:idx"}`,
		`{"method":"GET","path":"/"}`,
		`{"method":"GET","path":"/_search"}`,
		`{"method":"POST","path":"/tobira_people/_search?size=5&filter_path=hits.hits._id","body":{"query":{"match_all":{}}}}`,
		`{"method":"post","path":"/tobira_people/_count","body":{"query":{"match_all":{}}}}`,
		`{"method":"GET","path":"/tobira_people/_mapping"}`,
		`{"method":"GET","path":"/_mapping"}`,
		`{"method":"GET","path":"/tobira_people/_mapping/field/name"}`,
		`{"method":"GET","path":"/_cat/indices?format=json&h=index,docs.count&v=true"}`,
		`{"method":"GET","path":"/_cat/indices/tobira_*?format=json"}`,
		`{"method":"POST","path":"/tobira_people/_validate/query?explain=true","body":{"query":{"match_all":{}}}}`,
		`{"method":"POST","path":"/_msearch","body":[{"index":"tobira_people"},{"query":{"match_all":{}}}]}`,
		`{"method":"GET","path":"/tobira_people/_field_caps?fields=*"}`,
		`{"method":"GET","path":"/tobira_people/_doc/1"}`,
		`{"method":"GET","path":"/tobira_people/_source/1"}`,
		`{"method":"POST","path":"/_mget","body":{"docs":[{"_index":"tobira_people","_id":"1"}]}}`,
		`{"method":"GET","path":"/_cluster/health"}`,
		`{"method":"GET","path":"/_cluster/health/tobira_people"}`,
	} {
		if _, err := parseRequest(text, true); err != nil {
			t.Errorf("parseRequest(%s, readOnly): %v", text, err)
		}
	}
}

// On a writable path the raw form may write, but malformed text is still
// ErrSyntax and the path rules still apply.
func TestParseRequestWritable(t *testing.T) {
	for _, text := range []string{
		`{"method":"POST","path":"/tobira_people/_delete_by_query","body":{"query":{"term":{"id":1}}}}`,
		`{"method":"DELETE","path":"/tobira_people/_doc/1"}`,
		`{"method":"POST","path":"/_bulk","body":[{"delete":{"_index":"x","_id":"1"}}]}`,
		`{"index":"tobira_people","script_fields":{"x":{"script":"1"}}}`,
	} {
		if _, err := parseRequest(text, false); err != nil {
			t.Errorf("parseRequest(%s, writable): %v", text, err)
		}
	}
	for _, text := range []string{
		`{"index":`,
		`{"method":"GET","path":"/a/../b"}`,
		`{"method":"GET","path":"/a%2Fb"}`,
		`{"index":"a","index":"b"}`,
		`{"method":"PATCH","path":"/a"}`,
	} {
		_, err := parseRequest(text, false)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseRequest(%s, writable): want ErrSyntax, got %v", text, err)
		}
	}
}

func TestParseRequestForms(t *testing.T) {
	r, err := parseRequest(`{"index":"tobira_people","size":5,"query":{"match_all":{}}}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.raw || r.index != "tobira_people" {
		t.Fatalf("search form parsed as %+v", r)
	}
	if _, ok := r.body["index"]; ok {
		t.Error(`the search body still holds "index"`)
	}
	if string(r.body["size"]) != "5" {
		t.Errorf(`size = %s, want 5`, r.body["size"])
	}
	r, err = parseRequest(`{"method":"post","path":"/_msearch?typed_keys=true","body":[{"index":"a"},{"query":{"match_all":{}}}]}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.raw || r.method != "POST" || r.path != "/_msearch" || r.params.Get("typed_keys") != "true" {
		t.Fatalf("raw form parsed as %+v", r)
	}
	if !r.ndjson || string(r.rawBody) != "{\"index\":\"a\"}\n{\"query\":{\"match_all\":{}}}\n" {
		t.Errorf("msearch body = %q (ndjson %v)", r.rawBody, r.ndjson)
	}
}

func TestScanJSON(t *testing.T) {
	if key, err := scanJSON([]byte(`{"a":[{"b":1},{"b":2}],"c":{"d":{"Script_Score":1}}}`)); err != nil || !strings.EqualFold(key, "script_score") {
		t.Errorf("scanJSON: key %q, err %v", key, err)
	}
	if _, err := scanJSON([]byte(`{"a":1,"b":{"a":1}}`)); err != nil {
		t.Errorf("the same key in different objects is not a duplicate: %v", err)
	}
	if _, err := scanJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Error("a duplicate key passed")
	}
	if _, err := scanJSON([]byte(`[{"x":1,"x":1}]`)); err == nil {
		t.Error("a duplicate key inside an array passed")
	}
	if key, err := scanJSON([]byte(`{"q":"script"}`)); err != nil || key != "" {
		t.Errorf(`a string value "script" is not a key: key %q, err %v`, key, err)
	}
}

// Inference (egress and cost) is refused on a read-only path.
func TestReadOnlyRefusesInference(t *testing.T) {
	for name, text := range map[string]string{
		"query_vector_builder":    `{"index":"x","knn":{"field":"v","k":3,"num_candidates":10,"query_vector_builder":{"text_embedding":{"model_id":"m","model_text":"hi"}}}}`,
		"model_id":                `{"index":"x","query":{"text_expansion":{"ml.tokens":{"model_id":".elser_model_2","model_text":"hi"}}}}`,
		"semantic":                `{"index":"x","query":{"semantic":{"field":"body","query":"hi"}}}`,
		"sparse_vector inference": `{"index":"x","query":{"sparse_vector":{"field":"ml.tokens","inference_id":"my-elser","query":"hi"}}}`,
		"text_expansion":          `{"index":"x","query":{"text_expansion":{"ml.tokens":{"tokens":{"a":1}}}}}`,
		"reranker":                `{"index":"x","retriever":{"text_similarity_reranker":{"retriever":{"standard":{"query":{"match_all":{}}}},"field":"body","inference_id":"r","inference_text":"q"}}}`,
		"raw body":                `{"method":"POST","path":"/x/_search","body":{"query":{"semantic":{"field":"b","query":"q"}}}}`,
		"msearch line":            `{"method":"POST","path":"/_msearch","body":[{"index":"x"},{"query":{"semantic":{"field":"b","query":"q"}}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRequest(text, true)
			if !errors.Is(err, driver.ErrReadOnly) || !strings.Contains(err.Error(), "inference") {
				t.Fatalf("want an ErrReadOnly about inference, got %v", err)
			}
			if _, err := parseRequest(text, false); err != nil {
				t.Errorf("writable path: %v", err)
			}
		})
	}
	// sparse_vector with an explicit query vector runs no model.
	if _, err := parseRequest(`{"index":"x","query":{"sparse_vector":{"field":"t","query_vector":{"a":1}}}}`, true); err != nil {
		t.Errorf("sparse_vector without inference: %v", err)
	}
	if _, err := buildQueryAs(Name, "browse", driver.Filter{Raw: `{"semantic":{"field":"b","query":"q"}}`}, testMapping(t), true, false); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("a read-only raw filter with inference: want ErrReadOnly, got %v", err)
	}
}

// System and hidden indexes are refused on a read-only path unless
// allow_system_indices is set.
func TestReadOnlyRefusesSystemIndexes(t *testing.T) {
	for name, text := range map[string]string{
		"dot target":          `{"index":".security"}`,
		"dot pattern":         `{"index":".security*"}`,
		"dot in a list":       `{"index":"logs,.kibana"}`,
		"raw dot path":        `{"method":"GET","path":"/.security*/_search"}`,
		"raw dot doc":         `{"method":"GET","path":"/.security/_doc/1"}`,
		"cat dot":             `{"method":"GET","path":"/_cat/indices/.security*?format=json"}`,
		"cluster health dot":  `{"method":"GET","path":"/_cluster/health/.security"}`,
		"expand all param":    `{"method":"GET","path":"/_search?expand_wildcards=all"}`,
		"expand hidden param": `{"method":"GET","path":"/x*/_search?expand_wildcards=open,hidden"}`,
		"expand all in body":  `{"method":"POST","path":"/_msearch","body":[{"index":"x*","expand_wildcards":"all"},{"query":{"match_all":{}}}]}`,
		"msearch header":      `{"method":"POST","path":"/_msearch","body":[{"index":".security"},{"query":{"match_all":{}}}]}`,
		"msearch header list": `{"method":"POST","path":"/_msearch","body":[{"index":["a",".security"]},{"query":{"match_all":{}}}]}`,
		"mget _index":         `{"method":"POST","path":"/_mget","body":{"docs":[{"_index":".security","_id":"1"}]}}`,
		"terms lookup":        `{"index":"x","query":{"terms":{"f":{"index":".security","id":"1","path":"p"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRequest(text, true)
			if !errors.Is(err, driver.ErrReadOnly) || !strings.Contains(err.Error(), "allow_system_indices") {
				t.Fatalf("want an ErrReadOnly naming allow_system_indices, got %v", err)
			}
			if _, err := parseRequestAs(Name, text, true, true); err != nil {
				t.Errorf("with allow_system_indices: %v", err)
			}
			if _, err := parseRequest(text, false); err != nil {
				t.Errorf("writable path: %v", err)
			}
		})
	}
	for _, text := range []string{
		`{"index":"logs-*,-.old"}`, // an exclusion only narrows
		`{"method":"GET","path":"/_search?expand_wildcards=open"}`,
		`{"index":"x","query":{"term":{"name":".hidden-looking value"}}}`,
	} {
		if _, err := parseRequest(text, true); err != nil {
			t.Errorf("parseRequest(%s, readOnly): %v", text, err)
		}
	}
}

// A read-only refusal says so once.
func TestReadOnlyMessageNotRepeated(t *testing.T) {
	_, err := parseRequest(`{"method":"DELETE","path":"/x"}`, true)
	if n := strings.Count(err.Error(), "read-only"); n != 1 {
		t.Errorf("%q mentions read-only %d times", err.Error(), n)
	}
}
