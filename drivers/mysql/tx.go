package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

var (
	errTxDone     = errors.New("transaction has already finished")
	errTxEnded    = errors.New("a statement ended the transaction; roll back")
	errTxBusy     = errors.New("another call on this transaction is still running, or one of its result sets is open: close it first")
	errTxRunning  = errors.New("a statement is still running in this transaction: wait for it, or roll back")
	errReadOnlyTx = errors.New("the transaction is read-only")
	errReadOnlyDB = errors.New("the connection is read-only")
)

// isolationLevel maps a native isolation name to MySQL's spelling.
func isolationLevel(name string) (string, bool) {
	switch lvl := strings.ToUpper(strings.Join(strings.Fields(name), " ")); lvl {
	case "", "READ UNCOMMITTED", "READ COMMITTED", "REPEATABLE READ", "SERIALIZABLE":
		return lvl, true
	}
	return "", false
}

// Begin starts a transaction on a dedicated connection that it holds until
// Commit or Rollback; the connection is then discarded, never pooled, so
// nothing the transaction's statements did to the session reaches a later
// call.
//
// A read-only Tx runs inside SET SESSION TRANSACTION READ ONLY and START
// TRANSACTION READ ONLY, and its Query applies the read-only allow-list. A
// writable Tx refuses statements that would end it implicitly (see
// checkTxStatement). After every statement either kind proves that the
// transaction is still open; when it is not, the Tx is dead and only
// Rollback works.
//
// SingleStore has no way to prove that a statement left the transaction
// open (SET TRANSACTION inside one succeeds), so Begin is ErrUnsupported
// there; the driver's own mutations still run in a transaction.
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (_ driver.Tx, err error) {
	defer func() { err = c.named(err) }()
	if c.flavor == flavorSingleStore || c.detected == flavorSingleStore {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "begin", errNoTxSingleStore)
	}
	t, err := c.begin(ctx, "begin", opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

var errNoTxSingleStore = errors.New("transactions are not supported on SingleStore: the driver cannot prove that a statement left the transaction open")

func (c *conn) begin(ctx context.Context, op string, opts driver.TxOptions) (*myTx, error) {
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
	if opts.ReadOnly {
		if err := c.readOnlyRefusal(); err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, err)
		}
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	t := &myTx{c: c, sc: sc, readOnly: opts.ReadOnly}
	if err := t.start(ctx, lvl); err != nil {
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

// txStartStatements are the statements that open a transaction on a server
// of flavor server (see syntaxFlavor). SingleStore has no READ WRITE / READ
// ONLY characteristics and runs only READ COMMITTED, so a read-only
// transaction or an isolation level is refused there before anything is
// sent, rather than half-applied.
func txStartStatements(server flavor, readOnly bool, lvl string) ([]string, error) {
	if server == flavorSingleStore {
		switch {
		case readOnly:
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "begin", errors.New("read-only transactions are not supported on SingleStore"))
		case lvl != "":
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "begin", errors.New("isolation levels are not supported on SingleStore, which runs READ COMMITTED"))
		}
		return []string{"START TRANSACTION"}, nil
	}
	stmts := []string{"START TRANSACTION READ WRITE"}
	if readOnly {
		stmts = []string{"SET SESSION TRANSACTION READ ONLY", "START TRANSACTION READ ONLY"}
	}
	if lvl != "" {
		stmts = append([]string{"SET TRANSACTION ISOLATION LEVEL " + lvl}, stmts...)
	}
	return stmts, nil
}

// start opens the transaction on t's connection and checks the session's
// client charset, which the statement checks' lexer relies on.
func (t *myTx) start(ctx context.Context, lvl string) error {
	stmts, err := txStartStatements(t.c.syntaxFlavor(), t.readOnly, lvl)
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := t.sc.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	if err := t.sc.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&t.connID); err != nil {
		return err
	}
	_, err = t.sessionMode(ctx)
	return err
}

// killAcquireTimeout bounds how long killQuery waits for a pooled
// connection to send KILL QUERY from.
const killAcquireTimeout = 250 * time.Millisecond

