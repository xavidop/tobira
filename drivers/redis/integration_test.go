//go:build integration

package redis

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	testImage    = "redis:8"
	testPassword = "tobira"
	// Databases of the shared container.
	dbMisc        = 3
	dbConformance = 0 // RediSearch indexes only database 0
	dbKV          = 2
	peopleIndex   = "tobira_people_idx"
)

// testAddr is host:port of the container shared by every test.
var testAddr string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, addr, err := startRedis(ctx, "--requirepass", testPassword, "--notify-keyspace-events", "KEA")
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	testAddr = addr
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

// startRedis runs a redis:8 container with extra server arguments and
// returns it with its host:port (a random host port).
func startRedis(ctx context.Context, args ...string) (testcontainers.Container, string, error) {
	ctr, err := testcontainers.Run(ctx, testImage,
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd(append([]string{"redis-server"}, args...)...),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections").WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		return ctr, "", err
	}
	addr, err := ctr.PortEndpoint(ctx, "6379/tcp", "")
	return ctr, addr, err
}

// dsn is the URL of database db of the shared container.
func dsn(db int) string {
	return "redis://:" + testPassword + "@" + testAddr + "/" + strconv.Itoa(db)
}

// admin returns a go-redis client on database db, closed with the test.
func admin(t testing.TB, db int) *goredis.Client {
	t.Helper()
	cl := goredis.NewClient(&goredis.Options{Addr: testAddr, Password: testPassword, DB: db, Protocol: 2, DisableIdentity: true})
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func openTest(t testing.TB, db int, readOnly bool) *conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn(db), ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// peopleHash is the hash a People row is stored as: NULL fields are absent.
func peopleHash(row driver.Row) []any {
	var args []any
	for i, col := range drivertest.People.Columns {
		if row[i].IsNull() {
			continue
		}
		s, err := encodeValue(row[i])
		if err != nil {
			panic(err)
		}
		args = append(args, col.Name, s)
	}
	return args
}

func seed(ctx context.Context) error {
	opts := func(db int) *goredis.Options {
		return &goredis.Options{Addr: testAddr, Password: testPassword, DB: db, Protocol: 2, DisableIdentity: true}
	}
	conf := goredis.NewClient(opts(dbConformance))
	defer func() { _ = conf.Close() }()
	for _, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		if err := conf.HSet(ctx, "tobira_people:"+strconv.FormatInt(id, 10), peopleHash(r)...).Err(); err != nil {
			return err
		}
	}
	if err := conf.Do(ctx, "FT.CREATE", peopleIndex, "ON", "HASH", "PREFIX", "1", "tobira_people:",
		"SCHEMA", "id", "NUMERIC", "SORTABLE").Err(); err != nil {
		return err
	}
	kv := goredis.NewClient(opts(dbKV))
	defer func() { _ = kv.Close() }()
	for _, r := range drivertest.KeyValueDataset().Rows {
		if err := seedKV(ctx, kv, r); err != nil {
			return err
		}
	}
	if err := kv.XAdd(ctx, &goredis.XAddArgs{Stream: "events:s", ID: "1-0", Values: []any{"f", "v"}}).Err(); err != nil {
		return err
	}
	return kv.Do(ctx, "JSON.SET", "doc:1", "$", `{"a":[1,2],"b":{"c":"d"}}`).Err()
}

// seedKV writes one KeyValueDataset row.
func seedKV(ctx context.Context, cl *goredis.Client, r driver.Row) error {
	key, _ := r[0].AsText()
	typ, _ := r[1].AsText()
	ttl, _ := r[2].AsInt()
	_, err := cl.TxPipelined(ctx, func(p goredis.Pipeliner) error {
		if err := writeValue(ctx, p, "seed", key, typ, r[3]); err != nil {
			return err
		}
		if ttl > 0 {
			p.Expire(ctx, key, time.Duration(ttl)*time.Second)
		}
		return nil
	})
	return err
}
