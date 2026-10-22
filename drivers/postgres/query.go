package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

const (
	cursorName      = "tobira_cur"
	savepointPrefix = "tobira_sp_"
	// fetchBatch caps the rows pulled from the server cursor per round trip.
	fetchBatch = 500
	// typeLookupTimeout bounds waiting for a spare connection to name a type.
	typeLookupTimeout = time.Second
)

// Query runs one statement. Which path it takes decides how the guarantees
// hold:
//
//   - A read-only call (conn opened read-only, or q.ReadOnly) runs in
//     BEGIN READ ONLY and always ends in ROLLBACK, so the query text cannot
//     lift the guard: it only ever sees that one transaction.
//   - A limited call reads through a server-side cursor (DECLARE … FETCH
//     FORWARD), so the server never produces more than Limit+1 rows.
//   - A namespace needs a transaction so SET LOCAL-style search_path cannot
//     leak to the next user of the pooled connection.
//   - Anything else is a plain pool.Query, streaming.
//
// User text always travels through the extended protocol (see
// forceExtendedProtocol), which rejects multi-command strings with SQLSTATE
// 42601, so exactly one statement runs. pgx's Exec with no arguments falls
// back to the simple protocol, so Exec is never used for user text.
func (c *conn) Query(ctx context.Context, q driver.Query) (_ driver.ResultSet, err error) {
	defer func() { err = c.named(err) }()
	return c.query(ctx, q, false, nil)
}

// query is Query's implementation. discard makes a read-write call end in
// ROLLBACK instead of COMMIT, so nothing the statement did persists (EXPLAIN
// ANALYZE runs the statement it explains). It always uses a transaction.
//
// scope is non-nil for a statement inside a caller-owned transaction (Tx.Query):
// the result set then runs on that transaction and never ends it.
func (c *conn) query(ctx context.Context, q driver.Query, discard bool, scope *txScope) (driver.ResultSet, error) {
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	args := make([]any, len(q.Params))
	for i, p := range q.Params {
		a, err := paramToPG(p)
		if err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", fmt.Errorf("parameter %d: %w", i+1, err))
		}
		args[i] = a
	}
	kw := leadingKeyword(q.Text)
	ro := c.readOnly || q.ReadOnly
	if scope != nil {
		ro = scope.readOnly
	}
	if ro && isPrepareTransaction(q.Text) {
		// PREPARE TRANSACTION would detach our READ ONLY transaction from the
		// session and leave it behind on the server.
		return nil, driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("PREPARE TRANSACTION is not allowed on a read-only connection"))
	}
	if ro && endsWithChain(q.Text) {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("AND CHAIN is not allowed on a read-only path"))
	}
	if ro {
		if err := serverAccessRefusal("query", q.Text); err != nil {
			return nil, err
		}
		if err := flavorRefusal(c.flavor, "query", q.Text); err != nil {
			return nil, err
		}
	}
	if scope != nil && scope.readOnly && (isSelectLike(kw) || kw == "WITH") && hasTopLevelInto(q.Text) {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("SELECT INTO creates a table and is not allowed in a read-only transaction"))
	}
	if scope != nil && txEnded(scope.tx) {
		scope.onEscape()
		return nil, escapeError(ro)
	}
	r := &resultSet{c: c, ctx: ctx, start: time.Now(), limit: max(q.Limit, 0), ro: ro, discard: discard}
	if scope != nil {
		r.tx, r.owned, r.scope = scope.tx, false, scope
	}
	r.qctx, r.cancel = context.WithCancel(ctx)
	// Closing a half-read SELECT by draining could mean gigabytes; cancelling
	// it is safe because a read-only transaction or a plain SELECT has nothing
	// to lose. Other statements (data-modifying CTEs) are always drained.
	r.abortOnClose = scope == nil && (r.ro || isSelectLike(kw))

	var err error
	if scope == nil && !r.ro && !r.discard && r.limit == 0 && q.Namespace == "" {
		err = r.openPlain(q.Text, args)
	} else {
		err = r.openTx(q, args, kw)
	}
	if err == nil && len(r.fds) == 0 {
		return r.finishWithoutColumns(ctx)
	}
	if err != nil {
		r.release(false)
		return nil, r.mapErr(ctx, err)
	}
	return r, nil
}

