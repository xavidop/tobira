package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"modernc.org/sqlite"

	"github.com/xavidop/tobira/driver"
)

// sqliteLimitAttached is SQLITE_LIMIT_ATTACHED.
const sqliteLimitAttached = 7

func toArg(v driver.Value) any {
	switch v.Type() {
	case driver.TypeNull:
		return nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b
	case driver.TypeInt:
		i, _ := v.AsInt()
		return i
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format("2006-01-02 15:04:05.999999999")
	}
	return v.String()
}

// Query runs one statement. Statements without result columns are executed
// and report RowsAffected; others stream through the returned ResultSet.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	db := c.handle(q.ReadOnly)
	if db == c.ro {
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
	}
	return c.exec(ctx, db, q)
}

// exec runs q on db after the caller's gate. On the read-only handle it
// re-asserts query_only and the zero ATTACH limit first.
func (c *conn) exec(ctx context.Context, db *sql.DB, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	if err := checkSingle(q.Text); err != nil {
		return nil, err
	}
	sc, err := db.Conn(ctx)
	if err != nil {
		return nil, mapError("query", err)
	}
	if db == c.ro {
		if err := guardReadOnly(ctx, sc); err != nil {
			_ = sc.Close()
			return nil, err
		}
	}
	return execOn(ctx, sc, true, db == c.rw, q, start)
}

// guardReadOnly re-asserts query_only and the zero ATTACH limit on a pooled
// read-only connection: earlier text may have lifted them.
func guardReadOnly(ctx context.Context, sc *sql.Conn) error {
	if _, err := sc.ExecContext(ctx, "PRAGMA query_only=1"); err != nil {
		return mapError("query", err)
	}
	if _, err := sqlite.Limit(sc, sqliteLimitAttached, 0); err != nil {
		return mapError("query", err)
	}
	return nil
}

// execOn runs q on sc and returns its result. ownConn says the result set
// closes sc when it finishes; a transaction's connection stays open. writable
// asks for change counting on statements without result columns.
func execOn(ctx context.Context, sc *sql.Conn, ownConn, writable bool, q driver.Query, start time.Time) (driver.ResultSet, error) {
	drop := func() {
		if ownConn {
			_ = sc.Close()
		}
	}
	var before int64
	trackChanges := writable && !returnsRows(q.Text)
	if trackChanges {
		if err := sc.QueryRowContext(ctx, "select total_changes()").Scan(&before); err != nil {
			drop()
			return nil, mapError("query", err)
		}
	}
	args := make([]any, len(q.Params))
	for i, p := range q.Params {
		args[i] = toArg(p)
	}
	rows, err := sc.QueryContext(ctx, q.Text, args...)
	if err != nil {
		drop()
		return nil, mapError("query", err)
	}
	cts, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		drop()
		return nil, mapError("query", err)
	}
	if len(cts) == 0 {
		_ = rows.Close()
		var affected int64
		if trackChanges {
			var after int64
			if err := sc.QueryRowContext(ctx, "select total_changes()").Scan(&after); err != nil {
				drop()
				return nil, mapError("query", err)
			}
			affected = after - before
		}
		drop()
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: affected, Duration: time.Since(start)}), nil
	}
	rs := &resultSet{rows: rows, sc: sc, ownConn: ownConn, limit: q.Limit, start: start, declared: make([]string, len(cts)), types: make([]driver.Type, len(cts))}
	needSample := false
	for i, ct := range cts {
		rs.types[i] = mapType(ct.DatabaseTypeName())
		rs.declared[i] = strings.TrimSpace(ct.DatabaseTypeName())
		needSample = needSample || rs.declared[i] == ""
		rs.cols = append(rs.cols, driver.Column{Name: ct.Name(), NativeType: rs.declared[i], Type: mapType(rs.declared[i])})
	}
	if needSample { // expression columns: derive the type from the first row
		raw, ok, err := rs.scan()
		if err != nil {
			rs.release()
			return nil, mapError("query", err)
		}
		if ok {
			rs.pending, rs.hasPending = raw, true
			for i := range rs.cols {
				if rs.declared[i] == "" {
					rs.types[i] = columnType("", raw[i])
					rs.cols[i].Type = rs.types[i]
				}
			}
		}
	}
	return rs, nil
}

type resultSet struct {
	rows       *sql.Rows
	sc         *sql.Conn
	owner      *tx  // the transaction tracking this result set, if any
	ownConn    bool // release closes sc; a transaction's connection stays open
	cols       []driver.Column
	declared   []string
	types      []driver.Type
	limit      int
	count      int
	start      time.Time
	pending    []any
	hasPending bool
	done       bool
	duration   time.Duration
	truncated  bool
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
		_, more, err := r.scanOrPending()
		if err != nil {
			r.release()
			return nil, false, mapError("query", err)
		}
		r.truncated = more
		r.release()
		return nil, false, nil
	}
	raw, ok, err := r.scanOrPending()
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
		if r.declared[i] == "" { // expression column: the runtime type can vary per row
			row[i] = convertValue("", v)
			continue
		}
		row[i] = convertTyped(r.declared[i], r.types[i], v)
	}
	return row, true, nil
}

func (r *resultSet) scanOrPending() ([]any, bool, error) {
	if r.hasPending {
		r.hasPending = false
		return r.pending, true, nil
	}
	return r.scan()
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
	if r.ownConn {
		_ = r.sc.Close()
	}
}

func (r *resultSet) Stats() driver.Stats {
	d := r.duration
	if !r.done {
		d = time.Since(r.start)
	}
	return driver.Stats{RowsAffected: -1, Duration: d, Truncated: r.truncated}
}

func (r *resultSet) Close() error {
	r.release()
	return nil
}
