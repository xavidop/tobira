//go:build integration

package libsql

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// TestConformance runs drivertest Strict against sqld, with JWT auth on: the
// token reaches the driver through ConnConfig.Password, as a password ref
// resolves.
func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), cfgFor(readOnly, rwToken))
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
			c, err := New().Open(ctx, cfgFor(false, "not-a-jwt"))
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(true), // id INTEGER PRIMARY KEY is the rowid
			Namespace:        "main",
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
			// The driver runs one statement per call (and sqld refuses more),
			// so no multi-statement probe.
			CompoundWrites: []driver.Query{
				{Text: "with x as (select 1) delete from tobira_people where id = 1"},
				{Text: "insert into tobira_people (id, name, email) select 999, 'n', 'e@x' returning id"},
				{Text: "with x as (select 1) replace into tobira_people (id, name, email) select 1, 'n', 'e@x'"},
			},
			// About 6.5 s on sqld: a recursive count to 50 million.
			Slow:       driver.Query{Text: "with recursive c(x) as (select 1 union all select x+1 from c where x < 50000000) select count(*) from c"},
			ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
			TxWrite:    driver.Query{Text: "update tobira_people set name = 'tx' where id = 1"},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
			TxRestore:  driver.Query{Text: "update tobira_people set name = 'Ōtani Shōhei' where id = 1"},
		},
	})
}
