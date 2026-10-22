//go:build integration

package influxdb

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN(""), ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	const people = "tobira_people"
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN(""), Password: driver.NewSecret("apiv3_definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			Namespace:    testDB,
			Collection:   people,
			NoPrimaryKey: true, // the series key is (tags…, time), not id
			// What InfluxDB 3 stores (see peopleLP): it has no decimal or
			// JSON type and a single timestamp column, time.
			TypeOverrides: map[string]driver.Type{
				"score":      driver.TypeFloat,
				"tags":       driver.TypeString,
				"created_at": driver.TypeInt, // Unix nanoseconds
			},
			SelectAll:  driver.Query{Text: "SELECT id, name, email, age, score, active, created_at, tags FROM " + people + " ORDER BY id"},
			Missing:    driver.Query{Text: "SELECT * FROM nope"},
			Write:      driver.Query{Text: "DELETE FROM " + people + " WHERE id = 1"},
			TxWrite:    driver.Query{Text: "UPDATE " + people + " SET name = 'tx' WHERE id = 1"},
			ParamQuery: driver.Query{Text: "SELECT * FROM " + people + " WHERE id = $1", Params: []driver.Value{driver.Int(7)}},
			Slow:       driver.Query{Text: "SELECT count(*) FROM range(1, 20000000000)"},
			CompoundWrites: []driver.Query{
				{Text: "SELECT 1; DELETE FROM " + people + " WHERE id = 1"},
				{Text: "SELECT * INTO tobira_copy FROM " + people},
				{Text: "WITH x AS (SELECT 1) DELETE FROM " + people},
				{Text: "WITH d AS (DELETE FROM " + people + " RETURNING *) SELECT * FROM d"},
				{Text: "EXPLAIN ANALYZE DELETE FROM " + people},
				{Text: "COPY (SELECT * FROM " + people + ") TO '/tmp/tobira.csv'"},
				{Text: "CREATE TABLE tobira_copy AS SELECT * FROM " + people},
				{Text: "INSERT INTO " + people + " (time, id) VALUES (now(), 99)"},
				{Text: "DROP TABLE " + people},
			},
			TimeSeries: drivertest.KindTimeSeriesFixture(driver.CollectionRef{Namespace: testDB, Name: drivertest.TimeSeriesCollection}),
		},
	})
}
