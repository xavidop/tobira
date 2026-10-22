//go:build integration

package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // database/sql "mysql" for the admin connection
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// testDSN is the go-sql-driver DSN (root, database tobira) of the container
// shared by every test in the package; testURL is the same as a mysql:// URL.
var testDSN, testURL, testHostPort string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

// leakBaseline is taken after the container and seed connection exist, so
// testcontainers' and the docker client's goroutines are not counted.
var leakBaseline goleak.Option

func runMain(m *testing.M) int {
	ctx := context.Background()
	// The MariaDB server starts alongside mysql:8.4 (flavors_integration_test.go).
	flavors := startFlavorServers(ctx)
	defer flavors.terminate()
	ctr, err := tcmysql.Run(ctx, "mysql:8.4",
		tcmysql.WithDatabase("tobira"), tcmysql.WithUsername("root"), tcmysql.WithPassword("tobira"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "start mysql container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	testHostPort, err = ctr.PortEndpoint(ctx, "3306/tcp", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testDSN = "root:tobira@tcp(" + testHostPort + ")/tobira"
	testURL = "mysql://root:tobira@" + testHostPort + "/tobira"
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	if err := flavors.wait(); err != nil {
		fmt.Fprintln(os.Stderr, err)
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

func seed(ctx context.Context) error {
	db, err := sql.Open("mysql", testDSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	stmts := drivertest.SQLSeed("mysql")
	stmts = append(stmts,
		"CREATE DATABASE tobira_ns",
		"CREATE TABLE tobira_ns.things (id INT PRIMARY KEY, label VARCHAR(20))",
		"INSERT INTO tobira_ns.things VALUES (1, 'in-ns')",
		"CREATE VIEW tobira_people_view AS SELECT id, name FROM tobira_people",
		"CREATE TABLE tobira_orders (id INT PRIMARY KEY AUTO_INCREMENT, person_id INT NOT NULL, "+
			"total DECIMAL(8,2) DEFAULT 0, doubled DECIMAL(9,2) GENERATED ALWAYS AS (total * 2) STORED, "+
			"note VARCHAR(50) DEFAULT 'n/a' COMMENT 'free text', created DATETIME DEFAULT CURRENT_TIMESTAMP, "+
			"CONSTRAINT fk_orders_person FOREIGN KEY (person_id) REFERENCES tobira_people(id), "+
			"INDEX tobira_orders_person_total (person_id, total), UNIQUE INDEX tobira_orders_note (note)) COMMENT 'orders table'",
		"CREATE TABLE tobira_big (n INT PRIMARY KEY)",
		"INSERT INTO tobira_big WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 1000) SELECT n FROM s",
		"ANALYZE TABLE tobira_people",
	)
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%.60s: %w", s, err)
		}
	}
	return nil
}

// admin opens a plain database/sql handle on the container, outside the
// driver under test.
func admin(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openMy opens a connection to the shared container and checks on cleanup
// that every pooled connection is back.
func openMy(t *testing.T, readOnly bool) *conn {
	t.Helper()
	return openDSN(t, testDSN, readOnly)
}

func openDSN(t *testing.T, dsn string, readOnly bool) *conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	mc := c.(*conn)
	t.Cleanup(func() {
		requireIdle(t, mc)
		_ = c.Close()
	})
	return mc
}

// requireIdle fails unless no pooled connection is still in use.
func requireIdle(t *testing.T, c *conn) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.db.Stats().InUse != 0 {
		if time.Now().After(deadline) {
			t.Errorf("%d connections still in use", c.db.Stats().InUse)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var scratchSeq atomic.Int64

// scratch copies tobira_people into a private table, so tests can write.
func scratch(t *testing.T, db *sql.DB) string {
	t.Helper()
	name := fmt.Sprintf("tobira_scratch_%d", scratchSeq.Add(1))
	mustExec(t, db, "CREATE TABLE "+name+" (PRIMARY KEY (id)) AS SELECT * FROM tobira_people")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE IF EXISTS "+name) })
	return name
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustExec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
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

// tableState fingerprints a table: its row count and CHECKSUM TABLE, or
// "missing" when it does not exist.
func tableState(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var name string
	var sum sql.NullInt64
	if err := db.QueryRow("CHECKSUM TABLE "+table).Scan(&name, &sum); err != nil {
		t.Fatalf("checksum %s: %v", table, err)
	}
	if !sum.Valid {
		return "missing"
	}
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return fmt.Sprintf("%d rows, checksum %d", n, sum.Int64)
}

func columnNames(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.Query("SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'tobira' AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION", table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return strings.Join(names, ",")
}
