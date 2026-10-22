package d1

import (
	"context"
	"errors"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// Explain runs EXPLAIN QUERY PLAN for q. It never executes q, so Analyze is
// unsupported. Whatever the connection, the statement must pass
// sqlitecore.CheckExplain (reads, plus INSERT, UPDATE, DELETE and REPLACE,
// which EXPLAIN QUERY PLAN only compiles; PRAGMA assignments, whose effects
// SQLite applies at compile time, are refused) and the single-statement
// check, so explaining can never change the database.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	start := time.Now()
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("EXPLAIN ANALYZE is not supported by D1"))
	}
	if err := sqlitecore.CheckExplain(Name, q.Text); err != nil {
		return nil, err
	}
	text := "EXPLAIN QUERY PLAN " + q.Text
	if err := sqlitecore.CheckSingle(Name, text); err != nil {
		return nil, err
	}
	args, err := params(op, q.Params)
	if err != nil {
		return nil, err
	}
	r, err := c.client.exec(ctx, text, args)
	if err != nil {
		return nil, mapError(op, err)
	}
	rows, _, err := driver.Collect(ctx, resultSet(r, nil, 0, start), 0)
	if err != nil {
		return nil, mapError(op, err)
	}
	return sqlitecore.BuildPlan(rows, q.Text), nil
}
