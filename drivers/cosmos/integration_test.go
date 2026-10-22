//go:build integration

package cosmos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	// emulatorImage is the Linux emulator (vNext preview) of 2026-10-19;
	// vnext-preview pointed at this build (index digest
	// sha256:db53613dae19d0a3d5ad50688d14aa6e957af082dafea56b52e1bd3a6b3f1406)
	// when the suite was written.
	emulatorImage = "mcr.microsoft.com/cosmosdb/linux/azure-cosmos-emulator:vnext-EN20261019"
	// emulatorKey is the emulator's well-known, public account key. The
	// emulator accepts any key, so BadAuth cannot be tested against it.
	emulatorKey = "C2y6yDjf5/R+ob0N8A7Cgv30VRDJIWEHLM+4QDU5DE2nQ9nDuVTqobD4b8mGGyPMbIZnqyMsEcaGQy67XIw/Jw=="
	testDB      = "tobira"
	otherDB     = "tobira_other"
	vectorsName = "tobira_vectors"
	ordersName  = "orders"
	hierName    = "events"
	bigName     = "tobira_big"
	bigSize     = 250
	bigLateFrom = 201 // items from here on hold an extra "late" field
)

// testHostPort is the emulator shared by every test in the package; admin
// is an SDK client on it for seeding and checks, outside the driver.
var (
	testHostPort string
	admin        *azcosmos.Client
	leakBaseline goleak.Option
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        emulatorImage,
			Cmd:          []string{"--protocol", "http", "--enable-explorer", "false"},
			ExposedPorts: []string{"8081/tcp"},
			WaitingFor: wait.ForAll(
				wait.ForLog("System is now fully ready to accept requests"),
				wait.ForListeningPort("8081/tcp"),
			).WithDeadline(4 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start cosmos emulator:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if testHostPort, err = ctr.PortEndpoint(ctx, "8081/tcp", ""); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	cred, err := azcosmos.NewKeyCredential(emulatorKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "key:", err)
		return 1
	}
	if admin, err = azcosmos.NewClientWithKey("http://"+testHostPort+"/", cred, nil); err != nil {
		fmt.Fprintln(os.Stderr, "admin client:", err)
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
			code = 1
		}
	}
	admin.Close()
	return code
}

// dsnFor returns a DSN for the emulator with database db ("" for none).
func dsnFor(db string) string {
	dsn := "cosmos://" + testHostPort + "?tls=false"
	if db != "" {
		dsn += "&database=" + db
	}
	return dsn
}

// openTest opens the driver on database db and closes it with the test.
func openTest(t testing.TB, db string, readOnly bool) *conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor(db), Password: driver.NewSecret(emulatorKey), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

func createContainer(ctx context.Context, db string, props azcosmos.ContainerProperties) (*azcosmos.ContainerClient, error) {
	d, err := admin.NewDatabase(db)
	if err != nil {
		return nil, err
	}
	if _, err := d.CreateContainer(ctx, props, nil); err != nil {
		return nil, err
	}
	return d.NewContainer(props.ID)
}

func createDatabase(ctx context.Context, db string) error {
	_, err := admin.CreateDatabase(ctx, azcosmos.DatabaseProperties{ID: db}, nil)
	return err
}

func dropDatabase(db string) {
	if d, err := admin.NewDatabase(db); err == nil {
		_, _ = d.Delete(context.Background(), nil)
	}
}

// peopleDoc is People row r as a Cosmos DB item: id as its decimal text,
// score a number, created_at RFC 3339 text and tags a JSON array.
func peopleDoc(r driver.Row) ([]byte, string, error) {
	var obj driver.Object
	id := ""
	for k, col := range drivertest.People.Columns {
		v := r[k]
		switch col.Name {
		case "id":
			n, _ := v.AsInt()
			id = strconv.FormatInt(n, 10)
			v = driver.String(id)
		case "created_at":
			ts, _ := v.AsTime()
			v = driver.String(ts.UTC().Format(time.RFC3339Nano))
		}
		obj = append(obj, driver.Member{Key: col.Name, Value: v})
	}
	b, err := encodeValue(driver.ObjectValue(obj...))
	return b, id, err
}

// seedPeople creates db.tobira_people (partition key /id) and loads People.
func seedPeople(ctx context.Context, db string) error {
	cc, err := createContainer(ctx, db, azcosmos.ContainerProperties{
		ID: drivertest.PeopleCollection(), PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{Paths: []string{"/id"}},
	})
	if err != nil {
		return err
	}
	for _, r := range drivertest.People.Rows {
		b, id, err := peopleDoc(r)
		if err != nil {
			return err
		}
		if _, err := cc.CreateItem(ctx, azcosmos.NewPartitionKeyString(id), b, nil); err != nil {
			return err
		}
	}
	return nil
}

