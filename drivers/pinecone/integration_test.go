//go:build integration

package pinecone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// pineconeImage is pinned: v0.7.0 is the newest stable pinecone-local
// (latest points at v1.0.0.rc0, a release candidate). It is amd64 only and
// runs under emulation on arm64.
const pineconeImage = "ghcr.io/pinecone-io/pinecone-local:v0.7.0"

const (
	// testAPIKey is sent on every call; pinecone-local accepts any key.
	testAPIKey = "pclocal-test-key"
	// The indexes seeded by TestMain, all of dimension 4: People and the
	// vector fixture live in mainIndex (euclidean), the vector fixture
	// again in cosIndex and dotIndex.
	mainIndex  = "tobira"
	cosIndex   = "tobira-cos"
	dotIndex   = "tobira-dot"
	peopleNS   = "tobira_people"
	vectorsNS  = "vectors"
	controlPrt = "5080/tcp"
)

var (
	// controlAddr is the mapped host:port of the control plane.
	controlAddr string
	// indexAddr maps an index name to the mapped host:port of its data
	// plane. pinecone-local reports each index host on its own container
	// port (5081, 5082...), which the test maps to a random host port.
	indexAddr = map[string]string{}
	// indexMetric is the metric each seeded index was created with.
	indexMetric = map[string]string{mainIndex: metricEuclidean, cosIndex: metricCosine, dotIndex: metricDot}
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, pineconeImage,
		testcontainers.WithImagePlatform("linux/amd64"),
		testcontainers.WithEnv(map[string]string{"PORT": "5080", "PINECONE_HOST": "localhost"}),
		testcontainers.WithExposedPorts(controlPrt, "5081/tcp", "5082/tcp", "5083/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/indexes").WithPort(controlPrt).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start pinecone-local:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if controlAddr, err = ctr.PortEndpoint(ctx, controlPrt, ""); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	controlAddr = strings.Replace(controlAddr, "localhost", "127.0.0.1", 1)
	for _, name := range []string{mainIndex, cosIndex, dotIndex} {
		port, err := createIndex(ctx, name, indexMetric[name])
		if err != nil {
			fmt.Fprintln(os.Stderr, "create index:", err)
			return 1
		}
		addr, err := ctr.PortEndpoint(ctx, port+"/tcp", "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "index endpoint:", err)
			return 1
		}
		indexAddr[name] = strings.Replace(addr, "localhost", "127.0.0.1", 1)
	}
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

// createIndex creates a dense index of dimension 4 through the control
// plane and returns the container port of its host. It posts the request
// itself: pinecone-local speaks the older control-plane shape (dimension
// and metric at the top level), not the SDK's API version.
func createIndex(ctx context.Context, name, metric string) (string, error) {
	body := fmt.Sprintf(`{"name":%q,"dimension":4,"metric":%q,"spec":{"serverless":{"cloud":"aws","region":"us-east-1"}}}`, name, metric)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+controlAddr+"/indexes", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Api-Key", testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("create index %s: status %d, %v", name, resp.StatusCode, err)
	}
	_, port, ok := strings.Cut(out.Host, ":")
	if !ok {
		return "", fmt.Errorf("index %s host %q has no port", name, out.Host)
	}
	http.DefaultClient.CloseIdleConnections()
	return port, nil
}

// indexDSN is the host-mode DSN of a seeded index, with its metric.
func indexDSN(name string) string {
	return "pinecone://?host=" + indexAddr[name] + "&tls=false&index=" + name + "&metric=" + indexMetric[name]
}

// adminIndex returns a raw SDK connection to a seeded index; close it and
// the HTTP client's idle connections with the returned func.
func adminIndex(name, ns string) (*pinecone.IndexConnection, func(), error) {
	hc := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	pc, err := pinecone.NewClientBase(pinecone.NewClientBaseParams{Host: "http://" + controlAddr, Headers: map[string]string{"Api-Key": testAPIKey}, RestClient: hc})
	if err != nil {
		return nil, nil, err
	}
	ic, err := pc.Index(pinecone.NewIndexConnParams{Host: "http://" + indexAddr[name], Namespace: ns})
	if err != nil {
		return nil, nil, err
	}
	return ic, func() { _ = ic.Close(); hc.CloseIdleConnections() }, nil
}

// peopleVector is the dense vector a People record carries: Pinecone
// stores none without one.
func peopleVector(id int64) []float32 { return []float32{1, float32(id) / 100, 0, 0} }

func seed(ctx context.Context) error {
	ic, done, err := adminIndex(mainIndex, peopleNS)
	if err != nil {
		return err
	}
	defer done()
	var vecs []*pinecone.Vector
	for _, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		meta := &structpb.Struct{Fields: map[string]*structpb.Value{}}
		for i, col := range drivertest.People.Columns {
			v, err := toMeta(col.Name, r[i])
			if err != nil {
				return fmt.Errorf("people %d %s: %w", id, col.Name, err)
			}
			if v != nil {
				meta.Fields[col.Name] = v
			}
		}
		values := peopleVector(id)
		vecs = append(vecs, &pinecone.Vector{Id: strconv.FormatInt(id, 10), Values: &values, Metadata: meta})
	}
	if _, err := ic.UpsertVectors(ctx, vecs); err != nil {
		return fmt.Errorf("upsert people: %w", err)
	}
	for _, name := range []string{mainIndex, cosIndex, dotIndex} {
		vc, done, err := adminIndex(name, vectorsNS)
		if err != nil {
			return err
		}
		if _, err := vc.UpsertVectors(ctx, vectorRecords()); err != nil {
			done()
			return fmt.Errorf("upsert vectors into %s: %w", name, err)
		}
		done()
	}
	return nil
}

// vectorRecords is the vector fixture as records: id "1".."6", the vector,
// and metadata {id, label, n}.
func vectorRecords() []*pinecone.Vector {
	var out []*pinecone.Vector
	for _, r := range drivertest.VectorDataset().Rows {
		id, _ := r[0].AsInt()
		vec, _ := r[1].AsVector()
		obj, _ := r[2].AsObject()
		meta := &structpb.Struct{Fields: map[string]*structpb.Value{"id": structpb.NewNumberValue(float64(id))}}
		for _, m := range obj {
			v, _ := toMeta(m.Key, m.Value)
			meta.Fields[m.Key] = v
		}
		out = append(out, &pinecone.Vector{Id: strconv.FormatInt(id, 10), Values: &vec, Metadata: meta})
	}
	return out
}

// open opens a conn; cfg.DSN defaults to the main index.
func open(t *testing.T, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = indexDSN(mainIndex)
	}
	if cfg.Password.IsZero() {
		cfg.Password = driver.NewSecret(testAPIKey)
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

// collect drains a result set: collect(t)(c.Query(ctx, q)).
func collect(t *testing.T) func(driver.ResultSet, error) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	return func(rs driver.ResultSet, err error) ([]driver.Column, []driver.Row, driver.Stats) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		cols := rs.Columns()
		rows, st, err := driver.Collect(context.Background(), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		return cols, rows, st
	}
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
