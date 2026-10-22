package duckdb

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/xavidop/tobira/driver"
)

// endTimeout bounds the ROLLBACK that ends a read-only transaction or a
// Tx whose ctx is gone.
const endTimeout = 5 * time.Second

// Query runs one statement. On a read-only conn, or with q.ReadOnly, it
// takes the read-only path (readOnlyQuery) behind the lexical guard.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	if c.readOnly || q.ReadOnly {
		if c.external {
			return nil, refuseExternal("query")
		}
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
		return c.readOnlyQuery(ctx, q)
	}
	if err := checkSingle(q.Text); err != nil {
		return nil, err
	}
	if isTxControl(q.Text) {
		return nil, errTxControl(driver.ErrUnsupported)
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError("query", err)
	}
	return runOn(ctx, sc, true, false, q)
}

func errTxControl(kind error) error {
	return driver.Wrap(kind, Name, "query", errors.New("transaction control statements are not allowed here: use Begin, Commit and Rollback"))
}

// readOnlyQuery is the read-only path behind the lexical guard: on its own
// connection, inside a READ ONLY transaction that is always rolled back,
// and only for the parser's SELECT and EXPLAIN statement types.
func (c *conn) readOnlyQuery(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if c.external {
		return nil, refuseExternal("query")
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError("query", err)
	}
	if _, err := sc.ExecContext(ctx, "BEGIN TRANSACTION READ ONLY"); err != nil {
		discard(sc)
		return nil, mapError("query", err)
	}
	return runOn(ctx, sc, true, true, q)
}

// statementType asks DuckDB's parser what text is, by preparing it on sc.
func statementType(ctx context.Context, sc *sql.Conn, text string) (duckdb.StmtType, error) {
	var typ duckdb.StmtType
	err := sc.Raw(func(dc any) error {
		dconn, ok := dc.(*duckdb.Conn)
		if !ok {
			return errors.New("not a duckdb connection")
		}
		stmt, err := dconn.PrepareContext(ctx, text)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		ds, ok := stmt.(*duckdb.Stmt)
		if !ok {
			return errors.New("not a duckdb statement")
		}
		typ, err = ds.StatementType()
		return err
	})
	return typ, err
}

// readOnlyTypes are the statement types a read-only path runs: SHOW,
// DESCRIBE, SUMMARIZE, PIVOT, FROM-first and the read-only PRAGMAs all
// parse to SELECT.
func readOnlyType(t duckdb.StmtType) bool {
	return t == duckdb.STATEMENT_TYPE_SELECT || t == duckdb.STATEMENT_TYPE_EXPLAIN
}

// rowTypes are the statement types that return rows.
func rowType(t duckdb.StmtType) bool {
	switch t {
	case duckdb.STATEMENT_TYPE_SELECT, duckdb.STATEMENT_TYPE_EXPLAIN, duckdb.STATEMENT_TYPE_PRAGMA,
		duckdb.STATEMENT_TYPE_CALL, duckdb.STATEMENT_TYPE_EXECUTE, duckdb.STATEMENT_TYPE_RELATION:
		return true
	}
	return false
}

// runOn runs q on sc. own says the result closes sc when it finishes (a
// Tx's connection stays open). readOnly says sc is inside a READ ONLY
// transaction that the result rolls back and whose connection it
// discards; only SELECT and EXPLAIN statement types run there.
func runOn(ctx context.Context, sc *sql.Conn, own, readOnly bool, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	end := func() {
		switch {
		case readOnly && own:
			rollbackAndDiscard(ctx, sc)
		case own && q.Namespace != "": // the search_path must not reach the next user
			discard(sc)
		case own:
			_ = sc.Close()
		}
	}
	if q.Namespace != "" {
		if err := setSchema(ctx, sc, q.Namespace); err != nil {
			end()
			return nil, err
		}
	}
	typ, err := statementType(ctx, sc, q.Text)
	if err != nil {
		end()
		return nil, mapError("query", err)
	}
	if readOnly && !readOnlyType(typ) {
		end()
		return nil, refuse("the statement is not a query")
	}
	if typ == duckdb.STATEMENT_TYPE_TRANSACTION {
		end()
		kind := driver.ErrUnsupported
		if readOnly {
			kind = driver.ErrReadOnly
		}
		return nil, errTxControl(kind)
	}
	args := toArgs(q.Params)
	if !rowType(typ) && !hasReturning(q.Text) {
		res, err := sc.ExecContext(ctx, q.Text, args...)
		if err != nil {
			end()
			return nil, mapError("query", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			n = -1
		}
		end()
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: n, Duration: time.Since(start)}), nil
	}
	rows, err := sc.QueryContext(ctx, q.Text, args...)
	if err != nil {
		end()
		return nil, mapError("query", err)
	}
	cts, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		end()
		return nil, mapError("query", err)
	}
	rs := &resultSet{rows: rows, end: end, limit: q.Limit, start: start}
	for _, ct := range cts {
		native := ct.DatabaseTypeName()
		node := parseType(native)
		rs.types = append(rs.types, node)
		rs.cols = append(rs.cols, driver.Column{Name: ct.Name(), NativeType: native, Type: nodeType(node)})
	}
	return rs, nil
}