// seedVectors creates db.tobira_vectors with a cosine vector policy on
// /vec and loads drivertest.VectorDataset with string ids.
func seedVectors(ctx context.Context, db string) error {
	cc, err := createContainer(ctx, db, azcosmos.ContainerProperties{
		ID:                     vectorsName,
		PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{Paths: []string{"/id"}},
		VectorEmbeddingPolicy: &azcosmos.VectorEmbeddingPolicy{VectorEmbeddings: []azcosmos.VectorEmbedding{
			{Path: "/vec", DataType: azcosmos.VectorDataTypeFloat32, DistanceFunction: azcosmos.VectorDistanceFunctionCosine, Dimensions: 4},
		}},
		IndexingPolicy: &azcosmos.IndexingPolicy{
			Automatic: true, IndexingMode: azcosmos.IndexingModeConsistent,
			IncludedPaths: []azcosmos.IncludedPath{{Path: "/*"}},
			ExcludedPaths: []azcosmos.ExcludedPath{{Path: "/vec/*"}},
			VectorIndexes: []azcosmos.VectorIndex{{Path: "/vec", Type: "diskANN"}},
		},
	})
	if err != nil {
		return err
	}
	for _, r := range drivertest.VectorDataset().Rows {
		n, _ := r[0].AsInt()
		id := strconv.FormatInt(n, 10)
		b, err := encodeValue(driver.ObjectValue(
			driver.Member{Key: "id", Value: driver.String(id)},
			driver.Member{Key: "vec", Value: r[1]},
			driver.Member{Key: "payload", Value: r[2]},
		))
		if err != nil {
			return err
		}
		if _, err := cc.CreateItem(ctx, azcosmos.NewPartitionKeyString(id), b, nil); err != nil {
			return err
		}
	}
	return nil
}

func seed(ctx context.Context) error {
	for _, db := range []string{testDB, otherDB} {
		if err := createDatabase(ctx, db); err != nil {
			return err
		}
	}
	if err := seedPeople(ctx, testDB); err != nil {
		return err
	}
	if err := seedVectors(ctx, testDB); err != nil {
		return err
	}
	if _, err := createContainer(ctx, testDB, azcosmos.ContainerProperties{
		ID:                     ordersName,
		PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{Paths: []string{"/tenant"}},
		UniqueKeyPolicy:        &azcosmos.UniqueKeyPolicy{UniqueKeys: []azcosmos.UniqueKey{{Paths: []string{"/ref"}}}},
		IndexingPolicy: &azcosmos.IndexingPolicy{
			Automatic: true, IndexingMode: azcosmos.IndexingModeConsistent,
			IncludedPaths: []azcosmos.IncludedPath{{Path: "/*"}},
			CompositeIndexes: [][]azcosmos.CompositeIndex{{
				{Path: "/tenant", Order: azcosmos.CompositeIndexAscending}, {Path: "/total", Order: azcosmos.CompositeIndexDescending},
			}},
		},
	}); err != nil {
		return err
	}
	orders, err := admin.NewContainer(testDB, ordersName)
	if err != nil {
		return err
	}
	for i, o := range []string{
		`{"id":"o1","tenant":"acme","ref":"A-1","total":12.5,"lines":[{"sku":"x","qty":1}],"ship":{"city":"Lyon"}}`,
		`{"id":"o2","tenant":"acme","ref":"A-2","total":40,"lines":[],"ship":{"city":"Paris","zip":"75001"}}`,
		`{"id":"o1","tenant":"globex","ref":"G-1","total":7,"lines":[{"sku":"y","qty":3}]}`,
	} {
		tenant := "acme"
		if i == 2 {
			tenant = "globex"
		}
		if _, err := orders.CreateItem(ctx, azcosmos.NewPartitionKeyString(tenant), []byte(o), nil); err != nil {
			return err
		}
	}
	if _, err := createContainer(ctx, testDB, azcosmos.ContainerProperties{
		ID: hierName,
		PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{
			Paths: []string{"/tenant", "/user/name"}, Kind: azcosmos.PartitionKeyKindMultiHash, Version: 2,
		},
	}); err != nil {
		return err
	}
	big, err := createContainer(ctx, testDB, azcosmos.ContainerProperties{
		ID: bigName, PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{Paths: []string{"/id"}},
	})
	if err != nil {
		return err
	}
	for i := 1; i <= bigSize; i++ {
		doc := map[string]any{"id": strconv.Itoa(i), "n": i}
		if i >= bigLateFrom {
			doc["late"] = true
		}
		b, _ := json.Marshal(doc)
		if _, err := big.CreateItem(ctx, azcosmos.NewPartitionKeyString(strconv.Itoa(i)), b, nil); err != nil {
			return err
		}
	}
	_, err = createContainer(ctx, otherDB, azcosmos.ContainerProperties{
		ID: "things", PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{Paths: []string{"/id"}},
	})
	return err
}
