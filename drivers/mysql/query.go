package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

// Query runs one statement on a dedicated pooled connection, which is
// discarded afterwards: go-sql-driver cannot reset a session, so nothing a
// user statement does to its session (USE, SET, user variables, GET_LOCK,
// an open transaction, LOCK TABLES …) can reach a later call. Each call is
// therefore independent.
//
//   - A read-only call (conn opened read-only, or q.ReadOnly) needs a server
//     the guard was verified on (readOnlyRefusal) and must pass the allow-list
//     (checkReadOnly) before anything is sent. It then runs inside START
//     TRANSACTION READ ONLY … ROLLBACK with the session set read-only, and is
//     post-checked (endReadOnly).
//   - A namespaced call runs USE first.
//   - Anything else runs as is, in autocommit.
//
// One statement per query holds because multiStatements is always off: the
// server rejects a second statement (a trailing ';' is accepted).
//
// Limit is honoured by reading one row past it (Stats.Truncated). Results of
// a read-only call, and of SELECT, WITH, VALUES, TABLE and SHOW, that are
// closed before their end are cut off by closing the connection instead of
// being read to the end.
func (c *conn) Query(ctx context.Context, q driver.Query) (_ driver.ResultSet, err error) {
	defer func() { err = c.named(err) }()
	return c.run(ctx, q, c.readOnly || q.ReadOnly, checkReadOnly)
}

// checker is a read-only path's lexical allow-list: checkReadOnly for
// queries, Explain's for the statements it builds. mode is the session's
// lexMode (lexMode{} before the session is known).
type checker func(text string, mode lexMode) error

// run is Query's implementation. check is the allow-list a read-only call
// must pass; nil skips it, which only tests do, to prove the layers behind it.
func (c *conn) run(ctx context.Context, q driver.Query, ro bool, check checker) (driver.ResultSet, error) {
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	if ro {
		if err := c.readOnlyRefusal(); err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", err)
		}
	}
	if ro && check != nil {
		if err := check(q.Text, lexMode{}); err != nil {
			return nil, err
		}
	}
	args, err := toArgs(q.Params)
	if err != nil {
		return nil, err
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(ctx, "query", err)
	}
	return c.execute(ctx, sc, q, args, ro, check)
}

// toArgs converts query parameters to what go-sql-driver binds.
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

// execute runs q on its dedicated connection sc, which it always releases.
func (c *conn) execute(ctx context.Context, sc *sql.Conn, q driver.Query, args []any, ro bool, check checker) (driver.ResultSet, error) {
	r := newResultSet(ctx, c, sc, q, ro)
	return r.run(q, args, check)
}

// newResultSet prepares the result set of q on sc. Its statement context
// exists from the start, so a transaction ending concurrently can cancel it.
func newResultSet(ctx context.Context, c *conn, sc *sql.Conn, q driver.Query, ro bool) *resultSet {
	r := &resultSet{c: c, sc: sc, ctx: ctx, start: time.Now(), limit: max(q.Limit, 0), ro: ro}
	r.qctx, r.cancel = context.WithCancel(ctx)
	r.opening.Store(true)
	return r
}

