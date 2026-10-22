package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

const savepointMutation = "tobira_mutation"

// Insert, Update and Delete each run in their own transaction, so a
// conflicting match rolls back before anything is visible.

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (_ driver.Object, err error) {
	defer func() { err = c.named(err) }()
	return withTx(ctx, c, "insert", func(t *pgTx) (driver.Object, error) { return t.Insert(ctx, ref, values) })
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) (err error) {
	defer func() { err = c.named(err) }()
	_, err = withTx(ctx, c, "update", func(t *pgTx) (driver.Object, error) { return nil, t.Update(ctx, ref, key, changes) })
	return err
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) (err error) {
	defer func() { err = c.named(err) }()
	_, err = withTx(ctx, c, "delete", func(t *pgTx) (driver.Object, error) { return nil, t.Delete(ctx, ref, key) })
	return err
}

// withTx runs fn in a fresh read-write transaction: COMMIT when fn succeeds,
// ROLLBACK otherwise. A read-only connection fails in begin with ErrReadOnly.
func withTx[T any](ctx context.Context, c *conn, op string, fn func(*pgTx) (T, error)) (T, error) {
	var zero T
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

// target describes ref over tx and returns its quoted, schema-qualified name.
// Identifiers come only from Describe, never from the caller's text.
func target(ctx context.Context, tx pgx.Tx, f flavor, op string, ref driver.CollectionRef) (string, *driver.CollectionSchema, error) {
	schema, err := describeIn(ctx, tx, f, op, ref)
	if err != nil {
		return "", nil, mapError(ctx, op, err)
	}
	if schema.Type != driver.CollectionTable {
		return "", nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is a %s, not a table", schema.Ref.Name, schema.Type))
	}
	return pgx.Identifier{schema.Ref.Namespace, schema.Ref.Name}.Sanitize(), schema, nil
}

// column resolves a caller's field name to the quoted column: the exact name
// wins, otherwise a unique case-insensitive match. Unknown → ErrNotFound.
func column(op string, schema *driver.CollectionSchema, name string) (string, error) {
	var folded []string
	for _, f := range schema.Fields {
		if f.Name == name {
			return pgx.Identifier{f.Name}.Sanitize(), nil
		}
		if strings.EqualFold(f.Name, name) {
			folded = append(folded, f.Name)
		}
	}
	switch len(folded) {
	case 1:
		return pgx.Identifier{folded[0]}.Sanitize(), nil
	case 0:
		return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no field %q in %s", name, schema.Ref.Name))
	}
	return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("field %q is ambiguous in %s", name, schema.Ref.Name))
}

// stmt builds one statement with numbered parameters.
type stmt struct {
	op   string
	args []any
}

// bind adds a parameter and returns its placeholder.
func (s *stmt) bind(v driver.Value) (string, error) {
	a, err := paramToPG(v)
	if err != nil {
		return "", driver.Wrap(driver.ErrUnsupported, Name, s.op, fmt.Errorf("parameter %d: %w", len(s.args)+1, err))
	}
	s.args = append(s.args, a)
	return "$" + strconv.Itoa(len(s.args)), nil
}

// whereKey renders the WHERE clause matching key; a null value matches IS NULL.
func (s *stmt) whereKey(schema *driver.CollectionSchema, key driver.Object) (string, error) {
	if len(key) == 0 {
		return "", driver.Wrap(driver.ErrSyntax, Name, s.op, errors.New("the key is empty"))
	}
	parts := make([]string, len(key))
	for i, m := range key {
		col, err := column(s.op, schema, m.Key)
		if err != nil {
			return "", err
		}
		if m.Value.Type() == driver.TypeNull {
			parts[i] = col + " IS NULL"
			continue
		}
		ph, err := s.bind(m.Value)
		if err != nil {
			return "", err
		}
		parts[i] = col + " = " + ph
	}
	return " WHERE " + strings.Join(parts, " AND "), nil
}

// inSavepoint runs fn inside a savepoint, rolled back when fn fails, so a
// failed mutation leaves a caller's transaction usable.
func inSavepoint(ctx context.Context, tx pgx.Tx, op string, fn func() error) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT "+savepointMutation); err != nil {
		return mapError(ctx, op, err)
	}
	if err := fn(); err != nil {
		rctx := context.WithoutCancel(ctx)
		_, _ = tx.Exec(rctx, "ROLLBACK TO SAVEPOINT "+savepointMutation)
		_, _ = tx.Exec(rctx, "RELEASE SAVEPOINT "+savepointMutation)
		return err
	}
	_, err := tx.Exec(ctx, "RELEASE SAVEPOINT "+savepointMutation)
	return mapError(ctx, op, err)
}