// mapErr maps err like mapError. SQLSTATE 25001 (active_sql_transaction) in
// our READ ONLY transaction whose message is the "read-write mode must be set
// before any query" family means the text tried to leave read-only mode (SET
// TRANSACTION READ WRITE after the first snapshot), so it is ErrReadOnly. Any
// other 25001 (VACUUM inside a transaction block, SET TRANSACTION ISOLATION
// LEVEL) gets no kind. On a server with translated messages the routine name
// still identifies the error.
func (r *resultSet) mapErr(ctx context.Context, err error) error {
	out := mapError(ctx, "query", err)
	var de *driver.Error
	var pe *pgconn.PgError
	if r.ro && errors.As(out, &de) && errors.As(out, &pe) && pe.Code == "25001" && readWriteModeError(pe) {
		de.Kind = driver.ErrReadOnly
	}
	return out
}

// readWriteModeError reports whether pe is the server's complaint about
// switching a transaction to read-write mode. It matches the English message
// and, for translated servers, the check hook that raises it
// (call_bool_check_hook), except the DEFERRABLE variant which is a different
// complaint.
func readWriteModeError(pe *pgconn.PgError) bool {
	msg := strings.ToLower(pe.Message)
	if strings.Contains(msg, "read-write mode") {
		return true
	}
	return pe.Routine == "call_bool_check_hook" && !strings.Contains(msg, "deferrable")
}

// skipBlank returns the index of the first byte in text[i:] that is not
// whitespace or part of a comment (block comments nest in PostgreSQL). The
// lexing is driverutil/sqlscan's, shared with the other SQL drivers and with
// completion; comments and whitespace read the same under both settings of
// standard_conforming_strings.
func skipBlank(text string, i int) int {
	end := len(text)
	sqlscan.Scan(sqlscan.Postgres(true), text[i:], func(t sqlscan.Token) bool {
		switch {
		case t.Kind == sqlscan.Space, t.Kind == sqlscan.Comment:
			return true
		case t.Kind == sqlscan.Unterminated && text[i+t.Start] == '/': // a comment to the end
			return true
		}
		end = i + t.Start
		return false
	})
	return end
}

// skipEmpty is skipBlank that also skips empty statements (stray ';'), which
// PostgreSQL accepts before the real statement in a single-statement string.
func skipEmpty(text string, i int) int {
	for {
		i = skipBlank(text, i)
		if i >= len(text) || text[i] != ';' {
			return i
		}
		i++
	}
}

// leadingKeyword returns the upper-cased first keyword of text, skipping
// whitespace and comments. A statement starting with "(" returns "(".
func leadingKeyword(text string) string {
	i := skipEmpty(text, 0)
	if i >= len(text) {
		return ""
	}
	if text[i] == '(' {
		return "("
	}
	j := i
	for j < len(text) && (text[j] >= 'a' && text[j] <= 'z' || text[j] >= 'A' && text[j] <= 'Z' || text[j] == '_') {
		j++
	}
	return strings.ToUpper(text[i:j])
}

// isPrepareTransaction reports whether text starts with PREPARE TRANSACTION.
func isPrepareTransaction(text string) bool {
	if leadingKeyword(text) != "PREPARE" {
		return false
	}
	i := skipEmpty(text, 0)
	i += len("PREPARE")
	j := skipBlank(text, i)
	return j > i && leadingKeyword(text[j:]) == "TRANSACTION"
}

// isSelectLike reports whether a statement starting with kw only reads.
func isSelectLike(kw string) bool {
	switch kw {
	case "SELECT", "VALUES", "TABLE", "(":
		return true
	}
	return false
}

