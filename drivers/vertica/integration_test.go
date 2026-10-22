//go:build integration

package vertica

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The integration tests run against a real single-node Vertica 25.4 in
// the official opentext/vertica-k8s image (the Community Edition images
// left Docker Hub, and from 26.1 the server refuses the CE license). That
// image is built for the VerticaDB operator and has no entrypoint, so the
// container starts idle and testdata/bootstrap.sh creates the database:
// dbadmin, password testPassword, database tobira.

const (
	testImage    = "opentext/vertica-k8s:25.4.0-0-multiarch"
	testPassword = "tobira-test-pw"
	itSchema     = "tobira_it" // the driver's own tests; People lives in public
)

// testHost and testPort locate the container's SQL port; testDSN is a DSN
// on the tobira database.
var (
	testHost string
	testPort string
	testDSN  string
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        testImage,
			Entrypoint:   []string{"sleep", "infinity"},
			User:         "0",
			ExposedPorts: []string{"5433/tcp"},
			Files: []testcontainers.ContainerFile{
				{HostFilePath: "testdata/bootstrap.sh", ContainerFilePath: "/bootstrap.sh", FileMode: 0o755},
			},
			WaitingFor: wait.ForExec([]string{"true"}),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start vertica container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	bctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	code, out, err := ctr.Exec(bctx, []string{"bash", "/bootstrap.sh"}, tcexec.Multiplexed(), tcexec.WithEnv([]string{"VERTICA_PASSWORD=" + testPassword}))
	cancel()
	var log []byte
	if out != nil {
		log, _ = io.ReadAll(out)
	}
	if err != nil || code != 0 || !strings.Contains(string(log), "tobira: database ready") {
		fmt.Fprintf(os.Stderr, "bootstrap vertica (exit %d): %v\n%s\n", code, err, log)
		return 1
	}
	if testHost, err = ctr.Host(ctx); err == nil {
		var p interface{ Port() string }
		if p, err = ctr.MappedPort(ctx, "5433/tcp"); err == nil {
			testPort = p.Port()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testDSN = fmt.Sprintf("vertica://dbadmin@%s:%s/tobira?tlsmode=none", testHost, testPort)
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	baseline := goleak.IgnoreCurrent()
	code = m.Run()
	if code == 0 {
		if err := goleak.Find(baseline, goleak.IgnoreTopFunction("internal/poll.runtime_pollWait")); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

// openTest opens the driver on the test database.
func openTest(t testing.TB, readOnly bool) *conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN, Password: driver.NewSecret(testPassword), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// adminDB is a plain vertica-sql-go pool for seeding and checking, outside
// the driver under test.
func adminDB() *sql.DB {
	d, _ := parseDSN(testDSN)
	return sql.OpenDB(connector{url: buildURL(d, testPassword)})
}

func mustExec(t testing.TB, db *sql.DB, text string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(noStdin(context.Background()), text, args...); err != nil {
		t.Fatalf("%s: %v", text, err)
	}
}

// peopleDDL is People in Vertica types. Vertica has no JSON type, so tags
// is a VARCHAR holding the JSON text (TypeOverrides declares it).
func peopleDDL(table string) string {
	return "CREATE TABLE " + table + " (id INT NOT NULL PRIMARY KEY, name VARCHAR(255) NOT NULL, " +
		"email VARCHAR(255) NOT NULL UNIQUE, age INT, score NUMERIC(10,2), active BOOLEAN, " +
		"created_at TIMESTAMPTZ, tags VARCHAR(255))"
}

// insertPeople inserts the People rows into table, one bound INSERT each
// (Vertica has no multi-row VALUES).
func insertPeople(ctx context.Context, db *sql.DB, table string) error {
	text := "INSERT INTO " + table + " (id, name, email, age, score, active, created_at, tags) VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	for _, r := range drivertest.People.Rows {
		args := make([]any, len(r))
		for i, v := range r {
			a, err := bindValue(v)
			if err != nil {
				return err
			}
			args[i] = a
		}
		if _, err := db.ExecContext(noStdin(ctx), text, args...); err != nil {
			return err
		}
	}
	return nil
}

func seed(ctx context.Context) error {
	db := adminDB()
	defer func() { _ = db.Close() }()
	for _, s := range []string{
		peopleDDL("public.tobira_people"),
		"CREATE SCHEMA " + itSchema,
		"CREATE TABLE " + itSchema + ".things (id INT PRIMARY KEY, label VARCHAR(40))",
		"INSERT INTO " + itSchema + ".things VALUES (1, 'in-it-schema')",
		"COMMENT ON TABLE " + itSchema + ".things IS 'things to test'",
	} {
		if _, err := db.ExecContext(noStdin(ctx), s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return insertPeople(ctx, db, "public.tobira_people")
}
