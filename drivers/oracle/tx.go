package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

var (
	errTxDone     = errors.New("transaction has already finished")
	errTxEnded    = errors.New("a statement ended the transaction; roll back")
	errTxBusy     = errors.New("another call on this transaction is still running, or one of its result sets is open: close it first")
	errReadOnlyTx = errors.New("the transaction is read-only")
	errReadOnlyDB = errors.New("the connection is read-only")
)

// isolationLevel maps a native isolation name to Oracle's spelling. Oracle
// has READ COMMITTED (the default) and SERIALIZABLE.
func isolationLevel(name string) (string, bool) {
	switch lvl := strings.ToUpper(strings.Join(strings.Fields(name), " ")); lvl {
	case "", "READ COMMITTED", "SERIALIZABLE":
		return lvl, true
	}
	return "", false
}

// Begin starts a transaction on a dedicated session that it holds until
// Commit or Rollback.
//
// A read-only Tx runs inside SET TRANSACTION READ ONLY and its Query applies
// the read-only allow-list. A writable Tx refuses statements that would end
// it (see checkTxStatement). After every statement either kind checks that
// the transaction is still the same one (its id has not changed, or, for a
// writable Tx that had not changed anything yet, it has just begun); when it
// is not, the Tx is dead and only Rollback works.
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	t, err := c.begin(ctx, "begin", opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (c *conn) begin(ctx context.Context, op string, opts driver.TxOptions) (*oraTx, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	lvl, ok := isolationLevel(opts.Isolation)
	if !ok {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("unknown isolation level "+opts.Isolation+": Oracle has READ COMMITTED and SERIALIZABLE"))
	}
	if opts.ReadOnly && lvl != "" {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a read-only transaction takes no isolation level: it always reads one snapshot"))
	}
	if !opts.ReadOnly && c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyDB)
	}
	if opts.ReadOnly && !guardVerified(c.version) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, unverifiedServer(c.version))
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	t := &oraTx{c: c, sc: sc, readOnly: opts.ReadOnly}
	if err := t.start(ctx, lvl); err != nil {
		if t.tx != nil {
			_ = t.tx.Rollback()
		}
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

// start opens the transaction on t's session. Its context outlives ctx, so
// Commit and Rollback always reach the server.
func (t *oraTx) start(ctx context.Context, lvl string) error {
	tx, err := t.sc.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	t.tx = tx
	switch {
	case t.readOnly:
		_, err = tx.ExecContext(ctx, "SET TRANSACTION READ ONLY")
	case lvl != "":
		_, err = tx.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL "+lvl)
	}
	if err != nil {
		return err
	}
	if t.txID, err = readTxID(ctx, tx); err != nil {
		return err
	}
	if t.readOnly && t.txID == "" {
		return errors.New("the read-only transaction did not start")
	}
	return nil
}

// track registers t so Close can roll it back. It reports false when the
// connection is already closed.
func (c *conn) track(t *oraTx) bool {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if c.closed.Load() {
		return false
	}
	if c.txs == nil {
		c.txs = map[*oraTx]struct{}{}
	}
	c.txs[t] = struct{}{}
	return true
}

func (c *conn) untrack(t *oraTx) {
	c.txMu.Lock()
	delete(c.txs, t)
	c.txMu.Unlock()
}

// abortTxs rolls back every open transaction so the pool can close.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*oraTx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	for _, t := range open {
		_ = t.Rollback(context.Background())
	}
}

// oraTx is an open transaction on a dedicated session.
type oraTx struct {
	c        *conn
	sc       *sql.Conn
	tx       *sql.Tx
	readOnly bool

	mu     sync.Mutex
	done   bool
	dead   bool
	busy   bool       // a statement, mutation or explain is using the session
	open   *resultSet // the statement's result set, while it is open
	cancel context.CancelFunc
	txID   string // "" until a writable transaction changes something
}

// claim reserves the session for one call. It returns the context the call
// must run under (Rollback cancels it) and the function that ends the claim.
func (t *oraTx) claim(ctx context.Context, op string, write bool) (context.Context, func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.done:
		return nil, nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	case t.dead:
		return nil, nil, driver.Wrap(driver.ErrConnection, Name, op, errTxEnded)
	case t.busy:
		return nil, nil, driver.Wrap(driver.ErrUnsupported, Name, op, errTxBusy)
	case write && t.readOnly:
		return nil, nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyTx)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, mapError(ctx, op, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	t.busy, t.cancel = true, cancel
	return cctx, func() {
		cancel()
		t.mu.Lock()
		t.busy, t.open, t.cancel = false, nil, nil
		t.mu.Unlock()
	}, nil
}

// Query runs one statement inside the transaction. A read-only Tx applies
// the read-only allow-list (checkReadOnly); a writable one refuses
// statements that would end the transaction (checkTxStatement). A
// per-query ReadOnly cannot be honoured inside a read-write Tx. The result
// set must be closed before the next call; afterwards the driver proves the
// transaction is still the same one (statementDone).
func (t *oraTx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	check := checker(checkTxStatement)
	if t.readOnly {
		check = checkReadOnly
	}
	return t.query(ctx, q, check)
}

