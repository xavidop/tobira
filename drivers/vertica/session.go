package vertica

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"strings"
	"sync"
	"time"

	vertigo "github.com/vertica/vertica-sql-go"
)

// Two vertica-sql-go behaviours shape how statements run:
//
//   - With autocommit on (the SDK's default, and what tobira's plain
//     queries need), START TRANSACTION does not open a transaction: every
//     statement still commits on its own, READ ONLY included. A
//     transaction therefore pins one session, turns autocommit off, starts
//     the transaction, and turns autocommit back on when it ends; a
//     session whose restore fails is discarded.
//   - Its own cancellation (a goroutine that sends a cancel request when
//     ctx ends) writes into the buffer the reading goroutine uses: a data
//     race. So the SDK never sees a cancellable context. Each statement
//     runs on a pinned session in a goroutine; when ctx ends, the driver
//     closes that session on the server (CLOSE_SESSION, from a separate
//     short-lived session), which ends the statement, and the session is
//     discarded.

// sessionConn is a vertica-sql-go session that knows its server session
// id, for CLOSE_SESSION.
type sessionConn struct {
	sqldriver.Conn
	id string
}

func (s *sessionConn) PrepareContext(ctx context.Context, query string) (sqldriver.Stmt, error) {
	return s.Conn.(sqldriver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (s *sessionConn) BeginTx(context.Context, sqldriver.TxOptions) (sqldriver.Tx, error) {
	// Transactions go through session.begin: the SDK's own ignores them
	// under autocommit. database/sql never reaches here from this driver.
	return nil, errors.New("vertica: database/sql transactions are not used")
}

func (s *sessionConn) Ping(ctx context.Context) error {
	return s.Conn.(sqldriver.Pinger).Ping(ctx)
}

func (s *sessionConn) ResetSession(ctx context.Context) error {
	return s.Conn.(sqldriver.SessionResetter).ResetSession(ctx)
}

// connector opens vertica-sql-go sessions for database/sql. Unlike the
// SDK's own driver it honours ctx (vertica-sql-go dials and logs in
// without one: Connect returns when ctx ends and closes the session if it
// arrives later), runs the pool's setup statements on every new session,
// and reads the session's id.
type connector struct {
	url   string
	setup []string
}

func (c connector) Driver() sqldriver.Driver { return &vertigo.Driver{} }

func (c connector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		conn sqldriver.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		cn, err := c.open()
		done <- result{cn, err}
	}()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// open logs in, runs the setup statements and reads the session id.
func (c connector) open() (sqldriver.Conn, error) {
	cn, err := (&vertigo.Driver{}).Open(c.url)
	if err != nil {
		return nil, err
	}
	for _, text := range c.setup {
		if _, err := rawQuery(cn, text, false); err != nil {
			_ = cn.Close()
			return nil, err
		}
	}
	id, err := rawQuery(cn, "SELECT session_id FROM v_monitor.current_session", true)
	if err != nil {
		_ = cn.Close()
		return nil, err
	}
	return &sessionConn{Conn: cn, id: id}, nil
}

// rawQuery runs text on a bare SDK session and returns the first column
// of its first row as text when want is set. The SDK gets no cancellable
// context (see above).
func rawQuery(cn sqldriver.Conn, text string, want bool) (string, error) {
	pc, ok := cn.(sqldriver.ConnPrepareContext)
	if !ok {
		return "", errors.New("vertica-sql-go session cannot prepare statements")
	}
	ctx := noStdin(context.Background())
	st, err := pc.PrepareContext(ctx, text)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	q, ok := st.(sqldriver.StmtQueryContext)
	if !ok {
		return "", errors.New("vertica-sql-go statement cannot query")
	}
	rows, err := q.QueryContext(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	if !want {
		return "", nil
	}
	dest := make([]sqldriver.Value, len(rows.Columns()))
	if len(dest) == 0 || rows.Next(dest) != nil {
		return "", errors.New("vertica: no session id")
	}
	s, _ := dest[0].(string)
	return s, nil
}

// interruptTimeout bounds closing a session on the server after its
// statement was cancelled; interruptWait bounds waiting for the statement
// to end after that.
const (
	interruptTimeout = 10 * time.Second
	interruptWait    = 5 * time.Second
)

// interrupt closes the server session id from a short-lived session of its
// own, which ends the statement running there. Errors are dropped: the
// caller has already returned the cancellation, and the session is
// discarded either way.
func (c *conn) interrupt(id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), interruptTimeout)
	defer cancel()
	cn, err := connector{url: c.url}.Connect(ctx)
	if err != nil {
		return
	}
	defer func() { _ = cn.Close() }()
	_, _ = rawQuery(cn, "SELECT CLOSE_SESSION('"+strings.ReplaceAll(id, "'", "''")+"')", false)
}

// errSessionGone is returned for a statement on a session already given
// up (its last statement was cancelled and did not end in time).
var errSessionGone = errors.New("the session was closed after a cancelled statement")

// session is one pinned vertica-sql-go session of a pool.
type session struct {
	c  *conn
	sc *sql.Conn
	id string

	mu       sync.Mutex
	broken   bool // discard instead of returning to the pool
	inTx     bool // autocommit is off and a transaction is open
	released bool
}

// acquire pins a session of db. ctx bounds the wait for it.
func (c *conn) acquire(ctx context.Context, op string, db *sql.DB) (*session, error) {
	sc, err := db.Conn(ctx)
	if err != nil {
		return nil, c.scrub(mapError(ctx, op, err))
	}
	s := &session{c: c, sc: sc}
	_ = sc.Raw(func(dc any) error {
		if v, ok := dc.(*sessionConn); ok {
			s.id = v.id
		}
		return nil
	})
	return s, nil
}

// do runs fn, an SDK call, with a context the SDK cannot see cancelled.
// When ctx ends first, closing the session on the server starts in the
// background (bounded by interruptTimeout), do waits at most
// interruptWait for fn to end, and returns the context error; rows fn
// returns after that are closed. So the caller has the cancellation within
// interruptWait even when the server cannot be reached.
func (s *session) do(ctx context.Context, fn func(context.Context) (*sql.Rows, error)) (*sql.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	gone := s.released
	s.mu.Unlock()
	if gone {
		return nil, errSessionGone
	}
	type result struct {
		rows *sql.Rows
		err  error
	}
	done := make(chan result, 1)
	go func() {
		rows, err := fn(noStdin(context.WithoutCancel(ctx)))
		done <- result{rows, err}
	}()
	select {
	case r := <-done:
		return r.rows, r.err
	case <-ctx.Done():
	}
	s.mu.Lock()
	s.broken = true
	s.mu.Unlock()
	go s.c.interrupt(s.id) // its login may take up to interruptTimeout; the caller waits interruptWait at most
	timer := time.NewTimer(interruptWait)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.rows != nil {
			_ = r.rows.Close()
		}
	case <-timer.C:
		// Still running: the session is given up here, and discarded in the
		// background once the statement ends (sql.Conn serialises its use,
		// so releasing it now would wait for the statement).
		s.mu.Lock()
		s.released, s.inTx = true, false
		s.mu.Unlock()
		go func() {
			if r := <-done; r.rows != nil {
				_ = r.rows.Close()
			}
			_ = s.sc.Raw(func(any) error { return sqldriver.ErrBadConn })
			_ = s.sc.Close()
		}()
	}
	return nil, ctx.Err()
}

