//go:build integration

package elasticsearch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tces "github.com/testcontainers/testcontainers-go/modules/elasticsearch"
	tcos "github.com/testcontainers/testcontainers-go/modules/opensearch"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// Images are pinned: bump them deliberately. One engine runs per test
// process (the laptop's disk and memory allow one search engine at a
// time): Elasticsearch by default, OpenSearch with
// TOBIRA_SEARCH_ENGINE=opensearch (security plugin off, as the
// testcontainers module sets it) or TOBIRA_SEARCH_ENGINE=opensearch-secure
// (security plugin on: TLS with the demo CA and basic auth).
const (
	esImage = "docker.elastic.co/elasticsearch/elasticsearch:8.19.23"
	osImage = "opensearchproject/opensearch:2.19.7"
)

const (
	testPassword = "tobira-test-pw"
	// osSecurePassword meets OpenSearch's initial admin password rules.
	osSecurePassword = "Tobira-Test-Pw-2026!"
	peopleIndex      = "tobira_people"
	vecsIndex        = drivertest.VectorCollection
)

// engine is the running server shared by every test.
var engine struct {
	fl       flavor
	hostPort string
	tls      bool
	caFile   string
	client   *http.Client
	user     string
	password string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	var ctr testcontainers.Container
	var err error
	switch os.Getenv("TOBIRA_SEARCH_ENGINE") {
	case "opensearch":
		ctr, err = startOpenSearch(ctx)
	case "opensearch-secure":
		ctr, err = startOpenSearchSecure(ctx)
	default:
		ctr, err = startElasticsearch(ctx)
	}
	if ctr != nil {
		defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "start search engine:", err)
		return 1
	}
	if engine.caFile != "" {
		defer func() { _ = os.RemoveAll(filepath.Dir(engine.caFile)) }()
	}
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	code := m.Run()
	engine.client.CloseIdleConnections()
	return code
}

func startElasticsearch(ctx context.Context) (testcontainers.Container, error) {
	ctr, err := tces.Run(ctx, esImage,
		tces.WithPassword(testPassword),
		testcontainers.WithEnv(map[string]string{
			"ES_JAVA_OPTS":          "-Xms512m -Xmx512m",
			"xpack.ml.enabled":      "false",
			"xpack.watcher.enabled": "false",
		}),
	)
	if err != nil {
		return ctr, err
	}
	u, err := url.Parse(ctr.Settings.Address)
	if err != nil {
		return ctr, err
	}
	dir, err := os.MkdirTemp("", "tobira-es-ca")
	if err != nil {
		return ctr, err
	}
	engine.caFile = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(engine.caFile, ctr.Settings.CACert, 0o600); err != nil {
		return ctr, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ctr.Settings.CACert)
	engine.fl, engine.hostPort, engine.tls, engine.user, engine.password = flavorElastic, u.Host, true, "elastic", testPassword
	engine.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, Timeout: time.Minute}
	return ctr, nil
}

func startOpenSearch(ctx context.Context) (testcontainers.Container, error) {
	ctr, err := tcos.Run(ctx, osImage, testcontainers.WithEnv(map[string]string{
		"OPENSEARCH_JAVA_OPTS": "-Xms512m -Xmx512m",
	}))
	if err != nil {
		return ctr, err
	}
	addr, err := ctr.Address(ctx)
	if err != nil {
		return ctr, err
	}
	u, err := url.Parse(addr)
	if err != nil {
		return ctr, err
	}
	engine.fl, engine.hostPort = flavorOpenSearch, u.Host
	engine.client = &http.Client{Timeout: time.Minute}
	return ctr, nil
}

