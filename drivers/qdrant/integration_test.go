//go:build integration

package qdrant

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
	"github.com/testcontainers/testcontainers-go"
	tcqdrant "github.com/testcontainers/testcontainers-go/modules/qdrant"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// qdrantImage is pinned: bump it deliberately, with the go-client.
const qdrantImage = "qdrant/qdrant:v1.19.2"

const (
	testAPIKey   = "tobira-test-key"
	testROAPIKey = "tobira-read-only-key"
	// Collections seeded by TestMain.
	peopleColl = "tobira_people"
	vecsColl   = drivertest.VectorCollection // three named vectors, one per metric
	docsColl   = drivertest.DocumentsCollection
)

// testHostPort is the gRPC endpoint of the container shared by every test.
var testHostPort string

// leakBaseline is taken after the container exists, so testcontainers'
// goroutines are not counted.
var leakBaseline goleak.Option

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcqdrant.Run(ctx, qdrantImage, testcontainers.WithEnv(map[string]string{
		"QDRANT__SERVICE__API_KEY":           testAPIKey,
		"QDRANT__SERVICE__READ_ONLY_API_KEY": testROAPIKey,
		"QDRANT__TELEMETRY_DISABLED":         "true",
	}))
	if err != nil {
		fmt.Fprintln(os.Stderr, "start qdrant container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	testHostPort, err = ctr.GRPCEndpoint(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	// Docker Desktop answers "localhost" on ::1 only after gRPC's reconnect
	// backoff (1-3 s per Open); the IPv4 loopback answers at once.
	testHostPort = strings.Replace(testHostPort, "localhost", "127.0.0.1", 1)
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

// testDSN is the container's DSN with the given API key.
func testDSN(key string) string { return "qdrant://:" + key + "@" + testHostPort }

// admin returns a raw client with the full API key.
func admin() (*pb.Client, error) {
	host, port, _ := strings.Cut(testHostPort, ":")
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	return pb.NewClient(&pb.Config{Host: host, Port: p, APIKey: testAPIKey, SkipCompatibilityCheck: true, PoolSize: 1})
}

func seed(ctx context.Context) error {
	c, err := admin()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	wait := true
	// People: no vectors; score and tags declared in the metadata,
	// created_at typed by its datetime index, age sortable.
	types, _ := pb.NewStruct(map[string]any{"score": "decimal", "tags": "json"})
	if err := c.CreateCollection(ctx, &pb.CreateCollection{
		CollectionName: peopleColl,
		VectorsConfig:  pb.NewVectorsConfigMap(map[string]*pb.VectorParams{}),
		Metadata:       map[string]*pb.Value{metadataKey: pb.NewValueFromFields(map[string]*pb.Value{"types": pb.NewValueStruct(types)})},
	}); err != nil {
		return fmt.Errorf("create people: %w", err)
	}
	for key, typ := range map[string]pb.FieldType{"age": pb.FieldType_FieldTypeInteger, "created_at": pb.FieldType_FieldTypeDatetime} {
		if _, err := c.CreateFieldIndex(ctx, &pb.CreateFieldIndexCollection{CollectionName: peopleColl, FieldName: key, FieldType: &typ, Wait: &wait}); err != nil {
			return fmt.Errorf("index %s: %w", key, err)
		}
	}
	var pts []*pb.PointStruct
	for _, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		payload := map[string]*pb.Value{}
		for i, col := range drivertest.People.Columns[1:] {
			v, err := toValue(r[i+1])
			if err != nil {
				return err
			}
			payload[col.Name] = v
		}
		pts = append(pts, &pb.PointStruct{Id: pb.NewIDNum(uint64(id)), Payload: payload, Vectors: pb.NewVectorsMap(map[string]*pb.Vector{})})
	}
	if _, err := c.Upsert(ctx, &pb.UpsertPoints{CollectionName: peopleColl, Wait: &wait, Points: pts}); err != nil {
		return fmt.Errorf("upsert people: %w", err)
	}

	// vecs: the vector fixture under each metric, as three named vectors.
	params := func(d pb.Distance) *pb.VectorParams { return &pb.VectorParams{Size: 4, Distance: d} }
	if err := c.CreateCollection(ctx, &pb.CreateCollection{
		CollectionName: vecsColl,
		VectorsConfig: pb.NewVectorsConfigMap(map[string]*pb.VectorParams{
			"vec_l2": params(pb.Distance_Euclid), "vec_cos": params(pb.Distance_Cosine), "vec_ip": params(pb.Distance_Dot),
		}),
	}); err != nil {
		return fmt.Errorf("create vecs: %w", err)
	}
	pts = nil
	for _, r := range drivertest.VectorDataset().Rows {
		id, _ := r[0].AsInt()
		vec, _ := r[1].AsVector()
		obj, _ := r[2].AsObject()
		payload := map[string]*pb.Value{}
		for _, m := range obj {
			v, _ := toValue(m.Value)
			payload[m.Key] = v
		}
		pts = append(pts, &pb.PointStruct{Id: pb.NewIDNum(uint64(id)), Payload: payload, Vectors: pb.NewVectorsMap(map[string]*pb.Vector{
			"vec_l2": pb.NewVectorDense(vec), "vec_cos": pb.NewVectorDense(vec), "vec_ip": pb.NewVectorDense(vec),
		})})
	}
	if _, err := c.Upsert(ctx, &pb.UpsertPoints{CollectionName: vecsColl, Wait: &wait, Points: pts}); err != nil {
		return fmt.Errorf("upsert vecs: %w", err)
	}

	// docs: the schemaless document fixture, ids 1..5.
	if err := c.CreateCollection(ctx, &pb.CreateCollection{CollectionName: docsColl, VectorsConfig: pb.NewVectorsConfigMap(map[string]*pb.VectorParams{})}); err != nil {
		return fmt.Errorf("create docs: %w", err)
	}
	pts = nil
	for i, doc := range drivertest.Documents() {
		payload := map[string]*pb.Value{}
		for _, m := range doc {
			v, _ := toValue(m.Value)
			payload[m.Key] = v
		}
		pts = append(pts, &pb.PointStruct{Id: pb.NewIDNum(uint64(i + 1)), Payload: payload, Vectors: pb.NewVectorsMap(map[string]*pb.Vector{})})
	}
	if _, err := c.Upsert(ctx, &pb.UpsertPoints{CollectionName: docsColl, Wait: &wait, Points: pts}); err != nil {
		return fmt.Errorf("upsert docs: %w", err)
	}
	return nil
}

// createColl creates a scratch collection with one unnamed vector and
// drops it when the test ends.
func createColl(t *testing.T, name string, size uint64, d pb.Distance) *pb.Client {
	t.Helper()
	c, err := admin()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.CreateCollection(ctx, &pb.CreateCollection{CollectionName: name, VectorsConfig: pb.NewVectorsConfig(&pb.VectorParams{Size: size, Distance: d})}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.DeleteCollection(context.Background(), name)
		_ = c.Close()
	})
	return c
}

// open opens a conn with cfg's DSN defaulting to the full key.
func open(t *testing.T, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = testDSN(testAPIKey)
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
