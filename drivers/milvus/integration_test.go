//go:build integration

package milvus

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcmilvus "github.com/testcontainers/testcontainers-go/modules/milvus"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// milvusImage is pinned: bump it deliberately, with the client. It runs as
// a single container (milvus run standalone) with embedded etcd and local
// storage.
const milvusImage = "milvusdb/milvus:v2.6.25"

const (
	rootToken  = "root:Milvus" // the default root user, with authorization on
	peopleColl = "tobira_people"
	vecsColl   = drivertest.VectorCollection // three float vectors, one per metric
)

// userYAML turns authorization on, so tokens are checked.
const userYAML = `common:
  security:
    authorizationEnabled: true
`

// testAddr is the gRPC endpoint of the container shared by every test.
var testAddr string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcmilvus.Run(ctx, milvusImage,
		testcontainers.WithFiles(testcontainers.ContainerFile{
			ContainerFilePath: "/milvus/configs/user.yaml",
			Reader:            strings.NewReader(userYAML),
			FileMode:          0o644,
		}),
		// TestTextEmbeddingEgress runs a fake embedding provider on the
		// host: Docker Desktop resolves host.docker.internal itself, a
		// Linux engine needs this mapping.
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.ExtraHosts = append(hc.ExtraHosts, "host.docker.internal:host-gateway")
		}),
		// The module waits a minute; a cold standalone start under
		// emulation or on a busy laptop needs longer. Health first, then
		// the gRPC port.
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/healthz").WithPort("9091/tcp").WithStartupTimeout(4*time.Minute).WithPollInterval(time.Second),
			wait.ForListeningPort("19530/tcp").WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start milvus container:", err)
		if ctr != nil {
			_ = testcontainers.TerminateContainer(ctr)
		}
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	testAddr, err = ctr.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testAddr = strings.Replace(testAddr, "localhost", "127.0.0.1", 1)
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

// testDSN is the container's DSN with the given token in the password
// field.
func testDSN() string { return "milvus://root:Milvus@" + testAddr }

// admin returns an administrator client with the root token.
func admin(context.Context) (*adminClient, error) { return newAdminClient(testAddr, rootToken) }

// createIndexed creates a collection, indexes every vector field with
// AUTOINDEX under the given metric and, when load, loads it.
func createIndexed(ctx context.Context, c *adminClient, sch *entity.Schema, metrics map[string]entity.MetricType, load bool) error {
	if err := c.createCollection(ctx, sch); err != nil {
		return fmt.Errorf("create %s: %w", sch.CollectionName, err)
	}
	for field, metric := range metrics {
		if err := c.createIndex(ctx, sch.CollectionName, field, index.NewAutoIndex(metric)); err != nil {
			return fmt.Errorf("index %s.%s: %w", sch.CollectionName, field, err)
		}
	}
	if !load {
		return nil
	}
	if err := c.loadCollection(ctx, sch.CollectionName); err != nil {
		return fmt.Errorf("load %s: %w", sch.CollectionName, err)
	}
	return nil
}

