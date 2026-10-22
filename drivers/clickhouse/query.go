package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/xavidop/tobira/driver"
)

// Query runs one statement. ClickHouse has no session state between calls
// (clickhouse-go runs each query on a pooled connection with no session),
// so each call is independent.
//
//   - A read-only call (conn opened read-only, or q.ReadOnly) must pass the
//     allow-list (checkReadOnly, and checkParams when there are parameters)
//     before anything is sent, and then runs with the server setting
//     readonly=1, unless the server already runs the user read-only; then
//     it must also call no table function (checkTableFunctions).
//   - q.Namespace picks the database: the native protocol fixes it per
//     connection, so another database runs on a pool of its own.
//   - The server refuses several statements in one text.
//   - On a writable call, a statement outside the read allow-list runs
//     through Exec and returns no columns (the native protocol cannot run
//     INSERT … VALUES as a query).
//
// q.Params bind to ? placeholders (clickhouse-go writes them into the text
// as quoted literals, after the allow-list has judged it; see checkParams). Limit is honoured by reading one row past it
// (Stats.Truncated); a result closed before its end cancels the query on the
// server instead of reading the rest.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.run(ctx, "query", q, c.readOnly || q.ReadOnly, checkReadOnly)
}

// run is Query's implementation, shared with Browse and Explain. check is
// the allow-list a read-only call must pass; nil skips it, which only tests
// do, to prove the server layer behind it.
func (c *conn) run(ctx context.Context, op string, q driver.Query, ro bool, check func(string) error) (driver.ResultSet, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if ro && check != nil {
		if err := check(q.Text); err != nil {
			return nil, err
		}
		if err := checkParams(q.Text, q.Params); err != nil {
			return nil, err
		}
	}
	args, err := toArgs(op, q.Params)
	if err != nil {
		return nil, err
	}
	p, err := c.pool(ctx, q.Namespace)
	if err != nil {
		return nil, err
	}
	if ro && check != nil && p.serverRO {
		if err := p.checkTableFunctions(ctx, op, q.Text, args); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	dctx, done := detach(ctx)
	if !ro && checkReadOnly(q.Text) != nil {
		defer done()
		// Not a read: INSERT, DDL, SYSTEM, KILL … The native protocol only
		// runs these through Exec (a Query on INSERT … VALUES would wait for
		// data blocks).
		if err := p.db.Exec(dctx, q.Text, args...); err != nil {
			return nil, mapError(ctx, op, err)
		}
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
	}
	qctx, cancelQuery := context.WithCancel(dctx)
	cancel := func() {
		cancelQuery()
		done()
	}
	if ro {
		qctx = p.roCtx(qctx)
	}
	rows, err := p.db.Query(qctx, q.Text, args...)
	if err != nil {
		cancel()
		return nil, mapError(ctx, op, err)
	}
	cts := rows.ColumnTypes()
	if len(cts) == 0 {
		for rows.Next() {
		}
		err := errors.Join(rows.Err(), rows.Close())
		cancel()
		if err != nil {
			return nil, mapError(ctx, op, err)
		}
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
	}
	r := &resultSet{op: op, rows: rows, cancel: cancel, ctx: ctx, start: start, limit: max(q.Limit, 0)}
	r.cols = make([]driver.Column, len(cts))
	r.natives = make([]string, len(cts))
	r.dest = make([]any, len(cts))
	for i, ct := range cts {
		r.natives[i] = ct.DatabaseTypeName()
		r.cols[i] = driver.Column{Name: ct.Name(), NativeType: r.natives[i], Type: columnType(r.natives[i])}
		r.dest[i] = reflect.New(ct.ScanType()).Interface()
	}
	return r, nil
}

// toArgs converts query parameters to what clickhouse-go binds.
func toArgs(op string, params []driver.Value) ([]any, error) {
	args := make([]any, len(params))
	for i, p := range params {
		a, err := toArg(p)
		if err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("parameter %d: %w", i+1, err))
		}
		args[i] = a
	}
	return args, nil
}

// toArg converts one Value. Decimals travel as their exact text, which the
// server converts on comparison.
func toArg(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return n, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return string(b), nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t, nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return string(raw), nil
	case driver.TypeVector:
		vec, _ := v.AsVector()
		return vec, nil
	case driver.TypeList:
		list, _ := v.AsList()
		out := make([]any, len(list))
		for i, e := range list {
			a, err := toArg(e)
			if err != nil {
				return nil, err
			}
			out[i] = a
		}
		return out, nil
	}
	return nil, fmt.Errorf("a %s value cannot be bound", v.Type())
}

// resultSet streams one query's rows. mu serialises Next and Close.
type resultSet struct {
	mu      sync.Mutex
	op      string
	rows    chdriver.Rows
	cancel  context.CancelFunc // cancels the query on the server
	ctx     context.Context    // the caller's ctx, for error mapping
	start   time.Time
	cols    []driver.Column
	natives []string
	dest    []any // one scan target per column
	limit   int
	count   int
	done    bool

	// Stats reads these without mu, so it never waits for a Next blocked on
	// the network.
	stDone  atomic.Bool
	stTrunc atomic.Bool
	stDur   atomic.Int64
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

func (r *resultSet) Stats() driver.Stats {
	d := time.Duration(r.stDur.Load())
	if !r.stDone.Load() {
		d = time.Since(r.start)
	}
	return driver.Stats{RowsAffected: -1, Duration: d, Truncated: r.stTrunc.Load()}
}

func (r *resultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.release(false)
		return nil, false, mapError(ctx, r.op, err)
	}
	// A blocked read ends when ctx does: cancelling the query ends its stream.
	stop := context.AfterFunc(ctx, r.cancel)
	defer stop()
	if r.limit > 0 && r.count >= r.limit { // peek one row to learn whether we cut the result
		more := r.rows.Next()
		if more {
			r.stTrunc.Store(true)
		} else if err := r.rows.Err(); err != nil {
			r.release(true)
			return nil, false, mapError(errCtx(ctx, r.ctx), r.op, err)
		}
		r.release(!more)
		return nil, false, nil
	}
	if !r.rows.Next() {
		err := r.rows.Err()
		r.release(err == nil)
		if err != nil {
			return nil, false, mapError(errCtx(ctx, r.ctx), r.op, err)
		}
		return nil, false, nil
	}
	if err := r.rows.Scan(r.dest...); err != nil {
		r.release(false)
		return nil, false, mapError(r.ctx, r.op, err)
	}
	row := make(driver.Row, len(r.dest))
	for i, d := range r.dest {
		row[i] = convertValue(r.natives[i], reflect.ValueOf(d).Elem().Interface())
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
	r.release(false)
	return nil
}

// release ends the result set. Unless every row was read (exhausted), the
// query is cancelled first, so closing the rows does not read the rest of
// the stream. It is idempotent; r.mu must be held.
func (r *resultSet) release(exhausted bool) {
	if r.done {
		return
	}
	r.done = true
	if !exhausted {
		r.cancel()
	}
	_ = r.rows.Close()
	r.cancel()
	d := time.Since(r.start)
	r.stDur.Store(int64(d))
	r.stDone.Store(true)
}
