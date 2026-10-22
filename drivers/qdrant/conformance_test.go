//go:build integration

package qdrant

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleWrite deletes People's point 1: refused on every read-only path.
const peopleWrite = `{"op":"delete","collection":"tobira_people","points":{"points":{"ids":[{"num":"1"}]}}}`

func TestConformance(t *testing.T) {
	openWith := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN(testAPIKey), ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         openWith(false),
		OpenReadOnly: openWith(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN(testAPIKey), Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(false),
			Namespace:        defaultNamespace,
			Collection:       peopleColl,
			SelectAll:        driver.Query{Text: `{"op":"scroll","collection":"tobira_people"}`},
			Missing:          driver.Query{Text: `{"op":"scroll","collection":"nope"}`},
			Write:            driver.Query{Text: peopleWrite},
			CompoundWrites: []driver.Query{
				{Text: `{"op":"scroll","op":"delete","collection":"tobira_people","points":{"points":{"ids":[{"num":"1"}]}}}`},
				{Text: `{"op":"delete","op":"scroll","collection":"tobira_people","points":{"points":{"ids":[{"num":"1"}]}}}`},
				{Text: `{"op":"scroll","collection":"tobira_people","points":{"points":{"ids":[{"num":"1"}]}}}`},
				{Text: `{"op":"update_batch","collection":"tobira_people","operations":[{"delete_points":{"points":{"points":{"ids":[{"num":"1"}]}}}}]}`},
				{Text: `{"op":"set_payload","collection":"tobira_people","payload":{"name":{"string_value":"x"}},"points_selector":{"points":{"ids":[{"num":"1"}]}}}`},
				{Text: `{"op":"delete_collection","collection":"tobira_people"}`},
				{Text: `{"op":"Delete","collection":"tobira_people","points":{"points":{"ids":[{"num":"1"}]}}}`},
			},
			Vector: drivertest.KindVectorFixture(driver.CollectionRef{Namespace: defaultNamespace, Name: vecsColl}),
		},
	})
}