// run opens the statement and finishes a column-less one at once.
func (r *resultSet) run(q driver.Query, args []any, check checker) (driver.ResultSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.opening.Store(false)
	ctx := r.ctx
	if err := r.open(q, args, check); err != nil {
		r.release()
		if r.endErr != nil {
			if conflictErr(err) { // a deadlock or lock wait timeout rolled the transaction back
				return nil, &driver.Error{Kind: driver.ErrConflict, Driver: Name, Op: "query", Code: mysqlCode(err), Err: errors.Join(err, r.endErr)}
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
func (r *resultSet) open(q driver.Query, args []any, check checker) error {
	toks := lex(q.Text, lexMode{}).toks
	// Inside a transaction the connection must survive: unread rows are
	// drained up to a bound, then the statement is killed (see drainTx).
	r.cutOnClose = r.tx == nil && (r.ro || isPunct(toks, 0, "(") || isWord(toks, 0, "SELECT", "WITH", "VALUES", "TABLE", "SHOW"))
	if r.ro {
		if err := r.beginReadOnly(q.Text, check); err != nil {
			return err
		}
	}
	if q.Namespace != "" {
		if _, err := r.sc.ExecContext(r.qctx, "USE "+quoteIdent(q.Namespace)); err != nil {
			return err
		}
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

// finishWithoutColumns completes a statement that returns no columns (DML,
// DDL, USE, SELECT … INTO @v): it reports ROW_COUNT() as the rows affected.
func (r *resultSet) finishWithoutColumns() (driver.ResultSet, error) {
	err := r.rows.Close()
	r.rows = nil
	r.exhausted = true
	affected := int64(-1)
	if err == nil {
		err = r.sc.QueryRowContext(r.qctx, "SELECT ROW_COUNT()").Scan(&affected)
	}
	r.release()
	switch {
	case r.endErr != nil:
		return nil, r.endErr
	case err != nil:
		return nil, mapError(r.ctx, "query", err)
	}
	return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: affected, Duration: r.duration}), nil
}

// resultSet streams one statement's rows from its dedicated connection. mu
// serialises Next, Close, Stats and the opening of the statement with a
// transaction ending from another goroutine.
type resultSet struct {
	mu     sync.Mutex
	c      *conn
	sc     *sql.Conn
	ctx    context.Context // the caller's ctx, for error mapping
	qctx   context.Context // the statement's ctx; cancelled on release
	cancel context.CancelFunc
	start  time.Time
	ro     bool
	limit  int
	tx     *myTx // the transaction the statement runs in; nil for a call of its own

	rows       *sql.Rows
	dbTypes    []string
	cols       []driver.Column
	cutOnClose bool // close the connection rather than drain unread rows
	inTx       bool // START TRANSACTION READ ONLY succeeded
	exhausted  bool // every row was read
	killed     bool // the connection was closed to cut the result short

	count     int
	truncated bool

	// Stats reads these without r.mu, so it never waits for a Next blocked
	// on the network.
	stDone  atomic.Bool
	stTrunc atomic.Bool
	stDur   atomic.Int64

	opening   atomic.Bool // the statement is still being started (run)
	releasing atomic.Bool // a transaction result set is in its own bounded close
	done      bool
	endErr    error // the read-only post-check failure, reported once
	duration  time.Duration
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

func (r *resultSet) Next(ctx context.Context) (_ driver.Row, _ bool, err error) {
	defer func() { err = r.c.named(err) }()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.releaseMode(releaseKill)
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

func (r *resultSet) Close() (err error) {
	defer func() { err = r.c.named(err) }()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.end()
}

// How a transaction's result set ends (see releaseMode).
const (
	releaseBounded = iota // read what is left up to a bound, then kill the statement
	releaseKill           // kill the statement at once (a cancelled Next)
	releaseAbandon        // the transaction is ending: its connection is being closed
)

// release ends the result set; see releaseMode.
func (r *resultSet) release() { r.releaseMode(releaseBounded) }

// releaseMode closes the rows, ends the read-only transaction and discards
// the connection. Inside a transaction it instead keeps the connection:
// unread rows are drained up to a bound and then the statement is stopped
// with KILL QUERY (at once for releaseKill), and the post-check proves the
// transaction is still open. releaseAbandon only closes the rows of a
// connection that is being closed. It is idempotent; r.mu must be held.
func (r *resultSet) releaseMode(mode int) {
	if r.done {
		return
	}
	r.done = true
	if r.tx != nil {
		if mode == releaseAbandon {
			r.cancel() // closes the socket under a statement still streaming
		}
		if r.rows != nil {
			if !r.exhausted && mode != releaseAbandon {
				r.releasing.Store(true)
				r.drainTx(mode == releaseKill)
				r.releasing.Store(false)
			}
			_ = r.rows.Close()
			r.rows = nil
		}
		if mode == releaseAbandon {
			r.tx.forget(r)
		} else {
			r.endErr = r.tx.statementDone(r)
		}
		r.cancel()
		r.ended()
		return
	}
	if r.rows != nil {
		if !r.exhausted && r.cutOnClose {
			killConn(r.sc)
			r.killed = true
		}
		_ = r.rows.Close()
		r.rows = nil
	}
	if r.ro && r.inTx && !r.killed {
		r.endErr = r.endReadOnly()
	}
	discardConn(r.sc)
	_ = r.sc.Close()
	r.cancel()
	r.ended()
}

// Bounds on reading what is left of a transaction's result set closed early.
const (
	txDrainRows = 1000
	txDrainTime = 250 * time.Millisecond
)

// drainTx reads what is left of a transaction's result set, up to
// txDrainRows rows or txDrainTime, whichever comes first, so the connection
// stays usable; past that bound (or at once when now is set) it stops the
// statement with KILL QUERY from another connection (see killQuery, which
// closes the socket instead when the kill cannot be sent, so the drain is
// never unbounded). A timer enforces the time bound even while a read is
// blocked.
func (r *resultSet) drainTx(now bool) {
	if now {
		r.tx.killQuery(r)
		return
	}
	fired := make(chan struct{})
	timer := time.AfterFunc(txDrainTime, func() {
		defer close(fired)
		r.tx.killQuery(r)
	})
	for range txDrainRows {
		if !r.rows.Next() {
			r.exhausted = true
			break
		}
	}
	if !timer.Stop() {
		<-fired // the kill ran; wait for it so it cannot hit a later statement
		return
	}
	if !r.exhausted {
		r.tx.killQuery(r)
	}
}

// conflictErr reports a deadlock or lock wait timeout.
func conflictErr(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

// mysqlCode is the server error number of err as text, or "".
func mysqlCode(err error) string {
	var me *gomysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Sprint(me.Number)
	}
	return ""
}
