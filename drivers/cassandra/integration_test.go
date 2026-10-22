//go:build integration

package cassandra

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
	"gopkg.in/inf.v0"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// image is the Cassandra image the integration suite runs against.
const image = "cassandra:5.0.9"

const (
	testUser     = "cassandra"
	testPassword = "cassandra"
	testKS       = "tobira"
	otherKS      = "tobira_other"
)

// containerScript configures the node before starting it:
//
//   - PasswordAuthenticator, so the bad-credentials path is real;
//   - ByteOrderedPartitioner with one token. drivertest's SelectAll must
//     return People in id order, and CQL cannot order rows across
//     partitions; under this partitioner the token order of a positive int
//     partition key is its numeric order, so "SELECT * FROM tobira_people"
//     comes back ordered by id. The driver does not depend on it: it is
//     only how the fixture's ordered read is expressible in CQL;
//   - user-defined functions and materialized views on, so the read-only
//     guard's UDF refusal and the view handling run against the server.
const containerScript = `f=/etc/cassandra/cassandra.yaml
sed -i \
  -e 's/^authenticator:.*/authenticator: PasswordAuthenticator/' \
  -e 's/^partitioner:.*/partitioner: org.apache.cassandra.dht.ByteOrderedPartitioner/' \
  -e 's/^num_tokens:.*/num_tokens: 1/' \
  -e 's/^allocate_tokens_for_local_replication_factor:.*//' \
  -e 's/^user_defined_functions_enabled:.*/user_defined_functions_enabled: true/' \
  -e 's/^materialized_views_enabled:.*/materialized_views_enabled: true/' \
  "$f"
exec docker-entrypoint.sh cassandra -f`

// testHostPort is the node's mapped CQL address; admin is a gocql session
// on it for seeding and checks, outside the driver.
var (
	testHostPort string
	admin        *gocql.Session
	leakBaseline goleak.Option
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, image,
		testcontainers.WithEntrypoint("bash", "-c", containerScript),
		testcontainers.WithEnv(map[string]string{
			"MAX_HEAP_SIZE":  "1G",
			"HEAP_NEWSIZE":   "200M",
			"JVM_EXTRA_OPTS": "-Dcassandra.skip_wait_for_gossip_to_settle=0 -Dcassandra.superuser_setup_delay_ms=0",
		}),
		testcontainers.WithExposedPorts("9042/tcp"),
		// The superuser role is created after the CQL port opens; both must
		// be there before a client can log in.
		testcontainers.WithWaitStrategyAndDeadline(4*time.Minute,
			wait.ForLog("Created default superuser role"),
			wait.ForListeningPort("9042/tcp"),
		),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start cassandra container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if testHostPort, err = ctr.PortEndpoint(ctx, "9042/tcp", ""); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testHostPort = strings.Replace(testHostPort, "localhost:", "127.0.0.1:", 1)
	if admin, err = adminSession(); err != nil {
		fmt.Fprintln(os.Stderr, "admin session:", err)
		return 1
	}
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		admin.Close()
		return 1
	}
	leakBaseline = goleak.IgnoreCurrent()
	code := m.Run()
	admin.Close()
	if code == 0 {
		if err := goleak.Find(leakBaseline); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			code = 1
		}
	}
	return code
}