// query is Query's implementation. check nil skips the statement check,
// which only tests do, to prove the post-check behind it.
func (t *oraTx) query(ctx context.Context, q driver.Query, check checker) (driver.ResultSet, error) {
	const op = "query"
	if q.ReadOnly && !t.readOnly {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a read-only query cannot run inside a read-write transaction: begin a read-only one"))
	}
	if q.Namespace != "" {
		if err := checkSchemaName(op, q.Namespace); err != nil {
			return nil, err
		}
	}
	if check != nil {
		if err := check(q.Text); err != nil {
			return nil, err
		}
	}
	args, err := toArgs(q.Params)
	if err != nil {
		return nil, err
	}
	qctx, release, err := t.claim(ctx, op, false)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	schemaSet := false
	done := func() error {
		err := t.statementDone(ctx, schemaSet)
		release()
		return err
	}
	fail := func(err error) (driver.ResultSet, error) {
		if derr := done(); derr != nil {
			return nil, derr
		}
		return nil, mapError(ctx, op, err)
	}
	if q.Namespace != "" {
		schemaSet = true
		if _, err := t.tx.ExecContext(qctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quoteIdent(q.Namespace)); err != nil {
			return fail(err)
		}
	}
	if !isQueryText(q.Text) {
		res, err := t.tx.ExecContext(qctx, q.Text, args...)
		if err != nil {
			return fail(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			affected = -1
		}
		if err := done(); err != nil {
			return nil, err
		}
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: affected, Duration: time.Since(start)}), nil
	}
	rows, err := t.tx.QueryContext(qctx, q.Text, args...)
	if err != nil {
		return fail(err)
	}
	r, err := newResultSet(ctx, func() {}, rows, q.Limit, start, done)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.open = r
	t.mu.Unlock()
	return r, nil
}

// statementDone runs after each statement: it resets CURRENT_SCHEMA when
// the statement set it, and proves the transaction is still the same one.
// If it is not, or the proof cannot be made, the Tx is dead: every later
// call fails and only Rollback works.
func (t *oraTx) statementDone(ctx context.Context, schemaSet bool) error {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	defer cancel()
	err := t.proveOpen(bctx)
	if err == nil && schemaSet {
		if _, e := t.tx.ExecContext(bctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quoteIdent(t.c.user)); e != nil {
			err = driver.Wrap(driver.ErrConnection, Name, "query", fmt.Errorf("cannot reset the schema: %w; roll back", e))
		}
	}
	if err != nil {
		t.mu.Lock()
		t.dead = true
		t.mu.Unlock()
	}
	return err
}

// proveOpen checks the transaction id: it must not change once the
// transaction has one. A read-only Tx has one from the start.
func (t *oraTx) proveOpen(ctx context.Context) error {
	id, err := readTxID(ctx, t.tx)
	if err != nil {
		return driver.Wrap(driver.ErrConnection, Name, "query", fmt.Errorf("cannot verify that the transaction is still open: %w; roll back", err))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.txID != "" && id != t.txID {
		kind := driver.ErrUnsupported
		if t.readOnly {
			kind = driver.ErrReadOnly
		}
		return driver.Wrap(kind, Name, "query", errTxEnded)
	}
	t.txID = id
	return nil
}

// Commit makes the transaction's changes permanent. A result set still open
// is closed first. A dead Tx is rolled back instead and Commit says why.
// The session goes back to the pool afterwards.
func (t *oraTx) Commit(ctx context.Context) error {
	if err := t.finish("commit"); err != nil {
		return err
	}
	defer t.release(false)
	if t.isDead() {
		_ = t.tx.Rollback()
		return driver.Wrap(driver.ErrConnection, Name, "commit", fmt.Errorf("the transaction was rolled back: %w", errTxEnded))
	}
	if err := t.tx.Commit(); err != nil {
		_ = t.tx.Rollback()
		return mapError(ctx, "commit", err)
	}
	return nil
}

// Rollback discards the transaction. It is a no-op once the transaction
// ended. A statement still running is cancelled and an open result set is
// closed first.
func (t *oraTx) Rollback(ctx context.Context) error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return nil
	}
	if t.cancel != nil {
		t.cancel()
	}
	t.mu.Unlock()
	if err := t.finish("rollback"); err != nil {
		return nil
	}
	err := t.tx.Rollback()
	t.release(err != nil)
	return nil
}

// finish marks the transaction ended after closing an open result set. It
// fails when the transaction already ended, or a mutation or explain still
// holds the session (Commit only; Rollback cancels it first).
func (t *oraTx) finish(op string) error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	open := t.open
	t.mu.Unlock()
	if open != nil {
		_ = open.Close()
	}
	deadline := time.Now().Add(endTimeout)
	for {
		t.mu.Lock()
		if !t.busy || time.Now().After(deadline) {
			busy := t.busy
			if !busy || op == "rollback" {
				t.done = true
			}
			t.mu.Unlock()
			if busy && op != "rollback" {
				return driver.Wrap(driver.ErrUnsupported, Name, op, errTxBusy)
			}
			break
		}
		t.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.c.untrack(t)
	return nil
}

// release gives the session back: to the pool, or closed when discard is
// set or it may hold state from a broken call.
func (t *oraTx) release(discard bool) {
	if discard || t.isDead() {
		discardConn(t.sc)
	}
	_ = t.sc.Close()
}

func (t *oraTx) isDead() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead
}
