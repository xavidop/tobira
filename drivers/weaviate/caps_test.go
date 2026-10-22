package weaviate

import (
	"context"
	"errors"
	"math"
	"testing"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

var people = driver.CollectionRef{Namespace: defaultNamespace, Name: "People"}

// Writes on a read-only connection are refused before any request.
func TestMutatorReadOnlyRefusesFirst(t *testing.T) {
	c := newTestConn(t, driver.ConnConfig{ReadOnly: true})
	ctx := context.Background()
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	if _, err := c.Insert(ctx, people, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Insert: %v", err)
	}
	if err := c.Update(ctx, people, key, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Update: %v", err)
	}
	if err := c.Delete(ctx, people, key); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Delete: %v", err)
	}
}

func TestMutatorKeyChecks(t *testing.T) {
	c := newTestConn(t, driver.ConnConfig{})
	ctx := context.Background()
	for _, key := range []driver.Object{
		nil,
		{{Key: "name", Value: driver.String("x")}},
		{{Key: "id", Value: driver.Int(1)}, {Key: "name", Value: driver.String("x")}},
		{{Key: "id", Value: driver.Float(1.5)}},
	} {
		if err := c.Delete(ctx, people, key); err == nil || errors.Is(err, driver.ErrConnection) {
			t.Errorf("Delete(%v) = %v, want a refusal before the network", key, err)
		}
	}
	if err := c.Delete(ctx, driver.CollectionRef{Name: "../objects"}, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad collection name: %v", err)
	}
	if err := c.Delete(ctx, driver.CollectionRef{Namespace: "other", Name: "People"}, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("other namespace: %v", err)
	}
}

// Query refusals happen before the network, on every path.
func TestQueryRefusesBeforeNetwork(t *testing.T) {
	c := newTestConn(t, driver.ConnConfig{})
	ctx := context.Background()
	for _, q := range []driver.Query{
		{Text: `mutation { x }`, ReadOnly: true},
		{Text: `{"collection":"A","op":"delete"}`},
		{Text: `{"collection":"A"}`, Params: []driver.Value{driver.Int(1)}},
		{Text: `{"collection":"A"}`, Namespace: "nope"},
	} {
		if _, err := c.Query(ctx, q); err == nil || errors.Is(err, driver.ErrConnection) {
			t.Errorf("Query(%+v) = %v, want a refusal before the network", q, err)
		}
	}
}

func TestClosedConn(t *testing.T) {
	c := newTestConn(t, driver.ConnConfig{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping after Close = %v", err)
	}
}

type badEmbedder struct {
	vecs [][]float32
	err  error
}

func (b badEmbedder) Embed(context.Context, []string) ([][]float32, error) { return b.vecs, b.err }
func (badEmbedder) Dimensions() int                                        { return 2 }

func TestQueryVector(t *testing.T) {
	ctx := context.Background()
	plain := newTestConn(t, driver.ConnConfig{})
	if _, err := plain.queryVector(ctx, driver.VectorQuery{Text: "hi"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("no embedder: %v", err)
	}
	if _, err := plain.queryVector(ctx, driver.VectorQuery{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("nothing given: %v", err)
	}
	if _, err := plain.queryVector(ctx, driver.VectorQuery{Vector: []float32{1, float32(math.NaN())}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("NaN: %v", err)
	}
	if v, err := plain.queryVector(ctx, driver.VectorQuery{Vector: []float32{1, 2}}); err != nil || len(v) != 2 {
		t.Errorf("vector: %v %v", v, err)
	}
	emb := newTestConn(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 4}})
	if v, err := emb.queryVector(ctx, driver.VectorQuery{Text: "hi"}); err != nil || len(v) != 4 {
		t.Errorf("embedded: %v %v", v, err)
	}
	two := newTestConn(t, driver.ConnConfig{Embedder: badEmbedder{vecs: [][]float32{{1}, {2}}}})
	if _, err := two.queryVector(ctx, driver.VectorQuery{Text: "hi"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("two vectors: %v", err)
	}
	failing := newTestConn(t, driver.ConnConfig{Embedder: badEmbedder{err: driver.Wrap(driver.ErrAuth, "ai", "embed", errors.New("bad key"))}})
	if _, err := failing.queryVector(ctx, driver.VectorQuery{Text: "hi"}); !errors.Is(err, driver.ErrAuth) {
		t.Errorf("embedder error kind lost: %v", err)
	}
}

func TestPickVector(t *testing.T) {
	s := schemaFromClass(testClass())
	for _, tt := range []struct {
		field  string
		metric driver.VectorMetric
		want   string
		kind   error
	}{
		{"title_vec", "", "title_vec", nil},
		{"title_vec", driver.MetricL2, "title_vec", nil},
		{"title_vec", driver.MetricCosine, "", driver.ErrUnsupported},
		{"", driver.MetricL2, "title_vec", nil},
		{"", driver.MetricInnerProduct, "n", nil},
		{"", "", "", driver.ErrSyntax},
		{"colbert", "", "", driver.ErrUnsupported},
		{"nope", "", "", driver.ErrNotFound},
		{"", "manhattan", "", driver.ErrUnsupported},
	} {
		v, err := pickVector(s, tt.field, tt.metric)
		if tt.kind != nil {
			if !errors.Is(err, tt.kind) {
				t.Errorf("pickVector(%q, %q) = %+v, %v; want %v", tt.field, tt.metric, v, err, tt.kind)
			}
			continue
		}
		if err != nil || v.name != tt.want {
			t.Errorf("pickVector(%q, %q) = %+v, %v; want %s", tt.field, tt.metric, v, err, tt.want)
		}
	}
	// The only vector is picked without a field.
	one := schemaFromClass(peopleClassOneVector())
	if v, err := pickVector(one, "", ""); err != nil || v.name != "" {
		t.Errorf("single vector: %+v %v", v, err)
	}
}

// Concurrent first writes on a fresh connection must not race on the
// client's version provider (run with -race).
func TestConcurrentFirstWritesNoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/meta":
			_, _ = w.Write([]byte(`{"version":"1.40.2"}`))
		case r.URL.Path == "/v1/schema/People":
			_, _ = w.Write([]byte(`{"class":"People","properties":[{"name":"name","dataType":["text"]}]}`))
		case strings.HasPrefix(r.URL.Path, "/v1/objects/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "weaviate://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	m := c.(*conn)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := driver.Object{{Key: "id", Value: driver.Int(int64(i + 1))}}
			var err error
			if i%2 == 0 {
				err = m.Update(context.Background(), people, key, driver.Object{{Key: "name", Value: driver.String("x")}})
			} else {
				err = m.Delete(context.Background(), people, key)
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	m.transport.CloseIdleConnections()
}