// adminSession connects gocql directly, retrying while the node settles.
func adminSession() (*gocql.Session, error) {
	cl := gocql.NewCluster(testHostPort)
	cl.Authenticator = gocql.PasswordAuthenticator{Username: testUser, Password: testPassword}
	cl.Consistency = gocql.One
	cl.Timeout = 30 * time.Second
	cl.ConnectTimeout = 10 * time.Second
	cl.DisableInitialHostLookup = true
	cl.Logger = discardLogger{}
	var err error
	for range 30 {
		var s *gocql.Session
		if s, err = cl.CreateSession(); err == nil {
			return s, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, err
}

// dsnFor returns a DSN for the superuser with the given password, in
// keyspace ks.
func dsnFor(ks, password, params string) string {
	dsn := "cassandra://" + testUser
	if password != "" {
		dsn += ":" + password
	}
	dsn += "@" + testHostPort + "/" + ks + "?consistency=ONE"
	if params != "" {
		dsn += "&" + params
	}
	return dsn
}

// openTest opens the driver on keyspace ks and closes it with the test.
func openTest(t testing.TB, ks string, readOnly bool) driver.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor(ks, testPassword, ""), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func exec(ctx context.Context, stmts ...string) error {
	for _, s := range stmts {
		if err := admin.Query(s).ExecContext(ctx); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// seedPeople creates ks.tobira_people and loads People with plain gocql
// values, independently of the driver's encoder. tags is a frozen list:
// a non-frozen collection reads an empty list back as NULL.
func seedPeople(ctx context.Context, ks string) error {
	if err := exec(ctx,
		"CREATE KEYSPACE IF NOT EXISTS "+ks+" WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}",
		"CREATE TABLE "+ks+".tobira_people (id int PRIMARY KEY, name text, email text, age int, score decimal, "+
			"active boolean, created_at timestamp, tags frozen<list<text>>)",
	); err != nil {
		return err
	}
	for _, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		name, _ := r[1].AsText()
		email, _ := r[2].AsText()
		var age *int
		if n, ok := r[3].AsInt(); ok {
			a := int(n)
			age = &a
		}
		s, _ := r[4].AsText()
		score, _ := new(inf.Dec).SetString(s)
		active, _ := r[5].AsBool()
		created, _ := r[6].AsTime()
		raw, _ := r[7].AsJSON()
		tags := []string{}
		if err := json.Unmarshal(raw, &tags); err != nil {
			return err
		}
		if err := admin.Query("INSERT INTO "+ks+".tobira_people (id, name, email, age, score, active, created_at, tags) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			int(id), name, email, age, score, active, created, tags).ExecContext(ctx); err != nil {
			return err
		}
	}
	return nil
}

// seedEvents creates ks.events and loads drivertest.WideColumnDataset.
func seedEvents(ctx context.Context, ks string) error {
	if err := exec(ctx, "CREATE TABLE "+ks+".events (tenant text, ts timestamp, seq int, kind text, value double, "+
		"attrs map<text, text>, tags set<text>, PRIMARY KEY ((tenant), ts, seq)) WITH CLUSTERING ORDER BY (ts DESC, seq ASC)"); err != nil {
		return err
	}
	for _, r := range drivertest.WideColumnDataset().Rows {
		tenant, _ := r[0].AsText()
		ts, _ := r[1].AsTime()
		seq, _ := r[2].AsInt()
		kind, _ := r[3].AsText()
		var value *float64
		if f, ok := r[4].AsFloat(); ok {
			value = &f
		}
		var attrs map[string]string
		if obj, ok := r[5].AsObject(); ok {
			attrs = map[string]string{}
			for _, m := range obj {
				attrs[m.Key], _ = m.Value.AsText()
			}
		}
		list, _ := r[6].AsList()
		tags := make([]string, len(list))
		for i, v := range list {
			tags[i], _ = v.AsText()
		}
		if err := admin.Query("INSERT INTO "+ks+".events (tenant, ts, seq, kind, value, attrs, tags) VALUES (?, ?, ?, ?, ?, ?, ?)",
			tenant, ts, int(seq), kind, value, attrs, tags).ExecContext(ctx); err != nil {
			return err
		}
	}
	return nil
}

func seed(ctx context.Context) error {
	if err := seedPeople(ctx, testKS); err != nil {
		return err
	}
	if err := seedEvents(ctx, testKS); err != nil {
		return err
	}
	if err := exec(ctx,
		"CREATE TYPE "+testKS+".address (street text, zip int)",
		"CREATE TABLE "+testKS+".types (id int PRIMARY KEY, ti tinyint, si smallint, bi bigint, vi varint, "+
			"f float, d double, dec decimal, b boolean, t text, a ascii, bl blob, ts timestamp, dt date, tm time, "+
			"u uuid, tu timeuuid, ip inet, du duration, l list<int>, s set<text>, m map<text, int>, "+
			"tp tuple<int, text>, addr frozen<address>, vec vector<float, 3>, nested map<text, frozen<list<int>>>)",
		"CREATE TABLE "+testKS+".counters (id int PRIMARY KEY, hits counter)",
		"UPDATE "+testKS+".counters SET hits = hits + 5 WHERE id = 1",
		"CREATE INDEX people_email ON "+testKS+".tobira_people (email)",
		"CREATE MATERIALIZED VIEW "+testKS+".people_by_email AS SELECT id, email FROM "+testKS+".tobira_people "+
			"WHERE email IS NOT NULL AND id IS NOT NULL PRIMARY KEY (email, id)",
		"CREATE FUNCTION "+testKS+".plus_one (x int) RETURNS NULL ON NULL INPUT RETURNS int LANGUAGE java AS 'return x + 1;'",
		"CREATE TABLE "+testKS+".tobira_big (id int PRIMARY KEY, n int)",
		"CREATE KEYSPACE "+otherKS+" WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}",
		"CREATE TABLE "+otherKS+".things (id int PRIMARY KEY, label text)",
		"INSERT INTO "+otherKS+".things (id, label) VALUES (1, 'thing one')",
	); err != nil {
		return err
	}
	for i := 1; i <= 1500; i++ {
		if err := admin.Query("INSERT INTO "+testKS+".tobira_big (id, n) VALUES (?, ?)", i, i).ExecContext(ctx); err != nil {
			return err
		}
	}
	return nil
}