// resultSet streams one statement's rows. When it owns a transaction (tx is
// set) the transaction stays open until the result set is exhausted or
// closed, then ends in ROLLBACK (read-only) or COMMIT.
type resultSet struct {
	c       *conn
	ctx     context.Context // the caller's ctx, used for error mapping and the rollback
	qctx    context.Context // ctx of the statements; cancelled on release
	cancel  context.CancelFunc
	start   time.Time
	ro      bool
	discard bool // end in ROLLBACK even when read-write
	limit   int

	tx    pgx.Tx
	scope *txScope

	owned        bool     // the result set began tx and ends it; false inside a caller's Tx
	oldPath      *string  // search_path to restore when a namespaced query ends inside a caller's Tx
	rows         pgx.Rows // the open rows: the whole result, or the current FETCH batch
	cursor       bool
	exhausted    bool // a cursor has no more rows
	fetched      int  // rows read from the cursor
	batchWant    int
	inBatch      int
	abortOnClose bool
	aborted      bool // the statement was cancelled to skip its remaining rows

	fds   []pgconn.FieldDescription
	names []string
	cols  []driver.Column

	count     int
	truncated bool
	done      bool
	endErr    error // COMMIT failure, reported once
	duration  time.Duration
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

func (r *resultSet) Stats() driver.Stats {
	d := r.duration
	if !r.done {
		d = time.Since(r.start)
	}
	return driver.Stats{RowsAffected: -1, Duration: d, Truncated: r.truncated}
}

// openPlain runs the statement on the pool and streams it.
func (r *resultSet) openPlain(text string, args []any) error {
	rows, err := r.c.pool.Query(r.qctx, text, args...)
	if err != nil {
		return err
	}
	r.rows = rows
	r.describe(rows)
	return nil
}

// openTx begins the transaction and chooses between cursor and direct
// execution.
func (r *resultSet) openTx(q driver.Query, args []any, kw string) error {
	var err error
	if r.tx == nil {
		var opts pgx.TxOptions
		if r.ro {
			opts.AccessMode = pgx.ReadOnly
		}
		if r.tx, err = r.c.pool.BeginTx(r.ctx, opts); err != nil {
			return err
		}
		r.owned = true
	}
	tx := r.tx
	switch {
	case q.Namespace != "":
		if !r.owned { // the caller's Tx outlives this query: remember the path to put back
			var old string
			if err = tx.QueryRow(r.qctx, "select current_setting('search_path')").Scan(&old); err != nil {
				return err
			}
			r.oldPath = &old
		}
		// The value is parsed as an identifier list, so quote it. is_local=true
		// scopes it to this transaction.
		_, err = tx.Exec(r.qctx, "select set_config('search_path', $1, true)", quoteIdent(q.Namespace))
	case r.ro && r.owned:
		// Take the first snapshot now: after it, SET TRANSACTION READ WRITE is
		// refused, so the statement cannot flip the transaction to read-write.
		_, err = tx.Exec(r.qctx, "select 1")
	}
	if err != nil {
		return err
	}
	if (r.limit > 0 || !r.owned) && (isSelectLike(kw) || kw == "WITH") && !hasTopLevelInto(q.Text) {
		return r.openCursor(q.Text, args, kw)
	}
	return r.openDirect(q.Text, args)
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// openDirect runs the statement in the open transaction and streams it.
func (r *resultSet) openDirect(text string, args []any) error {
	rows, err := r.tx.Query(r.qctx, text, args...)
	if err != nil {
		return err
	}
	r.rows = rows
	r.describe(rows)
	return nil
}

// openCursor declares a cursor over text and fetches the first batch. A
// savepoint guards the attempt when it may legitimately fail, so the statement
// can run directly in the same transaction instead: a WITH may hold
// data-modifying CTEs, which DECLARE rejects, and inside a caller's Tx a failed
// DECLARE (FOR UPDATE variants, an unusual statement) must not abort the Tx.
func (r *resultSet) openCursor(text string, args []any, kw string) error {
	guarded := (kw == "WITH" || !r.owned) && r.c.flavor.savepoints()
	sp := savepointPrefix + randomHex() // per attempt, so it cannot collide with the caller's own
	if guarded {
		if _, err := r.tx.Exec(r.qctx, "SAVEPOINT "+sp); err != nil {
			return err
		}
	}
	// Query, not Exec: Exec with no arguments uses the simple protocol, which
	// would accept several statements.
	rows, err := r.tx.Query(r.qctx, r.c.flavor.declareCursor()+text, args...)
	if err == nil {
		rows.Close()
		err = rows.Err()
	}
	if err != nil {
		if !guarded {
			return err
		}
		if _, rerr := r.tx.Exec(r.qctx, "ROLLBACK TO SAVEPOINT "+sp); rerr != nil {
			return rerr
		}
		if _, rerr := r.tx.Exec(r.qctx, "RELEASE SAVEPOINT "+sp); rerr != nil { // ROLLBACK TO keeps it defined
			return rerr
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "0A000" || pe.Code == "42601" && kw == "WITH") {
			return r.openDirect(text, args)
		}
		return err
	}
	if guarded { // keep the Tx free of leftover savepoints
		if _, err := r.tx.Exec(r.qctx, "RELEASE SAVEPOINT "+sp); err != nil {
			return err
		}
	}
	r.cursor = true
	return r.fetchBatchRows(r.qctx)
}

// fetchBatchRows opens the next FETCH batch, never reading past Limit+1 rows.
// The FETCH statement has no parameters and is ours, so it can use the simple
// protocol, which also keeps its result description out of the statement
// cache (the same text describes different cursors over time).
func (r *resultSet) fetchBatchRows(ctx context.Context) error {
	n := fetchBatch // a cursor without a limit (inside a caller's Tx) reads to the end
	if r.limit > 0 {
		n = min(fetchBatch, r.limit+1-r.fetched)
	}
	if n <= 0 {
		r.exhausted = true
		return nil
	}
	rows, err := r.tx.Query(ctx, fmt.Sprintf("FETCH FORWARD %d FROM %s", n, cursorName), pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return err
	}
	r.rows, r.batchWant, r.inBatch = rows, n, 0
	if r.fds == nil {
		r.describe(rows)
	}
	return nil
}

// describe records the column metadata from rows.
func (r *resultSet) describe(rows pgx.Rows) {
	r.fds = slices.Clone(rows.FieldDescriptions())
	if r.fds == nil {
		r.fds = []pgconn.FieldDescription{}
	}
	if len(r.fds) == 0 {
		return
	}
	tm := rows.Conn().TypeMap()
	r.names = r.c.typeNamesFor(r.qctx, tm, r.fds)
	r.cols = make([]driver.Column, len(r.fds))
	for i, fd := range r.fds {
		r.cols[i] = driver.Column{Name: fd.Name, NativeType: r.names[i], Type: columnType(tm, fd.DataTypeOID, r.names[i])}
	}
}

// finishWithoutColumns completes a statement that returns no columns (DML,
// DDL, SET…): it reports the rows affected from the command tag.
func (r *resultSet) finishWithoutColumns(ctx context.Context) (driver.ResultSet, error) {
	rows := r.rows
	rows.Close()
	err := rows.Err()
	tag := rows.CommandTag()
	r.rows = nil
	if err != nil {
		r.release(false)
		return nil, r.mapErr(ctx, err)
	}
	r.release(true)
	if r.endErr != nil {
		return nil, r.mapErr(ctx, r.endErr)
	}
	return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: tag.RowsAffected(), Duration: r.duration}), nil
}

