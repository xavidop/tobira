package libsql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// The Mutator mirrors the sqlite driver's (drivers/sqlite mutate.go):
// identifiers come only from Describe, values are bound, and an UPDATE or
// DELETE must touch exactly one row inside a savepoint.

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
	if c.readOnly {
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
		_ = t.Rollback(ctx)
		return zero, err
	}
	return out, nil
}

// writer refuses mutations in a read-only transaction.
func (t *tx) writer(op string) error {
	if err := t.live(op); err != nil {
		return err
	}
	if t.readOnly {
		return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the transaction is read-only"))
	}
	return nil
}

// target returns the quoted, schema-qualified table of ref along with its
// schema.
func target(ctx context.Context, ex execer, op string, ref driver.CollectionRef) (string, *driver.CollectionSchema, error) {
	schema, err := describeOn(ctx, ex, ref)
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
	if err := t.writer(op); err != nil {
		return nil, err
	}
	table, schema, err := target(ctx, t, op, ref)
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
	s, err := statement(op, text, params, true)
	if err != nil {
		return nil, err
	}
	res, _, err := t.run(ctx, op, []hranaStmt{s}, false)
	if err != nil {
		return nil, err
	}
	return insertedKey(op, schema, res[0])
}

// insertedKey reads the stored primary key from the INSERT's RETURNING row:
// supplied values, defaults and the generated rowid alike. A table without a
// primary key returns a nil key.
func insertedKey(op string, schema *driver.CollectionSchema, r *stmtResult) (driver.Object, error) {
	if len(schema.PrimaryKey) == 0 {
		return nil, nil
	}
	rows, err := decodeRows(r)
	if err != nil {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	if len(rows) == 0 || len(rows[0]) != len(schema.PrimaryKey) {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errors.New("the insert returned no key"))
	}
	var key driver.Object
	for i, pk := range schema.PrimaryKey {
		declared := ""
		for _, f := range schema.Fields {
			if f.Name == pk {
				declared = strings.TrimSpace(f.NativeType)
			}
		}
		key = append(key, driver.Member{Key: pk, Value: convertValue(declared, rows[0][i])})
	}
	return key, nil
}

func (t *tx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	const op = "update"
	if err := t.writer(op); err != nil {
		return err
	}
	table, schema, err := target(ctx, t, op, ref)
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
	return t.affectOne(ctx, op, "UPDATE "+table+" SET "+strings.Join(sets, ", ")+where, append(params, kp...))
}

func (t *tx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := t.writer(op); err != nil {
		return err
	}
	table, schema, err := target(ctx, t, op, ref)
	if err != nil {
		return err
	}
	where, params, err := whereKey(op, schema, key)
	if err != nil {
		return err
	}
	return t.affectOne(ctx, op, "DELETE FROM "+table+where, params)
}

// affectOne runs a single UPDATE or DELETE inside a savepoint and requires it
// to touch exactly one row: none is ErrNotFound; more than one is undone and
// reported as ErrConflict, leaving the surrounding transaction usable.
func (t *tx) affectOne(ctx context.Context, op, text string, params []driver.Value) error {
	s, err := statement(op, text, params, false)
	if err != nil {
		return err
	}
	undo := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		defer cancel()
		_, _, _ = t.run(rctx, op, []hranaStmt{{SQL: "ROLLBACK TO " + savepointName}, {SQL: "RELEASE " + savepointName}}, false)
	}
	res, _, err := t.run(ctx, op, []hranaStmt{{SQL: "SAVEPOINT " + savepointName}, s}, false)
	if err != nil {
		undo()
		return err
	}
	switch n := res[1].AffectedRowCount; {
	case n == 0:
		undo()
		return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
	case n > 1:
		undo()
		return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
	}
	_, _, err = t.run(ctx, op, []hranaStmt{{SQL: "RELEASE " + savepointName}}, false)
	return err
}
