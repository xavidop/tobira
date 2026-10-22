//go:build integration

package weaviate

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleWrite asks to delete People's object 1: Weaviate's GraphQL has no
// mutations, and the driver refuses it on every path.
const peopleWrite = `mutation { Delete { TobiraPeople(where: {path: ["id"], operator: Equal, valueText: "00000000-0000-0000-0000-000000000001"}) } }`

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
		// The API-key container, with a wrong key.
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: "weaviate://" + testAuthHost, Password: driver.NewSecret("definitely-wrong")})
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
			// Weaviate has no decimal type and holds People's JSON tags as a
			// native text[] array.
			TypeOverrides: map[string]driver.Type{"score": driver.TypeFloat, "tags": driver.TypeList},
			SelectAll:     driver.Query{Text: `{"collection":"TobiraPeople"}`},
			Missing:       driver.Query{Text: `{"collection":"TobiraNoSuchCollection"}`},
			Write:         driver.Query{Text: peopleWrite},
			CompoundWrites: []driver.Query{
				{Text: `{ Get { TobiraPeople { name } } } mutation { Delete { TobiraPeople } }`},
				{Text: `query { Get { TobiraPeople { name } } } mutation M { Delete { TobiraPeople } }`},
				{Text: `subscription { TobiraPeople { name } }`},
				{Text: `{"collection":"TobiraPeople","op":"delete"}`},
				{Text: `{"collection":"TobiraPeople","collection":"x"}`},
				{Text: `[{"collection":"TobiraPeople","delete":true}]`},
				{Text: `{"collection":"TobiraPeople","objects":[{"id":"00000000-0000-0000-0000-000000000001"}]}`},
				{Text: `{ Get { TobiraPeople { name _additional { generate(singleResult: {prompt: "summarise {name}"}) { singleResult } } } } }`},
				{Text: `{ Get { TobiraPeople(ask: {question: "who?"}) { name } } }`},
				{Text: `{ Explore(concepts: ["x"]) { beacon } }`},
				{Text: `fragment F on TobiraPeople { name } { Get { TobiraPeople { ...F } } }`},
				{Text: `query($n: Int) { Get { TobiraPeople(limit: $n) { name } } }`},
			},
			Vector: drivertest.KindVectorFixture(driver.CollectionRef{Namespace: defaultNamespace, Name: vecsColl}),
		},
	})
}
