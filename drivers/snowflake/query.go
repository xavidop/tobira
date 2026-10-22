package snowflake

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/xavidop/tobira/driver"
)

// Query runs one statement.
//
//   - A read-only call (conn opened read-only, or q.ReadOnly) must pass the
//     allow-list (checkReadOnly) before anything is sent.
//   - Every request carries MULTI_STATEMENT_COUNT=1, so the server refuses
//     a text with several statements.
//   - q.Namespace ("database.schema") picks the session's schema: another
//     namespace runs on a pool of its own.
//   - q.Params bind to ? (or :1) placeholders server-side; nothing is
//     interpolated into the text.
//
// Every statement returns rows: a query its result, DML its counts ("number
// of rows inserted"), DDL its status line. Limit is honoured by reading one
// row past it (Stats.Truncated); a result closed early cancels the query.
// Session state set by one call (USE, SET) is not kept for the next: calls
// run on pooled sessions.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.run(ctx, "query", q, c.readOnly || q.ReadOnly)
}

// run is Query's implementation, shared with Browse and Explain.
func (c *conn) run(ctx context.Context, op string, q driver.Query, ro bool) (driver.ResultSet, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if ro {
		if err := checkReadOnlyOp(op, q.Text); err != nil {
			return nil, err
		}
	}
	args, err := bindArgs(op, q.Params)
	if err != nil {
		return nil, err
	}
	db, err := c.pool(ctx, op, q.Namespace)
	if err != nil {
		return nil, err
	}
	rs, err := runOn(ctx, op, db, q.Text, args, q.Limit)
	return rs, c.scrub(err)
}

// runOn sends text to qr and wraps the rows.
func runOn(ctx context.Context, op string, qr querier, text string, args []any, limit int) (driver.ResultSet, error) {
	start := time.Now()
	qctx, cancel := context.WithCancel(sf.WithMultiStatement(ctx, 1))
	rows, err := qr.QueryContext(qctx, text, args...)
	if err != nil {
		cancel()
		return nil, mapError(ctx, op, err)
	}
	cts, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		cancel()
		return nil, mapError(ctx, op, err)
	}
	r := &resultSet{op: op, rows: rows, cancel: cancel, ctx: ctx, start: start, limit: max(limit, 0)}
	r.cols = make([]driver.Column, len(cts))
	r.natives = make([]string, len(cts))
	r.scales = make([]int64, len(cts))
	for i, ct := range cts {
		r.natives[i] = ct.DatabaseTypeName()
		if _, scale, ok := ct.DecimalSize(); ok {
			r.scales[i] = scale
		}
		r.cols[i] = driver.Column{Name: ct.Name(), NativeType: r.natives[i], Type: columnType(r.natives[i], r.scales[i])}
	}
	r.dest = make([]any, len(cts))
	r.ptrs = make([]any, len(cts))
	for i := range r.dest {
		r.ptrs[i] = &r.dest[i]
	}
	return r, nil
}

// resultSet streams one statement's rows. mu serialises Next and Close.
type resultSet struct {
	mu      sync.Mutex
	op      string
	rows    *sql.Rows
	cancel  context.CancelFunc // cancels the query
	ctx     context.Context    // the caller's ctx, for error mapping
	start   time.Time
	cols    []driver.Column
	natives []string
	scales  []int64
	dest    []any // scan targets: gosnowflake's own values
	ptrs    []any
	limit   int
	count   int
	done    bool
	cut     bool // a value was cut at maxValueBytes

	// Stats reads these without mu, so it never waits for a Next blocked on
	// the network.
	stDone  atomic.Bool
	stTrunc atomic.Bool
	stCut   atomic.Bool
	stDur   atomic.Int64
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

func (r *resultSet) Stats() driver.Stats {
	d := time.Duration(r.stDur.Load())
	if !r.stDone.Load() {
		d = time.Since(r.start)
	}
	st := driver.Stats{RowsAffected: -1, Duration: d, Truncated: r.stTrunc.Load()}
	if r.stCut.Load() {
		st.Warnings = []string{cutWarning}
	}
	return st
}

func (r *resultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.release()
		return nil, false, mapError(ctx, r.op, err)
	}
	// A blocked read ends when ctx does: cancelling the query ends its rows.
	stop := context.AfterFunc(ctx, r.cancel)
	defer stop()
	if r.limit > 0 && r.count >= r.limit { // peek one row to learn whether we cut the result
		if r.rows.Next() {
			r.stTrunc.Store(true)
		} else if err := r.rows.Err(); err != nil {
			r.release()
			return nil, false, mapError(errCtx(ctx, r.ctx), r.op, err)
		}
		r.release()
		return nil, false, nil
	}
	if !r.rows.Next() {
		err := r.rows.Err()
		r.release()
		if err != nil {
			return nil, false, mapError(errCtx(ctx, r.ctx), r.op, err)
		}
		return nil, false, nil
	}
	if err := r.rows.Scan(r.ptrs...); err != nil {
		r.release()
		return nil, false, mapError(errCtx(ctx, r.ctx), r.op, err)
	}
	row := make(driver.Row, len(r.dest))
	for i, v := range r.dest {
		row[i] = convertValue(r.natives[i], r.scales[i], v, &r.cut)
	}
	if r.cut {
		r.stCut.Store(true)
	}
	r.count++
	return row, true, nil
}

// errCtx returns whichever of the Next ctx and the Query ctx is done, so a
// cancellation from either maps to the context error.
func errCtx(next, query context.Context) context.Context {
	if next.Err() != nil {
		return next
	}
	return query
}

func (r *resultSet) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release()
	return nil
}

// release ends the result set: the query is cancelled (a no-op once every
// row was read) and the rows closed. It is idempotent; r.mu must be held.
func (r *resultSet) release() {
	if r.done {
		return
	}
	r.done = true
	r.cancel()
	_ = r.rows.Close()
	r.stDur.Store(int64(time.Since(r.start)))
	r.stDone.Store(true)
}

// collectStrings runs text on qr and returns its rows as text, for the
// catalog reads. A NULL is "".
func collectStrings(ctx context.Context, op string, qr querier, text string, args ...any) ([]string, [][]string, error) {
	rs, err := runOn(ctx, op, qr, text, args, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rs.Close() }()
	cols := rs.Columns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	var out [][]string
	for {
		row, ok, err := rs.Next(ctx)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return names, out, nil
		}
		vals := make([]string, len(row))
		for i, v := range row {
			if !v.IsNull() {
				vals[i] = v.String()
			}
		}
		out = append(out, vals)
	}
}
