//go:build integration

package clickhouse

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// conformanceDB is the database the conformance kit runs against. It is
// separate from the shared one so the kit's writes can never disturb the
// driver's own tests.
const conformanceDB = "conformance"

func TestConformance(t *testing.T) {
	db := admin(t)
	mustExec(t, db, "CREATE DATABASE IF NOT EXISTS "+conformanceDB)
	mustExec(t, db, peopleDDL(conformanceDB+".tobira_people"))
	mustExec(t, db, peopleInsert(conformanceDB+".tobira_people"))
	t.Cleanup(func() { _ = db.Exec(context.Background(), "DROP DATABASE IF EXISTS "+conformanceDB) })

	for _, p := range []struct{ name, addr, params string }{
		{"native", nativeAddr, ""},
		{"http", httpAddr, "?protocol=http"},
	} {
		t.Run(p.name, func(t *testing.T) {
			dsn := "clickhouse://default:" + testPassword + "@" + p.addr + "/" + conformanceDB + p.params
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
				OpenBadAuth: func(ctx context.Context) error {
					c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("definitely-wrong")})
					if err == nil {
						_ = c.Close()
					}
					return err
				},
				Strict: true,
				Fixture: drivertest.Fixture{
					AutoIncrementKey: drivertest.Bool(false),
					Namespace:        conformanceDB,
					Collection:       "tobira_people",
					SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
					Missing:          driver.Query{Text: "select * from nope"},
					Write:            driver.Query{Text: "alter table tobira_people delete where id = 1 settings mutations_sync = 1"},
					CompoundWrites: []driver.Query{
						{Text: "select 1; delete from tobira_people where id = 1"},
						{Text: "insert into tobira_people (id, name, email, score, active, created_at) with 1 as x select 1000, 'n', 'e', 0, true, now()"},
						{Text: "insert into function remote('127.0.0.1', " + conformanceDB + ".tobira_people) (id, name, email, score, active, created_at) select 1001, 'n', 'e', 0, true, now()"},
					},
					Slow:       driver.Query{Text: "select sum(sleepEachRow(0.5)) from numbers(40) settings max_block_size = 1"},
					ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
					CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
				},
			})
		})
	}
}
