//go:build integration

package cassandra

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// conformanceKS is the keyspace the conformance kit runs against, separate
// from the shared one so the kit's writes never disturb the other tests.
const conformanceKS = "conformance"

func TestConformance(t *testing.T) {
	ctx := context.Background()
	if err := seedPeople(ctx, conformanceKS); err != nil {
		t.Fatal(err)
	}
	if err := seedEvents(ctx, conformanceKS); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec(context.Background(), "DROP KEYSPACE "+conformanceKS) })
	dsn := dsnFor(conformanceKS, testPassword, "")
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
	people := drivertest.PeopleCollection()
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
			Namespace:  conformanceKS,
			Collection: people,
			// Cassandra has no JSON type: People's tags are a
			// frozen<list<text>>, a declared storage limit.
			TypeOverrides: map[string]driver.Type{"tags": driver.TypeList},
			// Ordered by id under the test node's ByteOrderedPartitioner
			// (see containerScript): CQL has no ORDER BY across partitions.
			SelectAll:  driver.Query{Text: "SELECT * FROM " + people},
			ParamQuery: driver.Query{Text: "SELECT * FROM " + people + " WHERE id = ?", Params: []driver.Value{driver.Int(7)}},
			Missing:    driver.Query{Text: "SELECT * FROM tobira_no_such_table"},
			Write:      driver.Query{Text: "DELETE FROM " + people + " WHERE id = 1"},
			CompoundWrites: []driver.Query{
				{Text: "BEGIN BATCH DELETE FROM " + people + " WHERE id = 1; APPLY BATCH"},
				{Text: "BEGIN UNLOGGED BATCH UPDATE " + people + " SET name = 'x' WHERE id = 1 APPLY BATCH"},
				{Text: "SELECT * FROM " + people + "; DELETE FROM " + people + " WHERE id = 1"},
				{Text: "/* SELECT */ DELETE FROM " + people + " WHERE id = 1"},
				{Text: "INSERT INTO " + people + " JSON '{\"id\": 1, \"name\": \"json\"}'"},
				{Text: "UPDATE " + people + " SET name = 'x' WHERE id = 1 IF EXISTS"},
				{Text: "TRUNCATE " + people},
				{Text: "DROP TABLE " + people},
			},
			CompleteAt: driver.CompletionRequest{Text: "SELECT * FROM ", Cursor: len("SELECT * FROM "), Namespace: conformanceKS},
			WideColumn: drivertest.KindWideColumnFixture(
				driver.CollectionRef{Namespace: conformanceKS, Name: drivertest.WideColumnCollection},
				driver.Query{Text: "SELECT * FROM events WHERE tenant = '" + drivertest.WideColumnPartition + "'"},
			),
		},
	})
}
