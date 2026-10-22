package vertica

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	vertigo "github.com/vertica/vertica-sql-go"

	"github.com/xavidop/tobira/driver"
)

// Query runs one statement.
//
//   - Every call must hold exactly one statement and no COPY from the
//     client (checkShape), and transaction control is left to Begin
//     (txControlRefusal).
//   - A read-only call (conn opened read-only, or q.ReadOnly) must pass the
//     allow-list (checkReadOnly) before anything is sent, and runs in a READ
//     ONLY transaction on a pinned session, which always ends in ROLLBACK.
//   - q.Namespace (a schema) picks the session's search path: another
//     namespace runs on a pool of its own.
//   - q.Params bind to ? placeholders server-side; nothing is interpolated.
//
// vertica-sql-go reads a whole result into memory before the first row:
// Limit cuts what is returned (Stats.Truncated), not what the server
// sends, so add a LIMIT to a large query. Cancelling ctx ends the
// statement on the server (see session.go).
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if err := txControlRefusal(q.Text); err != nil {
		return nil, err
	}
	return c.run(ctx, "query", q, c.readOnly || q.ReadOnly, nil)
}

// run is Query's implementation, shared with Browse and Explain. wide
// marks result columns that hold NUMERIC values read as text.
func (c *conn) run(ctx context.Context, op string, q driver.Query, ro bool, wide []bool) (driver.ResultSet, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if ro {
		if err := checkReadOnlyOp(op, q.Text); err != nil {
			return nil, err
		}
	}
	if err := checkShape(op, q.Text); err != nil {
		return nil, err
	}
	args, err := bindArgs(op, q.Params)
	if err != nil {
		return nil, err
	}
	db, err := c.pool(ctx, op, q.Namespace)
	if err != nil {
		return nil, err
	}
	s, err := c.acquire(ctx, op, db)
	if err != nil {
		return nil, err
	}
	if ro {
		if err := s.begin(ctx, true, ""); err != nil {
			_ = s.end(false)
			s.release()
			return nil, c.scrub(mapError(ctx, op, err))
		}
	}
	rs, err := s.runOn(ctx, op, q.Text, args, q.Limit, wide, func() {
		_ = s.end(false)
		s.release()
	})
	if err != nil {
		_ = s.end(false)
		s.release()
	}
	return rs, c.scrub(err)
}

// emptyStdin stands in for the process's standard input: vertica-sql-go
// reads a COPY … FROM STDIN from the context's input stream, os.Stdin by
// default. checkShape refuses such a COPY; this is the layer under it.
type emptyStdin struct{}

func (emptyStdin) Read([]byte) (int, error) { return 0, io.EOF }

// noStdin wraps ctx so vertica-sql-go never reads os.Stdin.
func noStdin(ctx context.Context) context.Context {
	vctx := vertigo.NewVerticaContext(ctx)
	_ = vctx.SetCopyInputStream(emptyStdin{})
	return vctx
}

// runOn sends text on the session and wraps the rows. release, when set,
// runs once the result set ends.
func (s *session) runOn(ctx context.Context, op, text string, args []any, limit int, wide []bool, release func()) (driver.ResultSet, error) {
	start := time.Now()
	rows, err := s.query(ctx, text, args...)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	cts, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		return nil, mapError(ctx, op, err)
	}
	r := &resultSet{op: op, rows: rows, ctx: ctx, start: start, limit: max(limit, 0), onRelease: release}
	r.cols = make([]driver.Column, len(cts))
	r.natives = make([]string, len(cts))
	r.scales = make([]int64, len(cts))
	for i, ct := range cts {
		r.natives[i] = ct.DatabaseTypeName()
		if i < len(wide) && wide[i] {
			r.natives[i] = "NUMERIC"
		}
		if _, scale, ok := ct.DecimalSize(); ok {
			r.scales[i] = scale
		}
		r.cols[i] = driver.Column{Name: ct.Name(), NativeType: r.natives[i], Type: columnType(r.natives[i])}
		if p, _, ok := ct.DecimalSize(); ok && p > maxExactDigits && r.natives[i] == "NUMERIC" && (i >= len(wide) || !wide[i]) {
			r.warnings = append(r.warnings, fmt.Sprintf("NUMERIC column %q has %d digits of precision; vertica-sql-go decodes NUMERIC "+
				"through float64, so values beyond 15 significant digits are approximate: cast it to VARCHAR to see them exactly", ct.Name(), p))
		}
	}
	r.dest = make([]any, len(cts))
	r.ptrs = make([]any, len(cts))
	for i := range r.dest {
		r.ptrs[i] = &r.dest[i]
	}
	return r, nil
}

// resultSet serves one statement's rows, which vertica-sql-go has already
// read into memory. mu serialises Next and Close.
type resultSet struct {
	mu        sync.Mutex
	op        string
	rows      *sql.Rows
	ctx       context.Context // the caller's ctx, for error mapping
	onRelease func()
	start     time.Time
	cols      []driver.Column
	natives   []string
	scales    []int64
	dest      []any
	ptrs      []any
	warnings  []string // fixed at Query time
	limit     int
	count     int
	done      bool
	cut       bool // a value was cut at maxValueBytes

	// Stats reads these without mu, so it never waits for a Next.
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
	st.Warnings = append(st.Warnings, r.warnings...)
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

// release ends the result set: the rows are closed and onRelease run. It is
// idempotent; r.mu must be held.
func (r *resultSet) release() {
	if r.done {
		return
	}
	r.done = true
	_ = r.rows.Close()
	if r.onRelease != nil {
		r.onRelease()
	}
	r.stDur.Store(int64(time.Since(r.start)))
	r.stDone.Store(true)
}

// collectStrings runs a catalog read on a session of the main pool and
// returns its rows as text. A NULL is "".
func (c *conn) collectStrings(ctx context.Context, op, text string, args ...any) ([][]string, error) {
	s, err := c.acquire(ctx, op, c.db)
	if err != nil {
		return nil, err
	}
	rs, err := s.runOn(ctx, op, text, args, 0, nil, s.release)
	if err != nil {
		s.release()
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	var out [][]string
	for {
		row, ok, err := rs.Next(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
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