// killQuery stops the statement running on the transaction's connection with
// KILL QUERY, sent from another pooled connection, and reports whether it
// was sent. The transaction stays open: only the statement is interrupted.
// When no pooled connection is free within killAcquireTimeout, or the KILL
// fails, it cancels r's statement context instead, which closes the Tx
// connection's socket (the server rolls the transaction back), and marks the
// Tx dead: reading on would be unbounded.
func (t *myTx) killQuery(r *resultSet) bool {
	err := func() error {
		actx, cancel := context.WithTimeout(context.Background(), killAcquireTimeout)
		defer cancel()
		kc, err := t.c.db.Conn(actx)
		if err != nil {
			return err
		}
		defer func() { _ = kc.Close() }()
		ectx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		_, err = kc.ExecContext(ectx, "KILL QUERY "+strconv.FormatInt(t.connID, 10))
		return err
	}()
	if err == nil {
		return true
	}
	r.cancel()
	t.mu.Lock()
	t.dead = true
	t.mu.Unlock()
	return false
}

// sessionMode reads the session's sql_mode as a lexMode, and refuses a
// client charset in which the lexer could misread a 0x5c byte, and on
// MariaDB an sql_mode outside mariaDBSafeModes.
func (t *myTx) sessionMode(ctx context.Context) (lexMode, error) {
	var sqlMode, charset string
	if err := t.sc.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode, @@SESSION.character_set_client").Scan(&sqlMode, &charset); err != nil {
		return lexMode{}, err
	}
	if !asciiSafeCharsets[strings.ToLower(charset)] {
		return lexMode{}, driver.Wrap(driver.ErrUnsupported, Name, "query",
			fmt.Errorf("client charset %q is not supported in a transaction: the statement check needs an ASCII-transparent charset", charset))
	}
	if t.c.onMariaDB() {
		if err := mariaDBModeRefusal(sqlMode); err != nil {
			return lexMode{}, driver.Wrap(driver.ErrUnsupported, Name, "query", err)
		}
	}
	return modeFromSQLMode(sqlMode), nil
}

// track registers t so Close can roll it back. It reports false when the
// connection is already closed.
func (c *conn) track(t *myTx) bool {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	if c.closed.Load() {
		return false
	}
	if c.txs == nil {
		c.txs = map[*myTx]struct{}{}
	}
	c.txs[t] = struct{}{}
	return true
}

func (c *conn) untrack(t *myTx) {
	c.txMu.Lock()
	delete(c.txs, t)
	c.txMu.Unlock()
}

// abortTxs rolls back every open transaction so the pool can close.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*myTx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	for _, t := range open {
		_ = t.Rollback(context.Background())
	}
}

// myTx is an open transaction on a dedicated connection.
type myTx struct {
	c        *conn
	sc       *sql.Conn
	readOnly bool
	connID   int64 // CONNECTION_ID() of sc, for KILL QUERY

	mu   sync.Mutex
	done bool
	dead bool       // a statement ended the transaction, or the connection broke
	call *txCall    // a mutation using sc
	open *resultSet // a statement using sc: starting, or its result set open
}

// txCall is a mutation in progress, which Rollback can cancel and wait for.
type txCall struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// claim checks that the transaction is open and that no call is using its
// connection. t.mu must be held.
func (t *myTx) claim(ctx context.Context, op string) error {
	switch {
	case t.done:
		return driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	case t.dead:
		return driver.Wrap(driver.ErrConnection, Name, op, errTxEnded)
	case t.call != nil || t.open != nil:
		return driver.Wrap(driver.ErrUnsupported, Name, op, errTxBusy)
	}
	if err := ctx.Err(); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// acquireFor claims the connection for statement r: r becomes t.open in the
// same step, so there is no moment where the connection is in use but not
// visible to Commit and Rollback. The claim ends with the result set
// (statementDone, forget) or release.
func (t *myTx) acquireFor(ctx context.Context, op string, r *resultSet) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.claim(ctx, op); err != nil {
		return err
	}
	t.open = r
	return nil
}

