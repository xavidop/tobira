//go:build integration

package snowflake

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	sf "github.com/snowflakedb/gosnowflake/v2"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The integration tests run against fakesnow (testdata/Dockerfile), a
// Python fake of Snowflake's REST API backed by DuckDB, built and started
// with testcontainers. gosnowflake talks to it unchanged. It is not
// Snowflake: no warehouse, no EXPLAIN, no authentication, DuckDB's SQL
// under a Snowflake-to-DuckDB translator.

const (
	testDB     = "TOBIRA"
	testSchema = "PUBLIC"
	testNS     = testDB + "." + testSchema
)

// fakeHost and fakePort locate the fakesnow container; testDSN is a DSN on
// the TOBIRA database's PUBLIC schema.
var (
	fakeHost string
	fakePort int
	testDSN  string
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    "testdata",
				Dockerfile: "Dockerfile",
				Repo:       "tobira-fakesnow",
				Tag:        "0.11.22",
			},
			ExposedPorts: []string{"8080/tcp"},
			WaitingFor:   wait.ForLog("Application startup complete").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start fakesnow container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if fakeHost, err = ctr.Host(ctx); err == nil {
		var p string
		if p, err = portOf(ctx, ctr); err == nil {
			fakePort, err = strconv.Atoi(p)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testDSN = dsnFor(testDB, testSchema)
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

func portOf(ctx context.Context, ctr testcontainers.Container) (string, error) {
	p, err := ctr.MappedPort(ctx, "8080/tcp")
	if err != nil {
		return "", err
	}
	return p.Port(), nil
}

// dsnFor is a DSN on db.schema at the fakesnow container.
func dsnFor(db, schema string) string {
	return fmt.Sprintf("snowflake://tobira@fake/%s/%s?protocol=http&host=%s&port=%d", db, schema, fakeHost, fakePort)
}

// openTest opens the driver on the test schema. fakesnow accepts any
// password.
func openTest(t testing.TB, readOnly bool) driver.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN, Password: driver.NewSecret("fakesnow-ignores-it"), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// adminDB is a plain gosnowflake pool for seeding and checking, outside
// the driver under test. Its statements name tables fully: in Snowflake,
// CREATE SCHEMA switches the session's current schema, and the pool's
// sessions are shared.
func adminDB() *sql.DB {
	one := "1"
	return sql.OpenDB(sf.NewConnector(sf.SnowflakeDriver{}, sf.Config{
		Account: "fake", User: "admin", Password: "x", Host: fakeHost, Port: fakePort, Protocol: "http",
		Database: testDB, Schema: testSchema, MaxRetryCount: 1,
		Params: map[string]*string{multiStatementCountParam: &one},
	}))
}

func mustExec(t testing.TB, db *sql.DB, text string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), text, args...); err != nil {
		t.Fatalf("%s: %v", text, err)
	}
}

// peopleDDL is People in Snowflake types, with quoted lower-case names (so
// they keep their case): tags, a JSON array, is an ARRAY.
func peopleDDL(table string) string {
	return "CREATE OR REPLACE TABLE " + table + ` ("id" NUMBER(38,0) NOT NULL PRIMARY KEY, "name" VARCHAR NOT NULL, ` +
		`"email" VARCHAR NOT NULL, "age" NUMBER(38,0), "score" NUMBER(10,2) NOT NULL, "active" BOOLEAN NOT NULL, ` +
		`"created_at" TIMESTAMP_TZ NOT NULL, "tags" ARRAY NOT NULL)`
}

// insertPeople inserts the People rows into table, one bound INSERT each.
func insertPeople(ctx context.Context, db *sql.DB, table string) error {
	text := "INSERT INTO " + table + ` ("id", "name", "email", "age", "score", "active", "created_at", "tags") ` +
		"SELECT ?, ?, ?, ?, ?, ?, ?, PARSE_JSON(?)::ARRAY"
	for _, r := range drivertest.People.Rows {
		args := make([]any, len(r))
		for i, v := range r {
			a, err := bindValue(v)
			if err != nil {
				return err
			}
			args[i] = a
		}
		if _, err := db.ExecContext(ctx, text, args...); err != nil {
			return err
		}
	}
	return nil
}

func seed(ctx context.Context) error {
	db := adminDB()
	defer func() { _ = db.Close() }()
	for _, s := range []string{
		"CREATE DATABASE IF NOT EXISTS " + testDB,
		"CREATE SCHEMA IF NOT EXISTS " + testNS,
		"CREATE SCHEMA IF NOT EXISTS " + testDB + ".OTHER",
		`CREATE OR REPLACE TABLE ` + testDB + `.OTHER."things" ("id" INT PRIMARY KEY, "label" VARCHAR)`,
		`INSERT INTO ` + testDB + `.OTHER."things" VALUES (1, 'in-other')`,
		peopleDDL(testNS + `."tobira_people"`),
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return insertPeople(ctx, db, testNS+`."tobira_people"`)
}
