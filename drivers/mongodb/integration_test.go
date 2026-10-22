//go:build integration

package mongodb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	testUser     = "root"
	testPassword = "tobira-pw"
	testDB       = "tobira"
)

// mongoImage is the MongoDB image the integration tests run: mongo:8, or
// TOBIRA_TEST_MONGO_IMAGE when set. MongoDB 8 refuses to start on Linux
// kernels 6.19 through 7.0.13 (SERVER-121912), so CI runs mongo:7.0 there.
func mongoImage() string {
	if img := os.Getenv("TOBIRA_TEST_MONGO_IMAGE"); img != "" {
		return img
	}
	return "mongo:8"
}

// testHostPort is the address of the MongoDB single-node replica set shared
// by every test in the package; admin is a client on it for seeding and
// checks, outside the driver.
var (
	testHostPort string
	admin        *mongo.Client
	leakBaseline goleak.Option
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcmongo.Run(ctx, mongoImage(),
		tcmongo.WithReplicaSet("rs0"), tcmongo.WithUsername(testUser), tcmongo.WithPassword(testPassword))
	if err != nil {
		fmt.Fprintln(os.Stderr, "start mongo container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if testHostPort, err = ctr.PortEndpoint(ctx, "27017/tcp", ""); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	admin, err = mongo.Connect(options.Client().ApplyURI(dsnFor("", testPassword)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "admin client:", err)
		return 1
	}
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		_ = admin.Disconnect(ctx)
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
	_ = admin.Disconnect(ctx)
	return code
}

// dsnFor returns a DSN for the container's root user with the given
// password, defaulting to database db. The member's advertised host is the
// container's internal address, so the client connects directly.
func dsnFor(db, password string) string {
	u := url.URL{Scheme: "mongodb", Host: testHostPort, Path: "/" + db,
		RawQuery: "authSource=admin&directConnection=true"}
	if password != "" {
		u.User = url.UserPassword(testUser, password)
	} else {
		u.User = url.User(testUser)
	}
	return u.String()
}

// openTest opens the driver on database db and closes it with the test.
func openTest(t testing.TB, db string, readOnly bool) driver.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor(db, testPassword), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// peopleDocs is People as documents: _id and id are the row id, tags is an
// array and a NULL age is an explicit null.
func peopleDocs() ([]any, error) {
	out := make([]any, len(drivertest.People.Rows))
	for i, r := range drivertest.People.Rows {
		d := bson.D{{Key: "_id", Value: nil}}
		for k, col := range drivertest.People.Columns {
			v, err := toBSON(r[k])
			if err != nil {
				return nil, err
			}
			d = append(d, bson.E{Key: col.Name, Value: v})
		}
		d[0].Value = d[1].Value
		out[i] = d
	}
	return out, nil
}

// seedPeople loads People into db.tobira_people.
func seedPeople(ctx context.Context, db string) error {
	docs, err := peopleDocs()
	if err != nil {
		return err
	}
	_, err = admin.Database(db).Collection(drivertest.PeopleCollection()).InsertMany(ctx, docs)
	return err
}

// seedDocuments loads drivertest.Documents into db.docs.
func seedDocuments(ctx context.Context, db string) error {
	var docs []any
	for _, o := range drivertest.Documents() {
		d, err := objectToBSON(o)
		if err != nil {
			return err
		}
		docs = append(docs, d)
	}
	_, err := admin.Database(db).Collection(drivertest.DocumentsCollection).InsertMany(ctx, docs)
	return err
}

func seed(ctx context.Context) error {
	if err := seedPeople(ctx, testDB); err != nil {
		return err
	}
	if err := seedDocuments(ctx, testDB); err != nil {
		return err
	}
	db := admin.Database(testDB)
	people := db.Collection(drivertest.PeopleCollection())
	if _, err := people.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true).SetName("email_unique")},
		{Keys: bson.D{{Key: "age", Value: -1}, {Key: "name", Value: 1}}, Options: options.Index().SetName("age_name")},
		{Keys: bson.D{{Key: "name", Value: "text"}}, Options: options.Index().SetName("name_text")},
	}); err != nil {
		return err
	}
	if err := db.CreateView(ctx, "people_view", drivertest.PeopleCollection(),
		mongo.Pipeline{{{Key: "$project", Value: bson.D{{Key: "name", Value: 1}}}}}); err != nil {
		return err
	}
	// tobira_big: 1500 documents; those after the first 1000 hold an extra field.
	big := make([]any, 1500)
	for i := range big {
		d := bson.D{{Key: "_id", Value: int32(i + 1)}, {Key: "n", Value: int32(i + 1)}}
		if i >= 1000 {
			d = append(d, bson.E{Key: "late", Value: true})
		}
		big[i] = d
	}
	if _, err := db.Collection("tobira_big").InsertMany(ctx, big); err != nil {
		return err
	}
	if err := admin.Database("tobira_other").CreateCollection(ctx, "things"); err != nil {
		return err
	}
	return nil
}
