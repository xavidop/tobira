package milvus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseRequestOps(t *testing.T) {
	for _, tc := range []struct {
		text, op string
	}{
		{`{"collection":"c"}`, opQuery},
		{`{"op":"query","collection":"c","filter":"id > 1","limit":5,"offset":2,"outputFields":["id"],"partitions":["p"],"consistency":"bounded"}`, opQuery},
		{`{"collection":"c","ids":[1,2]}`, opGet},
		{`{"op":"get","collection":"c","ids":["a","b"]}`, opGet},
		{`{"op":"count","collection":"c","filter":"age > {a}","params":{"a":3}}`, opCount},
		{`{"collection":"c","vector":[1,0.5]}`, opSearch},
		{`{"collection":"c","text":"hello","annsField":"sparse"}`, opSearch},
		{`{"op":"search","collection":"c","vector":[1,0],"limit":3,"searchParams":{"ef":64,"radius":0.5},"rerank":{"inputFields":["ts"],"params":{"reranker":"decay","function":"gauss","origin":0,"scale":10}}}`, opSearch},
		{`{"collection":"c","database":"db2"}`, opQuery},
	} {
		for _, ro := range []bool{true, false} {
			r, err := parseRequest(tc.text, ro)
			if err != nil {
				t.Errorf("parseRequest(%s, ro=%v): %v", tc.text, ro, err)
				continue
			}
			if r.Op != tc.op {
				t.Errorf("parseRequest(%s): op %q, want %q", tc.text, r.Op, tc.op)
			}
		}
	}
}

// Every write, state change or unknown op is refused before anything is
// sent: ErrReadOnly on a read-only request, ErrUnsupported otherwise.
func TestParseRequestRefusesWrites(t *testing.T) {
	for _, op := range []string{
		"insert", "upsert", "delete", "Delete", "QUERY", "drop_collection", "create_collection",
		"load", "load_collection", "release", "release_collection", "flush", "compact",
		"create_index", "drop_index", "rename_collection", "truncate", "alter_collection",
		"hybrid_search", "run_analyzer", "embed", "rerank", "create_database", "drop_database",
		"grant", "create_user", "import", "backup",
	} {
		text := `{"op":"` + op + `","collection":"tobira_people","filter":"id == 1"}`
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("op %q read-only: %v, want ErrReadOnly", op, err)
		}
		_, err = parseRequest(text, false)
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("op %q writable: %v, want ErrUnsupported", op, err)
		}
	}
}

func TestParseRequestMalformed(t *testing.T) {
	for _, text := range []string{
		``,
		`[]`,
		`"query"`,
		`{"collection":"c"} {"op":"delete"}`,
		`{"collection":"c"} trailing`,
		`{"op":"query","op":"delete","collection":"c"}`,
		`{"op":"delete","op":"query","collection":"c"}`,
		`{"collection":"c","collection":"d"}`,
		`{"op":1,"collection":"c"}`,
		`{"op":null,"collection":"c"}`,
		`{"collection":"c","drop":true}`,
		`{"op":"query","collection":"c","vector":[1]}`,
		`{"op":"count","collection":"c","outputFields":["id"]}`,
		`{"op":"get","collection":"c","filter":"id > 1"}`,
		`{"op":"get","collection":"c"}`,
		`{"op":"search","collection":"c"}`,
		`{"collection":"c","vector":[1],"text":"x"}`,
		`{"collection":"c","vector":[]}`,
		`{"collection":"c","vector":["a"]}`,
		`{"collection":"c","ids":[1,"a"]}`,
		`{"collection":"c","ids":[{"a":1}]}`,
		`{"collection":"c","limit":-1}`,
		`{"collection":"c","limit":1.5}`,
		`{"collection":"c","offset":-3}`,
		`{"collection":"c","consistency":"psychic"}`,
		`{"collection":"c","params":{"a":{"b":1}}}`,
		`{"collection":"c","params":{"a":null}}`,
		`{"collection":"c","params":{"a":[1,"x"]}}`,
		`{"collection":"c","params":{"tobira_f0":1}}`,
		`{"collection":"c","params":{"bad name":1}}`,
		`{"collection":"c","filter":"id > 1)"}`,
		`{"collection":"c","filter":7}`,
		`{"collection":"c","outputFields":"id"}`,
		`{"collection":""}`,
		`{"filter":"id > 1"}`,
		`{"collection":"c","vector":[1],"rerank":{"params":{}}}`,
		`{"collection":"c","vector":[1],"rerank":{"params":{"reranker":"decay"},"extra":1}}`,
		`{"collection":"c","vector":[1],"rerank":{"params":{"reranker":"model"},"params":{"reranker":"decay"}}}`,
		`{"collection":"c","vector":[1],"rerank":{"inputFields":["a"],"inputFields":["b"],"params":{"reranker":"decay"}}}`,
		`{"collection":"c","vector":[1],"rerank":[1]}`,
		`{"collection":"c","vector":[1],"searchParams":[1]}`,
	} {
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("parseRequest(%q, ro) = %v, want ErrReadOnly", text, err)
		}
		_, err = parseRequest(text, false)
		if !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("parseRequest(%q, rw) = %v, want ErrSyntax or ErrUnsupported", text, err)
		}
	}
}

