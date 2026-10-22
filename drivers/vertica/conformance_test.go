//go:build integration

package vertica

import (
	"context"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// TestConformance runs the drivertest kit, Strict, against Vertica with
// one declared storage limit: Vertica has no JSON type, so tags is a
// VARCHAR holding the JSON text.
func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN, Password: driver.NewSecret(testPassword), ReadOnly: readOnly})
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
			c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN, Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(false),
			TypeOverrides:    map[string]driver.Type{"tags": driver.TypeString},
			Namespace:        "public",
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
			TxWrite:          driver.Query{Text: "update tobira_people set name = 'changed in tx' where id = 1"},
			TxRestore:        driver.Query{Text: "update tobira_people set name = 'Ōtani Shōhei' where id = 1"},
			CompoundWrites: []driver.Query{
				{Text: "select 1; delete from tobira_people where id = 1"},
				{Text: "with c as (select 1) delete from tobira_people where id = 1"},
				{Text: "select * from tobira_people for update"},
				{Text: "select * into tobira_people_copy from tobira_people"},
				{Text: "select 0x1delete from tobira_people where id = 1"},
				{Text: "select 1 -- c\rdelete from tobira_people where id = 1"},
				{Text: "explain analyze delete from tobira_people where id = 1"},
				{Text: "select set_config_parameter('MaxClientSessions', 0)"},
			},
			Slow:       driver.Query{Text: "SELECT SLEEP(10)"},
			ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		},
	})
}
