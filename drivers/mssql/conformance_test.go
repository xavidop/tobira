//go:build integration

package mssql

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// conformanceDB is the database the conformance kit runs against. It is
// separate from the shared one so the kit's writes can never disturb the
// driver's own tests.
const conformanceDB = "conformance"

func TestConformance(t *testing.T) {
	ctx := context.Background()
	if err := execAll(ctx, dsnFor("master"), "CREATE DATABASE "+conformanceDB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = execAll(context.Background(), dsnFor("master"),
			"ALTER DATABASE "+conformanceDB+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE", "DROP DATABASE "+conformanceDB)
	})
	dsn := dsnFor(conformanceDB)
	if err := execAll(ctx, dsn, peopleSeed()...); err != nil {
		t.Fatal(err)
	}
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			// SQL Server 2022 has no JSON type: tags is NVARCHAR(MAX).
			TypeOverrides:    map[string]driver.Type{"tags": driver.TypeString},
			AutoIncrementKey: drivertest.Bool(false), // id INT, no IDENTITY
			Namespace:        "dbo",
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
			CompoundWrites: []driver.Query{
				{Text: "select 1; delete from tobira_people where id = 1"},
				{Text: "select 1 delete from tobira_people where id = 1"},
				{Text: "with x as (select 1 as n) delete from tobira_people where id = 1"},
				{Text: "select * into tobira_copy from tobira_people"},
				{Text: "exec('delete from tobira_people where id = 1')"},
				{Text: "select 1 --x\rdelete from tobira_people where id = 1"},
				{Text: "select 0xdelete from tobira_people where id = 1"},
				{Text: "select 1\ngo\ndelete from tobira_people where id = 1"},
			},
			Slow:       driver.Query{Text: "waitfor delay '00:00:10'"},
			ParamQuery: driver.Query{Text: "select * from tobira_people where id = @p1", Params: []driver.Value{driver.Int(7)}},
			TxWrite:    driver.Query{Text: "update tobira_people set name = N'tx' where id = 1"},
			TxRestore:  driver.Query{Text: "update tobira_people set name = N'Ōtani Shōhei' where id = 1"},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		},
	})
}
