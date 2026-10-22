//go:build integration

package clickhouse

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	testImage    = "clickhouse/clickhouse-server:25.8"
	testPassword = "tobira"
	testDB       = "tobira"
)

// nativeAddr and httpAddr are host:port of the container shared by every
// test in the package; testDSN and testHTTPDSN are clickhouse:// DSNs on the
// tobira database over each protocol.
var nativeAddr, httpAddr, testDSN, testHTTPDSN string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcclickhouse.Run(ctx, testImage,
		tcclickhouse.WithUsername("default"), tcclickhouse.WithPassword(testPassword), tcclickhouse.WithDatabase(testDB),
		testcontainers.WithEnv(map[string]string{"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1"}))
	if err != nil {
		fmt.Fprintln(os.Stderr, "start clickhouse container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if nativeAddr, err = ctr.PortEndpoint(ctx, "9000/tcp", ""); err == nil {
		httpAddr, err = ctr.PortEndpoint(ctx, "8123/tcp", "")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testDSN = "clickhouse://default:" + testPassword + "@" + nativeAddr + "/" + testDB
	testHTTPDSN = "clickhouse://default:" + testPassword + "@" + httpAddr + "/" + testDB + "?protocol=http"
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

// peopleDDL is People as a MergeTree table: tags, a JSON array, is an
// Array(String), which the driver reports as JSON.
func peopleDDL(table string) string {
	return "CREATE TABLE " + table + " (id Int32, name String, email String, age Nullable(Int32), " +
		"score Decimal(10, 2), active Bool, created_at DateTime('UTC'), tags Array(String)) " +
		"ENGINE = MergeTree ORDER BY id"
}

// peopleInsert renders People as one INSERT into table.
func peopleInsert(table string) string {
	quote := func(s string) string {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO " + table + " (id, name, email, age, score, active, created_at, tags) VALUES")
	for i, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		name, _ := r[1].AsText()
		email, _ := r[2].AsText()
		age := "NULL"
		if n, ok := r[3].AsInt(); ok {
			age = fmt.Sprint(n)
		}
		score, _ := r[4].AsText()
		active, _ := r[5].AsBool()
		ts, _ := r[6].AsTime()
		tags, _ := r[7].AsJSON()
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "\n(%d, %s, %s, %s, %s, %t, %s, JSONExtract(%s, 'Array(String)'))",
			id, quote(name), quote(email), age, score, active, quote(ts.UTC().Format("2006-01-02 15:04:05")), quote(string(tags)))
	}
	return sb.String()
}

func seed(ctx context.Context) error {
	db, err := adminConn()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	stmts := []string{
		peopleDDL("tobira.tobira_people"),
		peopleInsert("tobira.tobira_people"),
		"CREATE DATABASE tobira_ns",
		"CREATE TABLE tobira_ns.things (id UInt32, label String) ENGINE = MergeTree ORDER BY id",
		"INSERT INTO tobira_ns.things VALUES (1, 'in-ns')",
		"CREATE VIEW tobira.tobira_people_view AS SELECT id, name FROM tobira.tobira_people",
		"CREATE TABLE tobira.tobira_events (ts DateTime64(3, 'UTC'), kind LowCardinality(String), " +
			"n UInt64 DEFAULT 0, doubled UInt64 MATERIALIZED n * 2, note Nullable(String) COMMENT 'free text', " +
			"INDEX idx_kind kind TYPE set(10) GRANULARITY 1) " +
			"ENGINE = MergeTree ORDER BY (toDate(ts), kind) COMMENT 'events table'",
		"CREATE TABLE tobira.tobira_memory (a Int32, m Map(String, UInt64)) ENGINE = Memory",
		"INSERT INTO tobira.tobira_memory VALUES (2, {'x': 1}), (1, {'y': 2})",
		"CREATE MATERIALIZED VIEW tobira.tobira_mv ENGINE = MergeTree ORDER BY id AS SELECT id FROM tobira.tobira_people",
		"CREATE TABLE tobira.tobira_big (n UInt64) ENGINE = MergeTree ORDER BY n",
		"INSERT INTO tobira.tobira_big SELECT number FROM numbers(1000)",
		// Users the server itself makes read-only, as BI users often are.
		"CREATE USER ro1 IDENTIFIED WITH plaintext_password BY 'ro1-pass' SETTINGS readonly = 1",
		"CREATE USER ro2 IDENTIFIED WITH plaintext_password BY 'ro2-pass' SETTINGS readonly = 2",
		"GRANT SELECT ON *.* TO ro1, ro2",
		// ro1 and ro2wide hold wide grants, so only readonly refuses them:
		// readonly=1 refuses writes and table functions as READONLY;
		// readonly=2 lets table functions run, which the driver must refuse
		// itself.
		"CREATE USER ro2wide IDENTIFIED WITH plaintext_password BY 'ro2wide-pass' SETTINGS readonly = 2",
		"GRANT SELECT ON *.* TO ro2wide",
		"GRANT ALL ON tobira.* TO ro1, ro2wide",
		"GRANT CREATE TEMPORARY TABLE, URL, FILE ON *.* TO ro1, ro2wide",
		// Read-only users whose profile picks another query language.
		"CREATE USER roprql IDENTIFIED WITH plaintext_password BY 'roprql-pass' SETTINGS readonly = 2, allow_experimental_prql_dialect = 1, dialect = 'prql'",
		"CREATE USER rokusto IDENTIFIED WITH plaintext_password BY 'rokusto-pass' SETTINGS readonly = 2, allow_experimental_kusto_dialect = 1, dialect = 'kusto'",
		"GRANT SELECT ON *.* TO roprql, rokusto",
		"GRANT CREATE TEMPORARY TABLE, URL, FILE ON *.* TO roprql, rokusto",
	}
	for _, s := range stmts {
		if err := db.Exec(ctx, s); err != nil {
			return fmt.Errorf("%.60s: %w", s, err)
		}
	}
	return nil
}

// adminConn opens a plain clickhouse-go connection, outside the driver
// under test.
func adminConn() (chdriver.Conn, error) {
	return ch.Open(&ch.Options{Addr: []string{nativeAddr}, Auth: ch.Auth{Username: "default", Password: testPassword, Database: testDB}})
}

func admin(t *testing.T) chdriver.Conn {
	t.Helper()
	db, err := adminConn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db chdriver.Conn, stmt string) {
	t.Helper()
	if err := db.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// openCH opens the driver on dsn and closes it on cleanup.
func openCH(t *testing.T, dsn string, readOnly bool) *conn {
	t.Helper()
	c, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// protocols runs fn once per protocol, with that protocol's DSN.
func protocols(t *testing.T, fn func(t *testing.T, dsn string)) {
	t.Run("native", func(t *testing.T) { fn(t, testDSN) })
	t.Run("http", func(t *testing.T) { fn(t, testHTTPDSN) })
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func collect(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Row, driver.Stats, []driver.Column) {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatalf("Collect(%q): %v", q.Text, err)
	}
	return rows, st, cols
}

func queryErr(t *testing.T, c driver.Querier, q driver.Query) error {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		return err
	}
	_, _, err = driver.Collect(ctxT(t), rs, 0)
	return err
}

// tableState fingerprints a table: row count and content hash, or
// "missing".
func tableState(t *testing.T, db chdriver.Conn, table string) string {
	t.Helper()
	var exists uint8
	if err := db.QueryRow(context.Background(), "EXISTS TABLE "+table).Scan(&exists); err != nil {
		t.Fatalf("exists %s: %v", table, err)
	}
	if exists == 0 {
		return "missing"
	}
	var n, sum uint64
	if err := db.QueryRow(context.Background(), "SELECT count(), groupBitXor(cityHash64(*)) FROM "+table).Scan(&n, &sum); err != nil {
		t.Fatalf("hash %s: %v", table, err)
	}
	return fmt.Sprintf("%d rows, hash %d", n, sum)
}