// step advances to the next row of the statement, fetching further cursor
// batches as needed. The row is then readable through r.rows.
func (r *resultSet) step(ctx context.Context) (bool, error) {
	for {
		if r.rows != nil {
			if r.rows.Next() {
				r.inBatch++
				r.fetched++
				return true, nil
			}
			err := r.rows.Err()
			r.rows = nil
			if err != nil {
				return false, err
			}
			if !r.cursor || r.inBatch < r.batchWant {
				r.exhausted = true // a short batch: the cursor is at its end
				return false, nil
			}
		}
		if !r.cursor || r.exhausted {
			return false, nil
		}
		if err := r.fetchBatchRows(ctx); err != nil {
			return false, err
		}
		if r.rows == nil {
			return false, nil
		}
	}
}

func (r *resultSet) Next(ctx context.Context) (_ driver.Row, _ bool, err error) {
	defer func() { err = r.c.named(err) }()
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.release(false)
		return nil, false, r.mapErr(ctx, err)
	}
	if r.limit > 0 && r.count >= r.limit { // peek one row to learn whether we cut the result
		more, err := r.step(ctx)
		if err != nil {
			r.release(false)
			return nil, false, r.mapErr(ctx, err)
		}
		r.truncated = more
		return nil, false, r.end(ctx)
	}
	ok, err := r.step(ctx)
	if err != nil {
		r.release(false)
		return nil, false, r.mapErr(ctx, err)
	}
	if !ok {
		return nil, false, r.end(ctx)
	}
	row, err := r.convert()
	if err != nil {
		r.release(false)
		return nil, false, r.mapErr(ctx, err)
	}
	r.count++
	return row, true, nil
}