// Model rerankers call an outside provider: egress, refused read-only.
// Local rerankers (decay, boost) run in Milvus and are allowed.
func TestParseRequestRerankEgress(t *testing.T) {
	for _, r := range []string{"model", "Model", "cohere", "tei", "vllm", "voyageai", "siliconflow", "mystery"} {
		text := `{"collection":"c","vector":[1,0],"rerank":{"inputFields":["doc"],"params":{"reranker":"` + r + `","provider":"cohere","queries":["q"]}}}`
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("reranker %q read-only: %v, want ErrReadOnly", r, err)
		}
		if _, err := parseRequest(text, false); err != nil {
			t.Errorf("reranker %q writable: %v, want it allowed", r, err)
		}
	}
	for _, r := range []string{"decay", "boost"} {
		text := `{"collection":"c","vector":[1,0],"rerank":{"inputFields":["ts"],"params":{"reranker":"` + r + `"}}}`
		if _, err := parseRequest(text, true); err != nil {
			t.Errorf("reranker %q read-only: %v, want it allowed", r, err)
		}
	}
}

func TestParseRequestParams(t *testing.T) {
	r, err := parseRequest(`{"collection":"c","filter":"a > {i} and b == {s} and c in {arr} and d == {f} and e == {b}","params":{"i":3,"s":"x","arr":[1,2],"f":1.5,"b":true}}`, true)
	if err != nil {
		t.Fatal(err)
	}
	p := r.params
	if p["i"].GetInt64Val() != 3 || p["s"].GetStringVal() != "x" || p["f"].GetFloatVal() != 1.5 || !p["b"].GetBoolVal() {
		t.Errorf("params = %v", p)
	}
	if got := p["arr"].GetArrayVal().GetLongData().GetData(); len(got) != 2 {
		t.Errorf("arr = %v", p["arr"])
	}
}

