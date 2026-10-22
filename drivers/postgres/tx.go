package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

var (
	errTxDone     = errors.New("transaction has already finished")
	errTxEnded    = errors.New("a statement ended the transaction; roll back")
	errReadOnlyTx = errors.New("the transaction is read-only")
	errReadOnlyDB = errors.New("the connection is read-only")
)

// txScope names a caller-owned transaction a statement runs inside.
type txScope struct {
	tx       pgx.Tx
	readOnly bool
	onEscape func() // called when a statement ended tx; kills the Tx
}

// isolationLevel maps a native isolation name to pgx's.
func isolationLevel(name string) (pgx.TxIsoLevel, bool) {
	switch lvl := pgx.TxIsoLevel(strings.ToLower(strings.Join(strings.Fields(name), " "))); lvl {
	case "", pgx.Serializable, pgx.RepeatableRead, pgx.ReadCommitted, pgx.ReadUncommitted:
		return lvl, true
	}
	return "", false
}

// Begin starts a transaction on a pooled connection that it holds until
// Commit or Rollback; the connection is then reset like any other. A read-only
// Tx is BEGIN READ ONLY.
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (_ driver.Tx, err error) {
	defer func() { err = c.named(err) }()
	t, err := c.begin(ctx, "begin", opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (c *conn) begin(ctx context.Context, op string, opts driver.TxOptions) (*pgTx, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if !c.flavor.has(driver.CapTransactor) { // Mutator calls come through here too
		return nil, c.flavor.unsupported(op, driver.CapTransactor)
	}
	lvl, ok := isolationLevel(opts.Isolation)
	if !ok {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("unknown isolation level "+opts.Isolation))
	}
	if !opts.ReadOnly && c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyDB)
	}
	txo := pgx.TxOptions{IsoLevel: lvl}
	if opts.ReadOnly {
		txo.AccessMode = pgx.ReadOnly
	}
	tx, err := c.pool.BeginTx(ctx, txo)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if opts.ReadOnly {
		// Take the first snapshot now: after it, SET TRANSACTION READ WRITE is
		// refused, so no statement can flip the transaction to read-write.
		if _, err := tx.Exec(ctx, "select 1"); err != nil {
			rollback(ctx, tx)
			return nil, mapError(ctx, op, err)
		}
	}
	t := &pgTx{c: c, tx: tx, readOnly: opts.ReadOnly}
	if !c.track(t) { // Close raced with Begin
		rollback(ctx, tx)
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	return t, nil
}

// track registers t so Close can roll it back. It reports false when the
// connection is already closed.
func (c *conn) track(t *pgTx) bool {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if c.closed.Load() {
		return false
	}
	if c.txs == nil {
		c.txs = map[*pgTx]struct{}{}
	}
	c.txs[t] = struct{}{}
	return true
}

func (c *conn) untrack(t *pgTx) {
	c.txMu.Lock()
	delete(c.txs, t)
	c.txMu.Unlock()
}

// abortTxs rolls back every open transaction so the pool can close.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*pgTx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	for _, t := range open {
		_ = t.Rollback(context.Background())
	}
}

// pgTx is an open transaction.
type pgTx struct {
	c        *conn
	tx       pgx.Tx
	readOnly bool

	mu   sync.Mutex
	done bool
	dead bool // a statement ended the transaction; the connection was closed
}

// kill discards the connection after a statement ended the transaction, so
// nothing else runs on it. Rollback then only releases the dead connection.
func (t *pgTx) kill() {
	t.mu.Lock()
	t.dead = true
	tx := t.tx
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_ = tx.Conn().Close(ctx)
}

// live returns the transaction while it is open.
func (t *pgTx) live(op string) (pgx.Tx, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	if t.dead {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxEnded)
	}
	return t.tx, nil
}

// writer returns the transaction for a mutation, refusing a read-only one.
func (t *pgTx) writer(ctx context.Context, op string) (pgx.Tx, error) {
	tx, err := t.live(op)
	if err != nil {
		return nil, err
	}
	if t.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyTx)
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return tx, nil
}

// Query runs one statement inside the transaction. Transaction-control
// statements are refused: a COMMIT or BEGIN from query text would end the
// transaction behind the driver's back and leave later statements outside it
// (and, for a read-only Tx, outside the READ ONLY guard). A read-only
// per-query flag cannot be honoured inside a read-write Tx.
func (t *pgTx) Query(ctx context.Context, q driver.Query) (_ driver.ResultSet, err error) {
	defer func() { err = t.c.named(err) }()
	tx, err := t.live("query")
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
	return t.c.query(ctx, q, false, &txScope{tx: tx, readOnly: t.readOnly, onEscape: t.kill})
}

// isTxControl reports whether text starts a statement that ends or nests
// transactions.
func isTxControl(text string) bool {
	switch leadingKeyword(text) {
	case "COMMIT", "END", "ABORT", "BEGIN", "START":
		return true
	case "ROLLBACK":
		return !rollsBackToSavepoint(text)
	}
	return isPrepareTransaction(text)
}

// Commit makes the transaction's changes permanent and releases its
// connection, even when COMMIT fails.
func (t *pgTx) Commit(ctx context.Context) (err error) {
	defer func() { err = t.c.named(err) }()
	tx, err := t.finish("commit")
	if err != nil {
		return err
	}
	return mapError(ctx, "commit", tx.Commit(ctx))
}

// Rollback discards the transaction. It is a no-op once the transaction ended.
func (t *pgTx) Rollback(ctx context.Context) (err error) {
	defer func() { err = t.c.named(err) }()
	tx, err := t.finish("rollback")
	if err != nil {
		return nil
	}
	rollback(ctx, tx)
	return nil
}

// finish marks the transaction ended and hands it back.
func (t *pgTx) finish(op string) (pgx.Tx, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	t.done = true
	t.c.untrack(t)
	return t.tx, nil
}

// rollsBackToSavepoint reports whether text is ROLLBACK [WORK|TRANSACTION] TO
// [SAVEPOINT] name, which stays inside the transaction. Every lexing (see
// guardDialects) must agree: otherwise it counts as transaction control and
// is refused.
func rollsBackToSavepoint(text string) bool {
	for _, words := range lexings(text, false) {
		if !rollbackTo(words) {
			return false
		}
	}
	return true
}

func rollbackTo(words []sqlWord) bool {
	i := 0
	for i < len(words) && words[i].text == ";" { // leading empty statements
		i++
	}
	if i >= len(words) || words[i].text != "ROLLBACK" {
		return false
	}
	i++
	if i < len(words) && (words[i].text == "WORK" || words[i].text == "TRANSACTION") {
		i++
	}
	return i < len(words) && words[i].text == "TO"
}
