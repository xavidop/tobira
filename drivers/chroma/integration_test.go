//go:build integration

package chroma

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// chromaImage is pinned: bump it deliberately.
const chromaImage = "chromadb/chroma:1.5.9"

const (
	testToken  = "tobira-test-token"
	defaultDB  = "default_database"
	otherDB    = "tobira_other"
	peopleColl = "tobira_people"
	vecsColl   = drivertest.VectorCollection // l2
	vecsCos    = "vecs_cos"
	vecsIP     = "vecs_ip"
	textsColl  = "texts" // documents embedded with HashEmbedder{Dims: 8}
	efColl     = "ef_openai"
)

var (
	// chromaAddr is the container's host:port; proxyAddr is the
	// token-checking proxy in front of it.
	chromaAddr string
	proxyAddr  string
	// textDocs are the documents of the texts collection.
	textDocs = []string{"the database stores vectors", "a terminal for every database", "cooking pasta at home", "vector search with chroma", "weather report for tomorrow"}
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, chromaImage,
		testcontainers.WithExposedPorts("8000/tcp"),
		testcontainers.WithEnv(map[string]string{"ANONYMIZED_TELEMETRY": "FALSE"}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/api/v2/heartbeat").WithPort("8000/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start chroma container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if chromaAddr, err = ctr.PortEndpoint(ctx, "8000/tcp", ""); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	chromaAddr = strings.Replace(chromaAddr, "localhost", "127.0.0.1", 1)
	proxy := startAuthProxy(chromaAddr)
	defer proxy.Close()
	proxyAddr = strings.TrimPrefix(proxy.URL, "http://")
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	baseline := goleak.IgnoreCurrent()
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(baseline); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

// startAuthProxy puts a token check in front of the container. The open
// source Chroma server (1.x) has no authentication of its own: deployments
// put a proxy in front of it, and Chroma Cloud checks the x-chroma-token
// header. This proxy answers 401 with Chroma's error body unless the
// request carries the test token in x-chroma-token or as a bearer token.
func startAuthProxy(addr string) *httptest.Server {
	target := &url.URL{Scheme: "http", Host: addr}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = &http.Transport{DisableKeepAlives: true}
	rp.ErrorLog = log.New(io.Discard, "", 0)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Chroma-Token") != testToken && r.Header.Get("Authorization") != "Bearer "+testToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"AuthError","message":"Unauthorized"}`)
			return
		}
		rp.ServeHTTP(w, r)
	}))
}

// testDSN goes through the proxy; the token comes as a password ref
// (testCfg) unless the DSN carries it.
func testDSN(params string) string {
	if params == "" {
		return "chroma://" + proxyAddr
	}
	return "chroma://" + proxyAddr + "?" + params
}

func testCfg(readOnly bool) driver.ConnConfig {
	return driver.ConnConfig{DSN: testDSN(""), Password: driver.NewSecret(testToken), ReadOnly: readOnly}
}

