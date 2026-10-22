package snowflake

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"github.com/xavidop/tobira/driver"
)

var (
	_ driver.Transactor = (*conn)(nil)
	_ driver.Explainer  = (*sfTx)(nil)
)

var (
	errTxDone     = errors.New("transaction has already finished")
	errReadOnlyDB = errors.New("the connection is read-only")
)

// Begin starts a transaction on one session, held until Commit or Rollback.
// Snowflake runs transactions at READ COMMITTED, the only isolation it has
// ("" or "read committed" are accepted; anything else is ErrUnsupported).
// It has no read-only transactions: a ReadOnly Tx is enforced by the
// driver, which judges every statement with the read-only allow-list.
//
// Snowflake commits the open transaction before any DDL statement, so
// inside a Tx the driver refuses DDL, TRUNCATE, CALL, EXECUTE IMMEDIATE,
// USE and the transaction-control statements (see txStatementRefusal).
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	t, err := c.begin(ctx, "begin", opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (c *conn) begin(ctx context.Context, op string, opts driver.TxOptions) (*sfTx, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if iso := strings.ToLower(strings.Join(strings.Fields(opts.Isolation), " ")); iso != "" && iso != "read committed" {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("transactions are READ COMMITTED only in Snowflake"))
	}
	if !opts.ReadOnly && c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyDB)
	}
	// gosnowflake keeps the BeginTx context for COMMIT and ROLLBACK, and
	// database/sql rolls back when it ends, so the Tx runs under a context
	// of its own: ctx bounds only the BEGIN itself.
	tctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	tx, err := c.db.BeginTx(tctx, nil)
	if !stop() {
		if err == nil {
			_ = tx.Rollback()
		}
		cancel()
		return nil, mapError(ctx, op, ctx.Err())
	}
	if err != nil {
		cancel()
		return nil, c.scrub(mapError(ctx, op, err))
	}
	t := &sfTx{c: c, tx: tx, readOnly: opts.ReadOnly, cancel: cancel}
	if !c.track(t) { // Close raced with Begin
		_ = tx.Rollback()
		cancel()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	return t, nil
}

// track registers t so Close can roll it back. It reports false when the
// connection is already closed.
func (c *conn) track(t *sfTx) bool {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if c.closed.Load() {
		return false
	}
	if c.txs == nil {
		c.txs = map[*sfTx]struct{}{}
	}
	c.txs[t] = struct{}{}
	return true
}

func (c *conn) untrack(t *sfTx) {
	c.txMu.Lock()
	delete(c.txs, t)
	c.txMu.Unlock()
}

// abortTxs rolls back every open transaction so the pool can close.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*sfTx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	for _, t := range open {
		_ = t.Rollback(context.Background())
	}
}

// sfTx is an open transaction.
type sfTx struct {
	c        *conn
	tx       *sql.Tx
	readOnly bool
	cancel   context.CancelFunc // ends the Tx's own context

	mu   sync.Mutex
	done bool
}

// live returns the transaction while it is open.
func (t *sfTx) live(ctx context.Context, op string) (*sql.Tx, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return t.tx, nil
}

// prepare judges a statement for the transaction and binds its parameters.
func (t *sfTx) prepare(op string, q driver.Query, ro bool) ([]any, error) {
	if q.Namespace != "" && q.Namespace != t.c.defaultNamespace() {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a transaction runs in the connection's namespace; qualify table names instead"))
	}
	if ro {
		if err := checkReadOnlyOp(op, q.Text); err != nil {
			return nil, err
		}
	}
	if err := txStatementRefusal(q.Text); err != nil {
		return nil, err
	}
	return bindArgs(op, q.Params)
}

// Query runs one statement inside the transaction. In a read-only Tx, on a
// read-only connection, or with q.ReadOnly, the statement must pass the
// read-only allow-list.
func (t *sfTx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	tx, err := t.live(ctx, op)
	if err != nil {
		return nil, err
	}
	args, err := t.prepare(op, q, t.readOnly || t.c.readOnly || q.ReadOnly)
	if err != nil {
		return nil, err
	}
	rs, err := runOn(ctx, op, tx, q.Text, args, q.Limit)
	return rs, t.c.scrub(err)
}

// Explain explains q inside the transaction, like conn.Explain.
func (t *sfTx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	tx, err := t.live(ctx, op)
	if err != nil {
		return nil, err
	}
	ro := t.readOnly || t.c.readOnly || q.ReadOnly
	return explainOn(ctx, q, opts, ro, func(eq driver.Query) (driver.ResultSet, error) {
		args, err := t.prepare(op, eq, false)
		if err != nil {
			return nil, err
		}
		rs, err := runOn(ctx, op, tx, eq.Text, args, 0)
		return rs, t.c.scrub(err)
	})
}

// Commit makes the transaction's changes permanent and releases its
// session, even when COMMIT fails. ctx ending cancels the COMMIT request.
func (t *sfTx) Commit(ctx context.Context) error {
	tx, err := t.finish("commit")
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, t.cancel)
	defer stop()
	defer t.cancel()
	return t.c.scrub(mapError(ctx, "commit", tx.Commit()))
}

// Rollback discards the transaction. It is a no-op once the transaction
// ended.
func (t *sfTx) Rollback(ctx context.Context) error {
	tx, err := t.finish("rollback")
	if err != nil {
		return nil
	}
	stop := context.AfterFunc(ctx, t.cancel)
	defer stop()
	defer t.cancel()
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return t.c.scrub(mapError(ctx, "rollback", err))
	}
	return nil
}

// finish marks the transaction ended and hands it back.
func (t *sfTx) finish(op string) (*sql.Tx, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	t.done = true
	t.c.untrack(t)
	return t.tx, nil
}