// writer claims the connection for a mutation, refusing a read-only Tx. It
// returns the context the mutation must run under (Rollback cancels it) and
// the function that ends the claim.
func (t *myTx) writer(ctx context.Context, op string) (*sql.Conn, context.Context, func(), error) {
	t.mu.Lock()
	if err := t.claim(ctx, op); err != nil {
		t.mu.Unlock()
		return nil, nil, nil, err
	}
	if t.readOnly {
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
// statements that would end the transaction (checkTxStatement). When the
// text holds a backslash, the check runs again with the session's sql_mode.
// A read-only per-query flag cannot be honoured inside a read-write Tx.
//
// The result set must be closed before the next statement. Closing it early
// reads the remaining rows, since the connection has to survive. After the
// statement the driver proves the transaction is still open; see
// statementDone.
func (t *myTx) Query(ctx context.Context, q driver.Query) (_ driver.ResultSet, err error) {
	defer func() { err = t.c.named(err) }()
	check := checker(checkTxStatement)
	if t.readOnly {
		check = checkReadOnly
	}
	return t.query(ctx, q, check)
}

// query is Query's implementation. check is the statement check; nil skips
// it, which only tests do, to prove the post-check behind it.
func (t *myTx) query(ctx context.Context, q driver.Query, check checker) (driver.ResultSet, error) {
	const op = "query"
	r := newResultSet(ctx, t.c, t.sc, q, false)
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
		// r.qctx, not ctx: Rollback cancels it, so it interrupts the sql_mode
		// round trip of the check as it interrupts the statement.
		if err := t.check(r.qctx, q.Text, check); err != nil {
			return fail(err)
		}
	}
	args, err := toArgs(q.Params)
	if err != nil {
		return fail(err)
	}
	return r.run(q, args, nil)
}

// check runs the statement check on text, and again with the session's
// sql_mode when the text holds a backslash.
func (t *myTx) check(ctx context.Context, text string, check checker) error {
	if err := check(text, lexMode{}); err != nil {
		return err
	}
	if strings.ContainsRune(text, '\\') {
		mode, err := t.sessionMode(ctx)
		if err != nil {
			return mapError(ctx, "query", err)
		}
		if mode != (lexMode{}) {
			return check(text, mode)
		}
	}
	return nil
}

// statementDone runs when a statement's rows are closed: it releases the
// connection for the next statement and proves the transaction is still open
// (and, for a read-only Tx, that the session is still read-only). If it is
// not, or the proof cannot be made, the Tx is dead: every later call fails
// and only Rollback works.
func (t *myTx) statementDone(r *resultSet) error {
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

// forget detaches a result set abandoned because the transaction is ending.
func (t *myTx) forget(r *resultSet) {
	t.mu.Lock()
	if t.open == r {
		t.open = nil
	}
	t.mu.Unlock()
}

// proveOpen checks that the connection is still inside a transaction:
// SET TRANSACTION without GLOBAL or SESSION fails with error 1568 exactly
// then. (@@in_transaction is MariaDB's; MySQL has no such variable.) For a
// read-only Tx it also checks @@SESSION.transaction_read_only. The probe
// cannot tell the original transaction from a new one, so a statement that
// ends the transaction and starts another (COMMIT AND CHAIN, or a procedure
// doing COMMIT and START TRANSACTION) passes it; checkTxStatement refuses
// those before they run. A probe interrupted by a KILL QUERY that arrived
// after its statement had finished (error 1317) is retried, up to three
// tries while ctx allows.
func (t *myTx) proveOpen(ctx context.Context) error {
	kind := driver.ErrUnsupported
	probe := "SET TRANSACTION READ WRITE"
	if t.readOnly {
		kind, probe = driver.ErrReadOnly, "SET TRANSACTION READ ONLY"
	}
	var err error
	var me *gomysql.MySQLError
	for try := 0; try < 3; try++ {
		_, err = t.sc.ExecContext(ctx, probe)
		if !errors.As(err, &me) || me.Number != mysqlQueryInterrupted || ctx.Err() != nil {
			break
		}
	}
	switch {
	case err == nil:
		return driver.Wrap(kind, Name, "query", errTxEnded)
	case !errors.As(err, &me) || me.Number != errCantChangeTx:
		return driver.Wrap(driver.ErrConnection, Name, "query", fmt.Errorf("cannot verify that the transaction is still open: %w; roll back", err))
	}
	if !t.readOnly {
		return nil
	}
	var ro int64
	switch err := t.sc.QueryRowContext(ctx, sessionCheckSQL).Scan(&ro); {
	case err != nil:
		return driver.Wrap(driver.ErrConnection, Name, "query", fmt.Errorf("cannot verify that the session is still read-only: %w; roll back", err))
	case ro != 1:
		return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("a statement switched the session to read-write; roll back"))
	}
	return nil
}

// Commit makes the transaction's changes permanent. A result set still
// open is first closed the way an early Close does (read at most
// txDrainRows rows or txDrainTime, then KILL QUERY, then the post-check), so
// Commit never reads a huge result to its end and does not lose the
// transaction. If that leaves the Tx broken, or it already was, Commit rolls
// back and says why. The connection is discarded either way.
func (t *myTx) Commit(ctx context.Context) (err error) {
	defer func() { err = t.c.named(err) }()
	sc, why, err := t.finish("commit", false)
	if err != nil {
		return err
	}
	defer closeTxConn(sc)
	if t.isDead() {
		endTx(ctx, sc, "ROLLBACK")
		if why == nil {
			why = errTxEnded
		}
		return driver.Wrap(driver.ErrConnection, Name, "commit", fmt.Errorf("the transaction was rolled back: %w", why))
	}
	if _, err := sc.ExecContext(ctx, "COMMIT"); err != nil {
		endTx(ctx, sc, "ROLLBACK")
		return mapError(ctx, "commit", err)
	}
	return nil
}

// Rollback discards the transaction. It is a no-op once the transaction
// ended. The connection is discarded; a failed ROLLBACK is not reported,
// since the server rolls back when the connection closes. With a result set
// still open, the connection is closed at once instead of reading the rest.
func (t *myTx) Rollback(ctx context.Context) (err error) {
	defer func() { err = t.c.named(err) }()
	sc, abandoned, err := t.finish("rollback", true)
	if err != nil {
		return nil
	}
	if abandoned == nil { // the socket is still open: roll back now, releasing locks before returning
		endTx(ctx, sc, "ROLLBACK")
	}
	closeTxConn(sc)
	return nil
}

// endTx runs COMMIT or ROLLBACK on sc with a bounded context that outlives a
// cancelled caller.
func endTx(ctx context.Context, sc *sql.Conn, stmt string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_, _ = sc.ExecContext(rctx, stmt)
}

// closeTxConn discards a transaction's connection instead of pooling it.
func closeTxConn(sc *sql.Conn) {
	discardConn(sc)
	_ = sc.Close()
}

func (t *myTx) isDead() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead
}

