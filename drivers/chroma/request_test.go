package chroma

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseRequestModes(t *testing.T) {
	for _, tt := range []struct {
		text string
		op   string
	}{
		{`{"collection":"c"}`, opGet},
		{`{"op":"get","collection":"c","ids":["1","2"],"limit":5,"offset":2,"include":["embeddings"]}`, opGet},
		{`{"collection":"c","where":{"age":{"$gt":3}},"whereDocument":{"$contains":"x"}}`, opGet},
		{`{"collection":"c","where_document":{"$contains":"x"}}`, opGet},
		{`{"collection":"c","queryEmbeddings":[[1,0]],"nResults":3}`, opQuery},
		{`{"collection":"c","query_embeddings":[[1,0]],"n_results":3,"include":["distances"]}`, opQuery},
		{`{"op":"query","collection":"c","queryTexts":["hello"]}`, opQuery},
		{`{"collection":"c","query_texts":["hello","bye"],"where":{"a":1}}`, opQuery},
		{`{"op":"count","collection":"c"}`, opCount},
	} {
		for _, ro := range []bool{false, true} {
			req, err := parseRequest(tt.text, ro)
			if err != nil {
				t.Fatalf("parseRequest(%s, ro=%v): %v", tt.text, ro, err)
			}
			if req.op != tt.op || req.collection != "c" {
				t.Errorf("parseRequest(%s) = op %q collection %q, want %q c", tt.text, req.op, req.collection, tt.op)
			}
		}
	}
}

func TestParseRequestFields(t *testing.T) {
	req, err := parseRequest(`{"collection":"c","ids":["a","b"],"limit":5,"offset":2,"include":["embeddings","documents"],"where":{"n":1}}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.ids, []string{"a", "b"}) || req.limit == nil || *req.limit != 5 || req.offset != 2 ||
		!slices.Equal(req.include, []string{"embeddings", "documents"}) || string(req.where) != `{"n":1}` {
		t.Errorf("req = %+v", req)
	}
	req, err = parseRequest(`{"collection":"c","queryEmbeddings":[[1,0.5],[0,1]],"nResults":4}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.queryEmbeddings) != 2 || req.queryEmbeddings[0][1] != 0.5 || req.nResults != 4 {
		t.Errorf("req = %+v", req)
	}
	req, err = parseRequest(`{"collection":"c","queryTexts":["x"]}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.queryTexts, []string{"x"}) || req.nResults != defaultK {
		t.Errorf("req = %+v", req)
	}
	// An oversized nResults is cut, with a warning.
	req, err = parseRequest(`{"collection":"c","queryEmbeddings":[[1]],"nResults":999999}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if req.nResults != maxK || len(req.warnings) != 1 {
		t.Errorf("nResults = %d, warnings %q", req.nResults, req.warnings)
	}
}

// writeOps are Chroma's record, collection, database, tenant and function
// writes under the names a user is likely to try, plus the search API,
// which can embed text on the server.
var writeOps = []string{
	"add", "upsert", "update", "delete", "modify", "create_collection", "createCollection",
	"delete_collection", "get_or_create_collection", "fork", "reset", "create_database",
	"delete_database", "create_tenant", "update_tenant", "attach_function", "detach_function",
	"search", "Search", "ADD", "Delete",
}

func TestParseRequestRefusesWrites(t *testing.T) {
	for _, op := range writeOps {
		text := `{"op":"` + op + `","collection":"c","ids":["1"],"embeddings":[[1]]}`
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %s: want ErrReadOnly, got %v", op, err)
		}
		_, err = parseRequest(text, false)
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("writable %s: want ErrUnsupported (Query never writes), got %v", op, err)
		}
	}
}

func TestParseRequestRefusals(t *testing.T) {
	for _, text := range []string{
		``,
		`[]`,
		`"get"`,
		`{"collection":"c"} {"op":"delete"}`,
		`{"collection":"c"}x`,
		`{"collection":"c","collection":"d"}`,
		`{"op":"get","op":"delete","collection":"c"}`,
		`{"op":"delete","op":"get","collection":"c"}`,
		`{"collection":"c","whereDocument":{},"where_document":{}}`,
		`{"collection":"c","queryEmbeddings":[[1]],"query_embeddings":[[1]]}`,
		`{"collection":"c","embeddings":[[1]]}`,
		`{"collection":"c","documents":["x"]}`,
		`{"collection":"c","metadatas":[{}]}`,
		`{"collection":"c","bogus":1}`,
		`{}`,
		`{"collection":""}`,
		`{"collection":7}`,
		`{"op":7,"collection":"c"}`,
		`{"collection":"c","ids":"1"}`,
		`{"collection":"c","ids":[1]}`,
		`{"collection":"c","ids":[""]}`,
		`{"collection":"c","limit":-1}`,
		`{"collection":"c","limit":1.5}`,
		`{"collection":"c","offset":"2"}`,
		`{"collection":"c","where":[]}`,
		`{"collection":"c","where":"x"}`,
		`{"collection":"c","whereDocument":1}`,
		`{"collection":"c","include":["secrets"]}`,
		`{"collection":"c","include":["distances"]}`,
		`{"collection":"c","include":"documents"}`,
		`{"collection":"c","nResults":3}`,
		`{"collection":"c","queryEmbeddings":[[1]],"limit":3}`,
		`{"collection":"c","queryEmbeddings":[[1]],"offset":3}`,
		`{"collection":"c","queryEmbeddings":[[1]],"queryTexts":["x"]}`,
		`{"collection":"c","queryEmbeddings":[]}`,
		`{"collection":"c","queryEmbeddings":[[]]}`,
		`{"collection":"c","queryEmbeddings":[["a"]]}`,
		`{"collection":"c","queryEmbeddings":[[1e999]]}`,
		`{"collection":"c","queryTexts":[]}`,
		`{"collection":"c","queryTexts":[1]}`,
		`{"collection":"c","queryEmbeddings":[[1]],"nResults":-2}`,
		`{"op":"query","collection":"c"}`,
		`{"op":"count","collection":"c","where":{"a":1}}`,
		`{"op":"get","collection":"c","queryTexts":["x"]}`,
	} {
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %s: want ErrReadOnly (fail closed), got %v", text, err)
		}
		_, err = parseRequest(text, false)
		if !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("writable %s: want ErrSyntax or ErrUnsupported, got %v", text, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("%s: want a *driver.Error from %s, got %T %v", text, Name, err, err)
		}
	}
}

func TestParseRequestMessageNamesOps(t *testing.T) {
	_, err := parseRequest(`{"op":"upsert","collection":"c"}`, false)
	if err == nil || !strings.Contains(err.Error(), "get, query and count") {
		t.Errorf("refusal should name the allowed ops: %v", err)
	}
}
