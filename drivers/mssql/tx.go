package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/xavidop/tobira/driver"
)

var (
	errTxDone     = errors.New("transaction has already finished")
	errTxBusy     = errors.New("another call on this transaction is still running, or one of its result sets is open: close it first")
	errTxRunning  = errors.New("a statement is still running in this transaction: wait for it, or roll back")
	errReadOnlyTx = errors.New("the transaction is read-only")
	errReadOnlyDB = errors.New("the connection is read-only")
)

// isolationLevel maps a native isolation name to SQL Server's spelling.
func isolationLevel(name string) (string, bool) {
	switch lvl := strings.ToUpper(strings.Join(strings.Fields(name), " ")); lvl {
	case "", "READ UNCOMMITTED", "READ COMMITTED", "REPEATABLE READ", "SNAPSHOT", "SERIALIZABLE":
		return lvl, true
	}
	return "", false
}

// Begin starts a transaction on a dedicated connection that it holds until
// Commit or Rollback.
//
// A read-only Tx applies the read-only allow-list (checkReadOnly) to every
// statement and never commits: SQL Server has no read-only transaction, so
// its Commit rolls back too, and nothing a statement might have written
// persists. A writable Tx refuses statements that would end it (see
// checkTxStatement). After every statement either kind proves that its
// transaction is still the only one open (@@TRANCOUNT = 1) and can still
// commit (XACT_STATE() = 1); when it is not, the Tx is dead and only
// Rollback works.
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	t, err := c.begin(ctx, "begin", opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (c *conn) begin(ctx context.Context, op string, opts driver.TxOptions) (*msTx, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	lvl, ok := isolationLevel(opts.Isolation)
	if !ok {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("unknown isolation level "+opts.Isolation))
	}
	if !opts.ReadOnly && c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyDB)
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	t := &msTx{c: c, sc: sc, readOnly: opts.ReadOnly}
	begin := []string{beginWritableSQL}
	if lvl != "" {
		begin = append(begin, "SET TRANSACTION ISOLATION LEVEL "+lvl)
	}
	begin = append(begin, "BEGIN TRANSACTION")
	if _, err := sc.ExecContext(ctx, prefixSQL(begin...)); err != nil {
		discardConn(sc)
		_ = sc.Close()
		return nil, mapError(ctx, op, err)
	}
	if !c.track(t) { // Close raced with Begin
		_ = t.Rollback(context.Background())
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	return t, nil
}

// track registers t so Close can roll it back. It reports false when the
// connection is already closed.
func (c *conn) track(t *msTx) bool {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if c.closed.Load() {
		return false
	}
	if c.txs == nil {
		c.txs = map[*msTx]struct{}{}
	}
	c.txs[t] = struct{}{}
	return true
}

func (c *conn) untrack(t *msTx) {
	c.txMu.Lock()
	delete(c.txs, t)
	c.txMu.Unlock()
}

// abortTxs rolls back every open transaction so the pool can close.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*msTx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	for _, t := range open {
		_ = t.Rollback(context.Background())
	}
}

// msTx is an open transaction on a dedicated connection.
type msTx struct {
	c        *conn
	sc       *sql.Conn
	readOnly bool

	mu   sync.Mutex
	done bool
	dead bool       // a statement ended the transaction, or the connection broke
	call *txCall    // a mutation or explain using sc
	open *resultSet // a statement using sc: starting, or its result set open
}

// txCall is a mutation in progress, which Rollback can cancel and wait for.
type txCall struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// claim checks that the transaction is open and that no call is using its
// connection. t.mu must be held.
func (t *msTx) claim(ctx context.Context, op string) error {
	switch {
	case t.done:
		return driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	case t.dead:
		return driver.Wrap(driver.ErrConnection, Name, op, errEnded)
	case t.call != nil || t.open != nil:
		return driver.Wrap(driver.ErrUnsupported, Name, op, errTxBusy)
	}
	if err := ctx.Err(); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// acquireFor claims the connection for statement r: r becomes t.open in the
// same step, so Commit and Rollback always see the connection in use.
func (t *msTx) acquireFor(ctx context.Context, op string, r *resultSet) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.claim(ctx, op); err != nil {
		return err
	}
	t.open = r
	return nil
}

// use claims the connection for a call other than a statement's result set
// (a mutation, an explain). writable refuses a read-only Tx. It returns the
// context the call must run under (Rollback cancels it) and the function
// that ends the claim.
func (t *msTx) use(ctx context.Context, op string, writable bool) (*sql.Conn, context.Context, func(), error) {
	t.mu.Lock()
	if err := t.claim(ctx, op); err != nil {
		t.mu.Unlock()
		return nil, nil, nil, err
	}
	if writable && t.readOnly {
		t.mu.Unlock()
		return nil, nil, nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyTx)
	}
	cctx, cancel := context.WithCancel(ctx)
	call := &txCall{cancel: cancel, done: make(chan struct{})}
	t.call = call
	t.mu.Unlock()
	return t.sc, cctx, func() {
		t.mu.Lock()
		if t.call == call {
			t.call = nil
		}
		t.mu.Unlock()
		cancel()
		close(call.done)
	}, nil
}

// Query runs one statement inside the transaction. A read-only Tx applies
// the read-only allow-list (checkReadOnly); a writable one refuses
// statements that would end the transaction (checkTxStatement). A namespace
// runs USE <database> first, inside the transaction. A read-only per-query
// flag cannot be honoured inside a read-write Tx.
//
// The result set must be closed before the next statement; closing it early
// cancels the statement on the server, which leaves the transaction open.
// After the statement the driver proves the transaction is still open; see
// statementDone.
func (t *msTx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	check := checker(checkTxStatement)
	if t.readOnly {
		check = checkReadOnly
	}
	return t.query(ctx, q, check)
}

