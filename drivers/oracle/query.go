package oracle

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xavidop/tobira/driver"
)

// endTimeout bounds the statements that end a call (the transaction check,
// ROLLBACK, the schema reset), which run after the caller's ctx may be gone.
const endTimeout = 5 * time.Second

// txIDSQL reads the session's current transaction id: NULL when no
// transaction is active. SET TRANSACTION READ ONLY starts one at once; a
// read-write transaction gets one at its first change. An implicit commit
// (DDL) or a COMMIT ends it, so a different id, or none, after a statement
// proves that the statement left its transaction.
const txIDSQL = "SELECT DBMS_TRANSACTION.LOCAL_TRANSACTION_ID FROM DUAL"

// Query runs one statement on a dedicated pooled session.
//
//   - A read-only call (conn opened read-only, or q.ReadOnly) needs a server
//     the guard is verified on (guardVerified) and must pass the allow-list
//     (checkReadOnly) before anything is sent. It then runs inside SET
//     TRANSACTION READ ONLY, and once its rows are closed the driver checks
//     that the transaction is still the one it started, then rolls back.
//   - A namespaced call sets CURRENT_SCHEMA first and resets it afterwards.
//   - Anything else runs as is; go-ora commits each statement run outside a
//     transaction.
//
// A session that ran a writable statement other than a plain query is
// discarded afterwards, so nothing the statement did to it (ALTER SESSION,
// package state) reaches a later call. Statements that open with SELECT,
// WITH or '(' return rows; any other statement runs through Exec and
// reports the rows it affected.
//
// Limit is honoured by reading one row past it (Stats.Truncated).
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.run(ctx, q, c.readOnly || q.ReadOnly, checkReadOnly)
}

// checker is a read-only path's lexical allow-list. nil skips it, which only
// tests do, to prove the layers behind it.
type checker func(text string) error

// querier is what runs a statement: a *sql.Conn or a *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (c *conn) run(ctx context.Context, q driver.Query, ro bool, check checker) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if q.Namespace != "" {
		if err := checkSchemaName(op, q.Namespace); err != nil {
			return nil, err
		}
	}
	if ro && !guardVerified(c.version) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, unverifiedServer(c.version))
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
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	s := &call{c: c, sc: sc, ctx: ctx, keep: ro || readOnlyReason(q.Text) == ""}
	qctx, cancel := context.WithCancel(ctx)
	start := time.Now()
	fail := func(err error) (driver.ResultSet, error) {
		cancel()
		endErr := s.end()
		if endErr != nil {
			return nil, endErr
		}
		return nil, mapError(ctx, op, err)
	}
	if q.Namespace != "" {
		if err := s.setSchema(qctx, q.Namespace); err != nil {
			return fail(err)
		}
	}
	var x querier = sc
	if ro {
		if err := s.beginReadOnly(qctx); err != nil {
			return fail(err)
		}
		x = s.tx
	}
	if !ro && !isQueryText(q.Text) {
		res, err := x.ExecContext(qctx, q.Text, args...)
		if err != nil {
			return fail(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			affected = -1
		}
		cancel()
		if err := s.end(); err != nil {
			return nil, err
		}
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: affected, Duration: time.Since(start)}), nil
	}
	rows, err := x.QueryContext(qctx, q.Text, args...)
	if err != nil {
		return fail(err)
	}
	r, err := newResultSet(ctx, cancel, rows, q.Limit, start, s.end)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// toArgs converts query parameters to what go-ora binds.
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

// call is one statement's dedicated session and what must be undone on it.
type call struct {
	c         *conn
	sc        *sql.Conn
	ctx       context.Context
	keep      bool // return the session to the pool afterwards
	schemaSet bool // CURRENT_SCHEMA was changed and must be reset
	tx        *sql.Tx
	txID      string
	once      sync.Once
	endErr    error
}

// setSchema points the session's CURRENT_SCHEMA at ns.
func (s *call) setSchema(ctx context.Context, ns string) error {
	s.schemaSet = true
	_, err := s.sc.ExecContext(ctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quoteIdent(ns))
	return err
}

// beginReadOnly starts the read-only transaction and records its id. The
// transaction lives on a context that outlives the caller's, so end can
// always roll it back.
func (s *call) beginReadOnly(ctx context.Context) error {
	tx, err := s.sc.BeginTx(context.WithoutCancel(s.ctx), nil)
	if err != nil {
		return err
	}
	s.tx = tx
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		return err
	}
	id, err := readTxID(ctx, tx)
	if err != nil {
		return err
	}
	if id == "" {
		return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("the read-only transaction did not start"))
	}
	s.txID = id
	return nil
}

// readTxID reads the current transaction id ("" for none).
func readTxID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var id sql.NullString
	err := q.QueryRowContext(ctx, txIDSQL).Scan(&id)
	return id.String, err
}

// end finishes the call once: it proves the read-only transaction is still
// the one begun (else ErrReadOnly), rolls it back, resets CURRENT_SCHEMA and
// releases the session, discarding it when it ran a writable statement or
// anything on it failed.
func (s *call) end() error {
	s.once.Do(func() { s.endErr = s.finish() })
	return s.endErr
}

