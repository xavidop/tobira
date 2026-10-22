package vertica

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/xavidop/tobira/driver"
)

var (
	_ driver.Transactor = (*conn)(nil)
	_ driver.Explainer  = (*vTx)(nil)
)

var (
	errTxDone     = errors.New("transaction has already finished")
	errReadOnlyDB = errors.New("the connection is read-only")
)

// isolations are the levels Begin accepts, as START TRANSACTION spells
// them. Vertica runs READ UNCOMMITTED as READ COMMITTED and REPEATABLE
// READ as SERIALIZABLE.
var isolations = map[string]string{
	"": "", "read committed": "READ COMMITTED", "serializable": "SERIALIZABLE",
	"read uncommitted": "READ UNCOMMITTED", "repeatable read": "REPEATABLE READ",
}

// Begin starts a transaction on one session, held until Commit or
// Rollback (see session.go for why it turns autocommit off). A ReadOnly
// Tx is START TRANSACTION READ ONLY, and its statements must also pass the
// read-only allow-list.
//
// Vertica commits the open transaction before any DDL statement, so inside
// a Tx the driver refuses DDL, TRUNCATE, COPY, SET and the
// transaction-control statements (see txStatementRefusal).
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	t, err := c.begin(ctx, "begin", opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (c *conn) begin(ctx context.Context, op string, opts driver.TxOptions) (*vTx, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	iso, ok := isolations[strings.ToLower(strings.Join(strings.Fields(opts.Isolation), " "))]
	if !ok {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("unknown isolation level: Vertica has READ COMMITTED and SERIALIZABLE"))
	}
	if !opts.ReadOnly && c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyDB)
	}
	s, err := c.acquire(ctx, op, c.db)
	if err != nil {
		return nil, err
	}
	if err := s.begin(ctx, opts.ReadOnly, iso); err != nil {
		_ = s.end(false)
		s.release()
		return nil, c.scrub(mapError(ctx, op, err))
	}
	t := &vTx{c: c, s: s, readOnly: opts.ReadOnly}
	if !c.track(t) { // Close raced with Begin
		_ = s.end(false)
		s.release()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	return t, nil
}

// track registers t so Close can roll it back. It reports false when the
// connection is already closed.
func (c *conn) track(t *vTx) bool {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if c.closed.Load() {
		return false
	}
	if c.txs == nil {
		c.txs = map[*vTx]struct{}{}
	}
	c.txs[t] = struct{}{}
	return true
}

func (c *conn) untrack(t *vTx) {
	c.txMu.Lock()
	delete(c.txs, t)
	c.txMu.Unlock()
}

// abortTxs rolls back every open transaction so the pool can close.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*vTx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	for _, t := range open {
		_ = t.Rollback(context.Background())
	}
}

// vTx is an open transaction on a pinned session.
type vTx struct {
	c        *conn
	s        *session
	readOnly bool

	mu   sync.Mutex
	done bool
}

// live returns the session while the transaction is open.
func (t *vTx) live(ctx context.Context, op string) (*session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return t.s, nil
}

// prepare judges a statement for the transaction and binds its parameters.
func (t *vTx) prepare(op string, q driver.Query, ro bool) ([]any, error) {
	if q.Namespace != "" && !strings.EqualFold(q.Namespace, t.c.defaultSchema) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a transaction runs in the connection's namespace; qualify table names instead"))
	}
	if ro {
		if err := checkReadOnlyOp(op, q.Text); err != nil {
			return nil, err
		}
	}
	if err := checkShape(op, q.Text); err != nil {
		return nil, err
	}
	if err := txStatementRefusal(q.Text); err != nil {
		return nil, err
	}
	return bindArgs(op, q.Params)
}

// Query runs one statement inside the transaction. In a read-only Tx, on a
// read-only connection, or with q.ReadOnly, the statement must pass the
// read-only allow-list.
func (t *vTx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	s, err := t.live(ctx, op)
	if err != nil {
		return nil, err
	}
	args, err := t.prepare(op, q, t.readOnly || t.c.readOnly || q.ReadOnly)
	if err != nil {
		return nil, err
	}
	rs, err := s.runOn(ctx, op, q.Text, args, q.Limit, nil, nil)
	return rs, t.c.scrub(err)
}

// Explain explains q inside the transaction, like conn.Explain.
func (t *vTx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	s, err := t.live(ctx, op)
	if err != nil {
		return nil, err
	}
	ro := t.readOnly || t.c.readOnly || q.ReadOnly
	return explainOn(ctx, q, opts, ro, func(eq driver.Query) (driver.ResultSet, error) {
		args, err := t.prepare(op, eq, ro)
		if err != nil {
			return nil, err
		}
		rs, err := s.runOn(ctx, op, eq.Text, args, 0, nil, nil)
		return rs, t.c.scrub(err)
	})
}

// Commit makes the transaction's changes permanent and releases its
// session, even when COMMIT fails.
func (t *vTx) Commit(ctx context.Context) error {
	s, err := t.finish("commit")
	if err != nil {
		return err
	}
	defer s.release()
	return t.c.scrub(mapError(ctx, "commit", s.end(true)))
}

// Rollback discards the transaction. It is a no-op once the transaction
// ended.
func (t *vTx) Rollback(ctx context.Context) error {
	s, err := t.finish("rollback")
	if err != nil {
		return nil
	}
	defer s.release()
	return t.c.scrub(mapError(ctx, "rollback", s.end(false)))
}

// finish marks the transaction ended and hands its session back.
func (t *vTx) finish(op string) (*session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	t.done = true
	t.c.untrack(t)
	return t.s, nil
}