// end finishes the result set after its last row and reports a COMMIT error.
func (r *resultSet) end(ctx context.Context) error {
	r.release(true)
	if err := r.endErr; err != nil {
		r.endErr = nil
		return r.mapErr(ctx, err)
	}
	return nil
}

// convert reads the current row of r.rows. The raw bytes are only valid until
// the next call to Next, so they are copied.
func (r *resultSet) convert() (driver.Row, error) {
	raw := r.rows.RawValues()
	tm := r.rows.Conn().TypeMap()
	row := make(driver.Row, len(raw))
	for i, b := range raw {
		v, err := valueFromPG(tm, r.fds[i].DataTypeOID, r.fds[i].Format, r.names[i], slices.Clone(b))
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", r.fds[i].Name, err)
		}
		row[i] = v
	}
	return row, nil
}

// release closes the open rows, ends the transaction and frees the context.
// It is idempotent. commit asks for COMMIT on a read-write transaction; a
// read-only one, an error or a cancelled ctx always ROLLBACK.
func (r *resultSet) release(commit bool) {
	if r.done {
		return
	}
	r.done = true
	if r.rows != nil {
		if r.abortOnClose && !r.cursor {
			r.cancel() // do not read the rest of a half-consumed SELECT
			r.aborted = true
		}
		r.rows.Close()
		r.rows = nil
	}
	if r.tx != nil && (r.ro || !r.owned) && txEnded(r.tx) {
		// Fail closed: the statement ended the transaction (a COMMIT the
		// keyword scan missed). Nothing more may run on this connection.
		if r.owned {
			discardConn(r.tx)
		} else {
			r.scope.onEscape()
		}
		r.endErr = escapeError(r.ro)
	}
	if r.tx != nil && !r.owned {
		r.leaveCallerTx()
	} else if r.tx != nil {
		if commit && !r.ro && !r.discard && !r.aborted && r.ctx.Err() == nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), rollbackTimeout)
			if err := r.tx.Commit(cctx); err != nil {
				r.endErr = err
			}
			cancel()
		} else {
			rollback(r.ctx, r.tx)
		}
		r.tx = nil
	}
	r.cancel()
	r.duration = time.Since(r.start)
}

// leaveCallerTx tidies what the statement left in a caller's transaction, once
// its rows are closed: the cursor and the search_path. The transaction itself
// stays open. Errors are dropped: a failed transaction fails the caller's next
// statement anyway.
func (r *resultSet) leaveCallerTx() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), rollbackTimeout)
	defer cancel()
	if r.endErr != nil { // the transaction is gone; the connection is being discarded
		return
	}
	if r.cursor {
		_, _ = r.tx.Exec(ctx, "CLOSE "+cursorName)
	}
	if r.oldPath != nil {
		_, _ = r.tx.Exec(ctx, "select set_config('search_path', $1, true)", *r.oldPath)
	}
}

func (r *resultSet) Close() (err error) {
	defer func() { err = r.c.named(err) }()
	r.release(true)
	if err := r.endErr; err != nil {
		r.endErr = nil
		return r.mapErr(r.ctx, err)
	}
	return nil
}