func TestQueryPositionalParams(t *testing.T) {
	r, err := parseRequest(`{"collection":"c","filter":"id == {p1}"}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.addPositional([]driver.Value{driver.Int(7)}); err != nil {
		t.Fatal(err)
	}
	if r.params["p1"].GetInt64Val() != 7 {
		t.Errorf("p1 = %v", r.params["p1"])
	}
	r, _ = parseRequest(`{"collection":"c","filter":"id == {p1}","params":{"p1":1}}`, false)
	if err := r.addPositional([]driver.Value{driver.Int(7)}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("a named param shadowing p1: %v, want ErrSyntax", err)
	}
}

// Query refuses before the network: the fake server sees no call.
func TestQueryRefusesBeforeNetwork(t *testing.T) {
	c, fake := newFakeConn(t, true)
	for _, text := range []string{
		`{"op":"delete","collection":"tobira_people","filter":"id == 1"}`,
		`{"op":"load","collection":"tobira_people"}`,
		`{"collection":"tobira_people","filter":"mystery(id)"}`,
		`{"collection":"c","vector":[1,0],"rerank":{"params":{"reranker":"model"}}}`,
	} {
		_, err := c.Query(context.Background(), driver.Query{Text: text})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query(%s) = %v, want ErrReadOnly", text, err)
		}
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("refused queries reached the server: %v", calls)
	}
	// Query.ReadOnly on a writable conn refuses the same way.
	w, wfake := newFakeConn(t, false)
	_, err := w.Query(context.Background(), driver.Query{Text: `{"op":"delete","collection":"x"}`, ReadOnly: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Query.ReadOnly delete = %v, want ErrReadOnly", err)
	}
	if len(wfake.Calls()) != 0 {
		t.Error("a refused query reached the server")
	}
}

func TestQueryClosedConn(t *testing.T) {
	c, _ := newFakeConn(t, false)
	_ = c.Close()
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"c"}`})
	if !errors.Is(err, driver.ErrConnection) || !strings.Contains(err.Error(), "closed") {
		t.Errorf("Query on a closed conn = %v", err)
	}
	if c.Close() != nil {
		t.Error("Close must be idempotent")
	}
}

// A read-only rerank takes only the decay/boost keys, each
// spelt once (case-insensitively), and "reranker" spelt exactly.
func TestRerankParamKeys(t *testing.T) {
	req := func(params string) string {
		return `{"collection":"c","vector":[1,0],"rerank":{"inputFields":["ts"],"params":` + params + `}}`
	}
	for _, p := range []string{
		`{"reranker":"decay","Reranker":"model","RERANKER":"model","provider":"cohere"}`,
		`{"reranker":"decay","reranker":"model"}`,
		`{"reranker":"model","reranker":"decay"}`,
		`{"reranker":"decay","provider":"cohere"}`,
		`{"reranker":"decay","endpoint":"http://x"}`,
		`{"reranker":"decay","scale":1,"Scale":2}`,
		`{"reranker":"boost","filter":"mystery(x)"}`,
		`{"Reranker":"decay"}`,
	} {
		if _, err := parseRequest(req(p), true); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("rerank %s (ro) = %v, want ErrReadOnly", p, err)
		}
	}
	// Ambiguous spellings are refused on a writable request too.
	for _, p := range []string{
		`{"reranker":"decay","Reranker":"model"}`,
		`{"reranker":"decay","reranker":"model"}`,
		`{"reranker":"decay","scale":1,"SCALE":2}`,
	} {
		if _, err := parseRequest(req(p), false); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("rerank %s (rw) = %v, want ErrSyntax", p, err)
		}
	}
	for _, p := range []string{
		`{"reranker":"decay","function":"gauss","origin":0,"scale":10,"offset":1,"decay":0.5}`,
		`{"reranker":"decay","Function":"exp","ORIGIN":0,"scale":10}`,
		`{"reranker":"boost","filter":"age > 3","weight":2}`,
		`{"reranker":"boost","random_score":{"seed":1,"field":"id"}}`,
	} {
		if _, err := parseRequest(req(p), true); err != nil {
			t.Errorf("rerank %s (ro) = %v, want allowed", p, err)
		}
	}
}

// The rerank params go out in a fixed order.
func TestRerankParamsOrder(t *testing.T) {
	r, err := parseRequest(`{"collection":"c","vector":[1,0],"rerank":{"inputFields":["ts"],"params":{"scale":10,"reranker":"decay","origin":0,"function":"gauss"}}}`, true)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		fn := rerankFunction(r.Rerank)
		var keys []string
		for _, kv := range fn.GetParams() {
			keys = append(keys, kv.GetKey())
		}
		if strings.Join(keys, ",") != "function,origin,reranker,scale" {
			t.Fatalf("params order = %v", keys)
		}
	}
}
