package clickhouse

import (
	"context"
	"errors"
	"strings"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Explainer = (*conn)(nil)

// errNoAnalyze is returned for ExplainOptions.Analyze: ClickHouse has no
// EXPLAIN ANALYZE (it never runs the query to explain it).
var errNoAnalyze = errors.New("ClickHouse has no EXPLAIN ANALYZE; run the query and read system.query_log instead")

// Explain returns the text of EXPLAIN indexes = 1 <query>: the query plan,
// with the parts and granules each index selects. On a read-only call the
// explained text must pass the allow-list first (so explaining a write is
// ErrReadOnly), and the EXPLAIN runs with readonly=1. Analyze is
// ErrUnsupported, or ErrReadOnly when a read-only call would explain a
// write.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	ro := c.readOnly || q.ReadOnly
	if ro {
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errNoAnalyze)
	}
	// The newline ends a line comment that opens the text.
	eq := q
	eq.Text = "EXPLAIN indexes = 1\n" + q.Text
	eq.Limit = 0
	rs, err := c.run(ctx, op, eq, ro, checkReadOnly)
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
