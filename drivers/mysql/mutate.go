package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

const savepointMutation = "tobira_mutation"

// Insert, Update and Delete each run in their own transaction on a
// dedicated connection, so a conflicting match rolls back before anything is
// visible. On a read-only connection they fail with ErrReadOnly before any
// statement is sent.

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (_ driver.Object, err error) {
	defer func() { err = c.named(err) }()
	return withTx(ctx, c, "insert", func(t *myTx) (driver.Object, error) { return t.Insert(ctx, ref, values) })
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) (err error) {
	defer func() { err = c.named(err) }()
	_, err = withTx(ctx, c, "update", func(t *myTx) (driver.Object, error) { return nil, t.Update(ctx, ref, key, changes) })
	return err
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) (err error) {
	defer func() { err = c.named(err) }()
	_, err = withTx(ctx, c, "delete", func(t *myTx) (driver.Object, error) { return nil, t.Delete(ctx, ref, key) })
	return err
}

// withTx runs fn in a fresh read-write transaction: COMMIT when fn succeeds,
// ROLLBACK otherwise.
func withTx[T any](ctx context.Context, c *conn, op string, fn func(*myTx) (T, error)) (T, error) {
	var zero T
	if err := c.check(ctx, op); err != nil {
		return zero, err
	}
	if c.readOnly {
		return zero, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyDB)
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

// table is the target of a mutation: its quoted name, schema and
// AUTO_INCREMENT column ("" when it has none).
type table struct {
	name    string
	schema  *driver.CollectionSchema
	autoInc string
}

const autoIncSQL = `
SELECT COLUMN_NAME FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND EXTRA LIKE '%auto_increment%'`

// target describes ref over sc. Identifiers come only from Describe, never
// from the caller's text.
func target(ctx context.Context, f flavor, sc *sql.Conn, op string, ref driver.CollectionRef) (*table, error) {
	schema, err := describeIn(ctx, f, sc, op, ref)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if schema.Type != driver.CollectionTable {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is a %s, not a table", schema.Ref.Name, schema.Type))
	}
	tb := &table{name: quoteIdent(schema.Ref.Namespace) + "." + quoteIdent(schema.Ref.Name), schema: schema}
	err = sc.QueryRowContext(ctx, autoIncSQL, schema.Ref.Namespace, schema.Ref.Name).Scan(&tb.autoInc)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, mapError(ctx, op, err)
	}
	return tb, nil
}

// field resolves a caller's field name to the column's own name: the exact
// name wins, otherwise a unique case-insensitive match. Unknown or ambiguous
// → ErrNotFound.
func field(op string, schema *driver.CollectionSchema, name string) (string, error) {
	var folded []string
	for _, f := range schema.Fields {
		if f.Name == name {
			return f.Name, nil
		}
		if strings.EqualFold(f.Name, name) {
			folded = append(folded, f.Name)
		}
	}
	switch len(folded) {
	case 1:
		return folded[0], nil
	case 0:
		return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no field %q in %s", name, schema.Ref.Name))
	}
	return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("field %q is ambiguous in %s (%s)", name, schema.Ref.Name, strings.Join(folded, ", ")))
}

// stmt accumulates one statement's parameters.
type stmt struct {
	op   string
	args []any
}

// bind adds a parameter and returns its placeholder.
func (s *stmt) bind(v driver.Value) (string, error) {
	a, err := toArg(v)
	if err != nil {
		return "", driver.Wrap(driver.ErrUnsupported, Name, s.op, fmt.Errorf("parameter %d: %w", len(s.args)+1, err))
	}
	s.args = append(s.args, a)
	return "?", nil
}

// whereKey renders the WHERE clause matching key; a null value matches IS NULL.
func (s *stmt) whereKey(schema *driver.CollectionSchema, key driver.Object) (string, error) {
	if len(key) == 0 {
		return "", driver.Wrap(driver.ErrSyntax, Name, s.op, errors.New("the key is empty"))
	}
	parts := make([]string, len(key))
	for i, m := range key {
		name, err := field(s.op, schema, m.Key)
		if err != nil {
			return "", err
		}
		if m.Value.IsNull() {
			parts[i] = quoteIdent(name) + " IS NULL"
			continue
		}
		ph, err := s.bind(m.Value)
		if err != nil {
			return "", err
		}
		parts[i] = quoteIdent(name) + " = " + ph
	}
	return " WHERE " + strings.Join(parts, " AND "), nil
}

// inSavepoint runs fn inside a savepoint, rolled back when fn fails, so a
// failed mutation leaves a caller's transaction as it was.
func inSavepoint(ctx context.Context, sc *sql.Conn, op string, fn func() error) error {
	if _, err := sc.ExecContext(ctx, "SAVEPOINT "+savepointMutation); err != nil {
		return mapError(ctx, op, err)
	}
	if err := fn(); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_, _ = sc.ExecContext(rctx, "ROLLBACK TO SAVEPOINT "+savepointMutation)
		_, _ = sc.ExecContext(rctx, "RELEASE SAVEPOINT "+savepointMutation)
		return err
	}
	_, err := sc.ExecContext(ctx, "RELEASE SAVEPOINT "+savepointMutation)
	return mapError(ctx, op, err)
}

