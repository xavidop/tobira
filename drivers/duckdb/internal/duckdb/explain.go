package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/xavidop/tobira/driver"
)

var (
	_ driver.Explainer = (*conn)(nil)
	_ driver.Explainer = (*tx)(nil)
)

// Explain returns DuckDB's EXPLAIN text for q. Plain EXPLAIN only plans,
// but planning binds table functions, so on a read-only path q passes the
// read-only guard first and the EXPLAIN runs on the read-only path.
// Analyze runs q (EXPLAIN ANALYZE); on a read-only path that is allowed
// only for a statement the guard accepts, and it runs inside the READ ONLY
// transaction.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	eq := explainQuery(q, opts)
	var rs driver.ResultSet
	var err error
	if c.readOnly || q.ReadOnly {
		if c.external {
			return nil, refuseExternal(op)
		}
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
		rs, err = c.readOnlyQuery(ctx, eq)
	} else {
		if err := checkSingle(q.Text); err != nil {
			return nil, err
		}
		var sc *sql.Conn
		if sc, err = c.db.Conn(ctx); err != nil {
			return nil, mapError(op, err)
		}
		rs, err = runOn(ctx, sc, true, false, eq)
	}
	if err != nil {
		return nil, err
	}
	return collectPlan(ctx, rs)
}

// Explain explains q inside the transaction, so it sees what the
// transaction made. A read-only transaction applies the read-only guard.
func (t *tx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if t.readOnly || q.ReadOnly {
		if t.c.external {
			return nil, refuseExternal(op)
		}
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
	} else if err := checkSingle(q.Text); err != nil {
		return nil, err
	}
	if q.ReadOnly && !t.readOnly && opts.Analyze {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyAnalyze)
	}
	sc, err := t.alive(ctx, op)
	if err != nil {
		return nil, err
	}
	rs, err := runOn(ctx, sc, false, t.readOnly, explainQuery(q, opts))
	if err != nil {
		return nil, err
	}
	return collectPlan(ctx, rs)
}

var errReadOnlyAnalyze = errors.New("EXPLAIN ANALYZE runs the statement: begin a read-only transaction to analyze with Query.ReadOnly")

// explainQuery is q behind EXPLAIN, or EXPLAIN ANALYZE.
func explainQuery(q driver.Query, opts driver.ExplainOptions) driver.Query {
	prefix := "EXPLAIN "
	if opts.Analyze {
		prefix = "EXPLAIN ANALYZE "
	}
	return driver.Query{Text: prefix + q.Text, Params: q.Params, Namespace: q.Namespace}
}

// collectPlan joins the plan rows' text (the explain_value column, or the
// last column) into Plan.Text.
func collectPlan(ctx context.Context, rs driver.ResultSet) (*driver.Plan, error) {
	cols := rs.Columns()
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	idx := len(cols) - 1
	for i, c := range cols {
		if c.Name == "explain_value" {
			idx = i
		}
	}
	var parts []string
	for _, r := range rows {
		if idx >= 0 && idx < len(r) && !r[idx].IsNull() {
			parts = append(parts, strings.TrimRight(r[idx].String(), "\n"))
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = "(no plan rows)"
	}
	return &driver.Plan{Format: "text", Text: text}, nil
}
