//go:build integration

package snowflake

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// conformanceSchema is the schema the conformance kit runs against,
// separate from PUBLIC so the kit's writes never disturb the driver's own
// tests.
const conformanceSchema = "CONFORMANCE"

// The Strict run differs from the production driver in exactly two
// declared ways, both because fakesnow is not Snowflake:
//
//   - Explainer is hidden from the kit (noExplain): fakesnow cannot run
//     EXPLAIN in any form (EXPLAIN USING TEXT is a parse error, a bare
//     EXPLAIN crashes the server). Explain is covered against restFake
//     (TestFakeExplain), and its read-only refusals need no server.
//   - BadAuth logs in to restFake: fakesnow accepts any password.
func TestConformance(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	table := testDB + "." + conformanceSchema + `."tobira_people"`
	mustExec(t, db, "CREATE SCHEMA IF NOT EXISTS "+testDB+"."+conformanceSchema)
	mustExec(t, db, peopleDDL(table))
	if err := insertPeople(context.Background(), db, table); err != nil {
		t.Fatal(err)
	}
	dsn := dsnFor(testDB, conformanceSchema)
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("fakesnow-ignores-it"), ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return noExplainConn{c.(*conn)}
		}
	}
	rest := newRestFake(t)
	rest.setPassword("the-right-password")
	drivertest.Run(t, drivertest.Config{
		Driver:       noExplainDriver{New()},
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: rest.dsn(""), Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			AutoIncrementKey: drivertest.Bool(false),
			Namespace:        testDB + "." + conformanceSchema,
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: `select * from "tobira_people" order by "id"`},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: `delete from "tobira_people" where "id" = 1`},
			TxWrite:          driver.Query{Text: `update "tobira_people" set "name" = 'changed in tx' where "id" = 1`},
			TxRestore:        driver.Query{Text: `update "tobira_people" set "name" = 'Ōtani Shōhei' where "id" = 1`},
			CompoundWrites: []driver.Query{
				{Text: `select 1; delete from "tobira_people" where "id" = 1`},
				{Text: `with c as (select 1) delete from "tobira_people" where "id" = 1`},
				{Text: `show tables ->> delete from "tobira_people" where "id" = 1`},
				{Text: `select * from "tobira_people" for update`},
			},
			// About 2e9 generated rows: several seconds of DuckDB work.
			Slow: driver.Query{Text: "SELECT SUM(a.n) FROM (SELECT SEQ8() AS n FROM TABLE(GENERATOR(ROWCOUNT => 20000))) a, " +
				"(SELECT SEQ8() FROM TABLE(GENERATOR(ROWCOUNT => 100000))) b"},
			ParamQuery: driver.Query{Text: `select * from "tobira_people" where "id" = ?`, Params: []driver.Value{driver.Int(7)}},
			CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		},
	})
}

// noExplainDriver is the driver without Explainer in its Info.
type noExplainDriver struct{ driver.Driver }

func (d noExplainDriver) Info() driver.Info {
	i := d.Driver.Info()
	i.Capabilities = slices.DeleteFunc(slices.Clone(i.Capabilities), func(c driver.Capability) bool { return c == driver.CapExplainer })
	return i
}

// noExplainConn reports Explainer unsupported (driver.Supporter), on the
// connection and on its transactions.
type noExplainConn struct{ *conn }

func (noExplainConn) Supports(c driver.Capability) bool { return c != driver.CapExplainer }

func (n noExplainConn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := n.conn.Begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return noExplainTx{tx.(*sfTx)}, nil
}

type noExplainTx struct{ *sfTx }

func (noExplainTx) Supports(c driver.Capability) bool { return c != driver.CapExplainer }
