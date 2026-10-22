//go:build integration

package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func needElastic(t *testing.T) {
	t.Helper()
	if engine.fl != flavorElastic {
		t.Skip("Elasticsearch only")
	}
}

// A read-only API key authenticates (id:key in the password field, or
// the encoded key in the DSN's password position); the server then
// refuses its writes too.
func TestAPIKeyAuth(t *testing.T) {
	needElastic(t)
	data := mustAdmin(t, http.MethodPost, "/_security/api_key", map[string]any{
		"name": "tobira-read",
		"role_descriptors": map[string]any{"reader": map[string]any{
			"indices": []any{map[string]any{"names": []string{"tobira_*"}, "privileges": []string{"read", "view_index_metadata"}}},
		}},
	})
	var key struct {
		ID      string `json:"id"`
		APIKey  string `json:"api_key"`
		Encoded string `json:"encoded"`
	}
	if err := json.Unmarshal(data, &key); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	host := "elasticsearch://" + engine.hostPort + "?ca_file=" + engine.caFile
	for name, cfg := range map[string]driver.ConnConfig{
		"id:key in the password field": {DSN: host, Password: driver.NewSecret(key.ID + ":" + key.APIKey)},
		"encoded key in the DSN":       {DSN: strings.Replace(host, "://", "://:"+key.Encoded+"@", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := New().Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			rs, err := c.Query(ctx, driver.Query{Text: `{"index":"tobira_people","size":2}`})
			if err != nil {
				t.Fatal(err)
			}
			rows, _, err := driver.Collect(ctx, rs, 0)
			if err != nil || len(rows) != 2 {
				t.Fatalf("query with the API key: %d rows, %v", len(rows), err)
			}
			m, _ := driver.As[driver.Mutator](c)
			err = m.Delete(ctx, peopleRef(), driver.Object{{Key: "_id", Value: driver.String("1")}})
			if !errors.Is(err, driver.ErrPermission) {
				t.Errorf("delete with a read-only key: want ErrPermission, got %v", err)
			}
		})
	}
	_, err := New().Open(ctx, driver.ConnConfig{DSN: host, Password: driver.NewSecret("bm9wZTpub3Bl")})
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("a wrong API key: want ErrAuth, got %v", err)
	}
}

// TLS is verified: without the cluster's CA the open fails, and the
// password never shows in the error.
func TestTLSVerified(t *testing.T) {
	needElastic(t)
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "elasticsearch://elastic@" + engine.hostPort, Password: driver.NewSecret(testPassword)})
	if !errors.Is(err, driver.ErrConnection) || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("open without the CA: want ErrConnection about the certificate, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), testPassword) {
		t.Errorf("the error carries the password: %v", err)
	}
	_, err = New().Open(context.Background(), driver.ConnConfig{DSN: "elasticsearch://elastic@" + engine.hostPort + "?tls=false", Password: driver.NewSecret(testPassword)})
	if err == nil {
		t.Error("plain HTTP to a TLS port succeeded")
	}
}

func TestSimilarTextAndFilter(t *testing.T) {
	needElastic(t)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: vecsIndex}
	c := openTest(t, true)
	if _, err := c.Similar(ctx, ref, driver.VectorQuery{Text: "near", K: 2}); !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		t.Errorf("text without an embedder: want ErrUnsupported with the hint, got %v", err)
	}
	cfg := testConfig(true)
	cfg.Embedder = drivertest.HashEmbedder{Dims: 4}
	ce, err := New().Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ce.Close() }()
	rs, err := ce.(*conn).Similar(ctx, ref, driver.VectorQuery{Text: "near", K: 2})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != 2 {
		t.Errorf("text similarity: %d rows, %v", len(rows), err)
	}
	// Of "large" (6,6,0,0) and "y-axis" (0,1,0,0), L2 (the kNN path)
	// prefers y-axis, cosine (the script path) prefers large.
	for metric, want := range map[driver.VectorMetric]int64{driver.MetricL2: 4, driver.MetricCosine: 3} {
		rs, err = c.Similar(ctx, ref, driver.VectorQuery{
			Vector: []float32{1, 0, 0, 0}, K: 3, Metric: metric,
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "payload.label", Op: driver.OpIn, Value: driver.List(driver.String("large"), driver.String("y-axis"))}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cols := rs.Columns()
		rows, _, err = driver.Collect(ctx, rs, 0)
		if err != nil || len(rows) != 2 {
			t.Fatalf("metric %s with a filter: %d rows, %v", metric, len(rows), err)
		}
		id := rows[0][colIndex(t, cols, "id")]
		if !drivertest.EqualValue(driver.Int(want), id) {
			t.Errorf("metric %s with a filter: nearest is %s, want %d", metric, id, want)
		}
	}
	if _, err := c.Similar(ctx, ref, driver.VectorQuery{Vector: []float32{1, 0}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("a vector of the wrong size: want ErrSyntax, got %v", err)
	}
	if _, err := c.Similar(ctx, peopleRef(), driver.VectorQuery{Vector: []float32{1}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("an index without vectors: want ErrUnsupported, got %v", err)
	}
}

// System indexes stay out of read-only queries unless the connection opts
// in with allow_system_indices=1; closing the connection releases a
// result's point in time.
func TestSystemIndexOptInAndCloseReleasesPIT(t *testing.T) {
	needElastic(t)
	ctx := context.Background()
	c := openTest(t, true)
	if _, err := c.Query(ctx, driver.Query{Text: `{"index":".security*","size":1}`}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("a read-only query on .security*: want ErrReadOnly, got %v", err)
	}
	cfg := testConfig(true)
	cfg.Options = map[string]string{"allow_system_indices": "1"}
	oc, err := New().Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oc.Close() }()
	if _, err := oc.Query(ctx, driver.Query{Text: `{"method":"GET","path":"/_cat/indices?expand_wildcards=all&format=json"}`}); err != nil {
		t.Errorf("with allow_system_indices=1: %v", err)
	}

	pc, err := New().Open(ctx, testConfig(false))
	if err != nil {
		t.Fatal(err)
	}
	pc.(*conn).pageSize = 10
	rs, err := pc.Query(ctx, driver.Query{Text: `{"index":"tobira_people"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(ctx); !ok || err != nil {
		t.Fatalf("Next: %v %v", ok, err)
	}
	openPITs := func() int {
		var st struct {
			Indices map[string]struct {
				Total struct {
					Search struct {
						PIT int `json:"open_contexts"` // a point in time holds one search context per shard
					} `json:"search"`
				} `json:"total"`
			} `json:"indices"`
		}
		raw := mustAdmin(t, http.MethodGet, "/tobira_people/_stats/search", nil)
		_ = json.Unmarshal(raw, &st)
		return st.Indices[peopleIndex].Total.Search.PIT
	}
	if n := openPITs(); n != 1 {
		t.Fatalf("open points in time on People mid-stream = %d, want 1", n)
	}
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	if n := openPITs(); n != 0 {
		t.Errorf("open points in time after Close = %d, want 0", n)
	}
	_ = rs.Close()
}
