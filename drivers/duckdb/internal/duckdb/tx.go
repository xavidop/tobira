package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"github.com/xavidop/tobira/driver"
)

var (
	errTxDone  = errors.New("transaction has already finished")
	errTxEnded = errors.New("the transaction ended or was aborted by an earlier error; roll back")
)

// Begin starts a transaction on a dedicated connection. DuckDB
// transactions are snapshot isolated; Isolation may be empty or
// "snapshot". A read-only one is BEGIN TRANSACTION READ ONLY and its
// statements pass the read-only guard; a read-only conn only begins
// read-only ones.
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	t, err := c.begin(ctx, "begin", opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (c *conn) begin(ctx context.Context, op string, opts driver.TxOptions) (*tx, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if iso := strings.ToLower(strings.TrimSpace(opts.Isolation)); iso != "" && iso != "snapshot" {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("DuckDB transactions are snapshot isolated; isolation "+opts.Isolation+" is not supported"))
	}
	if opts.ReadOnly && c.external {
		return nil, refuseExternal(op)
	}
	if !opts.ReadOnly && c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyConn)
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(op, err)
	}
	begin := "BEGIN TRANSACTION"
	if opts.ReadOnly {
		begin = "BEGIN TRANSACTION READ ONLY"
	}
	if _, err := sc.ExecContext(ctx, begin); err != nil {
		discard(sc)
		return nil, mapError(op, err)
	}
	t := &tx{c: c, sc: sc, readOnly: opts.ReadOnly}
	if t.id, err = txID(ctx, sc); err != nil {
		rollbackAndDiscard(ctx, sc)
		return nil, mapError(op, err)
	}
	if !c.track(t) { // Close raced with Begin
		t.abort()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	return t, nil
}

// txID is the id of the transaction sc is in. Outside a transaction every
// statement gets a new one, so an unchanged id proves the transaction is
// still the one Begin started.
func txID(ctx context.Context, sc *sql.Conn) (uint64, error) {
	var id uint64
	err := sc.QueryRowContext(ctx, "SELECT txid_current()").Scan(&id)
	return id, err
}

func (c *conn) track(t *tx) bool {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if c.closed.Load() {
		return false
	}
	if c.txs == nil {
		c.txs = map[*tx]struct{}{}
	}
	c.txs[t] = struct{}{}
	return true
}

func (c *conn) untrack(t *tx) {
	c.txMu.Lock()
	delete(c.txs, t)
	c.txMu.Unlock()
}

// abortTxs rolls back every open transaction; Close calls it first.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*tx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	for _, t := range open {
		t.abort()
	}
}

// tx is an open transaction on a dedicated connection.
type tx struct {
	c        *conn
	sc       *sql.Conn
	readOnly bool
	id       uint64

	mu   sync.Mutex
	done bool
	open map[*resultSet]struct{}
}

// alive returns the connection while the transaction is open and still
// the one Begin started (see txID). Otherwise the Tx is unusable: a
// statement ended it, or an error aborted it (DuckDB then refuses
// everything but ROLLBACK).
//
// It first releases the result sets still open on the connection: the
// next statement there ends them anyway.
func (t *tx) alive(ctx context.Context, op string) (*sql.Conn, error) {
	t.closeResults()
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(op, err)
	}
	id, err := txID(ctx, t.sc)
	if err != nil || id != t.id {
		if err != nil && ctx.Err() != nil {
			return nil, mapError(op, err)
		}
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxEnded)
	}
	return t.sc, nil
}

// writer returns the connection for a mutation, refusing read-only
// transactions.
func (t *tx) writer(ctx context.Context, op string) (*sql.Conn, error) {
	if t.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the transaction is read-only"))
	}
	return t.alive(ctx, op)
}

// Query runs one statement in the transaction. Transaction control is
// refused; a read-only transaction applies the read-only guard, and a
// per-query ReadOnly flag cannot be honoured inside a read-write one.
func (t *tx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if t.readOnly {
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
	} else {
		if q.ReadOnly {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("a read-only query cannot run inside a read-write transaction: begin a read-only one"))
		}
		if err := checkSingle(q.Text); err != nil {
			return nil, err
		}
		if isTxControl(q.Text) {
			return nil, errTxControl(driver.ErrUnsupported)
		}
	}
	sc, err := t.alive(ctx, "query")
	if err != nil {
		return nil, err
	}
	rs, err := runOn(ctx, sc, false, t.readOnly, q)
	if r, ok := rs.(*resultSet); ok {
		t.track(r)
	}
	return rs, err
}

func (t *tx) track(r *resultSet) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		r.release()
		return
	}
	if t.open == nil {
		t.open = map[*resultSet]struct{}{}
	}
	t.open[r] = struct{}{}
	r.owner = t
	t.mu.Unlock()
}

// closeResults releases the result sets still open on the connection.
func (t *tx) closeResults() {
	t.mu.Lock()
	open := t.open
	t.open = nil
	t.mu.Unlock()
	for r := range open {
		r.release()
	}
}

func (t *tx) forget(r *resultSet) {
	t.mu.Lock()
	delete(t.open, r)
	t.mu.Unlock()
}

// Commit makes the changes permanent. A failed COMMIT rolls back.
func (t *tx) Commit(ctx context.Context) error {
	if _, err := t.alive(ctx, "commit"); err != nil {
		return err
	}
	sc, err := t.finish("commit")
	if err != nil {
		return err
	}
	if _, err = sc.ExecContext(ctx, "COMMIT"); err != nil {
		rollbackAndDiscard(ctx, sc)
		return mapError("commit", err)
	}
	discard(sc)
	return nil
}

// Rollback discards the transaction; it is a no-op once it ended.
func (t *tx) Rollback(ctx context.Context) error {
	sc, err := t.finish("rollback")
	if err != nil {
		return nil
	}
	rollbackAndDiscard(ctx, sc)
	return nil
}

func (t *tx) abort() { _ = t.Rollback(context.Background()) }

// finish marks the transaction ended, closes its open result sets and
// hands back its connection.
func (t *tx) finish(op string) (*sql.Conn, error) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	t.done = true
	open := t.open
	t.open = nil
	t.mu.Unlock()
	for r := range open {
		r.release()
	}
	t.c.untrack(t)
	return t.sc, nil
}
