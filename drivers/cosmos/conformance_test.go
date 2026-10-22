//go:build integration

package cosmos

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// conformanceDB is the database the conformance kit runs against, separate
// from the shared one so the kit's writes never disturb the other tests.
const conformanceDB = "conformance"

func TestConformance(t *testing.T) {
	ctx := context.Background()
	if err := createDatabase(ctx, conformanceDB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropDatabase(conformanceDB) })
	if err := seedPeople(ctx, conformanceDB); err != nil {
		t.Fatal(err)
	}
	if err := seedVectors(ctx, conformanceDB); err != nil {
		t.Fatal(err)
	}
	dsn := dsnFor(conformanceDB)
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(emulatorKey), ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	people := drivertest.PeopleCollection()
	vector := drivertest.KindVectorFixture(driver.CollectionRef{Namespace: conformanceDB, Name: vectorsName})
	// Cosmos DB ids are strings: the vector rows' ids are their decimal text.
	vector.WantTopID = driver.String("1")
	vector.WantTopByMetric = map[driver.VectorMetric]driver.Value{
		driver.MetricL2: driver.String("1"), driver.MetricCosine: driver.String("2"), driver.MetricInnerProduct: driver.String("3"),
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		// OpenBadAuth is nil: the emulator accepts any account key (a
		// real account answers 401, mapped to ErrAuth; see
		// TestOpenMapsServerRefusals).
		Strict: true,
		Fixture: drivertest.Fixture{
			Namespace:  conformanceDB,
			Collection: people,
			// Declared storage limits: Cosmos DB ids are strings, numbers
			// are IEEE doubles (no decimal), times are RFC 3339 text and
			// JSON arrays are native lists.
			TypeOverrides: map[string]driver.Type{
				"id": driver.TypeString, "score": driver.TypeFloat, "created_at": driver.TypeString, "tags": driver.TypeList,
			},
			// created_at grows with id and RFC 3339 UTC text sorts as time.
			SelectAll:  driver.Query{Text: "SELECT * FROM " + people + " p ORDER BY p.created_at"},
			Missing:    driver.Query{Text: "SELECT * FROM tobira_no_such_container c"},
			Write:      driver.Query{Text: "DELETE FROM " + people + " p WHERE p.id = '1'"},
			ParamQuery: driver.Query{Text: "SELECT * FROM " + people + " p WHERE p.id = ToString(@p1)", Params: []driver.Value{driver.Int(7)}},
			CompoundWrites: []driver.Query{
				{Text: "SELECT * FROM " + people + " p; DELETE FROM " + people + " p"},
				{Text: `{"container":"` + people + `","query":"DELETE FROM c"}`},
				{Text: `{"container":"` + people + `","query":"SELECT * FROM c","storedProcedure":"bulkDelete"}`},
				{Text: `{"container":"` + people + `","query":"SELECT * FROM c","preTrigger":"wipe"}`},
				{Text: "SELECT udf.wipe(p) FROM " + people + " p"},
				{Text: "EXEC bulkDelete('" + people + "')"},
				{Text: "UPSERT INTO " + people + " VALUE {\"id\":\"1\"}"},
			},
			Watch:      &driver.WatchRequest{Collection: &driver.CollectionRef{Namespace: conformanceDB, Name: people}},
			CompleteAt: driver.CompletionRequest{Text: "SELECT * FROM tobira_", Cursor: len("SELECT * FROM tobira_"), Namespace: conformanceDB},
			Vector:     vector,
		},
	})
}
