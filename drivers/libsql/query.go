package libsql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

var (
	errNotExecuted     = errors.New("the server skipped the statement")
	errRollbackFailed  = errors.New("the read-only transaction could not be rolled back")
	errChangesUnknown  = errors.New("the server did not report the change count")
	errNoServerResults = errors.New("the server returned no result")
)

// toArg converts a parameter to the database/sql-style value encodeArg
// takes. It mirrors the sqlite driver: times are UTC text, decimals, JSON
// and strings are text.
func toArg(v driver.Value) any {
	switch v.Type() {
	case driver.TypeNull:
		return nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		if b {
			return int64(1)
		}
		return int64(0)
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

// statement builds the Hrana statement for text and params.
func statement(op, text string, params []driver.Value, wantRows bool) (hranaStmt, error) {
	s := hranaStmt{SQL: text, WantRows: wantRows}
	for i, p := range params {
		switch p.Type() {
		case driver.TypeVector, driver.TypeList, driver.TypeObject:
			return s, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("parameter %d: %s values cannot be bound", i+1, p.Type()))
		}
		hv, err := encodeArg(toArg(p))
		if err != nil {
			return s, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("parameter %d: %w", i+1, err))
		}
		s.Args = append(s.Args, hv)
	}
	return s, nil
}

// readOnlyExec runs stmts on a fresh stream inside BEGIN … ROLLBACK, all in
// one batch, and closes the stream in the same pipeline. Each statement
// runs only when the step before it succeeded, the first only when BEGIN
// did, so nothing runs outside the transaction; ROLLBACK runs whenever
// BEGIN succeeded. It returns one result per statement.
func (c *conn) readOnlyExecAll(ctx context.Context, stmts ...hranaStmt) ([]*stmtResult, error) {
	steps := make([]hranaStep, 0, len(stmts)+2)
	steps = append(steps, hranaStep{Stmt: hranaStmt{SQL: "BEGIN"}})
	for i, s := range stmts {
		steps = append(steps, hranaStep{Condition: condOK(i), Stmt: s})
	}
	steps = append(steps, hranaStep{Condition: condOK(0), Stmt: hranaStmt{SQL: "ROLLBACK"}})
	res, err := c.client.oneShot(ctx, batchReq(steps...), closeReq())
	if err != nil {
		return nil, err
	}
	b, err := batchOutcome(res[0], len(steps))
	if err != nil {
		return nil, err
	}
	if _, err := b.step(0); err != nil {
		return nil, err
	}
	out := make([]*stmtResult, len(stmts))
	for i := range stmts {
		r, err := b.step(i + 1)
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, errNotExecuted
		}
		out[i] = r
	}
	if r, err := b.step(len(steps) - 1); err != nil || r == nil {
		return nil, errRollbackFailed
	}
	return out, nil
}

func (c *conn) readOnlyExec(ctx context.Context, s hranaStmt) (*stmtResult, error) {
	out, err := c.readOnlyExecAll(ctx, s)
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// writeExec runs s on a fresh stream, outside any transaction (autocommit),
// and closes the stream in the same pipeline. With trackChanges it brackets
// s with total_changes() so the row count is exact.
func (c *conn) writeExec(ctx context.Context, s hranaStmt, trackChanges bool) (*stmtResult, int64, error) {
	if !trackChanges {
		res, err := c.client.oneShot(ctx, executeReq(s), closeReq())
		if err != nil {
			return nil, -1, err
		}
		r, err := executeResult(res[0])
		return r, -1, err
	}
	steps := changeSteps(s, -1)
	res, err := c.client.oneShot(ctx, batchReq(steps...), closeReq())
	if err != nil {
		return nil, -1, err
	}
	b, err := batchOutcome(res[0], len(steps))
	if err != nil {
		return nil, -1, err
	}
	return changeResult(b, 0)
}

// changeSteps brackets s with total_changes() reads. after is the step the
// first read depends on (-1: none); the steps after it depend on their
// predecessor.
func changeSteps(s hranaStmt, after int) []hranaStep {
	totals := hranaStmt{SQL: "SELECT total_changes()", WantRows: true}
	first := hranaStep{Stmt: totals}
	if after >= 0 {
		first.Condition = condError(after)
	}
	base := after + 1
	return []hranaStep{
		first,
		{Condition: condOK(base), Stmt: s},
		{Condition: condOK(base + 1), Stmt: totals},
	}
}

// changeResult reads the steps changeSteps built, starting at step base.
func changeResult(b *batchResult, base int) (*stmtResult, int64, error) {
	before, err := b.step(base)
	if err != nil {
		return nil, -1, err
	}
	r, err := b.step(base + 1)
	if err != nil {
		return nil, -1, err
	}
	after, err := b.step(base + 2)
	if err != nil {
		return nil, -1, err
	}
	if before == nil || r == nil || after == nil {
		return nil, -1, errNotExecuted
	}
	n0, ok0 := singleInt(before)
	n1, ok1 := singleInt(after)
	if !ok0 || !ok1 {
		return nil, -1, errChangesUnknown
	}
	return r, n1 - n0, nil
}

// singleInt reads a one-row, one-column integer result.
func singleInt(r *stmtResult) (int64, bool) {
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		return 0, false
	}
	v, err := r.Rows[0][0].decode()
	n, ok := v.(int64)
	return n, err == nil && ok
}