// setSchema makes ns the connection's default schema for this statement.
// The connection is released after the statement (closed and discarded on
// read-only paths; a Tx keeps it, as the caller asked).
func setSchema(ctx context.Context, sc *sql.Conn, ns string) error {
	if strings.ContainsAny(ns, "\x00") {
		return driver.Wrap(driver.ErrSyntax, Name, "query", errors.New("invalid namespace"))
	}
	lit := "'" + strings.ReplaceAll(quoteIdent(ns), "'", "''") + "'"
	if _, err := sc.ExecContext(ctx, "SET search_path = "+lit); err != nil {
		return mapError("query", err)
	}
	return nil
}

// rollbackAndDiscard ends a read-only transaction and drops sc from the
// pool, so nothing the statement left behind reaches the next user.
func rollbackAndDiscard(ctx context.Context, sc *sql.Conn) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	_, _ = sc.ExecContext(rctx, "ROLLBACK")
	cancel()
	discard(sc)
}

// discard closes sc and makes database/sql drop it instead of pooling it.
func discard(sc *sql.Conn) {
	_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
	_ = sc.Close()
}

type resultSet struct {
	rows      *sql.Rows
	end       func() // releases the connection; nil once released
	owner     *tx    // the transaction tracking this result set, if any
	cols      []driver.Column
	types     []*typeNode
	conv      converter
	limit     int
	count     int
	start     time.Time
	done      bool
	duration  time.Duration
	truncated bool
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

func (r *resultSet) scan() ([]any, bool, error) {
	if !r.rows.Next() {
		return nil, false, r.rows.Err()
	}
	raw := make([]any, len(r.cols))
	ptrs := make([]any, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := r.rows.Scan(ptrs...); err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (r *resultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	if err := ctx.Err(); err != nil {
		r.release()
		return nil, false, mapError("query", err)
	}
	if r.done {
		return nil, false, nil
	}
	if r.limit > 0 && r.count >= r.limit {
		_, more, err := r.scan()
		if err != nil {
			r.release()
			return nil, false, mapError("query", err)
		}
		r.truncated = more
		r.release()
		return nil, false, nil
	}
	raw, ok, err := r.scan()
	if err != nil {
		r.release()
		return nil, false, mapError("query", err)
	}
	if !ok {
		r.release()
		return nil, false, nil
	}
	r.count++
	row := make(driver.Row, len(raw))
	for i, v := range raw {
		row[i] = r.conv.value(r.types[i], v)
	}
	return row, true, nil
}

func (r *resultSet) release() {
	if r.done {
		return
	}
	r.done = true
	if r.owner != nil {
		r.owner.forget(r)
	}
	r.duration = time.Since(r.start)
	_ = r.rows.Close()
	if r.end != nil {
		r.end()
		r.end = nil
	}
}

func (r *resultSet) Stats() driver.Stats {
	d := r.duration
	if !r.done {
		d = time.Since(r.start)
	}
	st := driver.Stats{RowsAffected: -1, Duration: d, Truncated: r.truncated}
	if r.conv.cut {
		st.Warnings = []string{"some values were cut to fit: strings and blobs at 512 KiB, lists and maps at 10000 items, nesting at 32 levels"}
	}
	return st
}

func (r *resultSet) Close() error {
	r.release()
	return nil
}
