package d1

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// TestConformance runs drivertest Strict through the driver against the
// container-free D1 fake (fake_test.go), with bearer-token auth on. It
// proves the driver against our reading of the D1 REST API, not against
// Cloudflare.
func TestConformance(t *testing.T) {
	f := newFakeD1(t)
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), f.cfg(readOnly))
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
			cfg := f.cfg(false)
			cfg.Password = driver.NewSecret("not-the-token")
			c, err := New().Open(ctx, cfg)
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
			// The driver sends one statement per request, so no
			// multi-statement probe.
			CompoundWrites: []driver.Query{
				{Text: "with x as (select 1) delete from tobira_people where id = 1"},
				{Text: "insert into tobira_people (id, name, email) select 999, 'n', 'e@x' returning id"},
				{Text: "with x as (select 1) replace into tobira_people (id, name, email) select 1, 'n', 'e@x'"},
			},
			Slow:       driver.Query{Text: "with recursive c(x) as (select 1 union all select x+1 from c where x < 1000000000) select count(*) from c"},
			ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		},
	})
}