func (s *call) finish() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), endTimeout)
	defer cancel()
	discard := !s.keep
	var escaped error
	if s.tx != nil {
		switch id, err := readTxID(ctx, s.tx); {
		case err != nil:
			discard = true // the session is unusable; the server rolls back when it goes
		case id != s.txID:
			escaped = driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("the statement ended the read-only transaction; the session was discarded"))
			discard = true
		}
		if err := s.tx.Rollback(); err != nil {
			discard = true
		}
	}
	if s.schemaSet && !discard {
		if _, err := s.sc.ExecContext(ctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quoteIdent(s.c.user)); err != nil {
			discard = true
		}
	}
	if discard {
		discardConn(s.sc)
	}
	_ = s.sc.Close()
	return escaped
}

// discardConn makes database/sql close the session instead of returning it
// to the pool.
func discardConn(sc *sql.Conn) {
	_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
}

// quoteIdent quotes an Oracle identifier. Oracle has no escape for '"'
// inside one. Names from the dictionary never hold one; a caller's
// namespace is checked with checkSchemaName before it gets here.
func quoteIdent(s string) string { return `"` + s + `"` }

// resultSet streams one statement's rows. mu serialises Next and Close.
type resultSet struct {
	mu     sync.Mutex
	ctx    context.Context // the caller's ctx, for error mapping
	cancel context.CancelFunc
	rows   *sql.Rows
	infos  []colInfo
	cols   []driver.Column
	limit  int
	count  int
	done   bool
	onDone func() error // the owner's post-check and release, run once
	endErr error

	warnings []string
	cut      map[int]bool // columns with a value cut at maxLOBBytes

	start   time.Time
	stDone  atomic.Bool
	stTrunc atomic.Bool
	stDur   atomic.Int64
	stWarn  atomic.Pointer[[]string]
}

// newResultSet wraps rows; a failure to read the columns ends the call.
func newResultSet(ctx context.Context, cancel context.CancelFunc, rows *sql.Rows, limit int, start time.Time, onDone func() error) (*resultSet, error) {
	r := &resultSet{ctx: ctx, cancel: cancel, rows: rows, limit: max(limit, 0), onDone: onDone, start: start}
	cts, err := rows.ColumnTypes()
	if err != nil {
		r.release()
		if r.endErr != nil {
			return nil, r.endErr
		}
		return nil, mapError(ctx, "query", err)
	}
	r.infos = make([]colInfo, len(cts))
	r.cols = make([]driver.Column, len(cts))
	for i, ct := range cts {
		p, s, ok := ct.DecimalSize()
		r.infos[i] = resultColumn(ct.DatabaseTypeName(), p, s, ok)
		r.cols[i] = driver.Column{Name: ct.Name(), NativeType: r.infos[i].native, Type: r.infos[i].typ}
	}
	return r, nil
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

// Stats never waits for a Next blocked on the network.
func (r *resultSet) Stats() driver.Stats {
	d := time.Duration(r.stDur.Load())
	if !r.stDone.Load() {
		d = time.Since(r.start)
	}
	st := driver.Stats{RowsAffected: -1, Duration: d, Truncated: r.stTrunc.Load()}
	if w := r.stWarn.Load(); w != nil {
		st.Warnings = *w
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
		return nil, false, mapError(ctx, "query", err)
	}
	if r.limit > 0 && r.count >= r.limit { // peek one row to learn whether we cut the result
		if r.rows.Next() {
			r.stTrunc.Store(true)
		} else if err := r.rows.Err(); err != nil {
			return nil, false, r.fail(err)
		}
		return nil, false, r.end()
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return nil, false, r.fail(err)
		}
		return nil, false, r.end()
	}
	raw := make([]any, len(r.cols))
	ptrs := make([]any, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := r.rows.Scan(ptrs...); err != nil {
		return nil, false, r.fail(err)
	}
	row := make(driver.Row, len(raw))
	for i, v := range raw {
		var cut bool
		row[i], cut = convertValue(r.infos[i], v)
		if cut && !r.cut[i] {
			if r.cut == nil {
				r.cut = map[int]bool{}
			}
			r.cut[i] = true
			r.warnings = append(r.warnings, fmt.Sprintf("column %s: values longer than %d bytes were cut", r.cols[i].Name, maxLOBBytes))
			w := append([]string(nil), r.warnings...)
			r.stWarn.Store(&w)
		}
	}
	r.count++
	return row, true, nil
}

// fail ends the result set after a read error; a post-check failure wins.
func (r *resultSet) fail(err error) error {
	r.release()
	if r.endErr != nil {
		e := r.endErr
		r.endErr = nil
		return e
	}
	return mapError(r.ctx, "query", err)
}

// end finishes the result set and reports a post-check failure once.
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

// release closes the rows and runs the owner's post-check. It is
// idempotent; r.mu must be held (or r not yet shared).
func (r *resultSet) release() {
	if r.done {
		return
	}
	r.done = true
	_ = r.rows.Close()
	r.cancel()
	if r.onDone != nil {
		r.endErr = r.onDone()
	}
	r.stDur.Store(int64(time.Since(r.start)))
	r.stDone.Store(true)
}
