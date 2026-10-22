package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Insert, Update and Delete on the Conn run in their own transaction, so a
// key that matches the wrong number of rows changes nothing. DuckDB has no
// savepoints, so Update and Delete count the matching rows first (inside
// the transaction's snapshot) and only run on exactly one.

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	return withTx(ctx, c, "insert", func(t *tx) (driver.Object, error) { return t.Insert(ctx, ref, values) })
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	_, err := withTx(ctx, c, "update", func(t *tx) (driver.Object, error) { return nil, t.Update(ctx, ref, key, changes) })
	return err
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	_, err := withTx(ctx, c, "delete", func(t *tx) (driver.Object, error) { return nil, t.Delete(ctx, ref, key) })
	return err
}

// withTx runs fn in a fresh read-write transaction: COMMIT when fn
// succeeds, ROLLBACK otherwise.
func withTx[T any](ctx context.Context, c *conn, op string, fn func(*tx) (T, error)) (T, error) {
	var zero T
	if err := c.check(ctx, op); err != nil {
		return zero, err
	}
	if c.readOnly {
		return zero, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyConn)
	}
	t, err := c.begin(ctx, op, driver.TxOptions{})
	if err != nil {
		return zero, err
	}
	out, err := fn(t)
	if err != nil {
		_ = t.Rollback(ctx)
		return zero, err
	}
	if err := t.Commit(ctx); err != nil {
		return zero, err
	}
	return out, nil
}

// targetTable returns the quoted, schema-qualified table of ref and its
// schema. Identifiers come only from Describe, never from caller text.
func targetTable(ctx context.Context, db queryer, op string, ref driver.CollectionRef) (string, *driver.CollectionSchema, error) {
	schema, err := describeOn(ctx, db, ref)
	if err != nil {
		return "", nil, err
	}
	if schema.Type != driver.CollectionTable {
		return "", nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is a %s, not a table", schema.Ref.Name, schema.Type))
	}
	return quoteIdent(schema.Ref.Namespace) + "." + quoteIdent(schema.Ref.Name), schema, nil
}

func (t *tx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	sc, err := t.writer(ctx, op)
	if err != nil {
		return nil, err
	}
	table, schema, err := targetTable(ctx, sc, op, ref)
	if err != nil {
		return nil, err
	}
	text := "INSERT INTO " + table + " DEFAULT VALUES"
	var params []driver.Value
	if len(values) > 0 {
		cols := make([]string, len(values))
		marks := make([]string, len(values))
		for i, m := range values {
			if cols[i], err = column(op, schema, m.Key); err != nil {
				return nil, err
			}
			marks[i] = "?"
			params = append(params, m.Value)
		}
		text = "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	}
	if len(schema.PrimaryKey) == 0 {
		if _, err := sc.ExecContext(ctx, text, toArgs(params)...); err != nil {
			return nil, mapError(op, err)
		}
		return nil, nil
	}
	quoted := make([]string, len(schema.PrimaryKey))
	for i, pk := range schema.PrimaryKey {
		quoted[i] = quoteIdent(pk)
	}
	return insertReturning(ctx, sc, op, schema, text+" RETURNING "+strings.Join(quoted, ", "), params)
}

// insertReturning runs the INSERT and returns the stored primary key from
// its RETURNING clause, defaults and sequence values included.
func insertReturning(ctx context.Context, sc *sql.Conn, op string, schema *driver.CollectionSchema, text string, params []driver.Value) (driver.Object, error) {
	rows, err := sc.QueryContext(ctx, text, toArgs(params)...)
	if err != nil {
		return nil, mapError(op, err)
	}
	defer func() { _ = rows.Close() }()
	var key driver.Object
	if rows.Next() {
		raw := make([]any, len(schema.PrimaryKey))
		ptrs := make([]any, len(raw))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, mapError(op, err)
		}
		var conv converter
		for i, pk := range schema.PrimaryKey {
			var native string
			for _, f := range schema.Fields {
				if f.Name == pk {
					native = f.NativeType
				}
			}
			key = append(key, driver.Member{Key: pk, Value: conv.value(parseType(native), raw[i])})
		}
	}
	for rows.Next() { // step to the end so the statement completes
	}
	return key, mapError(op, rows.Err())
}

func (t *tx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	const op = "update"
	sc, err := t.writer(ctx, op)
	if err != nil {
		return err
	}
	table, schema, err := targetTable(ctx, sc, op, ref)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("there are no changes"))
	}
	sets := make([]string, len(changes))
	var params []driver.Value
	for i, m := range changes {
		col, err := column(op, schema, m.Key)
		if err != nil {
			return err
		}
		sets[i] = col + " = ?"
		params = append(params, m.Value)
	}
	where, kp, err := whereKey(op, schema, key)
	if err != nil {
		return err
	}
	if err := matchesOne(ctx, sc, op, table, where, kp); err != nil {
		return err
	}
	return affectOne(ctx, sc, op, "UPDATE "+table+" SET "+strings.Join(sets, ", ")+where, append(params, kp...))
}

func (t *tx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	sc, err := t.writer(ctx, op)
	if err != nil {
		return err
	}
	table, schema, err := targetTable(ctx, sc, op, ref)
	if err != nil {
		return err
	}
	where, params, err := whereKey(op, schema, key)
	if err != nil {
		return err
	}
	if err := matchesOne(ctx, sc, op, table, where, params); err != nil {
		return err
	}
	return affectOne(ctx, sc, op, "DELETE FROM "+table+where, params)
}

// whereKey renders the " WHERE …" clause matching key, with its values; a
// null value matches IS NULL. An empty key is ErrSyntax.
func whereKey(op string, schema *driver.CollectionSchema, key driver.Object) (string, []driver.Value, error) {
	if len(key) == 0 {
		return "", nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the key is empty"))
	}
	parts := make([]string, 0, len(key))
	var params []driver.Value
	for _, m := range key {
		col, err := column(op, schema, m.Key)
		if err != nil {
			return "", nil, err
		}
		if m.Value.IsNull() {
			parts = append(parts, col+" IS NULL")
			continue
		}
		parts = append(parts, col+" = ?")
		params = append(params, m.Value)
	}
	return " WHERE " + strings.Join(parts, " AND "), params, nil
}

// matchesOne requires the key to match exactly one row: none is
// ErrNotFound, more is ErrConflict, and nothing has changed either way.
func matchesOne(ctx context.Context, sc *sql.Conn, op, table, where string, params []driver.Value) error {
	var n int64
	if err := sc.QueryRowContext(ctx, "SELECT count(*) FROM "+table+where, toArgs(params)...).Scan(&n); err != nil {
		return mapError(op, err)
	}
	switch {
	case n == 0:
		return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
	case n > 1:
		return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
	}
	return nil
}

// affectOne runs the UPDATE or DELETE and checks it touched one row, as
// matchesOne promised inside the same snapshot.
func affectOne(ctx context.Context, sc *sql.Conn, op, text string, params []driver.Value) error {
	res, err := sc.ExecContext(ctx, text, toArgs(params)...)
	if err != nil {
		return mapError(op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapError(op, err)
	}
	if n != 1 {
		return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the statement changed %d rows, want 1: roll back", n))
	}
	return nil
}