// typeNamesFor returns the type name of each column: pgtype's name for
// builtin OIDs, pg_type's typname for the rest (enums, extensions such as
// pgvector), else "oid:<n>". Lookups use a second pooled connection, because
// the first is busy streaming; they are skipped on a one-connection pool.
func (c *conn) typeNamesFor(ctx context.Context, tm *pgtype.Map, fds []pgconn.FieldDescription) []string {
	names := make([]string, len(fds))
	var missing []int64
	for i, fd := range fds {
		if dt, ok := tm.TypeForOID(fd.DataTypeOID); ok {
			names[i] = dt.Name
		} else if v, ok := c.typeNames.Load(fd.DataTypeOID); ok {
			names[i] = v.(string)
		} else {
			missing = append(missing, int64(fd.DataTypeOID))
		}
	}
	if len(missing) > 0 && c.pool.Stat().AcquiredConns() < c.pool.Stat().MaxConns() {
		c.lookupTypeNames(ctx, missing)
		for i, fd := range fds {
			if names[i] != "" {
				continue
			}
			if v, ok := c.typeNames.Load(fd.DataTypeOID); ok {
				names[i] = v.(string)
			}
		}
	}
	for i, fd := range fds {
		if names[i] == "" {
			names[i] = fmt.Sprintf("oid:%d", fd.DataTypeOID)
		}
	}
	return names
}

func (c *conn) lookupTypeNames(ctx context.Context, oids []int64) {
	// The caller's connection is busy streaming, so this needs another one. A
	// bounded wait keeps a pool that fills up meanwhile from deadlocking; the
	// names then stay "oid:<n>" for this query.
	lctx, cancel := context.WithTimeout(ctx, typeLookupTimeout)
	defer cancel()
	pc, err := c.pool.Acquire(lctx)
	if err != nil {
		return
	}
	defer pc.Release()
	rows, err := pc.Query(lctx, "select oid::int8, typname from pg_type where oid::int8 = any($1)", oids)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var oid int64
		var name string
		if rows.Scan(&oid, &name) == nil {
			c.typeNames.Store(uint32(oid), name)
		}
	}
}

// columnType maps a column's type to tobira's normalised Type, matching what
// valueFromPG produces for its values.
func columnType(tm *pgtype.Map, oid uint32, typeName string) driver.Type {
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID:
		return driver.TypeInt
	case pgtype.Float4OID, pgtype.Float8OID:
		return driver.TypeFloat
	case pgtype.BoolOID:
		return driver.TypeBool
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.BPCharOID, pgtype.NameOID, pgtype.UUIDOID:
		return driver.TypeString
	case pgtype.NumericOID:
		return driver.TypeDecimal
	case pgtype.ByteaOID:
		return driver.TypeBytes
	case pgtype.TimestamptzOID, pgtype.TimestampOID, pgtype.DateOID:
		return driver.TypeTime
	case pgtype.JSONOID, pgtype.JSONBOID:
		return driver.TypeJSON
	}
	if typeName == vectorTypeName {
		return driver.TypeVector
	}
	if dt, ok := tm.TypeForOID(oid); ok {
		if _, isArray := dt.Codec.(*pgtype.ArrayCodec); isArray {
			return driver.TypeList
		}
	}
	return driver.TypeUnknown
}

// txEnded reports whether the connection behind tx is idle (outside any
// transaction) although tx is still supposed to be open.
func txEnded(tx pgx.Tx) bool {
	c := tx.Conn()
	return !c.IsClosed() && c.PgConn().TxStatus() == 'I'
}

// escapeError is reported when a statement ended the transaction it ran in.
func escapeError(readOnly bool) error {
	kind := driver.ErrUnsupported
	if readOnly {
		kind = driver.ErrReadOnly
	}
	return driver.Wrap(kind, Name, "query", errors.New("the statement ended the transaction; the connection was discarded"))
}

// discardConn closes the connection behind tx, so the pool destroys it on
// release instead of reusing it.
func discardConn(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_ = tx.Conn().Close(ctx)
}

// sqlWord is one word of a statement with its parenthesis depth.
type sqlWord struct {
	text  string // upper-cased
	depth int
}

// guardDialects are the readings of '…' and $tag$ the guards decide on.
// '…' is lexed as a standard string and with backslash escapes
// (standard_conforming_strings off: a role or database default can set it,
// and the read-only path refuses a SET); E'…' honours backslashes in both.
// Each is lexed with PostgreSQL's dollar-quote tags, which may hold bytes
// >= 0x80 ($é$), and again with ASCII-only tags, the reading these guards
// were first written against, so widening the tags could only add refusals.
func guardDialects() []sqlscan.Dialect {
	var ds []sqlscan.Dialect
	for _, std := range []bool{true, false} {
		d := sqlscan.Postgres(std)
		ascii := d
		ascii.DollarTagHighBit = false
		ds = append(ds, d, ascii)
	}
	return ds
}