// admin is a raw client straight to the container.
func admin() *apiClient {
	return &apiClient{base: "http://" + chromaAddr, http: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
}

// createColl creates a collection with a distance space (and an optional
// embedding function configuration) and returns its id.
func createColl(ctx context.Context, db, name, space string, ef map[string]any) (string, error) {
	cfg := map[string]any{"hnsw": map[string]any{"space": space}}
	if ef != nil {
		cfg["embedding_function"] = ef
	}
	var m collectionModel
	err := admin().do(ctx, http.MethodPost, dbPath(defaultTenant, db)+"/collections", map[string]any{"name": name, "configuration": cfg}, &m)
	return m.ID, err
}

func dropColl(db, name string) {
	_ = admin().do(context.Background(), http.MethodDelete, dbPath(defaultTenant, db)+"/collections/"+name, nil, nil)
}

func seed(ctx context.Context) error {
	a := admin()
	// People: embeddings [id, 1]; created_at as RFC 3339 text, tags as
	// JSON text and score as a float, Chroma's storage limits; a NULL age
	// is a missing key.
	id, err := createColl(ctx, defaultDB, peopleColl, "l2", nil)
	if err != nil {
		return fmt.Errorf("create people: %w", err)
	}
	var req addRequest
	for _, r := range drivertest.People.Rows {
		n, _ := r[0].AsInt()
		meta := map[string]any{}
		for i, col := range drivertest.People.Columns[1:] {
			v := r[i+1]
			if col.Name == "score" {
				s, _ := v.AsText()
				f, _ := strconv.ParseFloat(s, 64)
				v = driver.Float(f)
			}
			x, err := encodeMeta(v)
			if err != nil {
				return fmt.Errorf("people %d %s: %w", n, col.Name, err)
			}
			if x != nil {
				meta[col.Name] = x
			}
		}
		req.IDs = append(req.IDs, strconv.FormatInt(n, 10))
		req.Embeddings = append(req.Embeddings, []float32{float32(n), 1})
		req.Metadatas = append(req.Metadatas, meta)
	}
	if err := a.add(ctx, defaultTenant, defaultDB, id, req); err != nil {
		return fmt.Errorf("add people: %w", err)
	}
	// The vector fixture under each space.
	for name, space := range map[string]string{vecsColl: "l2", vecsCos: "cosine", vecsIP: "ip"} {
		id, err := createColl(ctx, defaultDB, name, space, nil)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		var req addRequest
		for _, r := range drivertest.VectorDataset().Rows {
			n, _ := r[0].AsInt()
			vec, _ := r[1].AsVector()
			obj, _ := r[2].AsObject()
			meta := map[string]any{}
			for _, m := range obj {
				meta[m.Key], _ = encodeMeta(m.Value)
			}
			req.IDs = append(req.IDs, strconv.FormatInt(n, 10))
			req.Embeddings = append(req.Embeddings, vec)
			req.Metadatas = append(req.Metadatas, meta)
		}
		if err := a.add(ctx, defaultTenant, defaultDB, id, req); err != nil {
			return fmt.Errorf("add %s: %w", name, err)
		}
	}
	// texts: documents embedded by the test embedder.
	id, err = createColl(ctx, defaultDB, textsColl, "l2", nil)
	if err != nil {
		return fmt.Errorf("create texts: %w", err)
	}
	embs, _ := drivertest.HashEmbedder{Dims: 8}.Embed(ctx, textDocs)
	req = addRequest{}
	for i, d := range textDocs {
		req.IDs = append(req.IDs, "t"+strconv.Itoa(i+1))
		req.Embeddings = append(req.Embeddings, embs[i])
		req.Documents = append(req.Documents, &d)
		req.Metadatas = append(req.Metadatas, map[string]any{"n": i + 1})
	}
	if err := a.add(ctx, defaultTenant, defaultDB, id, req); err != nil {
		return fmt.Errorf("add texts: %w", err)
	}
	// A collection whose stored configuration names OpenAI as its
	// embedding function: tobira must never use it.
	id, err = createColl(ctx, defaultDB, efColl, "l2", map[string]any{"type": "known", "name": "openai",
		"config": map[string]any{"api_key_env_var": "OPENAI_API_KEY", "model_name": "text-embedding-3-small"}})
	if err != nil {
		return fmt.Errorf("create ef collection: %w", err)
	}
	if err := a.add(ctx, defaultTenant, defaultDB, id, addRequest{IDs: []string{"e1"}, Embeddings: [][]float32{make8(1)}, Documents: []*string{&textDocs[0]}}); err != nil {
		return fmt.Errorf("add ef: %w", err)
	}
	// A second database with one collection.
	if err := a.do(ctx, http.MethodPost, "/tenants/"+defaultTenant+"/databases", map[string]any{"name": otherDB}, nil); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	if _, err := createColl(ctx, otherDB, "elsewhere", "l2", nil); err != nil {
		return fmt.Errorf("create elsewhere: %w", err)
	}
	return nil
}

func make8(first float32) []float32 {
	v := make([]float32, 8)
	v[0] = first
	return v
}

// open opens a conn, through the proxy with the token unless cfg says
// otherwise.
func open(t *testing.T, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = testDSN("")
		if cfg.Password.IsZero() {
			cfg.Password = driver.NewSecret(testToken)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := New().Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

func ctx30(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// collect runs q and drains it.
func collect(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	ctx := ctx30(t)
	rs, err := c.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatalf("Query(%s) rows: %v", q.Text, err)
	}
	return cols, rows, st
}

func colIndex(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %q in %v", name, cols)
	return -1
}