// Query runs one statement. On a read-only connection, or with
// Query.ReadOnly, the statement must pass the read-only allow-list and runs
// inside BEGIN … ROLLBACK (see readOnlyExecAll). Otherwise it runs in
// autocommit mode on a fresh stream.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	readOnly := c.readOnly || q.ReadOnly
	if readOnly {
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
	if readOnly {
		r, err := c.readOnlyExec(ctx, s)
		if err != nil {
			return nil, mapError(op, err)
		}
		return resultSet(op, r, q.Limit, -1, start)
	}
	r, affected, err := c.writeExec(ctx, s, !returnsRows(q.Text))
	if err != nil {
		return nil, mapError(op, err)
	}
	return resultSet(op, r, q.Limit, affected, start)
}

// resultSet converts a statement result. A result without columns reports
// affected as RowsAffected (0 when unknown); one with columns is limited to
// limit rows (Truncated when more came back). Column types come from the
// declared types; a column without one (an expression) takes the first
// row's runtime type, and its values are converted per row.
func resultSet(op string, r *stmtResult, limit int, affected int64, start time.Time) (driver.ResultSet, error) {
	if r == nil {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errNoServerResults)
	}
	if len(r.Cols) == 0 {
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: max(affected, 0), Duration: time.Since(start)}), nil
	}
	raw, err := decodeRows(r)
	if err != nil {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	truncated := false
	if limit > 0 && len(raw) > limit {
		raw, truncated = raw[:limit], true
	}
	cols := make([]driver.Column, len(r.Cols))
	declared := make([]string, len(r.Cols))
	types := make([]driver.Type, len(r.Cols))
	for i, col := range r.Cols {
		if col.DeclType != nil {
			declared[i] = strings.TrimSpace(*col.DeclType)
		}
		types[i] = mapType(declared[i])
		if declared[i] == "" && len(raw) > 0 {
			types[i] = columnType("", raw[0][i])
		}
		name := ""
		if col.Name != nil {
			name = *col.Name
		}
		cols[i] = driver.Column{Name: name, NativeType: declared[i], Type: types[i]}
	}
	rows := make([]driver.Row, len(raw))
	for j, vals := range raw {
		row := make(driver.Row, len(vals))
		for i, v := range vals {
			if declared[i] == "" {
				row[i] = convertValue("", v)
				continue
			}
			row[i] = convertTyped(declared[i], types[i], v)
		}
		rows[j] = row
	}
	return driver.NewSliceResultSet(cols, rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start), Truncated: truncated}), nil
}

// decodeRows decodes every value of r.
func decodeRows(r *stmtResult) ([][]any, error) {
	out := make([][]any, len(r.Rows))
	for j, row := range r.Rows {
		if len(row) != len(r.Cols) {
			return nil, fmt.Errorf("row %d has %d values for %d columns", j, len(row), len(r.Cols))
		}
		vals := make([]any, len(row))
		for i, v := range row {
			d, err := v.decode()
			if err != nil {
				return nil, fmt.Errorf("row %d column %d: %w", j, i, err)
			}
			vals[i] = d
		}
		out[j] = vals
	}
	return out, nil
}
