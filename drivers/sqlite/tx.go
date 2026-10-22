package sqlite

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// endTimeout bounds the ROLLBACK that ends a transaction whose ctx is gone.
const endTimeout = 5 * time.Second

var (
	errTxDone  = errors.New("transaction has already finished")
	errTxEnded = errors.New("a statement ended the transaction; roll back")
)

// Begin starts a transaction on a dedicated connection. A read-write one
// takes the write lock up front (BEGIN IMMEDIATE), so a busy database fails
// here rather than at the first write. A read-only one runs on the guarded
// read-only handle and its Query applies the read-only allow-list.
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
	if iso := strings.ToLower(strings.TrimSpace(opts.Isolation)); iso != "" && iso != "serializable" {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("sqlite transactions are serializable; isolation "+opts.Isolation+" is not supported"))
	}
	if !opts.ReadOnly && c.rw == nil {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyMutation)
	}
	db, begin := c.rw, "BEGIN IMMEDIATE"
	if opts.ReadOnly {
		db, begin = c.ro, "BEGIN"
	}
	sc, err := db.Conn(ctx)
	if err != nil {
		return nil, mapError(op, err)
	}
	if opts.ReadOnly {
		if err := guardReadOnly(ctx, sc); err != nil {
			_ = sc.Close()
			return nil, err
		}
	}
	if _, err := sc.ExecContext(ctx, begin); err != nil {
		_ = sc.Close()
		return nil, mapError(op, err)
	}
	t := &tx{c: c, sc: sc, readOnly: opts.ReadOnly}
	if !c.track(t) { // Close raced with Begin
		t.abort()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	return t, nil
}

// track registers t so Close can roll it back. It reports false when the
// connection is already closed.
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

// abortTxs rolls back every open transaction. Close calls it before the
// handles close.
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

	mu   sync.Mutex
	done bool
	dead bool                    // a statement ended the transaction
	open map[*resultSet]struct{} // result sets still reading from sc
}

// live returns the connection while the transaction is open.
func (t *tx) live(op string) (*sql.Conn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	if t.dead {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxEnded)
	}
	return t.sc, nil
}

// alive is live plus a fail-closed check that the connection is still inside
// the transaction: BEGIN fails with "within a transaction" exactly then. If it
// succeeds, a statement ended the transaction behind the driver's back; the
// probe is rolled back and the Tx is dead, so nothing more runs on it.
func (t *tx) alive(ctx context.Context, op string) (*sql.Conn, error) {
	sc, err := t.live(op)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(op, err)
	}
	_, err = sc.ExecContext(ctx, "BEGIN")
	switch {
	case err == nil:
		_, _ = sc.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		t.mu.Lock()
		t.dead = true
		t.mu.Unlock()
		kind := driver.ErrUnsupported
		if t.readOnly {
			kind = driver.ErrReadOnly
		}
		return nil, driver.Wrap(kind, Name, op, errTxEnded)
	case strings.Contains(err.Error(), "within a transaction"):
		return sc, nil
	}
	return nil, mapError(op, err)
}

// writer returns the connection for a mutation, refusing read-only transactions.
func (t *tx) writer(ctx context.Context, op string) (*sql.Conn, error) {
	if t.readOnly {
		if _, err := t.live(op); err != nil {
			return nil, err
		}
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the transaction is read-only"))
	}
	return t.alive(ctx, op)
}

// Query runs one statement in the transaction. Transaction-control statements
// are refused; the single-statement check and, for a read-only transaction,
// the read-only allow-list apply. A per-query ReadOnly flag cannot be honoured
// inside a read-write transaction.
func (t *tx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	sc, err := t.alive(ctx, "query")
	if err != nil {
		return nil, err
	}
	if isTxControl(q.Text) {
		kind := driver.ErrUnsupported
		if t.readOnly {
			kind = driver.ErrReadOnly
		}
		return nil, driver.Wrap(kind, Name, "query", errors.New("transaction control statements are not allowed inside a transaction: use Commit and Rollback"))
	}
	if q.ReadOnly && !t.readOnly {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("a read-only query cannot run inside a read-write transaction: begin a read-only one"))
	}
	if t.readOnly {
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
	}
	if err := checkSingle(q.Text); err != nil {
		return nil, err
	}
	rs, err := execOn(ctx, sc, false, !t.readOnly, q, start)
	if r, ok := rs.(*resultSet); ok {
		t.track(r)
	}
	return rs, err
}

// track remembers a streaming result set so finishing the transaction can
// close it first.
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

func (t *tx) forget(r *resultSet) {
	t.mu.Lock()
	delete(t.open, r)
	t.mu.Unlock()
}

// Commit makes the transaction's changes permanent. A failed COMMIT rolls
// back.
func (t *tx) Commit(ctx context.Context) error {
	if _, err := t.alive(ctx, "commit"); err != nil {
		return err
	}
	sc, err := t.finish("commit")
	if err != nil {
		return err
	}
	_, err = sc.ExecContext(ctx, "COMMIT")
	if err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		_, _ = sc.ExecContext(rctx, "ROLLBACK")
		cancel()
	}
	discard(sc)
	return mapError("commit", err)
}

// Rollback discards the transaction. It is a no-op once the transaction ended.
func (t *tx) Rollback(ctx context.Context) error {
	sc, err := t.finish("rollback")
	if err != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	defer cancel()
	_, err = sc.ExecContext(rctx, "ROLLBACK")
	if err != nil && t.isDead() { // nothing to roll back: the statement already ended it
		err = nil
	}
	discard(sc)
	return mapError("rollback", err)
}

// discard closes sc and makes database/sql drop it instead of pooling it:
// ATTACH, PRAGMAs, TEMP objects and the like that the transaction's text set
// must not reach the next user of the handle.
func discard(sc *sql.Conn) {
	_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
	_ = sc.Close()
}

func (t *tx) isDead() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead
}

// abort rolls back without a caller context (Conn.Close).
func (t *tx) abort() { _ = t.Rollback(context.Background()) }

// finish marks the transaction ended, closes its open result sets and hands
// back its connection.
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
