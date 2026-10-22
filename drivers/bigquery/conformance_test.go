//go:build integration

package bigquery

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// TestConformance runs the drivertest kit, Strict, against the emulator,
// on a dataset of its own so its writes never disturb the other tests.
func TestConformance(t *testing.T) {
	dsn := "bigquery://" + testProject + "/" + conformanceDataset + "?emulator=" + emulatorAddr
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
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		// The emulator takes no credentials, so this proves only the
		// client-side check of the key; TestServiceAccountRejectedIsErrAuth
		// covers a key the token endpoint rejects.
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(`{"type":"service_account","client_email":"x@y","private_key":"not a key"}`)})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(false),
			Namespace:        conformanceDataset,
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
			CompoundWrites: []driver.Query{
				{Text: "select 1; delete from tobira_people where id = 1"},
				{Text: "with x as (select 1) delete from tobira_people where true"},
				{Text: "begin delete from tobira_people where true; end"},
				{Text: "execute immediate 'delete from tobira_people where true'"},
				{Text: "create table tobira_people_copy as select * from tobira_people"},
				{Text: "export data options(uri='gs://bucket/*.csv', format='CSV') as select * from tobira_people"},
				{Text: "/* c */ (delete from tobira_people where true)"},
				{Text: "#legacySQL\nselect * from [tobira:conformance.tobira_people]"},
			},
			Slow:       driver.Query{Text: "select count(*) from unnest(generate_array(1, 30000)) a, unnest(generate_array(1, 30000)) b"},
			ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		},
	})
}
