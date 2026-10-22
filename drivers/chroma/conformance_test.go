//go:build integration

package chroma

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleWrite deletes People's record 1: refused on every read-only path.
const peopleWrite = `{"op":"delete","collection":"tobira_people","ids":["1"]}`

func TestConformance(t *testing.T) {
	openWith := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), testCfg(readOnly))
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	vec := drivertest.KindVectorFixture(driver.CollectionRef{Namespace: defaultDB, Name: vecsColl})
	// Chroma fixes the space per collection: vecs is l2. The cosine and
	// inner-product winners are checked on their own collections
	// (TestSimilarMetrics).
	delete(vec.WantTopByMetric, driver.MetricCosine)
	delete(vec.WantTopByMetric, driver.MetricInnerProduct)
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         openWith(false),
		OpenReadOnly: openWith(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN(""), Password: driver.NewSecret("definitely-wrong")})
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
			Missing:          driver.Query{Text: `{"collection":"tobira_nope"}`},
			Write:            driver.Query{Text: peopleWrite},
			// Chroma's get has no order guarantee (it follows internal
			// offsets), so SelectAll rows are matched by id.
			UnorderedSelectAll: true,
			// Declared storage limits: Chroma metadata holds bool, int,
			// float and string values only (no decimal, time or JSON
			// type, and empty arrays are dropped).
			TypeOverrides: map[string]driver.Type{
				"score":      driver.TypeFloat,
				"created_at": driver.TypeString,
				"tags":       driver.TypeString,
			},
			CompoundWrites: []driver.Query{
				{Text: `{"op":"get","op":"delete","collection":"tobira_people","ids":["1"]}`},
				{Text: `{"op":"delete","op":"get","collection":"tobira_people","ids":["1"]}`},
				{Text: `{"collection":"tobira_people","ids":["1"],"embeddings":[[1,1]],"metadatas":[{"name":"x"}]}`},
				{Text: `{"op":"upsert","collection":"tobira_people","ids":["1"],"embeddings":[[1,1]]}`},
				{Text: `{"op":"update","collection":"tobira_people","ids":["1"],"metadatas":[{"name":"x"}]}`},
				{Text: `{"op":"add","collection":"tobira_people","ids":["999"],"embeddings":[[1,1]]}`},
				{Text: `{"op":"delete_collection","collection":"tobira_people"}`},
				{Text: `{"op":"search","collection":"tobira_people","rank":{"$knn":{"query":"hello"}}}`},
				{Text: `{"op":"Delete","collection":"tobira_people","ids":["1"]}`},
				{Text: `{"op":"get","collection":"tobira_people"} {"op":"delete","collection":"tobira_people","ids":["1"]}`},
				{Text: `{"op":"count","collection":"tobira_people","ids":["1"],"delete":true}`},
			},
			Vector: vec,
			// Every Chroma record needs an embedding; the kit's probe row
			// has none.
			InsertDefaults: map[string]driver.Value{embeddingColumn: driver.Vector([]float32{1000, 1})},
		},
	})
}
