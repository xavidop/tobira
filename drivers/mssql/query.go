package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"

	"github.com/xavidop/tobira/driver"
)

// Query runs one statement on a dedicated pooled connection. The
// connection goes back to the pool afterwards and is reset
// (sp_reset_connection) before its next use, so nothing a statement does to
// its session (USE, SET, temporary tables …) reaches a later call.
//
//   - A read-only call (conn opened read-only, or q.ReadOnly) must pass the
//     allow-list (checkReadOnly) before anything is sent. It then runs
//     inside BEGIN TRANSACTION … ROLLBACK (see beginReadOnlySQL) and is
//     post-checked (endReadOnly).
//   - A namespaced call runs USE <database> first; the schema part of the
//     namespace does not change name resolution (SQL Server resolves
//     unqualified names in the user's default schema).
//   - Anything else runs as is, in autocommit; a transaction it leaves
//     open is rolled back when its result closes.
//
// Parameters are @p1, @p2 … in the text. A data change or DDL without an
// OUTPUT clause runs with Exec and reports its rows affected. Limit is
// honoured by reading one row past it (Stats.Truncated); a result closed
// before its end is cancelled on the server rather than read to the end.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.run(ctx, q, c.readOnly || q.ReadOnly, checkReadOnly)
}

// checker is a read-only path's lexical allow-list: checkReadOnly for
// queries, Explain's own for the statements it plans.
type checker func(text string) error

// run is Query's implementation. check is the allow-list a read-only call
// must pass; nil skips it, which only tests do, to prove the layer behind
// it.
func (c *conn) run(ctx context.Context, q driver.Query, ro bool, check checker) (driver.ResultSet, error) {
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	if ro && check != nil {
		if err := check(q.Text); err != nil {
			return nil, err
		}
	}
	args, err := toArgs(q.Params)
	if err != nil {
		return nil, err
	}
	use, err := c.useSQL(q.Namespace)
	if err != nil {
		return nil, err
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(ctx, "query", err)
	}
	r := newResultSet(ctx, c, sc, q, ro)
	return r.run(q, args, use)
}

// toArgs converts query parameters to what go-mssqldb binds (@p1, @p2 …).
func toArgs(params []driver.Value) ([]any, error) {
	args := make([]any, len(params))
	for i, p := range params {
		a, err := toArg(p)
		if err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", fmt.Errorf("parameter %d: %w", i+1, err))
		}
		args[i] = a
	}
	return args, nil
}

// newResultSet prepares the result set of q on sc. Its statement context
// exists from the start, so a transaction ending concurrently can cancel it.
func newResultSet(ctx context.Context, c *conn, sc *sql.Conn, q driver.Query, ro bool) *resultSet {
	r := &resultSet{c: c, sc: sc, ctx: ctx, start: time.Now(), limit: max(q.Limit, 0), ro: ro, affected: -1}
	r.qctx, r.cancel = context.WithCancel(ctx)
	r.opening.Store(true)
	return r
}

// run opens the statement and finishes a column-less one at once. use is
// the USE statement of the call's namespace, or "".
func (r *resultSet) run(q driver.Query, args []any, use string) (driver.ResultSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.opening.Store(false)
	ctx := r.ctx
	if err := r.open(q, args, use); err != nil {
		r.release()
		if r.endErr != nil {
			if conflictErr(err) { // a deadlock or lock timeout rolled the transaction back
				return nil, &driver.Error{Kind: driver.ErrConflict, Driver: Name, Op: "query", Code: serverCode(err), Err: errors.Join(err, r.endErr)}
			}
			return nil, r.endErr
		}
		return nil, mapError(ctx, "query", err)
	}
	if len(r.cols) == 0 {
		return r.finishWithoutColumns()
	}
	return r, nil
}

// open prepares the session and starts the statement.
func (r *resultSet) open(q driver.Query, args []any, use string) error {
	var prefix string
	switch {
	case r.tx != nil:
		prefix = use
	case r.ro:
		prefix = prefixSQL(use, beginReadOnlySQL)
	default:
		prefix = prefixSQL(use, beginWritableSQL)
	}
	if prefix != "" {
		if _, err := r.sc.ExecContext(r.qctx, prefix); err != nil {
			return err
		}
	}
	r.inTx = r.ro && r.tx == nil
	if toks := lex(q.Text).toks; !r.ro && useExec(toks) {
		if isDML(toks) {
			// The statement's own count, not the sum go-mssqldb reports,
			// which includes what triggers did.
			n, err := rowCount(r.qctx, r.sc, q.Text, args...)
			if err != nil {
				return err
			}
			r.affected = n
			return nil
		}
		res, err := r.sc.ExecContext(r.qctx, q.Text, args...)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil {
			r.affected = n
		}
		return nil
	}
	rows, err := r.sc.QueryContext(r.qctx, q.Text, args...)
	if err != nil {
		return err
	}
	r.rows = rows
	cts, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	r.dbTypes = make([]string, len(cts))
	r.cols = make([]driver.Column, len(cts))
	for i, ct := range cts {
		r.dbTypes[i] = ct.DatabaseTypeName()
		r.cols[i] = driver.Column{Name: ct.Name(), NativeType: r.dbTypes[i], Type: columnType(r.dbTypes[i])}
	}
	return nil
}

