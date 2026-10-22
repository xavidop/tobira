//go:build integration

package mysql

import (
	"context"
	"database/sql"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// conformanceDB is the database the conformance kit runs against. It is
// separate from the shared one so the kit's writes can never disturb the
// driver's own tests.
const conformanceDB = "conformance"

// conformanceDSN creates and seeds the conformance database on the mysql:8.4
// container and returns its DSN.
func conformanceDSN(t *testing.T) string {
	t.Helper()
	return seedConformanceDB(t, testDSN, testHostPort)
}

// seedConformanceDB creates and seeds the conformance database on the server
// at hostPort (root password tobira; adminDSN reaches it) and returns its DSN.
func seedConformanceDB(t *testing.T, adminDSN, hostPort string) string {
	t.Helper()
	db, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mustExec(t, db, "CREATE DATABASE "+conformanceDB)
	t.Cleanup(func() {
		c, err := sql.Open("mysql", adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+conformanceDB)
	})
	dsn := "root:tobira@tcp(" + hostPort + ")/" + conformanceDB
	seedDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seedDB.Close() }()
	for _, s := range drivertest.SQLSeed("mysql") {
		if _, err := seedDB.Exec(s); err != nil {
			t.Fatalf("seed %.60s: %v", s, err)
		}
	}
	return dsn
}

func TestConformance(t *testing.T) {
	runConformance(t, New(), conformanceDSN(t), nil)
}

// runConformance runs the drivertest kit in strict mode with driver d
// against dsn, a database seeded by seedConformanceDB. overrides are the
// flavor's declared storage differences (Fixture.TypeOverrides).
func runConformance(t *testing.T, d driver.Driver, dsn string, overrides map[string]driver.Type) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := d.Open(context.Background(), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       d,
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := d.Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			TypeOverrides:    overrides,
			AutoIncrementKey: drivertest.Bool(false), // id INT, no AUTO_INCREMENT
			Namespace:        conformanceDB,
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
			CompoundWrites: []driver.Query{
				{Text: "select 1; delete from tobira_people where id = 1"},
				{Text: "with x as (select 1 as n) delete from tobira_people where id = 1"},
			},
			Slow:       driver.Query{Text: "select sleep(10)"},
			ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
			TxWrite:    driver.Query{Text: "update tobira_people set name = 'tx' where id = 1"},
			TxRestore:  driver.Query{Text: "update tobira_people set name = 'Ōtani Shōhei' where id = 1"},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		},
	})
}
