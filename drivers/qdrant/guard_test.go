package qdrant

import (
	"context"
	"errors"
	"strings"
	"testing"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

// mutationOps are every write operation of Qdrant's points, collections,
// aliases, snapshots, shard and cluster APIs, under the names a user is
// likely to try (gRPC method and REST path spellings).
var mutationOps = []string{
	"upsert", "delete", "update_vectors", "delete_vectors", "set_payload",
	"overwrite_payload", "delete_payload", "clear_payload", "create_field_index",
	"delete_field_index", "update_batch", "batch", "create_vector_name", "delete_vector_name",
	"create_collection", "delete_collection", "update_collection", "recreate_collection",
	"create_alias", "delete_alias", "rename_alias", "update_aliases",
	"create_snapshot", "delete_snapshot", "create_full_snapshot", "delete_full_snapshot",
	"recover_snapshot", "create_shard_key", "delete_shard_key", "update_collection_cluster_setup",
	"Upsert", "UPSERT", "Delete", "points/delete", "update",
}

func TestParseRequestAllowList(t *testing.T) {
	for _, tt := range []struct {
		text string
		op   string
	}{
		{`{"op":"scroll","collection":"c"}`, opScroll},
		{`{"op":"scroll","collection_name":"c","limit":5,"filter":{"must":[{"field":{"key":"a","match":{"boolean":true}}}]}}`, opScroll},
		{`{"op":"search","collection":"c","vector":[1,0],"limit":3}`, opSearch},
		{`{"op":"query","collection":"c","query":{"nearest":{"dense":{"data":[1,0]}}},"limit":3}`, opQuery},
		{`{"op":"query","collectionName":"c"}`, opQuery},
		{`{"op":"count","collection":"c","exact":true}`, opCount},
		{`{"op":"retrieve","collection":"c","ids":[{"num":"1"},{"uuid":"5c56c793-69f3-4fbf-87e6-c4bf54c28c26"}]}`, opRetrieve},
	} {
		for _, ro := range []bool{false, true} {
			req, err := parseRequest(tt.text, ro)
			if err != nil {
				t.Fatalf("parseRequest(%s, ro=%v): %v", tt.text, ro, err)
			}
			if req.op != tt.op || req.collection() != "c" {
				t.Errorf("parseRequest(%s) = op %q collection %q", tt.text, req.op, req.collection())
			}
		}
	}
}

func TestParseRequestTypes(t *testing.T) {
	req, err := parseRequest(`{"op":"scroll","collection":"c","limit":7}`, true)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := req.msg.(*pb.ScrollPoints)
	if !ok || s.GetLimit() != 7 || s.GetCollectionName() != "c" {
		t.Fatalf("scroll message = %#v", req.msg)
	}
	req, err = parseRequest(`{"op":"retrieve","collection":"c","ids":[{"num":"3"}]}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := req.msg.(*pb.GetPoints); !ok || len(g.GetIds()) != 1 || g.GetIds()[0].GetNum() != 3 {
		t.Fatalf("retrieve message = %#v", req.msg)
	}
}

// TestParseRequestMutationsRefused: every write op is refused before
// anything is sent. With read-only it is ErrReadOnly; without, Query
// still never writes (the Mutator does), so it is ErrUnsupported.
func TestParseRequestMutationsRefused(t *testing.T) {
	for _, op := range mutationOps {
		text := `{"op":"` + op + `","collection":"c","points":[{"id":{"num":"1"}}]}`
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %q: err = %v, want ErrReadOnly", op, err)
		}
		_, err = parseRequest(text, false)
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("writable %q: err = %v, want ErrUnsupported", op, err)
		}
	}
}

// TestParseRequestEscapes: forms that try to smuggle a write past the
// allow-list. On the read-only path every one is ErrReadOnly (fail
// closed); on a writable call it is a syntax or unsupported error.
func TestParseRequestEscapes(t *testing.T) {
	for _, text := range []string{
		`{"op":"scroll","op":"upsert","collection":"c"}`,                  // duplicate op: last wins in encoding/json
		`{"op":"upsert","op":"scroll","collection":"c"}`,                  // duplicate op, read op last
		`{"op":"scroll","collection":"c","points":[{"id":1}]}`,            // write fields on a read op
		`{"op":"scroll","collection":"c","operations":[]}`,                // update_batch body on a read op
		`{"op":" scroll","collection":"c"}`,                               // padded name
		`{"op":"scroll\u0000","collection":"c"}`,                          // NUL suffix
		`{"op":["scroll"],"collection":"c"}`,                              // not a string
		`{"op":null,"collection":"c"}`,                                    // null op
		`{"collection":"c"}`,                                              // no op
		`[{"op":"scroll","collection":"c"}]`,                              // not an object
		`{"op":"scroll","collection":"c"} {"op":"upsert"}`,                // trailing document
		`{"op":"scroll","collection":"c"`,                                 // truncated
		`{"op":"scroll","collection":"c","collection_name":"d"}`,          // two collection keys
		`{"op":"scroll","collection":"c","Op":"upsert"}`,                  // case variant key
		`{"op":"scroll","op":"upsert","collection":"c"}`,                  // escaped duplicate op
		`{"op":"scroll","collection_name":"c","collectionName":"d"}`,      // both spellings of one proto field
		`{"op":"scroll","collection":"c","filter":{"must":[],"must":[]}}`, // nested duplicate key
		``,
		`   `,
		`upsert c`,
	} {
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %s: err = %v, want ErrReadOnly", text, err)
		}
		_, err = parseRequest(text, false)
		if !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("writable %s: err = %v, want ErrSyntax or ErrUnsupported", text, err)
		}
	}
}

func TestParseRequestNoCollection(t *testing.T) {
	_, err := parseRequest(`{"op":"scroll"}`, false)
	if !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("err = %v, want ErrSyntax", err)
	}
}

// TestQueryRefusesBeforeNetwork: a closed-port connection proves the guard
// runs before any RPC (the error is ErrReadOnly, not ErrConnection).
func TestQueryRefusesBeforeNetwork(t *testing.T) {
	c := newTestConn(t, false)
	for _, ro := range []bool{false, true} {
		_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"delete","collection":"c"}`, ReadOnly: ro})
		want := driver.ErrUnsupported
		if ro {
			want = driver.ErrReadOnly
		}
		if !errors.Is(err, want) {
			t.Errorf("ro=%v: err = %v, want %v", ro, err, want)
		}
	}
	roConn := newTestConn(t, true)
	_, err := roConn.Query(context.Background(), driver.Query{Text: `{"op":"delete","collection":"c"}`})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only conn: err = %v, want ErrReadOnly", err)
	}
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"scroll","collection":"c"}`, Params: []driver.Value{driver.Int(1)}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("params: err = %v, want ErrUnsupported", err)
	}
}

// TestMutatorReadOnlyBeforeNetwork: a read-only connection refuses every
// Mutator call with ErrReadOnly before any RPC.
func TestMutatorReadOnlyBeforeNetwork(t *testing.T) {
	c := newTestConn(t, true)
	ctx, ref := context.Background(), driver.CollectionRef{Namespace: defaultNamespace, Name: "c"}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	_, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "a", Value: driver.Int(2)}})
	for name, err := range map[string]error{
		"Insert": err,
		"Update": c.Update(ctx, ref, key, driver.Object{{Key: "a", Value: driver.Int(3)}}),
		"Delete": c.Delete(ctx, ref, key),
	} {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", name, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("%s: not a qdrant *driver.Error: %v", name, err)
		}
	}
}

// TestParseRequestRefusesInferenceReadOnly: a document, image or object
// vector input asks the server to run an embedding model (egress and cost),
// so a read-only request carrying one anywhere is refused before it is
// sent. A writable connection lets it through.
func TestParseRequestRefusesInferenceReadOnly(t *testing.T) {
	doc := `{"document":{"text":"cats","model":"sentence-transformers/all-minilm-l6-v2"}}`
	img := `{"image":{"image":{"stringValue":"https://example.com/cat.png"},"model":"qdrant/clip"}}`
	obj := `{"object":{"object":{"stringValue":"x"},"model":"m"}}`
	for _, text := range []string{
		`{"op":"query","collection":"c","query":{"nearest":` + doc + `}}`,
		`{"op":"query","collection":"c","query":{"nearest":` + img + `}}`,
		`{"op":"query","collection":"c","query":{"nearest":` + obj + `}}`,
		`{"op":"query","collection":"c","query":{"recommend":{"positive":[{"dense":{"data":[1,0]}},` + doc + `]}}}`,
		`{"op":"query","collection":"c","query":{"discover":{"target":` + doc + `,"context":{"pairs":[]}}}}`,
		`{"op":"query","collection":"c","query":{"context":{"pairs":[{"positive":` + img + `,"negative":{"dense":{"data":[1,0]}}}]}}}`,
		`{"op":"query","collection":"c","prefetch":[{"prefetch":[{"query":{"nearest":` + doc + `}}]}],"query":{"fusion":"RRF"}}`,
		`{"op":"query","collection":"c","query":{"nearestWithMmr":{"nearest":` + obj + `}}}`,
	} {
		_, err := parseRequest(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("parseRequest(%s, read-only) = %v, want ErrReadOnly", text, err)
			continue
		}
		if !strings.Contains(err.Error(), "inference") {
			t.Errorf("parseRequest(%s, read-only): the error does not say why: %v", text, err)
		}
		if _, err := parseRequest(text, false); err != nil {
			t.Errorf("parseRequest(%s, writable): %v", text, err)
		}
	}
	for _, text := range []string{
		`{"op":"query","collection":"c","query":{"nearest":{"dense":{"data":[1,0]}}}}`,
		`{"op":"query","collection":"c","query":{"recommend":{"positive":[{"id":{"num":"1"}}]}}}`,
		`{"op":"query","collection":"c","prefetch":[{"query":{"nearest":{"dense":{"data":[1,0]}}}}],"query":{"fusion":"RRF"}}`,
		`{"op":"search","collection":"c","vector":[1,0]}`,
	} {
		if _, err := parseRequest(text, true); err != nil {
			t.Errorf("parseRequest(%s, read-only): %v", text, err)
		}
	}
}