// startOpenSearchSecure runs OpenSearch with its security plugin on: the
// demo TLS certificates (the root CA is copied out for ca_file) and the
// admin user.
func startOpenSearchSecure(ctx context.Context) (testcontainers.Container, error) {
	ctr, err := testcontainers.Run(ctx, osImage,
		testcontainers.WithExposedPorts("9200/tcp"),
		testcontainers.WithEnv(map[string]string{
			"discovery.type":                    "single-node",
			"OPENSEARCH_INITIAL_ADMIN_PASSWORD": osSecurePassword,
			"OPENSEARCH_JAVA_OPTS":              "-Xms512m -Xmx512m",
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("9200/tcp").
			WithTLS(true, &tls.Config{InsecureSkipVerify: true}). //nolint:gosec // only to wait for startup
			WithBasicAuth("admin", osSecurePassword).
			WithStatusCodeMatcher(func(s int) bool { return s == http.StatusOK }).
			WithStartupTimeout(4*time.Minute)),
	)
	if err != nil {
		return ctr, err
	}
	rc, err := ctr.CopyFileFromContainer(ctx, "/usr/share/opensearch/config/root-ca.pem")
	if err != nil {
		return ctr, err
	}
	pem, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return ctr, err
	}
	dir, err := os.MkdirTemp("", "tobira-os-ca")
	if err != nil {
		return ctr, err
	}
	engine.caFile = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(engine.caFile, pem, 0o600); err != nil {
		return ctr, err
	}
	ep, err := ctr.PortEndpoint(ctx, "9200/tcp", "")
	if err != nil {
		return ctr, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	engine.fl, engine.hostPort, engine.tls, engine.user, engine.password = flavorOpenSearch, ep, true, "admin", osSecurePassword
	engine.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, Timeout: time.Minute}
	return ctr, nil
}

// testDriver is the driver under test for the running engine.
func testDriver() driver.Driver { return esDriver{fl: engine.fl} }

// testDSN is the DSN of the running engine, with the password from a ref.
func testDSN() string {
	dsn := engine.fl.name() + "://"
	if engine.user != "" {
		dsn += engine.user + "@"
	}
	dsn += engine.hostPort
	if engine.tls {
		return dsn + "?ca_file=" + url.QueryEscape(engine.caFile)
	}
	return dsn + "?tls=false"
}

func testConfig(readOnly bool) driver.ConnConfig {
	cfg := driver.ConnConfig{DSN: testDSN(), ReadOnly: readOnly}
	if engine.user != "" {
		cfg.Password = driver.NewSecret(engine.password)
	}
	return cfg
}

