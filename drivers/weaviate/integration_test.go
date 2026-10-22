//go:build integration

package weaviate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// weaviateImage is pinned: bump it deliberately, with the client.
const weaviateImage = "cr.weaviate.io/semitechnologies/weaviate:1.40.2"

const (
	testAPIKey = "tobira-test-key"
	// Classes seeded by TestMain.
	peopleColl = "TobiraPeople"
	vecsColl   = "Vecs" // three named vectors, one per metric
	docsColl   = "Docs"
)

var (
	// testHost is the anonymous container, testAuthHost the API-key one
	// (host:port each).
	testHost, testAuthHost string
	leakBaseline           goleak.Option
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	anon, err := startWeaviate(ctx, map[string]string{"AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED": "true"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start weaviate:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(anon) }()
	auth, err := startWeaviate(ctx, map[string]string{
		"AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED": "false",
		"AUTHENTICATION_APIKEY_ENABLED":           "true",
		"AUTHENTICATION_APIKEY_ALLOWED_KEYS":      testAPIKey,
		"AUTHENTICATION_APIKEY_USERS":             "tobira",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start weaviate (api key):", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(auth) }()
	if testHost, err = endpoint(ctx, anon); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	if testAuthHost, err = endpoint(ctx, auth); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	leakBaseline = goleak.IgnoreCurrent()
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(leakBaseline); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

func startWeaviate(ctx context.Context, env map[string]string) (testcontainers.Container, error) {
	base := map[string]string{
		"PERSISTENCE_DATA_PATH":     "/var/lib/weaviate",
		"DEFAULT_VECTORIZER_MODULE": "none",
		"CLUSTER_HOSTNAME":          "node1",
		"DISABLE_TELEMETRY":         "true",
		"QUERY_DEFAULTS_LIMIT":      "10",
	}
	for k, v := range env {
		base[k] = v
	}
	return testcontainers.Run(ctx, weaviateImage,
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithEnv(base),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/v1/.well-known/ready").WithPort("8080/tcp").WithStartupTimeout(2*time.Minute)),
	)
}

func endpoint(ctx context.Context, c testcontainers.Container) (string, error) {
	ep, err := c.PortEndpoint(ctx, "8080/tcp", "")
	if err != nil {
		return "", err
	}
	return strings.Replace(ep, "localhost", "127.0.0.1", 1), nil
}

// testDSN is the anonymous container's DSN.
func testDSN() string { return "weaviate://" + testHost }

func open(t testing.TB, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = testDSN()
	}
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// rest calls the anonymous container's REST API directly (seeding and
// checks independent of the driver).
func rest(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+testHost+"/v1"+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	return out, resp.StatusCode, err
}

func mustREST(ctx context.Context, method, path string, body any) error {
	out, code, err := rest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, code, out)
	}
	// A batch answers 200 with per-object errors.
	if strings.HasPrefix(path, "/batch") && bytes.Contains(out, []byte(`"errors"`)) && !bytes.Contains(out, []byte(`"errors":null`)) {
		return fmt.Errorf("batch: %s", out)
	}
	return nil
}

func seed(ctx context.Context) error {
	if err := mustREST(ctx, http.MethodPost, "/schema", map[string]any{
		"class":               peopleColl,
		"description":         "the drivertest People fixture",
		"vectorizer":          "none",
		"invertedIndexConfig": map[string]any{"indexNullState": true},
		"properties": []map[string]any{
			{"name": "name", "dataType": []string{"text"}},
			{"name": "email", "dataType": []string{"text"}, "tokenization": "field"},
			{"name": "age", "dataType": []string{"int"}, "indexRangeFilters": true},
			{"name": "score", "dataType": []string{"number"}},
			{"name": "active", "dataType": []string{"boolean"}},
			{"name": "created_at", "dataType": []string{"date"}},
			{"name": "tags", "dataType": []string{"text[]"}},
		},
	}); err != nil {
		return fmt.Errorf("create people: %w", err)
	}
	var objs []map[string]any
	for _, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		props := map[string]any{}
		for i, col := range drivertest.People.Columns[1:] {
			v := r[i+1]
			switch {
			case v.IsNull():
				continue
			case col.Name == "tags":
				raw, _ := v.AsJSON()
				props[col.Name] = json.RawMessage(raw)
			case col.Name == "score":
				s, _ := v.AsText()
				props[col.Name] = json.Number(s)
			case col.Name == "created_at":
				tm, _ := v.AsTime()
				props[col.Name] = tm.Format(time.RFC3339Nano)
			default:
				props[col.Name] = v.Any()
			}
		}
		objs = append(objs, map[string]any{"class": peopleColl, "id": intUUID(id), "properties": props})
	}
	if err := mustREST(ctx, http.MethodPost, "/batch/objects", map[string]any{"objects": objs}); err != nil {
		return fmt.Errorf("seed people: %w", err)
	}

	flat := func(distance string) map[string]any {
		return map[string]any{"vectorizer": map[string]any{"none": map[string]any{}}, "vectorIndexType": "flat", "vectorIndexConfig": map[string]any{"distance": distance}}
	}
	if err := mustREST(ctx, http.MethodPost, "/schema", map[string]any{
		"class": vecsColl,
		"properties": []map[string]any{{"name": "payload", "dataType": []string{"object"}, "nestedProperties": []map[string]any{
			{"name": "label", "dataType": []string{"text"}},
			{"name": "n", "dataType": []string{"int"}},
		}}},
		"vectorConfig": map[string]any{"l2": flat("l2-squared"), "cos": flat("cosine"), "dot": flat("dot")},
	}); err != nil {
		return fmt.Errorf("create vecs: %w", err)
	}
	objs = nil
	for _, r := range drivertest.VectorDataset().Rows {
		id, _ := r[0].AsInt()
		vec, _ := r[1].AsVector()
		payload, _ := r[2].AsObject()
		p := map[string]any{}
		for _, m := range payload {
			p[m.Key] = m.Value.Any()
		}
		objs = append(objs, map[string]any{"class": vecsColl, "id": intUUID(id), "properties": map[string]any{"payload": p},
			"vectors": map[string]any{"l2": vec, "cos": vec, "dot": vec}})
	}
	if err := mustREST(ctx, http.MethodPost, "/batch/objects", map[string]any{"objects": objs}); err != nil {
		return fmt.Errorf("seed vecs: %w", err)
	}

	if err := mustREST(ctx, http.MethodPost, "/schema", map[string]any{
		"class":      docsColl,
		"vectorizer": "none",
		"properties": []map[string]any{
			{"name": "title", "dataType": []string{"text"}},
			{"name": "body", "dataType": []string{"text"}},
			{"name": "uid", "dataType": []string{"uuid"}},
			{"name": "where", "dataType": []string{"geoCoordinates"}},
			{"name": "ratings", "dataType": []string{"number[]"}},
			{"name": "meta", "dataType": []string{"object"}, "nestedProperties": []map[string]any{
				{"name": "lang", "dataType": []string{"text"}},
				{"name": "pages", "dataType": []string{"int"}},
			}},
		},
	}); err != nil {
		return fmt.Errorf("create docs: %w", err)
	}
	objs = nil
	for i, d := range drivertest.SearchDocuments() {
		title, _ := d.Get("title")
		body, _ := d.Get("body")
		t, _ := title.AsText()
		b, _ := body.AsText()
		vec := []float32{float32(i + 1), 0, 0, 1}
		objs = append(objs, map[string]any{"class": docsColl, "id": intUUID(int64(i + 1)), "vector": vec, "properties": map[string]any{
			"title": t, "body": b, "uid": "5c56c793-69f3-4fbf-87e6-c4bf54c28c26",
			"where":   map[string]any{"latitude": 52.37, "longitude": 4.9},
			"ratings": []float64{1.5, float64(i)},
			"meta":    map[string]any{"lang": "en", "pages": i * 10},
		}})
	}
	if err := mustREST(ctx, http.MethodPost, "/batch/objects", map[string]any{"objects": objs}); err != nil {
		return fmt.Errorf("seed docs: %w", err)
	}
	return nil
}

// query runs q on c and collects every row.
func query(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatalf("Query(%s): reading: %v", q.Text, err)
	}
	return cols, rows, st
}

func colNames(cols []driver.Column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return strings.Join(names, ",")
}

func col(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %q in %s", name, colNames(cols))
	return -1
}