// query is Query's implementation. check is the statement check; nil skips
// it, which only tests do, to prove the post-check behind it.
func (t *msTx) query(ctx context.Context, q driver.Query, check checker) (driver.ResultSet, error) {
	const op = "query"
	r := newResultSet(ctx, t.c, t.sc, q, t.readOnly)
	r.tx = t
	if err := t.acquireFor(ctx, op, r); err != nil {
		r.cancel()
		return nil, err
	}
	fail := func(err error) (driver.ResultSet, error) {
		r.cancel()
		t.forget(r)
		return nil, err
	}
	if q.ReadOnly && !t.readOnly {
		return fail(driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a read-only query cannot run inside a read-write transaction: begin a read-only one")))
	}
	if check != nil {
		if err := check(q.Text); err != nil {
			return fail(err)
		}
	}
	args, err := toArgs(q.Params)
	if err != nil {
		return fail(err)
	}
	use, err := t.c.useSQL(q.Namespace)
	if err != nil {
		return fail(err)
	}
	return r.run(q, args, use)
}

// statementDone runs when a statement's rows are closed: it releases the
// connection for the next statement and proves the transaction is still
// open. If it is not, or the proof cannot be made, the Tx is dead: every
// later call fails and only Rollback works.
func (t *msTx) statementDone(r *resultSet) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), rollbackTimeout)
	defer cancel()
	err := t.proveOpen(ctx)
	t.mu.Lock()
	if t.open == r {
		t.open = nil
	}
	if err != nil {
		t.dead = true
	}
	t.mu.Unlock()
	return err
}

// forget detaches a result set abandoned before it ran.
func (t *msTx) forget(r *resultSet) {
	t.mu.Lock()
	if t.open == r {
		t.open = nil
	}
	t.mu.Unlock()
}

// proveOpen checks that the connection is still inside exactly the
// transaction Begin opened (@@TRANCOUNT = 1) and that it can still commit
// (XACT_STATE() = 1). A failure is ErrReadOnly in a read-only Tx and
// ErrUnsupported otherwise; a check that cannot run is ErrConnection.
func (t *msTx) proveOpen(ctx context.Context) error {
	kind := driver.ErrUnsupported
	if t.readOnly {
		kind = driver.ErrReadOnly
	}
	var count, state int64
	if err := t.sc.QueryRowContext(ctx, "SELECT @@TRANCOUNT, XACT_STATE()").Scan(&count, &state); err != nil {
		return driver.Wrap(driver.ErrConnection, Name, "query", fmt.Errorf("cannot verify that the transaction is still open: %w; roll back", err))
	}
	switch {
	case count == 0:
		return driver.Wrap(kind, Name, "query", errEnded)
	case count != 1:
		return driver.Wrap(kind, Name, "query", fmt.Errorf("a statement opened a nested transaction (@@TRANCOUNT %d); roll back", count))
	case state != 1:
		return driver.Wrap(kind, Name, "query", errors.New("the transaction can no longer commit after an error; roll back"))
	}
	return nil
}

// Commit makes the transaction's changes permanent. A result set still
// open is closed first (its statement is cancelled) and post-checked. If
// the Tx is broken, Commit rolls back and says why. A read-only Tx rolls
// back: it has nothing to keep.
func (t *msTx) Commit(ctx context.Context) error {
	why, err := t.finish("commit", false)
	if err != nil {
		return err
	}
	if t.isDead() {
		t.end(ctx, "ROLLBACK")
		if why == nil {
			why = errEnded
		}
		return driver.Wrap(driver.ErrConnection, Name, "commit", fmt.Errorf("the transaction was rolled back: %w", why))
	}
	if t.readOnly {
		t.end(ctx, "ROLLBACK")
		return nil
	}
	if _, err := t.sc.ExecContext(ctx, "COMMIT TRANSACTION"); err != nil {
		t.end(ctx, "IF @@TRANCOUNT > 0 ROLLBACK")
		return mapError(ctx, "commit", err)
	}
	_ = t.sc.Close()
	return nil
}

// Rollback discards the transaction. It is a no-op once the transaction
// ended. A failed ROLLBACK is not reported: the connection is then
// discarded, and the server rolls back when it goes.
func (t *msTx) Rollback(ctx context.Context) error {
	if _, err := t.finish("rollback", true); err != nil {
		return nil
	}
	t.end(ctx, "IF @@TRANCOUNT > 0 ROLLBACK")
	return nil
}

// end runs stmt on the transaction's connection with a bounded context that
// outlives a cancelled caller, then releases the connection: to the pool
// (which resets it) when stmt worked, closed otherwise.
func (t *msTx) end(ctx context.Context, stmt string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if _, err := t.sc.ExecContext(rctx, stmt); err != nil {
		discardConn(t.sc)
	}
	_ = t.sc.Close()
}

func (t *msTx) isDead() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead
}

// finish marks the transaction ended after dealing with a call or result
// set still using its connection. Rollback (abandon) cancels it and waits
// for it to stop; Commit refuses while a mutation runs or a statement is
// still starting, and otherwise closes an open result set (cancelling its
// statement) and returns its post-check failure, if any.
func (t *msTx) finish(op string, abandon bool) (closed error, err error) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	open, call := t.open, t.call
	if !abandon && (call != nil || open != nil && open.opening.Load()) {
		t.mu.Unlock()
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errTxRunning)
	}
	t.done = true
	t.mu.Unlock()
	if call != nil {
		call.cancel()
		<-call.done
	}
	if open != nil {
		open.cancel() // unblocks a reader waiting on the network
		open.mu.Lock()
		open.release()
		closed, open.endErr = open.endErr, nil
		open.mu.Unlock()
	}
	t.c.untrack(t)
	return closed, nil
}
