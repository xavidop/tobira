package sqlite

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestConformance(t *testing.T) {
	path := seedFile(t)
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + path, ReadOnly: readOnly})
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
		Strict:       true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(true), // id INTEGER PRIMARY KEY is the rowid
			Namespace:        "main",
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
			// The driver runs one statement per call, so no multi-statement probe.
			CompoundWrites: []driver.Query{
				{Text: "with x as (select 1) delete from tobira_people where id = 1"},
				{Text: "insert into tobira_people (id, name, email) select 999, 'n', 'e@x' returning id"},
			},
			Slow:       driver.Query{Text: "with recursive c(x) as (select 1 union all select x+1 from c where x < 1000000000) select count(*) from c"},
			ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
			TxWrite:    driver.Query{Text: "update tobira_people set name = 'tx' where id = 1"},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
			TxRestore:  driver.Query{Text: "update tobira_people set name = 'Ōtani Shōhei' where id = 1"},
		},
	})
}