// errAbandoned marks a statement or result set abandoned by Rollback or
// Close: its connection's socket was closed.
var errAbandoned = errors.New("a statement or result set was still open: its connection was closed")

// finish marks the transaction ended and hands back the connection, after
// dealing with a result set still open:
//
//   - abandon (Rollback, Close): it is not read; its statement context is
//     cancelled, which closes the connection's socket (the server rolls the
//     transaction back), and the returned error is errAbandoned.
//   - otherwise (Commit): it is closed like an early Close, bounded, and the
//     returned error is the post-check's failure, if any. A reader holding
//     the result set past txDrainTime gets its statement killed first.
//
// Commit refuses (ErrUnsupported) while a mutation runs or a statement is
// still starting; Rollback cancels it, which closes the socket, and waits for
// it to stop.
func (t *myTx) finish(op string, abandon bool) (sc *sql.Conn, closed error, err error) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return nil, nil, driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	open, call := t.open, t.call
	if !abandon && (call != nil || open != nil && open.opening.Load()) {
		t.mu.Unlock()
		return nil, nil, driver.Wrap(driver.ErrUnsupported, Name, op, errTxRunning)
	}
	t.done = true
	t.mu.Unlock()
	if call != nil {
		call.cancel()
		<-call.done
		closed = errAbandoned
	}
	switch {
	case open == nil:
	case abandon:
		open.cancel() // unblocks a reader waiting on the socket
		open.mu.Lock()
		if !open.done { // it may have just ended on its own
			closed = errAbandoned
		}
		open.releaseMode(releaseAbandon)
		open.mu.Unlock()
	default:
		fired := make(chan struct{})
		timer := time.AfterFunc(txDrainTime, func() { // a reader is blocked holding the lock
			defer close(fired)
			if !open.releasing.Load() { // its own bounded close kills the statement itself
				t.killQuery(open)
			}
		})
		open.mu.Lock()
		if !timer.Stop() {
			<-fired
		}
		open.releaseMode(releaseBounded)
		closed, open.endErr = open.endErr, nil
		open.mu.Unlock()
	}
	t.c.untrack(t)
	return t.sc, closed, nil
}
