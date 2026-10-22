package chroma

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// fakeChroma is a small in-memory stand-in for the Chroma v2 REST API, for
// unit tests that need a server: authentication, the request bodies the
// driver sends, and the record semantics the driver relies on (add ignores
// an existing id, update merges metadata and a null removes a key). Filters
// are recorded but not evaluated: filter semantics run against the real
// server in the integration suite.
type fakeChroma struct {
	t      *testing.T
	srv    *httptest.Server
	token  string // expected token; "" = no auth
	header string // header carrying it: "x-chroma-token" or "authorization"
	tenant string
	dbs    []string

	mu    sync.Mutex
	colls map[string]*fakeColl // "db/name"
	reqs  []fakeReq
}

type fakeReq struct {
	method, path string
	body         map[string]any
}

type fakeColl struct {
	id, name, space string
	dim             int
	recs            []fakeRec
	config          map[string]any
}

type fakeRec struct {
	id   string
	emb  []float64
	doc  *string
	uri  *string
	meta map[string]any
}

func newFake(t *testing.T) *fakeChroma {
	t.Helper()
	f := &fakeChroma{t: t, tenant: "default_tenant", dbs: []string{"default_database"}, colls: map[string]*fakeColl{}, header: "x-chroma-token"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// dsn is a DSN for the fake (a loopback http URL).
func (f *fakeChroma) dsn(params string) string {
	host := strings.TrimPrefix(f.srv.URL, "http://")
	if params != "" {
		return "chroma://" + host + "?" + params
	}
	return "chroma://" + host
}

func (f *fakeChroma) addColl(db, name, space string, dim int) *fakeColl {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeColl{id: fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.colls)+1), name: name, space: space, dim: dim}
	c.config = map[string]any{"hnsw": map[string]any{"space": space}, "embedding_function": nil}
	f.colls[db+"/"+name] = c
	return c
}

func (f *fakeChroma) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// writes counts the record writes the fake received.
func (f *fakeChroma) writes() int {
	n := 0
	for _, r := range f.requests() {
		for _, w := range []string{"/add", "/update", "/upsert", "/delete"} {
			if strings.HasSuffix(r.path, w) {
				n++
			}
		}
	}
	return n
}

func (f *fakeChroma) fail(w http.ResponseWriter, status int, name, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": name, "message": msg})
}

func (f *fakeChroma) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeChroma) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil {
			f.fail(w, 422, "ChromaError", "Failed to deserialize the JSON body")
			return
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, fakeReq{method: r.Method, path: r.URL.Path, body: body})
	f.mu.Unlock()
	if f.token != "" {
		got := r.Header.Get("X-Chroma-Token")
		if f.header == "authorization" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if got != f.token {
			f.fail(w, 401, "AuthError", "Unauthorized")
			return
		}
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v2/"), "/")
	switch {
	case r.URL.Path == "/api/v2/auth/identity":
		f.reply(w, map[string]any{"user_id": "", "tenant": f.tenant, "databases": f.dbs})
	case len(p) == 3 && p[0] == "tenants" && p[2] == "databases":
		var out []map[string]any
		for _, d := range f.dbs {
			out = append(out, map[string]any{"id": "x", "name": d, "tenant": p[1]})
		}
		f.reply(w, out)
	case len(p) == 4 && p[2] == "databases":
		if p[1] != f.tenant || !slices.Contains(f.dbs, p[3]) {
			f.fail(w, 404, "NotFoundError", fmt.Sprintf("Database [%s] not found.", p[3]))
			return
		}
		f.reply(w, map[string]any{"id": "x", "name": p[3], "tenant": p[1]})
	case len(p) == 5 && p[4] == "collections":
		f.mu.Lock()
		var out []map[string]any
		for k, c := range f.colls {
			if strings.HasPrefix(k, p[3]+"/") {
				out = append(out, c.model(p[1], p[3]))
			}
		}
		f.mu.Unlock()
		slices.SortFunc(out, func(a, b map[string]any) int { return strings.Compare(a["name"].(string), b["name"].(string)) })
		f.reply(w, out)
	case len(p) == 6 && r.Method == http.MethodGet:
		f.mu.Lock()
		c := f.colls[p[3]+"/"+p[5]]
		f.mu.Unlock()
		if c == nil {
			f.fail(w, 404, "NotFoundError", fmt.Sprintf("Collection [%s] does not exist", p[5]))
			return
		}
		f.reply(w, c.model(p[1], p[3]))
	case len(p) == 7:
		f.records(w, p[3], p[5], p[6], body)
	default:
		f.fail(w, 404, "NotFoundError", "no route "+r.URL.Path)
	}
}

