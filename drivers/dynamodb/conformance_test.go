//go:build integration

package dynamodb

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// selectAll is the conformance kit's SelectAll: a full PartiQL scan. A
// scan returns items in hash order and PartiQL orders only within one
// partition, so the fixture sets UnorderedSelectAll and the kit matches the
// rows by id.
const selectAll = `SELECT * FROM "tobira_people"`

func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), cfg(readOnly))
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
		// DynamoDB Local accepts any credentials, so a bad-auth open cannot
		// fail against it; TestOpenBadCredentialsAgainstFakeServer covers
		// the mapping of AWS's refusal to ErrAuth.
		OpenBadAuth: nil,
		Strict:      true,
		Fixture: drivertest.Fixture{
			Namespace:  namespaceName,
			Collection: people,
			// DynamoDB has no time type: created_at is an RFC 3339 string.
			// A document store holds People's JSON tags as a native list.
			TypeOverrides:      map[string]driver.Type{"created_at": driver.TypeString, "tags": driver.TypeList},
			SelectAll:          driver.Query{Text: selectAll},
			UnorderedSelectAll: true,
			Missing:            driver.Query{Text: `SELECT * FROM "tobira_no_such_table"`},
			Write:              driver.Query{Text: `DELETE FROM "tobira_people" WHERE id = 1`},
			ParamQuery:         driver.Query{Text: `SELECT * FROM "tobira_people" WHERE id = ?`, Params: []driver.Value{driver.Int(7)}},
			CompoundWrites: []driver.Query{
				{Text: `UPDATE "tobira_people" SET name = 'x' WHERE id = 1`},
				{Text: `update "tobira_people" set name = 'x' where id = 1`},
				{Text: `INSERT INTO "tobira_people" VALUE {'id': 1001, 'name': 'x'}`},
				{Text: `/* SELECT */ DELETE FROM "tobira_people" WHERE id = 1`},
				{Text: "-- SELECT\nDELETE FROM \"tobira_people\" WHERE id = 1"},
				{Text: `SELECT * FROM "tobira_people"; DELETE FROM "tobira_people" WHERE id = 1`},
				{Text: `UPDATE "tobira_people" REMOVE email WHERE id = 1`},
				{Text: `DELETE FROM "tobira_people" WHERE id = 1 RETURNING ALL OLD *`},
				{Text: `EXISTS(SELECT * FROM "tobira_people" WHERE id = 1)`},
			},
			CompleteAt: driver.CompletionRequest{Text: `SELECT * FROM tobira_`, Cursor: len(`SELECT * FROM tobira_`)},
			Watch:      &driver.WatchRequest{Collection: &driver.CollectionRef{Namespace: namespaceName, Name: people}},
		},
	})
}
