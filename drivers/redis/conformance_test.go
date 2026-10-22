//go:build integration

package redis

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// TestConformance runs the drivertest kit in strict mode. People lives in
// database 0 as hashes tobira_people:<id>, which makes "tobira_people" a
// record collection; SelectAll reads it through a RediSearch index.
func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn(dbConformance), ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	people := driver.CollectionRef{Namespace: "0", Name: "tobira_people"}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn(dbConformance), Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			Namespace:  "0",
			Collection: "tobira_people",
			SelectAll:  driver.Query{Text: "FT.SEARCH " + peopleIndex + " * SORTBY id ASC LIMIT 0 1000"},
			Missing:    driver.Query{Text: "FT.SEARCH tobira_no_such_index *"},
			Write:      driver.Query{Text: "DEL tobira_people:1"},
			CompoundWrites: []driver.Query{
				{Text: `EVAL "return redis.call('DEL', KEYS[1])" 1 tobira_people:1`},
				{Text: `EVAL_RO "return redis.call('DEL', KEYS[1])" 1 tobira_people:1`},
				{Text: "MULTI"},
				{Text: "FUNCTION FLUSH"},
				{Text: "FLUSHDB"},
				{Text: "del tobira_people:1"},
				{Text: `"DEL" tobira_people:1`},
				{Text: "SORT tobira_people:1 STORE tobira_people:1"},
			},
			Watch: &driver.WatchRequest{Collection: &people},
		},
	})
}