// query runs a statement and returns its rows (fully read by the SDK).
func (s *session) query(ctx context.Context, text string, args ...any) (*sql.Rows, error) {
	return s.do(ctx, func(sctx context.Context) (*sql.Rows, error) {
		return s.sc.QueryContext(sctx, text, args...)
	})
}

// exec runs a statement and returns its row count: Vertica answers DML
// with one row holding it, and anything else with no rows (count -1).
func (s *session) exec(ctx context.Context, text string, args ...any) (int64, error) {
	var n int64 = -1
	_, err := s.do(ctx, func(sctx context.Context) (*sql.Rows, error) {
		res, err := s.sc.ExecContext(sctx, text, args...)
		if err == nil {
			if m, rerr := res.RowsAffected(); rerr == nil {
				n = m
			}
		}
		return nil, err
	})
	return n, err
}

// queryRow runs a statement and scans its first row into dest.
func (s *session) queryRow(ctx context.Context, text string, args []any, dest ...any) error {
	rows, err := s.query(ctx, text, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return rows.Scan(dest...)
}

// endTimeout bounds the statements that end a transaction.
const endTimeout = 30 * time.Second

// begin turns autocommit off and starts a transaction: READ ONLY or READ
// WRITE, at isolation iso ("" for the default).
func (s *session) begin(ctx context.Context, readOnly bool, iso string) error {
	if _, err := s.exec(ctx, "SET SESSION AUTOCOMMIT TO OFF"); err != nil {
		s.markBroken()
		return err
	}
	s.mu.Lock()
	s.inTx = true
	s.mu.Unlock()
	// With autocommit just turned off the session may already be inside an
	// implicit transaction, and START TRANSACTION would then keep it,
	// READ WRITE: end it first.
	if _, err := s.exec(ctx, "ROLLBACK"); err != nil {
		return err
	}
	text := "START TRANSACTION"
	if iso != "" {
		text += " ISOLATION LEVEL " + iso
	}
	if readOnly {
		text += " READ ONLY"
	} else {
		text += " READ WRITE"
	}
	_, err := s.exec(ctx, text)
	return err
}

// end commits or rolls back the open transaction and turns autocommit back
// on. It runs on a context of its own, so a cancelled caller still leaves
// the session clean; a session it cannot restore is discarded.
func (s *session) end(commit bool) error {
	s.mu.Lock()
	open := s.inTx
	s.inTx = false
	s.mu.Unlock()
	if !open {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), endTimeout)
	defer cancel()
	stmt := "ROLLBACK"
	if commit {
		stmt = "COMMIT"
	}
	_, err := s.exec(ctx, stmt)
	if _, rerr := s.exec(ctx, "SET SESSION AUTOCOMMIT TO ON"); rerr != nil {
		s.markBroken()
	}
	if err != nil && !commit {
		s.markBroken()
	}
	return err
}

func (s *session) markBroken() {
	s.mu.Lock()
	s.broken = true
	s.mu.Unlock()
}

// release returns the session to its pool, or discards it when it is
// broken or still inside a transaction. It is idempotent.
func (s *session) release() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	discard := s.broken || s.inTx
	s.mu.Unlock()
	if discard {
		_ = s.sc.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
	_ = s.sc.Close()
}
