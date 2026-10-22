package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

const savepointName = "tobira_mutation"

// errReadOnlyMutation explains an ErrReadOnly from a Mutator call.
var errReadOnlyMutation = errors.New("the connection is read-only")

// Insert, Update and Delete run in their own IMMEDIATE transaction, so a
// conflicting match rolls back before anything is visible.

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

// withTx runs fn in a fresh read-write transaction: COMMIT when fn succeeds,
// ROLLBACK otherwise.
func withTx[T any](ctx context.Context, c *conn, op string, fn func(*tx) (T, error)) (T, error) {
	var zero T
	if err := c.check(ctx, op); err != nil {
		return zero, err
	}
	if c.rw == nil {
		return zero, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyMutation)
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

// target returns the quoted, schema-qualified table of ref along with its
// schema. Identifiers come only from Describe, never from the caller's text.
func target(ctx context.Context, db queryer, op string, ref driver.CollectionRef) (string, *driver.CollectionSchema, error) {
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
	table, schema, err := target(ctx, sc, op, ref)
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
	if len(schema.PrimaryKey) > 0 {
		quoted := make([]string, len(schema.PrimaryKey))
		for i, pk := range schema.PrimaryKey {
			quoted[i] = quoteIdent(pk)
		}
		text += " RETURNING " + strings.Join(quoted, ", ")
	}
	return insertReturning(ctx, sc, op, schema, text, params)
}

// insertReturning runs the INSERT and returns the stored primary key from its
// RETURNING clause: the supplied values, defaults and the generated rowid
// alike. A table without a primary key returns a nil key.
func insertReturning(ctx context.Context, sc *sql.Conn, op string, schema *driver.CollectionSchema, text string, params []driver.Value) (driver.Object, error) {
	rows, err := sc.QueryContext(ctx, text, toArgs(params)...)
	if err != nil {
		return nil, mapError(op, err)
	}
	defer func() { _ = rows.Close() }()
	var key driver.Object
	if len(schema.PrimaryKey) > 0 && rows.Next() {
		raw := make([]any, len(schema.PrimaryKey))
		ptrs := make([]any, len(raw))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, mapError(op, err)
		}
		for i, pk := range schema.PrimaryKey {
			declared := ""
			for _, f := range schema.Fields {
				if f.Name == pk {
					declared = strings.TrimSpace(f.NativeType)
				}
			}
			key = append(key, driver.Member{Key: pk, Value: convertValue(declared, raw[i])})
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
	table, schema, err := target(ctx, sc, op, ref)
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
	return affectOne(ctx, sc, op, "UPDATE "+table+" SET "+strings.Join(sets, ", ")+where, append(params, kp...))
}

func (t *tx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	sc, err := t.writer(ctx, op)
	if err != nil {
		return err
	}
	table, schema, err := target(ctx, sc, op, ref)
	if err != nil {
		return err
	}
	where, params, err := whereKey(op, schema, key)
	if err != nil {
		return err
	}
	return affectOne(ctx, sc, op, "DELETE FROM "+table+where, params)
}

// affectOne runs a single UPDATE or DELETE inside a savepoint and requires it
// to touch exactly one row: none is ErrNotFound; more than one is undone and
// reported as ErrConflict, leaving the surrounding transaction usable.
func affectOne(ctx context.Context, sc *sql.Conn, op, text string, params []driver.Value) error {
	if _, err := sc.ExecContext(ctx, "SAVEPOINT "+savepointName); err != nil {
		return mapError(op, err)
	}
	rollback := func() {
		rctx := context.WithoutCancel(ctx)
		_, _ = sc.ExecContext(rctx, "ROLLBACK TO "+savepointName)
		_, _ = sc.ExecContext(rctx, "RELEASE "+savepointName)
	}
	res, err := sc.ExecContext(ctx, text, toArgs(params)...)
	if err != nil {
		rollback()
		return mapError(op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		rollback()
		return mapError(op, err)
	}
	switch {
	case n == 0:
		rollback()
		return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
	case n > 1:
		rollback()
		return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
	}
	if _, err := sc.ExecContext(ctx, "RELEASE "+savepointName); err != nil {
		return mapError(op, err)
	}
	return nil
}

func toArgs(params []driver.Value) []any {
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = toArg(p)
	}
	return args
}
