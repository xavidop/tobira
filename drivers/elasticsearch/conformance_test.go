//go:build integration

package elasticsearch

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleWrite deletes People's document 1: refused on every read-only
// path.
const peopleWrite = `{"method":"POST","path":"/tobira_people/_delete_by_query","body":{"query":{"term":{"id":1}}}}`

// compoundWrites are the guard bypasses tried with Query.ReadOnly; each
// would change People if it ran.
var compoundWrites = []string{
	`{"method":"POST","path":"/tobira_people/_update_by_query","body":{"query":{"term":{"id":1}},"script":{"source":"ctx._source.name = 'x'"}}}`,
	`{"method":"POST","path":"/tobira_people/_update_by_query?conflicts=proceed","body":{"query":{"term":{"id":1}}}}`,
	`{"method":"POST","path":"/_bulk","body":[{"delete":{"_index":"tobira_people","_id":"1"}}]}`,
	`{"method":"POST","path":"/tobira_people/_bulk","body":[{"delete":{"_id":"1"}}]}`,
	`{"method":"DELETE","path":"/tobira_people/_doc/1"}`,
	`{"method":"PUT","path":"/tobira_people/_doc/1","body":{"id":1}}`,
	`{"method":"POST","path":"/tobira_people/_update/1","body":{"doc":{"name":"x"}}}`,
	`{"method":"POST","path":"/tobira_people/_search/../_delete_by_query","body":{"query":{"term":{"id":1}}}}`,
	`{"method":"POST","path":"/tobira_people%2F_delete_by_query","body":{"query":{"term":{"id":1}}}}`,
	`{"method":"POST","path":"//tobira_people/_delete_by_query","body":{"query":{"term":{"id":1}}}}`,
	`{"method":"GET","path":"/tobira_people/_search","method":"POST","path":"/tobira_people/_delete_by_query","body":{"query":{"term":{"id":1}}}}`,
	`{"method":"post","path":"/tobira_people/_delete_by_query","body":{"query":{"term":{"id":1}}}}`,
	`{"method":"POST","path":"/_reindex","body":{"source":{"index":"articles"},"dest":{"index":"tobira_people"}}}`,
	`{"method":"PUT","path":"/tobira_people/_mapping","body":{"properties":{"name":{"type":"text","fields":{"raw":{"type":"keyword"}}}}}}`,
	`{"method":"POST","path":"/tobira_people/_close"}`,
	`{"method":"DELETE","path":"/tobira_people"}`,
	`{"index":"tobira_people","query":{"script":{"script":{"source":"doc['id'].value == 1"}}}}`,
	`{"index":"tobira_people","script_fields":{"x":{"script":{"source":"1"}}}}`,
	`{"method":"GET","path":"/tobira_people/_search?source={\"query\":{\"match_all\":{}}}&source_content_type=application/json"}`,
	`{"index":"tobira_people"} {"method":"DELETE","path":"/tobira_people"}`,
}

func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := testDriver().Open(context.Background(), testConfig(readOnly))
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	var badAuth func(ctx context.Context) error
	if engine.user != "" {
		badAuth = func(ctx context.Context) error {
			cfg := testConfig(false)
			cfg.Password = driver.NewSecret("definitely-wrong")
			c, err := testDriver().Open(ctx, cfg)
			if err == nil {
				_ = c.Close()
			}
			return err
		}
	}
	writes := make([]driver.Query, len(compoundWrites))
	for i, w := range compoundWrites {
		writes[i] = driver.Query{Text: w}
	}
	fx := drivertest.Fixture{
		Namespace:  defaultNamespace,
		Collection: peopleIndex,
		// The key is _id; People's id is a plain field.
		NoPrimaryKey:   true,
		SelectAll:      driver.Query{Text: `{"index":"tobira_people","sort":[{"id":"asc"}]}`},
		Missing:        driver.Query{Text: `{"index":"tobira_nope","query":{"match_all":{}}}`},
		Write:          driver.Query{Text: peopleWrite},
		CompoundWrites: writes,
		Search: drivertest.KindSearchFixture(
			driver.CollectionRef{Namespace: defaultNamespace, Name: drivertest.SearchCollection},
			driver.Query{Text: `{"index":"articles","query":{"multi_match":{"query":"database","fields":["title","body"]}}}`},
		),
	}
	if engine.fl == flavorElastic {
		fx.Vector = drivertest.KindVectorFixture(driver.CollectionRef{Namespace: defaultNamespace, Name: vecsIndex})
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       testDriver(),
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth:  badAuth,
		Strict:       true,
		Fixture:      fx,
	})
}
