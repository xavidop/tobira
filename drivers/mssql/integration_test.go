//go:build integration

package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/microsoft/go-mssqldb" // database/sql "sqlserver" for the admin connection
	"github.com/testcontainers/testcontainers-go"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// testImage is SQL Server 2022, pinned. It is an amd64 image; on arm64
// Docker Desktop runs it under Rosetta emulation.
const testImage = "mcr.microsoft.com/mssql/server:2022-CU27-GDR1-ubuntu-22.04"

const testPassword = "Tobira-Pw-1234"

// testHostPort is the container's host:port; testDSN is the sa DSN of the
// shared database "tobira".
var testHostPort, testDSN string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

// leakBaseline is taken after the container and seed connection exist, so
// testcontainers' and the docker client's goroutines are not counted.
var leakBaseline goleak.Option

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcmssql.Run(ctx, testImage, tcmssql.WithAcceptEULA(), tcmssql.WithPassword(testPassword),
		testcontainers.WithImagePlatform("linux/amd64"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "start mssql container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	testHostPort, err = ctr.PortEndpoint(ctx, "1433/tcp", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testDSN = dsnFor("tobira")
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
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

// dsnFor is the sa DSN of database db on the container.
func dsnFor(db string) string {
	return "sqlserver://sa:" + testPassword + "@" + testHostPort + "?database=" + db + "&encrypt=disable"
}

// peopleSeed renders the People table and rows in T-SQL (N” strings,
// BIT, DATETIME2). SQL Server 2022 has no JSON type: tags is NVARCHAR(MAX).
func peopleSeed() []string {
	quote := func(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }
	stmts := []string{"CREATE TABLE tobira_people (id INT PRIMARY KEY, name NVARCHAR(255) NOT NULL, email NVARCHAR(255) NOT NULL UNIQUE, " +
		"age INT, score DECIMAL(10,2), active BIT, created_at DATETIME2(0), tags NVARCHAR(MAX))"}
	rows := drivertest.People.Rows
	for start := 0; start < len(rows); start += 20 {
		var sb strings.Builder
		sb.WriteString("INSERT INTO tobira_people (id, name, email, age, score, active, created_at, tags) VALUES")
		for i, r := range rows[start:min(start+20, len(rows))] {
			id, _ := r[0].AsInt()
			name, _ := r[1].AsText()
			email, _ := r[2].AsText()
			age := "NULL"
			if n, ok := r[3].AsInt(); ok {
				age = fmt.Sprint(n)
			}
			score, _ := r[4].AsText()
			active, _ := r[5].AsBool()
			bit := 0
			if active {
				bit = 1
			}
			ts, _ := r[6].AsTime()
			tags, _ := r[7].AsJSON()
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "\n(%d, %s, %s, %s, %s, %d, '%s', %s)", id, quote(name), quote(email), age, score, bit,
				ts.UTC().Format("2006-01-02 15:04:05"), quote(string(tags)))
		}
		stmts = append(stmts, sb.String())
	}
	return stmts
}

// execAll runs stmts on dsn, one batch each.
func execAll(ctx context.Context, dsn string, stmts ...string) error {
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%.70s: %w", s, err)
		}
	}
	return nil
}

func seed(ctx context.Context) error {
	if err := execAll(ctx, dsnFor("master"), "CREATE DATABASE tobira", "CREATE DATABASE tobira_ns", "CREATE DATABASE [we.ird]"); err != nil {
		return err
	}
	stmts := peopleSeed()
	stmts = append(stmts,
		"CREATE SCHEMA sales",
		"CREATE TABLE sales.orders (id INT IDENTITY(1,1) PRIMARY KEY, person_id INT NOT NULL, "+
			"total DECIMAL(8,2) CONSTRAINT df_total DEFAULT 0, doubled AS (total * 2), note NVARCHAR(50) DEFAULT N'n/a', "+
			"created DATETIME2 DEFAULT SYSUTCDATETIME(), CONSTRAINT fk_orders_person FOREIGN KEY (person_id) REFERENCES dbo.tobira_people(id))",
		"CREATE INDEX ix_orders_person_total ON sales.orders (person_id, total)",
		"CREATE UNIQUE INDEX ux_orders_note ON sales.orders (note) WHERE note IS NOT NULL",
		"EXEC sp_addextendedproperty N'MS_Description', N'orders table', N'SCHEMA', N'sales', N'TABLE', N'orders'",
		"EXEC sp_addextendedproperty N'MS_Description', N'free text', N'SCHEMA', N'sales', N'TABLE', N'orders', N'COLUMN', N'note'",
		"CREATE VIEW tobira_people_view AS SELECT id, name FROM tobira_people",
		"CREATE TABLE tobira_big (n INT PRIMARY KEY)",
		"WITH s(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM s WHERE n < 1000) INSERT INTO tobira_big SELECT n FROM s OPTION (MAXRECURSION 1000)",
		"CREATE TABLE tobira_types (id INT PRIMARY KEY, g UNIQUEIDENTIFIER, m MONEY, r REAL, f FLOAT, t TIME(3), d DATE, "+
			"dto DATETIMEOFFSET(3), b VARBINARY(8), x XML, v SQL_VARIANT, tiny TINYINT)",
		"INSERT INTO tobira_types VALUES (1, '01234567-89AB-CDEF-0123-456789ABCDEF', 12.3456, 0.1, 2.5, '13:04:05.120', '2026-01-02', "+
			"'2026-01-02T03:04:05.678+01:00', 0x0102, '<a/>', CAST(42 AS INT), 255)",
		"CREATE SEQUENCE tobira_seq START WITH 1",
		"CREATE TABLE tobira_trig (id INT IDENTITY PRIMARY KEY, v INT)",
		"CREATE TABLE tobira_trig_log (n INT)",
		"CREATE TRIGGER tr_tobira_trig ON tobira_trig AFTER INSERT AS INSERT INTO tobira_trig_log SELECT v FROM inserted",
	)
	if err := execAll(ctx, testDSN, stmts...); err != nil {
		return err
	}
	if err := execAll(ctx, dsnFor("tobira_ns"), "CREATE TABLE things (id INT PRIMARY KEY, label NVARCHAR(20))", "INSERT INTO things VALUES (1, N'in-ns')"); err != nil {
		return err
	}
	return execAll(ctx, dsnFor("we.ird"), "CREATE SCHEMA [s.x]", "CREATE TABLE [s.x].[t]]1] (id INT PRIMARY KEY)", "INSERT INTO [s.x].[t]]1] VALUES (5)")
}

// admin opens a plain database/sql handle on the shared database, outside
// the driver under test.
func admin(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlserver", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// openMS opens a connection to the shared database.
func openMS(t *testing.T, readOnly bool) *conn {
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
		_ = mc.Close()
	})
	return mc
}

func ctx30(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// collect runs q on qr and drains it.
func collect(t *testing.T, qr driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats, error) {
	t.Helper()
	rs, err := qr.Query(ctx30(t), q)
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx30(t), rs, 0)
	return cols, rows, st, err
}

// scalar runs a one-value query through the admin connection.
func scalar(t *testing.T, query string) int64 {
	t.Helper()
	var n int64
	if err := admin(t).QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
