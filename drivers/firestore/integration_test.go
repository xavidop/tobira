//go:build integration

package firestore

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	fs "cloud.google.com/go/firestore"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	// testImage is the official Cloud SDK emulators image, pinned exactly
	// (multi-arch: arm64 and amd64).
	testImage = "gcr.io/google.com/cloudsdktool/google-cloud-cli:590.0.0-emulators"
	// The emulator keeps each project apart: the conformance kit's writes
	// never touch the driver's own tests.
	testProject        = "tobira-test"
	conformanceProject = "tobira-conformance"
	testDatabase       = "(default)"
)

// emulatorAddr is host:port of the emulator shared by every test.
var emulatorAddr string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, testImage,
		testcontainers.WithCmd("gcloud", "emulators", "firestore", "start", "--host-port=0.0.0.0:8080"),
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Dev App Server is now running").WithStartupTimeout(3*time.Minute)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start firestore emulator:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if emulatorAddr, err = ctr.PortEndpoint(ctx, "8080/tcp", ""); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	baseline := goleak.IgnoreCurrent()
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(baseline, goleak.IgnoreTopFunction("internal/poll.runtime_pollWait")); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

// dsnFor is a firestore:// DSN on the emulator for a project.
func dsnFor(project string) string { return "firestore://" + project + "?emulator=" + emulatorAddr }

// admin is a plain client library connection to the emulator, used to
// seed and to verify, bypassing the driver. close releases it.
func admin(ctx context.Context, t testing.TB, project string) *fs.Client {
	c, closeFn, err := adminClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeFn)
	return c
}

func adminClient(ctx context.Context, project string) (*fs.Client, func(), error) {
	gc, err := grpc.NewClient("passthrough:///"+emulatorAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(emulatorCreds{}))
	if err != nil {
		return nil, nil, err
	}
	c, err := fs.NewClient(ctx, project, option.WithGRPCConn(gc), option.WithoutAuthentication(), option.WithLogger(discard))
	if err != nil {
		_ = gc.Close()
		return nil, nil, err
	}
	return c, func() { _ = c.Close(); _ = gc.Close() }, nil
}

// openTest opens the driver on a project and closes it with the test.
func openTest(t testing.TB, project string, readOnly bool) driver.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor(project), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// peopleDoc is one People row as a document: its id is the row id as
// text; score is a double and tags an array (the fixture's declared
// storage limits), a NULL age an explicit null.
func peopleDoc(r driver.Row) (string, map[string]any, error) {
	doc := map[string]any{}
	for k, col := range drivertest.People.Columns {
		v, err := toFirestore(r[k])
		if err != nil {
			return "", nil, err
		}
		doc[col.Name] = v
	}
	id, _ := r[0].AsInt()
	return strconv.FormatInt(id, 10), doc, nil
}

func seedPeople(ctx context.Context, c *fs.Client) error {
	coll := c.Collection(drivertest.PeopleCollection())
	for _, r := range drivertest.People.Rows {
		id, doc, err := peopleDoc(r)
		if err != nil {
			return err
		}
		if _, err := coll.Doc(id).Set(ctx, doc); err != nil {
			return err
		}
	}
	return nil
}

func seed(ctx context.Context) error {
	for _, p := range []string{conformanceProject, testProject} {
		c, closeFn, err := adminClient(ctx, p)
		if err != nil {
			return err
		}
		err = seedPeople(ctx, c)
		if err == nil && p == testProject {
			err = seedTest(ctx, c)
		}
		closeFn()
		if err != nil {
			return err
		}
	}
	return nil
}

// seedTest adds the driver tests' own data: the documents fixture, a
// subcollection, typed values and a big collection.
func seedTest(ctx context.Context, c *fs.Client) error {
	for _, o := range drivertest.Documents() {
		id, _ := o[0].Value.AsText()
		data, err := mapOf(o[1:], 1)
		if err != nil {
			return err
		}
		if _, err := c.Collection(drivertest.DocumentsCollection).Doc(id).Set(ctx, data); err != nil {
			return err
		}
	}
	users := c.Collection("users")
	for _, u := range []string{"ada", "grace"} {
		if _, err := users.Doc(u).Set(ctx, map[string]any{"name": u}); err != nil {
			return err
		}
		for i := 1; i <= 3; i++ {
			if _, err := users.Doc(u).Collection("orders").Doc(fmt.Sprintf("o%d", i)).Set(ctx, map[string]any{
				"total": int64(i * 10), "user": u, "item": map[string]any{"sku": "s" + strconv.Itoa(i)},
			}); err != nil {
				return err
			}
		}
	}
	typed := map[string]any{
		"ref":    c.Doc("users/ada"),
		"geo":    &latlng.LatLng{Latitude: 51.5, Longitude: -0.12},
		"bytes":  []byte{1, 2, 3},
		"when":   time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC),
		"vec":    fs.Vector64{0.5, 1.5},
		"nan":    math.NaN(),
		"mixed":  int64(1),
		"nested": map[string]any{"a": []any{map[string]any{"b": true}}},
	}
	if _, err := c.Collection("typed").Doc("t1").Set(ctx, typed); err != nil {
		return err
	}
	if _, err := c.Collection("typed").Doc("t2").Set(ctx, map[string]any{"mixed": 2.5}); err != nil {
		return err
	}
	big := c.Collection("tobira_big")
	b := c.BulkWriter(ctx)
	for i := 1; i <= 1500; i++ {
		d := map[string]any{"n": int64(i)}
		if i > 1000 {
			d["late"] = true
		}
		if _, err := b.Set(big.Doc(fmt.Sprintf("b%04d", i)), d); err != nil {
			return err
		}
	}
	b.End()
	return nil
}