// finishWithoutColumns completes a statement that returns no columns (a data
// change, DDL, WAITFOR): it reports the rows affected Exec counted, or -1.
func (r *resultSet) finishWithoutColumns() (driver.ResultSet, error) {
	var err error
	if r.rows != nil {
		err = r.rows.Close()
		r.rows = nil
	}
	r.exhausted = true
	r.release()
	switch {
	case r.endErr != nil:
		return nil, r.endErr
	case err != nil:
		return nil, mapError(r.ctx, "query", err)
	}
	return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: r.affected, Duration: r.duration}), nil
}

// resultSet streams one statement's rows. mu serialises Next, Close, Stats
// and the opening of the statement with a transaction ending from another
// goroutine.
type resultSet struct {
	mu     sync.Mutex
	c      *conn
	sc     *sql.Conn
	ctx    context.Context // the caller's ctx, for error mapping
	qctx   context.Context // the statement's ctx; cancelled on release
	cancel context.CancelFunc
	start  time.Time
	ro     bool  // a read-only call on its own connection, or in a read-only Tx
	tx     *msTx // the transaction the statement runs in; nil for a call of its own
	limit  int

	rows      *sql.Rows
	dbTypes   []string
	cols      []driver.Column
	inTx      bool  // the read-only call's BEGIN TRANSACTION ran
	exhausted bool  // every row was read
	discard   bool  // the connection's state is unknown: close it instead of pooling it
	affected  int64 // rows affected of an Exec, or -1

	count     int
	truncated bool

	// Stats reads these without r.mu, so it never waits for a Next blocked
	// on the network.
	stDone  atomic.Bool
	stTrunc atomic.Bool
	stDur   atomic.Int64

	opening  atomic.Bool // the statement is still being started (run)
	done     bool
	endErr   error // the post-check failure, reported once
	duration time.Duration
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

func (r *resultSet) Stats() driver.Stats {
	d := time.Duration(r.stDur.Load())
	if !r.stDone.Load() {
		d = time.Since(r.start)
	}
	return driver.Stats{RowsAffected: -1, Duration: d, Truncated: r.stTrunc.Load()}
}

// ended records the duration for Stats once the result set is released.
func (r *resultSet) ended() {
	r.duration = time.Since(r.start)
	r.stDur.Store(int64(r.duration))
	r.stDone.Store(true)
}

func (r *resultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.release()
		return nil, false, mapError(ctx, "query", err)
	}
	if r.limit > 0 && r.count >= r.limit { // peek one row to learn whether we cut the result
		if r.rows.Next() {
			r.truncated = true
			r.stTrunc.Store(true)
		} else if err := r.rows.Err(); err != nil {
			r.release()
			return nil, false, mapError(r.ctx, "query", err)
		} else {
			r.exhausted = true
		}
		return nil, false, r.end()
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			r.release()
			return nil, false, mapError(r.ctx, "query", err)
		}
		r.exhausted = true
		return nil, false, r.end()
	}
	raw := make([]any, len(r.cols))
	ptrs := make([]any, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := r.rows.Scan(ptrs...); err != nil {
		r.release()
		return nil, false, mapError(r.ctx, "query", err)
	}
	row := make(driver.Row, len(raw))
	for i, v := range raw {
		row[i] = convertValue(r.dbTypes[i], v)
	}
	r.count++
	return row, true, nil
}

// end finishes the result set after its last row and reports a post-check
// failure.
func (r *resultSet) end() error {
	r.release()
	err := r.endErr
	r.endErr = nil
	return err
}

func (r *resultSet) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.end()
}

// release ends the result set: it closes the rows (go-mssqldb cancels a
// statement still streaming), runs the post-check and gives the connection
// back. In a transaction the connection stays with the Tx, which proves the
// transaction is still open (statementDone). It is idempotent; r.mu must
// be held.
func (r *resultSet) release() {
	if r.done {
		return
	}
	r.done = true
	if r.rows != nil {
		_ = r.rows.Close()
		r.rows = nil
	}
	if r.tx != nil {
		r.endErr = r.tx.statementDone(r)
		r.cancel()
		r.ended()
		return
	}
	if r.inTx {
		r.endErr = r.endReadOnly()
	} else if !r.ro {
		r.endWritable()
	}
	if r.discard {
		discardConn(r.sc)
	}
	_ = r.sc.Close()
	r.cancel()
	r.ended()
}

// conflictErr reports a deadlock, lock timeout or snapshot update conflict.
func conflictErr(err error) bool {
	var me mssqldb.Error
	return errors.As(err, &me) && (me.Number == 1205 || me.Number == 1222 || me.Number == 3960)
}

// serverCode is the server error number of err as text, or "".
func serverCode(err error) string {
	var me mssqldb.Error
	if errors.As(err, &me) {
		return fmt.Sprint(me.Number)
	}
	return ""
}
