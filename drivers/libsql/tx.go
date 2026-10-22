package libsql

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

const (
	// endTimeout bounds the ROLLBACK that ends a transaction whose ctx is
	// gone.
	endTimeout = 5 * time.Second
	// keepAliveEvery is how often an idle transaction pings its stream:
	// sqld and Turso expire a Hrana-over-HTTP stream after about 10 s
	// without a request, which would roll the transaction back.
	keepAliveEvery   = 4 * time.Second
	keepAliveTimeout = 10 * time.Second
)

var (
	errTxDone  = errors.New("transaction has already finished")
	errTxEnded = errors.New("a statement ended the transaction; roll back")
)

// Begin starts a transaction on its own Hrana stream. A read-write one takes
// the write lock up front (BEGIN IMMEDIATE). A read-only one applies the
// read-only allow-list to every statement and never commits: Commit rolls
// it back. While the transaction is open a goroutine keeps the stream alive.
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
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("libsql transactions are serializable; isolation "+opts.Isolation+" is not supported"))
	}
	if !opts.ReadOnly && c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyMutation)
	}
	begin := "BEGIN IMMEDIATE"
	if opts.ReadOnly {
		begin = "BEGIN"
	}
	s := &stream{c: c.client}
	res, err := s.send(ctx, executeReq(hranaStmt{SQL: begin}))
	if err == nil {
		_, err = executeResult(res[0])
	}
	if err != nil {
		closeStream(ctx, s)
		return nil, mapError(op, err)
	}
	t := &tx{c: c, s: s, readOnly: opts.ReadOnly, stop: make(chan struct{}), stopped: make(chan struct{})}
	t.keepCtx, t.keepCancel = context.WithCancel(context.Background())
	go t.keepAlive()
	if !c.track(t) { // Close raced with Begin
		t.abort()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	return t, nil
}

// closeStream closes s within endTimeout, whatever ctx's state.
func closeStream(ctx context.Context, s *stream) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	defer cancel()
	s.close(cctx)
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

// abortTxs rolls back every open transaction, concurrently and within
// endTimeout in total, so a dead server cannot hold Close up for long.
// Close calls it.
func (c *conn) abortTxs() {
	c.txMu.Lock()
	open := make([]*tx, 0, len(c.txs))
	for t := range c.txs {
		open = append(open, t)
	}
	c.txMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), endTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, t := range open {
		wg.Go(func() { t.abortWithin(ctx) })
	}
	wg.Wait()
}

// tx is an open transaction on its own stream.
type tx struct {
	c        *conn
	s        *stream
	readOnly bool

	stop    chan struct{} // closed by finish: the keep-alive goroutine exits
	stopped chan struct{} // closed by the keep-alive goroutine on exit
	// keepCtx bounds the keep-alive requests; abort cancels it so a ping
	// stuck on a dead server cannot hold Close up.
	keepCtx    context.Context
	keepCancel context.CancelFunc

	mu   sync.Mutex
	done bool
	dead bool // a statement ended the transaction
}

func (t *tx) keepAlive() {
	defer close(t.stopped)
	tick := time.NewTicker(keepAliveEvery)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			ctx, cancel := context.WithTimeout(t.keepCtx, keepAliveTimeout)
			t.s.keepAlive(ctx)
			cancel()
		}
	}
}

// live fails once the transaction finished or died.
func (t *tx) live(op string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	if t.dead {
		return driver.Wrap(driver.ErrConnection, Name, op, errTxEnded)
	}
	return nil
}

func (t *tx) endedKind() error {
	if t.readOnly {
		return driver.ErrReadOnly
	}
	return driver.ErrUnsupported
}

// run executes stmts on the transaction's stream in one batch, behind a
// fail-closed liveness probe: step 0 is BEGIN, which fails with "within a
// transaction" exactly while the transaction is open. The statements run
// only when it failed (each further one only when the one before it
// succeeded); if it succeeded, a statement ended the transaction behind the
// driver's back, so nothing runs, the probe is rolled back and the Tx is
// dead. If BEGIN fails for any other reason, the statements have already
// run: inside the transaction if one was open (closing the stream rolls
// them back), but in autocommit, so already committed, if none was. A
// deferred BEGIN fails only "within a transaction", so this is not expected
// in practice; Hrana v3's is_autocommit condition would close the gap, but
// this client speaks v2. Either way the Tx is marked dead and the error
// returned, so nothing runs after them. With track (one statement only) the statement is bracketed by
// total_changes() and the change count is returned.
func (t *tx) run(ctx context.Context, op string, stmts []hranaStmt, track bool) ([]*stmtResult, int64, error) {
	if err := t.live(op); err != nil {
		return nil, -1, err
	}
	if err := ctx.Err(); err != nil {
		return nil, -1, mapError(op, err)
	}
	steps := []hranaStep{{Stmt: hranaStmt{SQL: "BEGIN"}}}
	if track && len(stmts) == 1 {
		steps = append(steps, changeSteps(stmts[0], 0)...)
	} else {
		track = false
		for i, s := range stmts {
			cond := condOK(i)
			if i == 0 {
				cond = condError(0)
			}
			steps = append(steps, hranaStep{Condition: cond, Stmt: s})
		}
	}
	steps = append(steps, hranaStep{Condition: condOK(0), Stmt: hranaStmt{SQL: "ROLLBACK"}})
	res, err := t.s.send(ctx, batchReq(steps...))
	if err != nil {
		return nil, -1, mapError(op, err)
	}
	b, err := batchOutcome(res[0], len(steps))
	if err != nil {
		return nil, -1, mapError(op, err)
	}
	if _, probeErr := b.step(0); probeErr == nil || !strings.Contains(probeErr.Error(), "within a transaction") {
		t.mu.Lock()
		t.dead = true
		t.mu.Unlock()
		if probeErr != nil {
			return nil, -1, mapError(op, probeErr)
		}
		return nil, -1, driver.Wrap(t.endedKind(), Name, op, errTxEnded)
	}
	if track {
		r, n, err := changeResult(b, 1)
		if err != nil {
			return nil, -1, mapError(op, err)
		}
		return []*stmtResult{r}, n, nil
	}
	out := make([]*stmtResult, len(stmts))
	for i := range stmts {
		r, err := b.step(i + 1)
		if err != nil {
			return nil, -1, mapError(op, err)
		}
		if r == nil {
			return nil, -1, driver.Wrap(driver.ErrConnection, Name, op, errNotExecuted)
		}
		out[i] = r
	}
	return out, -1, nil
}

