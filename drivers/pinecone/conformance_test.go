//go:build integration

package pinecone

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleWrite deletes People's record 1: refused on every read-only path.
const peopleWrite = `{"op":"delete","namespace":"tobira_people","ids":["1"]}`

// compoundWrites are the guard bypasses tried with Query.ReadOnly; each
// would change People if it ran.
var compoundWrites = []string{
	`{"op":"list","op":"delete","namespace":"tobira_people","ids":["1"]}`,
	`{"op":"delete","op":"list","namespace":"tobira_people","ids":["1"]}`,
	`{"op":"list","namespace":"tobira_people","deleteAll":true}`,
	`{"op":"query","namespace":"tobira_people","id":"1","deleteAll":true}`,
	`{"op":"fetch","namespace":"tobira_people","ids":["1"],"setMetadata":{"name":"x"}}`,
	`{"op":"upsert","namespace":"tobira_people","vectors":[{"id":"1","values":[0,0,0,1]}]}`,
	`{"op":"update","namespace":"tobira_people","id":"1","setMetadata":{"name":"x"}}`,
	`{"op":"delete","namespace":"tobira_people","deleteAll":true}`,
	`{"op":"Delete","namespace":"tobira_people","ids":["1"]}`,
	`{"op":"deleteNamespace","namespace":"tobira_people"}`,
	`{"op":"delete_index","index":"tobira"}`,
	`{"op":"list","namespace":"tobira_people"} {"op":"delete","namespace":"tobira_people","ids":["1"]}`,
}

func TestConformance(t *testing.T) {
	openWith := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: indexDSN(mainIndex), Password: driver.NewSecret(testAPIKey), ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	writes := make([]driver.Query, len(compoundWrites))
	for i, w := range compoundWrites {
		writes[i] = driver.Query{Text: w}
	}
	vec := drivertest.KindVectorFixture(driver.CollectionRef{Namespace: mainIndex, Name: vectorsNS})
	// The metric is fixed per index: this one is euclidean. The cosine and
	// dot-product winners are checked on their own indexes (TestMetrics).
	vec.WantTopByMetric = map[driver.VectorMetric]driver.Value{driver.MetricL2: vec.WantTopByMetric[driver.MetricL2]}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         openWith(false),
		OpenReadOnly: openWith(true),
		// pinecone-local accepts any API key: ErrAuth is covered against a
		// fake control plane (TestControlPlaneBadAuth).
		OpenBadAuth: nil,
		Strict:      true,
		Fixture: drivertest.Fixture{
			Namespace:  mainIndex,
			Collection: peopleNS,
			// The key is _id; People's id is a metadata key.
			NoPrimaryKey: true,
			// A listing comes in id order as text ("1", "10", "11"...).
			UnorderedSelectAll: true,
			// Declared storage limits: metadata numbers are float64 (a
			// fractional score reads as Float), there is no time type (RFC
			// 3339 text) and JSON is only a list of strings.
			TypeOverrides:  map[string]driver.Type{"score": driver.TypeFloat, "created_at": driver.TypeString, "tags": driver.TypeList},
			SelectAll:      driver.Query{Text: `{"op":"list","namespace":"tobira_people"}`},
			Missing:        driver.Query{Text: `{"op":"list","index":"tobira-missing","namespace":"tobira_people"}`},
			Write:          driver.Query{Text: peopleWrite},
			CompoundWrites: writes,
			Vector:         vec,
			// Every Pinecone record needs its dense vector; the kit's probe
			// row has none.
			InsertDefaults: map[string]driver.Value{valuesColumn: driver.Vector(peopleVector(1000))},
		},
	})
}