// lexings returns the words of text under every guardDialects reading. The
// guards decide on all of them and refuse when any one does.
func lexings(text string, quoted bool) [][]sqlWord {
	var out [][]sqlWord
	for _, d := range guardDialects() {
		out = append(out, scanIdents(text, quoted, d))
	}
	return out
}

// scanIdents returns the words of text outside string literals, quoted
// identifiers, dollar quotes and comments, lexed with driverutil/sqlscan's
// Postgres dialect. It is a lexical scan, good enough for spotting a
// top-level INTO or CHAIN; it never decides anything alone. With quoted set
// it also returns each quoted identifier ("x", U&"x" [UESCAPE 'c']) as a
// word, unquoted, unescaped and upper-cased. d is one of guardDialects.
func scanIdents(text string, quoted bool, d sqlscan.Dialect) []sqlWord {
	return scanWords(text, quoted, false, d)
}

// scanWords is scanIdents that, with punct set, also returns each
// punctuation byte ("(", ".", …) as a word of its own.
func scanWords(text string, quoted, punct bool, d sqlscan.Dialect) []sqlWord {
	var out []sqlWord
	depth := 0
	for pos := 0; pos < len(text); {
		at := pos
		pos = len(text)
		sqlscan.Scan(d, text[at:], func(t sqlscan.Token) bool {
			start, end := at+t.Start, at+t.End
			raw := text[start:end]
			switch t.Kind {
			case sqlscan.Punct:
				switch raw {
				case "(":
					depth++
				case ")":
					depth = max(depth-1, 0)
				}
				if punct {
					out = append(out, sqlWord{raw, depth})
				}
			case sqlscan.Semicolon:
				out = append(out, sqlWord{";", depth}) // keeps word-sequence matches from spanning statements
			case sqlscan.QuotedIdent, sqlscan.Unterminated:
				if quoted && raw[0] == '"' { // an unterminated one runs to the end
					out = append(out, sqlWord{strings.ToUpper(quotedIdent(raw)), depth})
				}
			case sqlscan.Word:
				w := strings.ToUpper(raw)
				if quoted && w == "U" && strings.HasPrefix(text[end:], `&"`) {
					// U&"…" [UESCAPE 'c']: decode it, then scan on after the clause.
					qend := quotedEnd(d, text, end+1)
					esc, next := uescape(text, qend)
					out = append(out, sqlWord{strings.ToUpper(unicodeIdent(quotedIdent(text[end+1:qend]), esc)), depth})
					pos = next
					return false
				}
				out = append(out, sqlWord{w, depth})
			}
			return true
		})
	}
	return out
}

// quotedEnd returns the index after the quoted identifier starting at
// text[i] (an unterminated one runs to the end).
func quotedEnd(d sqlscan.Dialect, text string, i int) int {
	end := len(text)
	sqlscan.Scan(d, text[i:], func(t sqlscan.Token) bool {
		end = i + t.End
		return false
	})
	return end
}

// hasTopLevelInto reports whether text has INTO outside parentheses, as in
// SELECT … INTO table, which creates a table and cannot be declared as a cursor.
// Any lexing seeing one is enough.
func hasTopLevelInto(text string) bool {
	for _, words := range lexings(text, false) {
		for _, w := range words {
			if w.text == "INTO" && w.depth == 0 {
				return true
			}
		}
	}
	return false
}

// endsWithChain reports whether text is COMMIT/END/ROLLBACK/ABORT … AND CHAIN,
// which would start a new transaction behind the driver's back.
func endsWithChain(text string) bool {
	switch leadingKeyword(text) {
	case "COMMIT", "END", "ROLLBACK", "ABORT":
		for _, words := range lexings(text, false) { // any lexing seeing it is enough
			for i := 1; i < len(words); i++ {
				if words[i-1].text == "AND" && words[i].text == "CHAIN" {
					return true
				}
			}
		}
	}
	return false
}

// randomHex returns 8 random hex digits.
func randomHex() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