// Insert adds one row and returns its primary key. MySQL has no RETURNING:
// an AUTO_INCREMENT key column that was not supplied (or was supplied as
// NULL or 0) gets LAST_INSERT_ID(); every other key column is the supplied
// value. A key column that is neither supplied nor AUTO_INCREMENT takes its
// default on the server and is left out of the returned key.
func (t *myTx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (_ driver.Object, err error) {
	defer func() { err = t.c.named(err) }()
	const op = "insert"
	sc, ctx, release, err := t.writer(ctx, op)
	if err != nil {
		return nil, err
	}
	defer release()
	tb, err := target(ctx, t.c.syntaxFlavor(), sc, op, ref)
	if err != nil {
		return nil, err
	}
	s := &stmt{op: op}
	supplied := map[string]driver.Value{}
	cols := make([]string, len(values))
	marks := make([]string, len(values))
	for i, m := range values {
		name, err := field(op, tb.schema, m.Key)
		if err != nil {
			return nil, err
		}
		cols[i] = quoteIdent(name)
		if marks[i], err = s.bind(m.Value); err != nil {
			return nil, err
		}
		supplied[name] = m.Value
	}
	text := "INSERT INTO " + tb.name + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	var key driver.Object
	err = inSavepoint(ctx, sc, op, func() error {
		res, err := sc.ExecContext(ctx, text, s.args...)
		if err != nil {
			return mapError(ctx, op, err)
		}
		for _, pk := range tb.schema.PrimaryKey {
			v, given := supplied[pk]
			if pk == tb.autoInc && (!given || v.IsNull() || isZeroInt(v)) {
				id, err := res.LastInsertId()
				if err != nil {
					return mapError(ctx, op, err)
				}
				v, given = driver.Int(id), true
			}
			if given {
				key = append(key, driver.Member{Key: pk, Value: v})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

func isZeroInt(v driver.Value) bool {
	n, ok := v.AsInt()
	return ok && v.Type() == driver.TypeInt && n == 0
}

// Update changes the one row matching key. It first counts the matching rows
// under FOR UPDATE, so "exactly one row" means rows matched, not rows
// changed: MySQL reports 0 affected rows for an UPDATE that leaves the
// values as they were, which is still a success. None matching is
// ErrNotFound and more than one is ErrConflict; nothing is changed then.
func (t *myTx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) (err error) {
	defer func() { err = t.c.named(err) }()
	const op = "update"
	sc, ctx, release, err := t.writer(ctx, op)
	if err != nil {
		return err
	}
	defer release()
	tb, err := target(ctx, t.c.syntaxFlavor(), sc, op, ref)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("there are no changes"))
	}
	s := &stmt{op: op}
	sets := make([]string, len(changes))
	for i, m := range changes {
		name, err := field(op, tb.schema, m.Key)
		if err != nil {
			return err
		}
		ph, err := s.bind(m.Value)
		if err != nil {
			return err
		}
		sets[i] = quoteIdent(name) + " = " + ph
	}
	w := &stmt{op: op}
	where, err := w.whereKey(tb.schema, key)
	if err != nil {
		return err
	}
	return inSavepoint(ctx, sc, op, func() error {
		var n int64
		if err := sc.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tb.name+where+" FOR UPDATE", w.args...).Scan(&n); err != nil {
			return mapError(ctx, op, err)
		}
		if err := oneRow(op, n); err != nil {
			return err
		}
		res, err := sc.ExecContext(ctx, "UPDATE "+tb.name+" SET "+strings.Join(sets, ", ")+where, append(s.args, w.args...)...)
		if err != nil {
			return mapError(ctx, op, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return mapError(ctx, op, err)
		} else if n > 1 { // cannot happen under the lock; undo rather than trust it
			return oneRow(op, n)
		}
		return nil
	})
}

// Delete removes the one row matching key: none is ErrNotFound; more than
// one (it stops after two) is undone and reported as ErrConflict.
func (t *myTx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) (err error) {
	defer func() { err = t.c.named(err) }()
	const op = "delete"
	sc, ctx, release, err := t.writer(ctx, op)
	if err != nil {
		return err
	}
	defer release()
	tb, err := target(ctx, t.c.syntaxFlavor(), sc, op, ref)
	if err != nil {
		return err
	}
	s := &stmt{op: op}
	where, err := s.whereKey(tb.schema, key)
	if err != nil {
		return err
	}
	return inSavepoint(ctx, sc, op, func() error {
		// LIMIT 2 is enough to tell one matching row from several.
		res, err := sc.ExecContext(ctx, "DELETE FROM "+tb.name+where+" LIMIT 2", s.args...)
		if err != nil {
			return mapError(ctx, op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapError(ctx, op, err)
		}
		return oneRow(op, n)
	})
}

// oneRow maps a matched-row count other than one to its error.
func oneRow(op string, n int64) error {
	switch {
	case n == 0:
		return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
	case n > 1:
		return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
	}
	return nil
}
