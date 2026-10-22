package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// fakeES is a tiny in-memory stand-in for an Elasticsearch node: one index
// "docs" of n documents {"id": i, "name": "doc-i"}, always in id order. It
// records the requests and the points in time it opened and closed.
type fakeES struct {
	t        *testing.T
	n        int
	product  bool // send X-Elastic-Product
	mu       sync.Mutex
	requests []string
	opened   int
	closed   int
}

func (f *fakeES) log(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
}

func (f *fakeES) counts() (opened, closed int, reqs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.closed, append([]string(nil), f.requests...)
}

func (f *fakeES) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.log(r)
	if f.product {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
	}
	w.Header().Set("Content-Type", "application/json")
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/":
		_, _ = fmt.Fprint(w, `{"version":{"number":"8.19.23"}}`)
	case r.URL.Path == "/docs/_mapping":
		_, _ = fmt.Fprint(w, `{"docs":{"mappings":{"properties":{"id":{"type":"integer"},"name":{"type":"keyword"}}}}}`)
	case r.URL.Path == "/docs/_pit" && r.Method == http.MethodPost:
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
		_, _ = fmt.Fprint(w, `{"id":"pit-1"}`)
	case r.URL.Path == "/_pit" && r.Method == http.MethodDelete:
		f.mu.Lock()
		f.closed++
		f.mu.Unlock()
		_, _ = fmt.Fprint(w, `{"succeeded":true,"num_freed":1}`)
	case r.URL.Path == "/_search" || r.URL.Path == "/docs/_search":
		f.search(w, r.URL.Path == "/_search", body)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":{"type":"index_not_found_exception","reason":"no such index [%s]"},"status":404}`, r.URL.Path)
	}
}

func (f *fakeES) search(w http.ResponseWriter, viaPIT bool, body []byte) {
	var req struct {
		Size        *int                 `json:"size"`
		PIT         *struct{ ID string } `json:"pit"`
		SearchAfter []int                `json:"search_after"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("fake: bad search body %s: %v", body, err)
	}
	if viaPIT != (req.PIT != nil) {
		f.t.Errorf("fake: a search on /_search must carry a pit and one on /docs/_search must not: %s", body)
	}
	size := 10
	if req.Size != nil {
		size = *req.Size
	}
	start := 1
	if len(req.SearchAfter) > 0 {
		start = req.SearchAfter[0] + 1
	}
	var hits []string
	for i := start; i <= f.n && len(hits) < size; i++ {
		hits = append(hits, fmt.Sprintf(`{"_index":"docs","_id":"d%d","_score":null,"_source":{"id":%d,"name":"doc-%d"},"sort":[%d,%d]}`, i, i, i, i, i))
	}
	pit := ""
	if viaPIT {
		pit = `"pit_id":"pit-1",`
	}
	_, _ = fmt.Fprintf(w, `{%s"took":1,"hits":{"hits":[%s]}}`, pit, strings.Join(hits, ","))
}

// newFakeConn starts a fake node and opens a conn to it.
func newFakeConn(t *testing.T, f *fakeES, fl flavor, readOnly bool) *conn {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	scheme := map[flavor]string{flavorElastic: "elasticsearch", flavorOpenSearch: "opensearch"}[fl]
	dsn := scheme + "://" + strings.TrimPrefix(srv.URL, "http://") + "?tls=false"
	c, err := esDriver{fl: fl}.Open(context.Background(), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// newServer starts f and returns its host:port.
func newServer(t *testing.T, f *fakeES) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}
