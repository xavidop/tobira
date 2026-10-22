//go:build integration

package mongodb

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
	if err := seedPeople(ctx, conformanceDB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Database(conformanceDB).Drop(context.Background()) })
	dsn := dsnFor(conformanceDB, testPassword)
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
			Namespace:    conformanceDB,
			Collection:   people,
			NoPrimaryKey: true, // the key is _id; People's id is a plain field
			// A document store holds People's JSON tags as a native array.
			TypeOverrides: map[string]driver.Type{"tags": driver.TypeList},
			SelectAll:     driver.Query{Text: `{"find":"` + people + `","sort":{"_id":1}}`},
			// find on a missing collection returns no documents, as in
			// MongoDB; listIndexes fails with NamespaceNotFound.
			Missing: driver.Query{Text: `{"listIndexes":"nope"}`},
			Write:   driver.Query{Text: `{"delete":"` + people + `","deletes":[{"q":{"_id":1},"limit":1}]}`},
			CompoundWrites: []driver.Query{
				{Text: `{"aggregate":"` + people + `","pipeline":[{"$match":{"_id":1}},{"$out":"` + people + `"}]}`},
				{Text: `{"aggregate":"` + people + `","pipeline":[{"$set":{"name":"merged"}},{"$merge":{"into":"` + people + `","whenMatched":"replace"}}]}`},
				{Text: `{"aggregate":"` + people + `","pipeline":[{"$facet":{"x":[{"$out":"` + people + `"}]}}]}`},
				{Text: `{"findAndModify":"` + people + `","query":{"_id":1},"remove":true}`},
				{Text: `{"mapReduce":"` + people + `","map":"function(){emit(this._id,1)}","reduce":"function(k,v){return 1}","out":{"replace":"` + people + `"}}`},
				{Text: `{"explain":{"delete":"` + people + `","deletes":[{"q":{},"limit":0}]},"verbosity":"executionStats"}`},
				{Text: `{"drop":"` + people + `"}`},
			},
			Watch:      &driver.WatchRequest{Collection: &driver.CollectionRef{Namespace: conformanceDB, Name: people}},
			CompleteAt: driver.CompletionRequest{Text: `{"find":"tobira_`, Cursor: len(`{"find":"tobira_`)},
			Slow:       driver.Query{Text: `{"find":"` + people + `","filter":{"_id":1,"$where":"sleep(10000) || true"}}`},
		},
	})
}
