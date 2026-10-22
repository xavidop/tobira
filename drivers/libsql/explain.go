package libsql

import (
	"context"
	"errors"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// maxPlanDepth caps the plan tree's depth and indentation.
const maxPlanDepth = sqlitecore.MaxPlanDepth

// Explain runs EXPLAIN QUERY PLAN for q. It never executes q, so Analyze is
// unsupported. Whatever the connection, it runs on the read-only path
// (BEGIN … ROLLBACK on a fresh stream) behind checkExplain and the
// single-statement check, so explaining can never persist side effects.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	start := time.Now()
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("EXPLAIN ANALYZE is not supported by libSQL"))
	}
	if err := checkExplain(q.Text); err != nil {
		return nil, err
	}
	eq := explainQuery(q)
	if err := checkSingle(eq.Text); err != nil {
		return nil, err
	}
	s, err := statement(op, eq.Text, eq.Params, true)
	if err != nil {
		return nil, err
	}
	r, err := c.readOnlyExec(ctx, s)
	if err != nil {
		return nil, mapError(op, err)
	}
	return planFrom(ctx, op, r, q.Text, start)
}

var _ driver.Explainer = (*tx)(nil)

// Explain runs EXPLAIN QUERY PLAN for q on the transaction's stream, so it
// sees what the transaction made. The guards of the connection's Explain
// apply, so q is only compiled, never run.
func (t *tx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	start := time.Now()
	if err := t.live(op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("EXPLAIN ANALYZE is not supported by libSQL"))
	}
	if err := checkExplain(q.Text); err != nil {
		return nil, err
	}
	eq := explainQuery(q)
	if err := checkSingle(eq.Text); err != nil {
		return nil, err
	}
	s, err := statement(op, eq.Text, eq.Params, true)
	if err != nil {
		return nil, err
	}
	res, _, err := t.run(ctx, op, []hranaStmt{s}, false)
	if err != nil {
		return nil, err
	}
	return planFrom(ctx, op, res[0], q.Text, start)
}

func planFrom(ctx context.Context, op string, r *stmtResult, text string, start time.Time) (*driver.Plan, error) {
	rs, err := resultSet(op, r, 0, -1, start)
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, mapError(op, err)
	}
	return buildPlan(rows, text), nil
}

// explainQuery is q prefixed with EXPLAIN QUERY PLAN.
func explainQuery(q driver.Query) driver.Query {
	return driver.Query{Text: "EXPLAIN QUERY PLAN " + q.Text, Params: q.Params}
}

func buildPlan(rows []driver.Row, text string) *driver.Plan { return sqlitecore.BuildPlan(rows, text) }