func seed(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	c, err := admin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// People: score is a Double (Milvus has no decimal type), created_at
	// a Timestamptz, tags JSON, and a nullable vector, since every Milvus
	// collection needs one.
	people := entity.NewSchema().WithName(peopleColl).WithDynamicFieldEnabled(true).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("name").WithDataType(entity.FieldTypeVarChar).WithMaxLength(256)).
		WithField(entity.NewField().WithName("email").WithDataType(entity.FieldTypeVarChar).WithMaxLength(256)).
		WithField(entity.NewField().WithName("age").WithDataType(entity.FieldTypeInt64).WithNullable(true)).
		WithField(entity.NewField().WithName("score").WithDataType(entity.FieldTypeDouble)).
		WithField(entity.NewField().WithName("active").WithDataType(entity.FieldTypeBool)).
		WithField(entity.NewField().WithName("created_at").WithDataType(entity.FieldTypeTimestamptz)).
		WithField(entity.NewField().WithName("tags").WithDataType(entity.FieldTypeJSON)).
		WithField(entity.NewField().WithName("embedding").WithDataType(entity.FieldTypeFloatVector).WithDim(2).WithNullable(true))
	if err := createIndexed(ctx, c, people, map[string]entity.MetricType{"embedding": entity.L2}, true); err != nil {
		return err
	}
	var (
		ids, ages     []int64
		ageValid      []bool
		names, emails []string
		scores        []float64
		actives       []bool
		created       []time.Time
		tags          [][]byte
		vecs          [][]float32
		vecValid      []bool
	)
	for _, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		ids = append(ids, id)
		n, _ := r[1].AsText()
		names = append(names, n)
		e, _ := r[2].AsText()
		emails = append(emails, e)
		if a, ok := r[3].AsInt(); ok && !r[3].IsNull() {
			ages = append(ages, a)
			ageValid = append(ageValid, true)
		} else {
			ageValid = append(ageValid, false)
		}
		sc, _ := r[4].AsText()
		var f float64
		_, _ = fmt.Sscan(sc, &f)
		scores = append(scores, f)
		b, _ := r[5].AsBool()
		actives = append(actives, b)
		t, _ := r[6].AsTime()
		created = append(created, t)
		j, _ := r[7].AsJSON()
		tags = append(tags, j)
		vecValid = append(vecValid, false)
	}
	ageCol, err := column.NewNullableColumnInt64("age", ages, ageValid)
	if err != nil {
		return err
	}
	vecCol, err := column.NewNullableColumnFloatVector("embedding", 2, vecs, vecValid)
	if err != nil {
		return err
	}
	if err := c.insert(ctx, peopleColl,
		column.NewColumnInt64("id", ids), column.NewColumnVarChar("name", names), column.NewColumnVarChar("email", emails),
		ageCol, column.NewColumnDouble("score", scores), column.NewColumnBool("active", actives),
		column.NewColumnTimestamptz("created_at", created), column.NewColumnJSONBytes("tags", tags), vecCol); err != nil {
		return fmt.Errorf("insert people: %w", err)
	}

	// vecs: the vector fixture under each metric, as three vector fields,
	// with its payload as dynamic keys.
	vecsSchema := entity.NewSchema().WithName(vecsColl).WithDynamicFieldEnabled(true).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("vec_l2").WithDataType(entity.FieldTypeFloatVector).WithDim(4)).
		WithField(entity.NewField().WithName("vec_cos").WithDataType(entity.FieldTypeFloatVector).WithDim(4)).
		WithField(entity.NewField().WithName("vec_ip").WithDataType(entity.FieldTypeFloatVector).WithDim(4))
	if err := createIndexed(ctx, c, vecsSchema, map[string]entity.MetricType{"vec_l2": entity.L2, "vec_cos": entity.COSINE, "vec_ip": entity.IP}, true); err != nil {
		return err
	}
	var (
		vids   []int64
		vv     [][]float32
		labels []string
		ns     []int64
	)
	for _, r := range drivertest.VectorDataset().Rows {
		id, _ := r[0].AsInt()
		vec, _ := r[1].AsVector()
		obj, _ := r[2].AsObject()
		l, _ := obj.Get("label")
		n, _ := obj.Get("n")
		ls, _ := l.AsText()
		ni, _ := n.AsInt()
		vids, vv, labels, ns = append(vids, id), append(vv, vec), append(labels, ls), append(ns, ni)
	}
	if err := c.insert(ctx, vecsColl,
		column.NewColumnInt64("id", vids), column.NewColumnFloatVector("vec_l2", 4, vv), column.NewColumnFloatVector("vec_cos", 4, vv),
		column.NewColumnFloatVector("vec_ip", 4, vv), column.NewColumnVarChar("label", labels), column.NewColumnInt64("n", ns)); err != nil {
		return fmt.Errorf("insert vecs: %w", err)
	}
	return nil
}

// open opens a conn with cfg's DSN defaulting to the root token.
func open(t *testing.T, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = testDSN()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// adminT is admin for a test, closed at its end.
func adminT(t *testing.T) *adminClient {
	t.Helper()
	c, err := admin(ctx30(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctx30(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

// browse runs one Browse page and drains it.
func browse(t *testing.T, c *conn, ref driver.CollectionRef, opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	ctx := ctx30(t)
	rs, err := c.Browse(ctx, ref, opts)
	if err != nil {
		t.Fatalf("Browse(%s, %+v): %v", ref.Name, opts, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
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
