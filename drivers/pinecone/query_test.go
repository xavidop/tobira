package pinecone

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseRequest(t *testing.T) {
	r, err := parseRequest(`{"index":"docs","namespace":"ns1","vector":[1,2.5],"topK":3,"filter":{"genre":{"$eq":"x"}},"includeValues":true}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.op != opQuery || r.index != "docs" || r.namespace != "ns1" || !slices.Equal(r.vector, []float32{1, 2.5}) ||
		r.topK != 3 || r.filter == nil || !r.includeMetadata || !r.includeValues {
		t.Errorf("query = %+v", r)
	}
	r, err = parseRequest(`{"op":"query","id":"7","includeMetadata":false}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.id != "7" || r.topK != defaultK || r.includeMetadata || r.includeValues {
		t.Errorf("query by id = %+v", r)
	}
	r, err = parseRequest(`{"op":"query","sparseVector":{"indices":[1,9],"values":[0.5,0.25]},"topK":5000}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.sparse == nil || !slices.Equal(r.sparse.Indices, []uint32{1, 9}) || r.topK != maxK || len(r.warnings) != 1 {
		t.Errorf("sparse query = %+v", r)
	}
	r, err = parseRequest(`{"op":"fetch","namespace":"__default__","ids":["a","b"]}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.op != opFetch || r.namespace != "" || !slices.Equal(r.ids, []string{"a", "b"}) || !r.includeMetadata {
		t.Errorf("fetch = %+v", r)
	}
	r, err = parseRequest(`{"op":"list","namespace":"people","prefix":"doc#","limit":25,"includeValues":true}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.op != opList || r.prefix != "doc#" || r.limit != 25 || !r.includeValues {
		t.Errorf("list = %+v", r)
	}
	r, err = parseRequest(` {"op":"stats","index":"docs"} `, true)
	if err != nil || r.op != opStats || r.index != "docs" {
		t.Errorf("stats = %+v, %v", r, err)
	}
}

// Every write, and every text the parser cannot read, is refused before a
// byte is sent: ErrReadOnly on a read-only path, otherwise ErrUnsupported
// (a write op) or ErrSyntax (a malformed text).
func TestParseRequestRefusals(t *testing.T) {
	writes := []string{
		`{"op":"upsert","namespace":"people","vectors":[{"id":"1","values":[1,2,3,4]}]}`,
		`{"op":"update","namespace":"people","id":"1","setMetadata":{"name":"x"}}`,
		`{"op":"delete","namespace":"people","ids":["1"]}`,
		`{"op":"delete","namespace":"people","deleteAll":true}`,
		`{"op":"Delete","namespace":"people","ids":["1"]}`,
		`{"op":"QUERY","vector":[1]}`,
		`{"op":"deleteNamespace","namespace":"people"}`,
		`{"op":"create_index","name":"x"}`,
		`{"op":"configure_index","name":"x"}`,
		`{"op":"start_import","uri":"s3://x"}`,
		`{"op":"upsert_records","records":[]}`,
		`{"op":"upsertRecords","records":[]}`,
		// Integrated inference (server-side embedding, reranking): egress
		// and cost, never run from a query.
		`{"op":"search","namespace":"people","query":{"inputs":{"text":"x"},"top_k":3}}`,
		`{"op":"searchRecords","namespace":"people","query":{"inputs":{"text":"x"},"top_k":3}}`,
		`{"op":"search_records","namespace":"people","query":{"inputs":{"text":"x"},"top_k":3}}`,
		`{"op":"rerank","model":"bge-reranker-v2-m3","query":"x","documents":["a"]}`,
		`{"op":"embed","model":"multilingual-e5-large","inputs":[{"text":"x"}]}`,
	}
	for _, w := range writes {
		if _, err := parseRequest(w, true); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %s: %v, want ErrReadOnly", w, err)
		}
		if _, err := parseRequest(w, false); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", w, err)
		}
	}
	malformed := []string{
		``,
		`[]`,
		`"query"`,
		`{"op":"list"} {"op":"delete","ids":["1"]}`,
		`{"op":"list"}x`,
		`{"op":"list","op":"delete"}`,
		`{"op":"delete","op":"list"}`,
		`{"op":"list","namespace":"a","namespace":"b"}`,
		`{"op":1}`,
		`{"op":"list","deleteAll":true}`,
		`{"op":"list","ids":["1"]}`,
		`{"op":"query","vector":[1],"deleteAll":true}`,
		`{"op":"query","vector":[1],"setMetadata":{"a":"b"}}`,
		`{"op":"query","vector":[1],"vectors":[{"id":"1"}]}`,
		`{"op":"query"}`,
		`{"op":"query","vector":[1],"id":"x"}`,
		`{"op":"query","vector":[]}`,
		`{"op":"query","vector":["a"]}`,
		`{"op":"query","vector":[1],"topK":-1}`,
		`{"op":"query","vector":[1],"topK":1.5}`,
		`{"op":"query","vector":[1],"filter":[1]}`,
		`{"op":"query","vector":[1],"filter":"x"}`,
		`{"op":"query","sparseVector":{"indices":[1],"values":[]}}`,
		`{"op":"query","sparseVector":{"indices":[1],"values":[1],"x":1}}`,
		`{"op":"fetch"}`,
		`{"op":"fetch","ids":[]}`,
		`{"op":"fetch","ids":[""]}`,
		`{"op":"fetch","ids":[1]}`,
		`{"op":"fetch","ids":["a"],"vector":[1]}`,
		`{"op":"list","limit":-3}`,
		`{"op":"list","prefix":7}`,
		`{"op":"stats","namespace":"x"}`,
		`{"index":7,"vector":[1]}`,
		`{"namespace":null,"vector":[1]}`,
		`{"op":"query","vector":[1],"includeValues":"yes"}`,
	}
	for _, m := range malformed {
		if _, err := parseRequest(m, true); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %q: %v, want ErrReadOnly (fail closed)", m, err)
		}
		_, err := parseRequest(m, false)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%q: %v, want ErrSyntax", m, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("%q: not a pinecone driver.Error: %#v", m, err)
		}
	}
	// fetch is capped at maxK ids.
	ids := make([]string, maxK+1)
	for i := range ids {
		ids[i] = `"x"`
	}
	if _, err := parseRequest(`{"op":"fetch","ids":[`+strings.Join(ids, ",")+`]}`, false); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("fetch over the cap: %v", err)
	}
}

// Query refuses parameters and checks the request before contacting the
// server: the test conn points at a closed port, so any other error proves
// the decision came first.
func TestQueryRefusesBeforeNetwork(t *testing.T) {
	c := newTestConn(t, false)
	ctx := context.Background()
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"delete","ids":["1"]}`}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("delete: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"delete","ids":["1"]}`, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("delete with Query.ReadOnly: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"list"}`, Params: []driver.Value{driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("params: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"list","index":"other"}`}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown index in host mode: %v", err)
	}
	ro := newTestConn(t, true)
	if _, err := ro.Query(ctx, driver.Query{Text: `{"op":"upsert","vectors":[]}`}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only conn: %v", err)
	}
}

// topK 0 means the default; the message says so.
func TestTopKMessage(t *testing.T) {
	_, err := parseRequest(`{"vector":[1],"topK":-1}`, false)
	if err == nil || !strings.Contains(err.Error(), "0 means the default") || strings.Contains(err.Error(), "1 or more") {
		t.Errorf("topK -1: %v", err)
	}
	r, err := parseRequest(`{"vector":[1],"topK":0}`, false)
	if err != nil || r.topK != defaultK {
		t.Errorf("topK 0: %+v %v", r, err)
	}
}
