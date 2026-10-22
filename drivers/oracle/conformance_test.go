//go:build integration

package oracle

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN, ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	const del = `delete from "tobira_people" where "id" = 1`
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: testDSN, Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(false), // "id" NUMBER(10), no identity
			Namespace:        "TOBIRA",
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: `select * from "tobira_people" order by "id"`},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: del},
			CompoundWrites: []driver.Query{
				{Text: "select 1 from dual; " + del},
				{Text: "begin " + del + "; end;"},
				{Text: "declare n number; begin " + del + "; commit; end;"},
				{Text: "with function f return number is pragma autonomous_transaction; begin " + del + "; commit; return 1; end; select f from dual"},
				{Text: `select * from "tobira_people" for update`},
				{Text: `create table tobira_ro_probe as select * from "tobira_people"`},
				{Text: `truncate table "tobira_people"`},
				{Text: `call dbms_output.put_line('x')`},
				{Text: `select dbms_pipe.purge('x') from dual`},
				{Text: `merge into "tobira_people" p using dual on (p."id" = 1) when matched then update set p."name" = 'x'`},
				{Text: "select q'[x]' from dual where 1 = 0 union all select 1 from dual; " + del},
				{Text: "select 1 from dual -- c\r; " + del},
				{Text: "select 1delete from \"tobira_people\""},
			},
			Slow:       driver.Query{Text: "select count(*) from all_objects a, all_objects b, all_objects c"},
			ParamQuery: driver.Query{Text: `select * from "tobira_people" where "id" = :1`, Params: []driver.Value{driver.Int(7)}},
			TxWrite:    driver.Query{Text: `update "tobira_people" set "name" = 'tx' where "id" = 1`},
			TxRestore:  driver.Query{Text: `update "tobira_people" set "name" = 'Ōtani Shōhei' where "id" = 1`},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		},
	})
}
