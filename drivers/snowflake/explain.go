package snowflake

import (
	"context"
	"errors"
	"strings"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Explainer = (*conn)(nil)

// errNoAnalyze is returned for ExplainOptions.Analyze: Snowflake has no
// EXPLAIN ANALYZE (EXPLAIN only compiles; run the query and open its
// Query Profile instead).
var errNoAnalyze = errors.New("there is no EXPLAIN ANALYZE in Snowflake; run the query and read its Query Profile")

// Explain returns the text of EXPLAIN USING TEXT <query>: the compiled plan,
// with the partitions and bytes it expects to scan. Snowflake's EXPLAIN
// never runs the statement. On a read-only call the explained text must
// pass the allow-list first, so explaining a write is ErrReadOnly. Analyze
// is ErrUnsupported (ErrReadOnly first when a read-only call would explain
// a write); neither sends anything.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	return explainOn(ctx, q, opts, c.readOnly || q.ReadOnly, func(eq driver.Query) (driver.ResultSet, error) {
		return c.run(ctx, op, eq, false)
	})
}

// explainOn is Explain over run, which sends the EXPLAIN text (already
// judged) on a pool or a transaction.
func explainOn(ctx context.Context, q driver.Query, opts driver.ExplainOptions, ro bool, run func(driver.Query) (driver.ResultSet, error)) (*driver.Plan, error) {
	const op = "explain"
	if ro {
		if err := checkReadOnlyOp(op, q.Text); err != nil {
			return nil, err
		}
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errNoAnalyze)
	}
	// The newline ends a line comment that opens the text.
	eq := q
	eq.Text = "EXPLAIN USING TEXT\n" + q.Text
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
