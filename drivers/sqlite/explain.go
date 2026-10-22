package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// noPlanRows and noPlanRowsWrite are Plan.Text for a statement whose EXPLAIN
// QUERY PLAN has no rows (see sqlitecore.BuildPlan).
const (
	noPlanRows      = sqlitecore.NoPlanRows
	noPlanRowsWrite = sqlitecore.NoPlanRowsWrite
)

// Explain runs EXPLAIN QUERY PLAN for q. It never executes q, so Analyze is
// unsupported. Whatever the connection, it runs on the read-only handle
// (query_only, no ATTACH) behind checkExplain and the single-statement
// check, so explaining can never persist side effects. Because of that handle
// it cannot see temp tables or attached databases that earlier queries made
// on the writable handle; explaining a statement over them fails.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("EXPLAIN ANALYZE is not supported by SQLite"))
	}
	if err := checkExplain(q.Text); err != nil {
		return nil, err
	}
	rs, err := c.exec(ctx, c.ro, explainQuery(q))
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	return buildPlan(rows, q.Text), nil
}

var _ driver.Explainer = (*tx)(nil)

// Explain runs EXPLAIN QUERY PLAN for q on the transaction's connection, so
// it sees what the transaction made (temp tables, uncommitted rows). The
// guards of the connection's Explain apply (checkExplain and the
// single-statement check; Analyze is unsupported), so q is only compiled,
// never run, read-only transaction or not.
func (t *tx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	start := time.Now()
	sc, err := t.alive(ctx, op)
	if err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("EXPLAIN ANALYZE is not supported by SQLite"))
	}
	if err := checkExplain(q.Text); err != nil {
		return nil, err
	}
	eq := explainQuery(q)
	if err := checkSingle(eq.Text); err != nil {
		return nil, err
	}
	rs, err := execOn(ctx, sc, false, false, eq, start)
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	return buildPlan(rows, q.Text), nil
}

// explainQuery is q prefixed with EXPLAIN QUERY PLAN.
func explainQuery(q driver.Query) driver.Query {
	return driver.Query{Text: "EXPLAIN QUERY PLAN " + q.Text, Params: q.Params}
}

// buildPlan turns EXPLAIN QUERY PLAN rows into a Plan, with the shared
// depth cap (local SQLite never nests that deep).
func buildPlan(rows []driver.Row, text string) *driver.Plan { return sqlitecore.BuildPlan(rows, text) }