func (c *fakeColl) model(tenant, db string) map[string]any {
	m := map[string]any{"id": c.id, "name": c.name, "tenant": tenant, "database": db, "configuration_json": c.config, "metadata": nil, "log_position": 0, "version": 0, "dimension": nil}
	if c.dim > 0 && len(c.recs) > 0 {
		m["dimension"] = c.dim
	}
	return m
}

func (f *fakeChroma) collByID(db, id string) *fakeColl {
	for k, c := range f.colls {
		if strings.HasPrefix(k, db+"/") && c.id == id {
			return c
		}
	}
	return nil
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func floats(v any) []float64 {
	var out []float64
	l, _ := v.([]any)
	for _, x := range l {
		n, _ := x.(json.Number).Float64()
		out = append(out, n)
	}
	return out
}

func intOr(v any, def int) int {
	if n, ok := v.(json.Number); ok {
		i, _ := n.Int64()
		return int(i)
	}
	return def
}

func (f *fakeChroma) records(w http.ResponseWriter, db, id, op string, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.collByID(db, id)
	if c == nil {
		f.fail(w, 404, "NotFoundError", fmt.Sprintf("Collection [%s] does not exist.", id))
		return
	}
	find := func(id string) int { return slices.IndexFunc(c.recs, func(r fakeRec) bool { return r.id == id }) }
	switch op {
	case "count":
		f.reply(w, len(c.recs))
	case "get":
		ids := strs(body["ids"])
		var sel []fakeRec
		for _, r := range c.recs {
			if body["ids"] == nil || slices.Contains(ids, r.id) {
				sel = append(sel, r)
			}
		}
		off := min(intOr(body["offset"], 0), len(sel))
		sel = sel[off:]
		if l := intOr(body["limit"], -1); l >= 0 && l < len(sel) {
			sel = sel[:l]
		}
		f.reply(w, f.result(sel, body["include"], nil, false))
	case "query":
		qs, _ := body["query_embeddings"].([]any)
		n := intOr(body["n_results"], 10)
		var all [][]fakeRec
		var dists [][]float64
		for _, q := range qs {
			qv := floats(q)
			type scored struct {
				r fakeRec
				d float64
			}
			var sc []scored
			for _, r := range c.recs {
				sc = append(sc, scored{r, distance(c.space, qv, r.emb)})
			}
			slices.SortStableFunc(sc, func(a, b scored) int { return cmpFloat(a.d, b.d) })
			sc = sc[:min(n, len(sc))]
			var rs []fakeRec
			var ds []float64
			for _, s := range sc {
				rs, ds = append(rs, s.r), append(ds, s.d)
			}
			all, dists = append(all, rs), append(dists, ds)
		}
		f.reply(w, f.queryResult(all, dists, body["include"]))
	case "add":
		ids, embs := strs(body["ids"]), body["embeddings"].([]any)
		docs, _ := body["documents"].([]any)
		metas, _ := body["metadatas"].([]any)
		for i, id := range ids {
			if find(id) >= 0 {
				continue // Chroma ignores an existing id
			}
			e := floats(embs[i])
			if c.dim > 0 && len(e) != c.dim {
				f.fail(w, 400, "InvalidArgumentError", fmt.Sprintf("Collection expecting embedding with dimension of %d, got %d", c.dim, len(e)))
				return
			}
			c.dim = len(e)
			rec := fakeRec{id: id, emb: e}
			if docs != nil {
				if s, ok := docs[i].(string); ok {
					rec.doc = &s
				}
			}
			if metas != nil {
				rec.meta, _ = metas[i].(map[string]any)
			}
			c.recs = append(c.recs, rec)
		}
		f.reply(w, map[string]any{})
	case "update":
		ids := strs(body["ids"])
		metas, _ := body["metadatas"].([]any)
		embs, _ := body["embeddings"].([]any)
		docs, _ := body["documents"].([]any)
		for i, id := range ids {
			j := find(id)
			if j < 0 {
				continue
			}
			if metas != nil {
				m, _ := metas[i].(map[string]any)
				if c.recs[j].meta == nil {
					c.recs[j].meta = map[string]any{}
				}
				for k, v := range m {
					if v == nil {
						delete(c.recs[j].meta, k)
					} else {
						c.recs[j].meta[k] = v
					}
				}
			}
			if embs != nil {
				c.recs[j].emb = floats(embs[i])
			}
			if docs != nil {
				if s, ok := docs[i].(string); ok {
					c.recs[j].doc = &s
				}
			}
		}
		f.reply(w, map[string]any{})
	case "delete":
		ids := strs(body["ids"])
		c.recs = slices.DeleteFunc(c.recs, func(r fakeRec) bool { return slices.Contains(ids, r.id) })
		f.reply(w, map[string]any{"deleted": len(ids)})
	default:
		f.fail(w, 404, "NotFoundError", "no op "+op)
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// distance is Chroma's distance for a space: squared L2, 1 - cosine
// similarity, or 1 - inner product.
func distance(space string, q, v []float64) float64 {
	var dot, qq, vv, l2 float64
	for i := range q {
		if i >= len(v) {
			break
		}
		dot += q[i] * v[i]
		qq += q[i] * q[i]
		vv += v[i] * v[i]
		l2 += (q[i] - v[i]) * (q[i] - v[i])
	}
	switch space {
	case "cosine":
		return 1 - dot/(math.Sqrt(qq)*math.Sqrt(vv))
	case "ip":
		return 1 - dot
	}
	return l2
}

func includes(v any) []string {
	if v == nil {
		return []string{"documents", "metadatas"}
	}
	return strs(v)
}

func (f *fakeChroma) result(rs []fakeRec, include any, _ []float64, _ bool) map[string]any {
	inc := includes(include)
	out := map[string]any{"ids": []string{}, "include": inc}
	ids := []string{}
	var docs, uris, metas, embs []any
	for _, r := range rs {
		ids = append(ids, r.id)
		docs = append(docs, ptrOrNil(r.doc))
		uris = append(uris, ptrOrNil(r.uri))
		metas = append(metas, metaOrNil(r.meta))
		embs = append(embs, r.emb)
	}
	out["ids"] = ids
	for _, k := range inc {
		switch k {
		case "documents":
			out["documents"] = orEmpty(docs)
		case "uris":
			out["uris"] = orEmpty(uris)
		case "metadatas":
			out["metadatas"] = orEmpty(metas)
		case "embeddings":
			out["embeddings"] = orEmpty(embs)
		}
	}
	return out
}

func (f *fakeChroma) queryResult(all [][]fakeRec, dists [][]float64, include any) map[string]any {
	inc := []string{"documents", "metadatas", "distances"}
	if include != nil {
		inc = strs(include)
	}
	out := map[string]any{"include": inc}
	var ids, docs, uris, metas, embs, ds []any
	for i, rs := range all {
		r := f.result(rs, []any{"documents", "uris", "metadatas", "embeddings"}, nil, false)
		ids = append(ids, r["ids"])
		docs = append(docs, r["documents"])
		uris = append(uris, r["uris"])
		metas = append(metas, r["metadatas"])
		embs = append(embs, r["embeddings"])
		ds = append(ds, dists[i])
	}
	out["ids"] = orEmpty(ids)
	for _, k := range inc {
		switch k {
		case "documents":
			out["documents"] = orEmpty(docs)
		case "uris":
			out["uris"] = orEmpty(uris)
		case "metadatas":
			out["metadatas"] = orEmpty(metas)
		case "embeddings":
			out["embeddings"] = orEmpty(embs)
		case "distances":
			out["distances"] = orEmpty(ds)
		}
	}
	return out
}

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func ptrOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func metaOrNil(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// seedFake adds records to c directly.
func (c *fakeColl) add(id string, emb []float64, doc string, meta map[string]any) {
	r := fakeRec{id: id, emb: emb, meta: map[string]any{}}
	if doc != "" {
		r.doc = &doc
	}
	for k, v := range meta {
		switch n := v.(type) {
		case int:
			r.meta[k] = json.Number(fmt.Sprint(n))
		case float64:
			r.meta[k] = json.Number(strings.TrimSuffix(fmt.Sprintf("%.6f", n), "0"))
		default:
			r.meta[k] = v
		}
	}
	c.dim = len(emb)
	c.recs = append(c.recs, r)
}

// openFake opens a conn to the fake.
func openFake(t *testing.T, f *fakeChroma, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = f.dsn("")
	}
	c, err := New().Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", cfg.DSN, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// countingEmbedder wraps HashEmbedder and counts calls.
type countingEmbedder struct {
	mu    sync.Mutex
	calls int
	texts []string
	dims  int
	err   error
}

func (e *countingEmbedder) Dimensions() int { return e.dims }

func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.texts = append(e.texts, texts...)
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, e.dims)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}
