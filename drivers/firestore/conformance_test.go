//go:build integration

package firestore

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleWrite deletes People's document 1: refused on every read-only
// path, and on a writable one too, since a request only reads.
const peopleWrite = `{"delete":"tobira_people/1"}`

// TestConformance runs the drivertest kit, Strict, against the emulator,
// in a project of its own so its writes never disturb the other tests.
func TestConformance(t *testing.T) {
	dsn := dsnFor(conformanceProject)
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
		// The emulator takes no credentials, so this proves the client-side
		// check of the key only (the key's private key does not parse).
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(
				`{"type":"service_account","client_email":"x@y","private_key":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"}`)})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			Namespace:    testDatabase,
			Collection:   people,
			NoPrimaryKey: true, // the key is _id, the document id; People's id is a plain field
			// Firestore has no decimal type and holds People's JSON tags as
			// a native array.
			TypeOverrides: map[string]driver.Type{"score": driver.TypeFloat, "tags": driver.TypeList},
			SelectAll:     driver.Query{Text: `{"collection":"` + people + `","orderBy":["id"]}`},
			// A query on a missing collection returns no documents, as in
			// Firestore; reading a missing document is NotFound.
			Missing: driver.Query{Text: `{"document":"tobira_no_such_collection/x"}`},
			Write:   driver.Query{Text: peopleWrite},
			CompoundWrites: []driver.Query{
				{Text: `{"collection":"` + people + `","delete":true}`},
				{Text: `{"collection":"` + people + `"} {"delete":"` + people + `/1"}`},
				{Text: `{"collection":"` + people + `","collection":"x"}`},
				{Text: `[{"delete":"` + people + `/1"}]`},
				{Text: `{"set":"` + people + `/1","data":{"name":"x"}}`},
				{Text: `{"update":"` + people + `/1","data":{"name":"x"}}`},
				{Text: `{"batch":[{"delete":"` + people + `/1"}]}`},
				{Text: `{"commit":{"writes":[{"delete":"` + people + `/1"}]}}`},
				{Text: `{"document":"` + people + `/1","data":{"name":"x"}}`},
			},
			Watch:      &driver.WatchRequest{Collection: &driver.CollectionRef{Namespace: testDatabase, Name: people}},
			CompleteAt: driver.CompletionRequest{Text: `{"collection":"tobira_`, Cursor: len(`{"collection":"tobira_`)},
		},
	})
}