// execAll makes the transaction an execer for the catalog, so Describe in a
// transaction sees the tables it created.
func (t *tx) execAll(ctx context.Context, stmts ...hranaStmt) ([]*stmtResult, error) {
	out, _, err := t.run(ctx, "describe", stmts, false)
	return out, err
}

// Query runs one statement in the transaction. Transaction-control
// statements are refused; the single-statement check and, for a read-only
// transaction, the read-only allow-list apply. A per-query ReadOnly flag
// cannot be honoured inside a read-write transaction.
func (t *tx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	start := time.Now()
	if err := t.live(op); err != nil {
		return nil, err
	}
	if isTxControl(q.Text) {
		return nil, driver.Wrap(t.endedKind(), Name, op, errors.New("transaction control statements are not allowed inside a transaction: use Commit and Rollback"))
	}
	if q.ReadOnly && !t.readOnly {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a read-only query cannot run inside a read-write transaction: begin a read-only one"))
	}
	if t.readOnly {
		if err := readOnlyGuard(q.Text); err != nil {
			return nil, err
		}
	}
	if err := checkSingle(q.Text); err != nil {
		return nil, err
	}
	s, err := statement(op, q.Text, q.Params, true)
	if err != nil {
		return nil, err
	}
	res, affected, err := t.run(ctx, op, []hranaStmt{s}, !t.readOnly && !returnsRows(q.Text))
	if err != nil {
		return nil, err
	}
	return resultSet(op, res[0], q.Limit, affected, start)
}

// Commit makes the transaction's changes permanent; a read-only transaction
// is rolled back instead. The stream is closed in the same request, so a
// failed COMMIT rolls back.
func (t *tx) Commit(ctx context.Context) error {
	const op = "commit"
	if err := t.live(op); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return mapError(op, err)
	}
	if err := t.finish(op); err != nil {
		return err
	}
	end := "COMMIT"
	if t.readOnly {
		end = "ROLLBACK"
	}
	steps := []hranaStep{
		{Stmt: hranaStmt{SQL: "BEGIN"}},
		{Condition: condError(0), Stmt: hranaStmt{SQL: end}},
		{Condition: condOK(0), Stmt: hranaStmt{SQL: "ROLLBACK"}},
	}
	res, err := t.s.send(ctx, batchReq(steps...), closeReq())
	if err != nil {
		closeStream(ctx, t.s)
		return mapError(op, err)
	}
	b, err := batchOutcome(res[0], len(steps))
	if err != nil {
		return mapError(op, err)
	}
	if _, probeErr := b.step(0); probeErr == nil {
		return driver.Wrap(driver.ErrConnection, Name, op, errTxEnded)
	}
	r, err := b.step(1)
	if err != nil {
		return mapError(op, err)
	}
	if r == nil {
		return driver.Wrap(driver.ErrConnection, Name, op, errNotExecuted)
	}
	return nil
}

// Rollback discards the transaction. It is a no-op once the transaction
// ended.
func (t *tx) Rollback(ctx context.Context) error {
	if err := t.finish("rollback"); err != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	defer cancel()
	res, err := t.s.send(rctx, executeReq(hranaStmt{SQL: "ROLLBACK"}), closeReq())
	if err != nil {
		if errors.Is(err, errStreamClosed) {
			return nil // the server dropped the stream, and the transaction with it
		}
		closeStream(ctx, t.s)
		return mapError("rollback", err)
	}
	if _, err := executeResult(res[0]); err != nil && !t.isDead() && !strings.Contains(err.Error(), "no transaction is active") {
		return mapError("rollback", err)
	}
	return nil
}

func (t *tx) isDead() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead
}

// abort rolls back within ctx (Conn.Close), first cancelling a keep-alive
// ping in flight.
func (t *tx) abortWithin(ctx context.Context) {
	t.keepCancel()
	_ = t.Rollback(ctx)
}

// abort rolls back without a caller context (a Close that raced Begin).
func (t *tx) abort() {
	ctx, cancel := context.WithTimeout(context.Background(), endTimeout)
	defer cancel()
	t.abortWithin(ctx)
}

// finish marks the transaction ended, stops the keep-alive goroutine and
// forgets the transaction.
func (t *tx) finish(op string) error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return driver.Wrap(driver.ErrConnection, Name, op, errTxDone)
	}
	t.done = true
	t.mu.Unlock()
	close(t.stop)
	<-t.stopped
	t.keepCancel()
	t.c.untrack(t)
	return nil
}
