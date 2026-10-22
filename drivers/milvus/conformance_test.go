//go:build integration

package milvus

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleWrite deletes People's entity 1: refused on every read-only path.
const peopleWrite = `{"op":"delete","collection":"tobira_people","filter":"id == 1"}`

func TestConformance(t *testing.T) {
	openWith := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN(), ReadOnly: readOnly})
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
			c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN(), Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(false),
			Namespace:        defaultDB,
			Collection:       peopleColl,
			SelectAll:        driver.Query{Text: `{"collection":"tobira_people"}`},
			Missing:          driver.Query{Text: `{"collection":"nope"}`},
			Write:            driver.Query{Text: peopleWrite},
			ParamQuery:       driver.Query{Text: `{"collection":"tobira_people","filter":"id == {p1}"}`, Params: []driver.Value{driver.Int(7)}},
			// Declared storage limit: Milvus has no decimal type, so
			// score is a Double.
			TypeOverrides: map[string]driver.Type{"score": driver.TypeFloat},
			CompoundWrites: []driver.Query{
				{Text: `{"op":"query","op":"delete","collection":"tobira_people","filter":"id == 1"}`},
				{Text: `{"op":"delete","op":"query","collection":"tobira_people","filter":"id == 1"}`},
				{Text: `{"op":"upsert","collection":"tobira_people","data":[{"id":1,"name":"x"}]}`},
				{Text: `{"op":"insert","collection":"tobira_people","data":[{"id":1000}]}`},
				{Text: `{"op":"Delete","collection":"tobira_people","filter":"id == 1"}`},
				{Text: `{"op":"drop_collection","collection":"tobira_people"}`},
				{Text: `{"op":"release","collection":"tobira_people"}`},
				{Text: `{"op":"load","collection":"tobira_people"}`},
				{Text: `{"collection":"tobira_people","filter":"id == 1","delete":true}`},
				{Text: `{"collection":"tobira_people","filter":"id == 1) or (id > 0"}`},
				{Text: `{"collection":"tobira_people","filter":"delete_entities(id)"}`},
				{Text: `{"collection":"tobira_people","vector":[1,0],"annsField":"embedding","rerank":{"inputFields":["name"],"params":{"reranker":"model","provider":"cohere","queries":["x"]}}}`},
			},
			Vector: drivertest.KindVectorFixture(driver.CollectionRef{Namespace: defaultDB, Name: vecsColl}),
		},
	})
}
