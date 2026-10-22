package query

import (
	"context"
	"errors"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Options configures Run.
type Options struct {
	Limit    int           // max rows; 0 = no limit
	Timeout  time.Duration // 0 = no timeout
	ReadOnly bool          // hard read-only guard, passed to the driver
	Explain  bool          // return the query plan instead of running the query
	Driver   string        // driver name, used only to attribute errors raised here
}

// Result is a fully drained query result.
type Result struct {
	Columns []driver.Column
	Rows    []driver.Row
	Stats   driver.Stats
	Plan    *driver.Plan // set instead of Columns/Rows when Options.Explain was used
}

// Run executes text on conn. The timeout, when set, covers the whole call
// including draining the cursor. Rows beyond opts.Limit are dropped and
// Stats.Truncated is set.
func Run(ctx context.Context, conn driver.Conn, text string, opts Options) (*Result, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	q := driver.Query{Text: text, Limit: opts.Limit, ReadOnly: opts.ReadOnly}

	if opts.Explain {
		ex, ok := driver.As[driver.Explainer](conn)
		if !ok {
			return nil, driver.Wrap(driver.ErrUnsupported, opts.Driver, "explain",
				errors.New("explain is not supported by this driver"))
		}
		plan, err := ex.Explain(ctx, q, driver.ExplainOptions{})
		if err != nil {
			return nil, err
		}
		return &Result{Plan: plan, Stats: driver.Stats{RowsAffected: -1}}, nil
	}

	start := time.Now()
	rs, err := conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	cols := rs.Columns()
	rows, stats, err := driver.Collect(ctx, rs, opts.Limit)
	if err != nil {
		return nil, err
	}
	if stats.Duration == 0 {
		stats.Duration = time.Since(start)
	}
	return &Result{Columns: cols, Rows: rows, Stats: stats}, nil
}