// openTest opens a conn to the running engine, closed at cleanup.
func openTest(t testing.TB, readOnly bool) *conn {
	t.Helper()
	c, err := testDriver().Open(context.Background(), testConfig(readOnly))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// admin sends one request to the engine as its superuser, outside the
// driver, and fails on a non-2xx answer.
func admin(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	ct := "application/json"
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
		if strings.Contains(path, "_bulk") {
			ct = "application/x-ndjson"
		}
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	scheme := "http"
	if engine.tls {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+engine.hostPort+path, rd)
	if err != nil {
		return nil, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", ct)
	}
	if engine.user != "" {
		req.SetBasicAuth(engine.user, engine.password)
	}
	res, err := engine.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode > 299 {
		return data, fmt.Errorf("%s %s: HTTP %d: %s", method, path, res.StatusCode, data)
	}
	return data, nil
}

func mustAdmin(t testing.TB, method, path string, body any) []byte {
	t.Helper()
	data, err := admin(context.Background(), method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// bulkLines builds an NDJSON bulk body indexing docs into index.
func bulkLines(index string, ids []string, docs []json.RawMessage) string {
	var b strings.Builder
	for i, d := range docs {
		meta, _ := json.Marshal(map[string]any{"index": map[string]string{"_index": index, "_id": ids[i]}})
		b.Write(meta)
		b.WriteByte('\n')
		b.Write(d)
		b.WriteByte('\n')
	}
	return b.String()
}

func bulk(ctx context.Context, index string, ids []string, docs []json.RawMessage) error {
	data, err := admin(ctx, http.MethodPost, "/_bulk?refresh=true", bulkLines(index, ids, docs))
	if err != nil {
		return err
	}
	var r struct {
		Errors bool `json:"errors"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Errors {
		return fmt.Errorf("bulk into %s failed: %s", index, data)
	}
	return nil
}

// rowDoc encodes a dataset row as a source document.
func rowDoc(cols []driver.Column, row driver.Row) (json.RawMessage, error) {
	obj := make(driver.Object, len(cols))
	for i, c := range cols {
		obj[i] = driver.Member{Key: c.Name, Value: row[i]}
	}
	return objectJSON(obj, nil)
}

// seed creates People, the search fixture and (Elasticsearch only) the
// vector fixture.
func seed(ctx context.Context) error {
	people := map[string]any{"mappings": map[string]any{"properties": map[string]any{
		"id":         map[string]any{"type": "integer"},
		"name":       map[string]any{"type": "text", "fields": map[string]any{"keyword": map[string]any{"type": "keyword"}}},
		"email":      map[string]any{"type": "keyword"},
		"age":        map[string]any{"type": "integer"},
		"score":      map[string]any{"type": "scaled_float", "scaling_factor": 100},
		"active":     map[string]any{"type": "boolean"},
		"created_at": map[string]any{"type": "date"},
		"tags":       map[string]any{"type": "object", "enabled": false},
	}}}
	if _, err := admin(ctx, http.MethodPut, "/"+peopleIndex, people); err != nil {
		return err
	}
	ds := drivertest.PeopleDataset()
	ids := make([]string, len(ds.Rows))
	docs := make([]json.RawMessage, len(ds.Rows))
	for i, row := range ds.Rows {
		n, _ := row[0].AsInt()
		ids[i] = strconv.FormatInt(n, 10)
		d, err := rowDoc(ds.Columns, row)
		if err != nil {
			return err
		}
		docs[i] = d
	}
	if err := bulk(ctx, peopleIndex, ids, docs); err != nil {
		return err
	}
	articles := map[string]any{"mappings": map[string]any{"properties": map[string]any{
		"title":  map[string]any{"type": "text"},
		"body":   map[string]any{"type": "text"},
		"tags":   map[string]any{"type": "keyword"},
		"views":  map[string]any{"type": "integer"},
		"rating": map[string]any{"type": "float"},
		"author": map[string]any{"properties": map[string]any{
			"name": map[string]any{"type": "keyword"}, "country": map[string]any{"type": "keyword"},
		}},
	}}}
	if _, err := admin(ctx, http.MethodPut, "/"+drivertest.SearchCollection, articles); err != nil {
		return err
	}
	ids, docs = nil, nil
	for _, d := range drivertest.SearchDocuments() {
		id, _ := d.Get("_id")
		s, _ := id.AsText()
		raw, err := objectJSON(d, map[string]bool{"_id": true})
		if err != nil {
			return err
		}
		ids, docs = append(ids, s), append(docs, raw)
	}
	if err := bulk(ctx, drivertest.SearchCollection, ids, docs); err != nil {
		return err
	}
	if engine.fl != flavorElastic {
		return nil
	}
	vecs := map[string]any{"mappings": map[string]any{"properties": map[string]any{
		"id":      map[string]any{"type": "integer"},
		"vec":     map[string]any{"type": "dense_vector", "dims": 4, "index": true, "similarity": "l2_norm"},
		"payload": map[string]any{"properties": map[string]any{"label": map[string]any{"type": "keyword"}, "n": map[string]any{"type": "integer"}}},
	}}}
	if _, err := admin(ctx, http.MethodPut, "/"+vecsIndex, vecs); err != nil {
		return err
	}
	vds := drivertest.VectorDataset()
	ids, docs = nil, nil
	for _, row := range vds.Rows {
		n, _ := row[0].AsInt()
		d, err := rowDoc(vds.Columns, row)
		if err != nil {
			return err
		}
		ids, docs = append(ids, strconv.FormatInt(n, 10)), append(docs, d)
	}
	return bulk(ctx, vecsIndex, ids, docs)
}
