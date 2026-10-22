package vertica

import (
	"context"
	"errors"
	"strings"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Explainer = (*conn)(nil)

// errNoAnalyze is returned for ExplainOptions.Analyze: Vertica's EXPLAIN
// ANALYZE and PROFILE run the statement, which an explain must not do.
var errNoAnalyze = errors.New("EXPLAIN ANALYZE runs the statement; run PROFILE <statement> as a query on a connection that is not read-only instead")

// Explain returns the text of EXPLAIN <statement>: the plan Vertica's
// optimizer chose, which it never runs. The statement must be one SELECT,
// WITH, INSERT, UPDATE, DELETE or MERGE without EXPLAIN options
// (explainTarget), on every path. On a read-only call it must also pass the
// allow-list, so explaining a write is ErrReadOnly, and the EXPLAIN runs in
// a READ ONLY transaction. Analyze is ErrUnsupported (ErrReadOnly first when
// a read-only call would explain a write); neither sends anything.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	ro := c.readOnly || q.ReadOnly
	return explainOn(ctx, q, opts, ro, func(eq driver.Query) (driver.ResultSet, error) {
		return c.run(ctx, op, eq, ro, nil)
	})
}

// explainOn is Explain over run, which sends the EXPLAIN text on a pool or
// a transaction.
func explainOn(ctx context.Context, q driver.Query, opts driver.ExplainOptions, ro bool, run func(driver.Query) (driver.ResultSet, error)) (*driver.Plan, error) {
	const op = "explain"
	if ro {
		if err := checkReadOnlyOp(op, q.Text); err != nil {
			return nil, err
		}
	}
	if err := explainTarget(op, q.Text); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errNoAnalyze)
	}
	// The newline ends a line comment that opens the text.
	eq := q
	eq.Text = "EXPLAIN\n" + q.Text
	eq.Limit = 0
	rs, err := run(eq)
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			lines = append(lines, r[0].String())
		}
	}
	return &driver.Plan{Format: "text", Text: strings.Join(lines, "\n")}, nil
}
