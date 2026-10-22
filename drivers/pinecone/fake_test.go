package pinecone

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// fakeControlPlane serves the control-plane calls the driver makes, in the
// SDK's API version shape, and checks the API key.
func fakeControlPlane(t *testing.T, key string) *httptest.Server {
	t.Helper()
	index := func(name, metric string) string {
		return `{"name":"` + name + `","host":"127.0.0.1:9","deletion_protection":"disabled",` +
			`"schema":{"fields":{"_values":{"type":"dense_vector","dimension":4,"metric":"` + metric + `"}}},` +
			`"status":{"ready":true,"state":"Ready"}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Api-Key") != key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"Invalid API Key"},"status":401}`))
			return
		}
		switch r.URL.Path {
		case "/indexes":
			_, _ = w.Write([]byte(`{"indexes":[` + index("docs", "cosine") + `,` + index("alpha", "dotproduct") + `]}`))
		case "/indexes/docs":
			_, _ = w.Write([]byte(index("docs", "cosine")))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"Resource not found"},"status":404}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestControlPlane(t *testing.T) {
	srv := fakeControlPlane(t, "k3y")
	dsn := "pinecone://" + strings.TrimPrefix(srv.URL, "http://") + "?tls=false"
	ctx := context.Background()
	dc, err := New().Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("k3y")})
	if err != nil {
		t.Fatal(err)
	}
	c := dc.(*conn)
	defer func() { _ = c.Close() }()
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nss) != 2 || nss[0].Name != "alpha" || nss[1].Name != "docs" || nss[0].Label != "index" {
		t.Errorf("namespaces = %+v", nss)
	}
	h, err := c.index(ctx, "query", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if h.host != "http://127.0.0.1:9" || h.metric != metricCosine || h.dim != 4 {
		t.Errorf("handle = %+v", h)
	}
	if again, _ := c.index(ctx, "query", "docs"); again != h {
		t.Error("the index handle is not cached")
	}
	if _, err := c.index(ctx, "query", "missing"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing index: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"stats","index":"missing"}`}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("query on a missing index: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"stats"}`}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("query without an index on a project: %v", err)
	}
}

// Bad credentials fail Open with ErrAuth, without the key in the error.
func TestControlPlaneBadAuth(t *testing.T) {
	srv := fakeControlPlane(t, "right-key")
	dsn := "pinecone://" + strings.TrimPrefix(srv.URL, "http://") + "?tls=false"
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("wrong-key-123")})
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("Open with a wrong key: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-key-123") {
		t.Errorf("error leaks the key: %v", err)
	}
}

// Control-plane responses are bounded: an endless body fails instead of
// filling memory.
func TestControlPlaneResponseCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"indexes":[{"name":"`))
		chunk := []byte(strings.Repeat("a", 1<<16))
		for range (maxResponseBytes >> 16) + 2 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	dsn := "pinecone://" + strings.TrimPrefix(srv.URL, "http://") + "?tls=false"
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn})
	if !errors.Is(err, driver.ErrLimit) || !strings.Contains(err.Error(), "too large") {
		t.Errorf("Open on an endless response: %v, want ErrLimit", err)
	}
}

// An index host from the control plane is reached over TLS unless it is
// this machine and the control plane itself is plaintext: a scheme the
// control plane names never downgrades it.
func TestIndexHostURL(t *testing.T) {
	for _, tc := range []struct {
		host      string
		plaintext bool
		want      string
	}{
		{"docs-abc.svc.aped.pinecone.io", false, "https://docs-abc.svc.aped.pinecone.io"},
		{"http://docs-abc.svc.aped.pinecone.io", false, "https://docs-abc.svc.aped.pinecone.io"},
		{"http://evil.example:9", true, "https://evil.example:9"},
		{"idx.example.com:5081", true, "https://idx.example.com:5081"},
		{"localhost:5081", true, "http://localhost:5081"},
		{"127.0.0.1:5081", true, "http://127.0.0.1:5081"},
		{"http://127.0.0.1:5081", true, "http://127.0.0.1:5081"},
		{"https://127.0.0.1:5081", true, "https://127.0.0.1:5081"},
		{"localhost:5081", false, "https://localhost:5081"},
	} {
		got, err := indexHostURL(tc.host, tc.plaintext)
		if err != nil || got != tc.want {
			t.Errorf("indexHostURL(%q, %v) = %q, %v; want %q", tc.host, tc.plaintext, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "http://", "x/y"} {
		if _, err := indexHostURL(bad, true); err == nil {
			t.Errorf("indexHostURL(%q) accepted", bad)
		}
	}
}