func (t *pgTx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (_ driver.Object, err error) {
	defer func() { err = t.c.named(err) }()
	const op = "insert"
	tx, err := t.writer(ctx, op)
	if err != nil {
		return nil, err
	}
	table, schema, err := target(ctx, tx, t.c.flavor, op, ref)
	if err != nil {
		return nil, err
	}
	s := &stmt{op: op}
	text := "INSERT INTO " + table + " DEFAULT VALUES"
	if len(values) > 0 {
		cols := make([]string, len(values))
		marks := make([]string, len(values))
		for i, m := range values {
			if cols[i], err = column(op, schema, m.Key); err != nil {
				return nil, err
			}
			if marks[i], err = s.bind(m.Value); err != nil {
				return nil, err
			}
		}
		text = "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	}
	if len(schema.PrimaryKey) > 0 {
		quoted := make([]string, len(schema.PrimaryKey))
		for i, pk := range schema.PrimaryKey {
			quoted[i] = pgx.Identifier{pk}.Sanitize()
		}
		text += " RETURNING " + strings.Join(quoted, ", ")
	}
	var key driver.Object
	err = inSavepoint(ctx, tx, op, func() error {
		var err error
		key, err = insertReturning(ctx, tx, op, text, s.args, schema.PrimaryKey)
		return err
	})
	return key, err
}

func (t *pgTx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) (err error) {
	defer func() { err = t.c.named(err) }()
	const op = "update"
	tx, err := t.writer(ctx, op)
	if err != nil {
		return err
	}
	table, schema, err := target(ctx, tx, t.c.flavor, op, ref)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("there are no changes"))
	}
	s := &stmt{op: op}
	sets := make([]string, len(changes))
	for i, m := range changes {
		col, err := column(op, schema, m.Key)
		if err != nil {
			return err
		}
		ph, err := s.bind(m.Value)
		if err != nil {
			return err
		}
		sets[i] = col + " = " + ph
	}
	where, err := s.whereKey(schema, key)
	if err != nil {
		return err
	}
	return affectOne(ctx, tx, op, "UPDATE "+table+" SET "+strings.Join(sets, ", ")+where, s.args)
}

func (t *pgTx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) (err error) {
	defer func() { err = t.c.named(err) }()
	const op = "delete"
	tx, err := t.writer(ctx, op)
	if err != nil {
		return err
	}
	table, schema, err := target(ctx, tx, t.c.flavor, op, ref)
	if err != nil {
		return err
	}
	s := &stmt{op: op}
	where, err := s.whereKey(schema, key)
	if err != nil {
		return err
	}
	return affectOne(ctx, tx, op, "DELETE FROM "+table+where, s.args)
}

// affectOne runs a single UPDATE or DELETE inside a savepoint and requires it
// to touch exactly one row: none is ErrNotFound; more than one is undone and
// reported as ErrConflict, leaving the surrounding transaction usable.
func affectOne(ctx context.Context, tx pgx.Tx, op, text string, args []any) error {
	return inSavepoint(ctx, tx, op, func() error {
		tag, err := tx.Exec(ctx, text, args...)
		if err != nil {
			return mapError(ctx, op, err)
		}
		switch n := tag.RowsAffected(); {
		case n == 0:
			return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
		case n > 1:
			return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
		}
		return nil
	})
}

// insertReturning runs an INSERT … RETURNING <key columns> and returns the key.
// text without a RETURNING clause (a table with no key) yields a nil key.
func insertReturning(ctx context.Context, tx pgx.Tx, op, text string, args []any, pk []string) (driver.Object, error) {
	rows, err := tx.Query(ctx, text, args...)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer rows.Close()
	var key driver.Object
	if rows.Next() {
		fds, raw, tm := rows.FieldDescriptions(), rows.RawValues(), rows.Conn().TypeMap()
		for i, fd := range fds {
			v, err := valueFromPG(tm, fd.DataTypeOID, fd.Format, "", slices.Clone(raw[i]))
			if err != nil {
				return nil, mapError(ctx, op, fmt.Errorf("column %q: %w", fd.Name, err))
			}
			key = append(key, driver.Member{Key: pk[i], Value: v})
		}
	}
	rows.Close()
	return key, mapError(ctx, op, rows.Err())
}
