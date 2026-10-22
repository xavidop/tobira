package oracle

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	go_ora "github.com/sijms/go-ora/v2"

	"github.com/xavidop/tobira/driver"
)

const savepointMutation = "tobira_mutation"

// Insert, Update and Delete each run in their own transaction on a
// dedicated session, so a conflicting match rolls back before anything is
// visible. On a read-only connection they fail with ErrReadOnly before any
// statement is sent.

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	return withTx(ctx, c, "insert", func(t *oraTx) (driver.Object, error) { return t.Insert(ctx, ref, values) })
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	_, err := withTx(ctx, c, "update", func(t *oraTx) (driver.Object, error) { return nil, t.Update(ctx, ref, key, changes) })
	return err
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	_, err := withTx(ctx, c, "delete", func(t *oraTx) (driver.Object, error) { return nil, t.Delete(ctx, ref, key) })
	return err
}

// withTx runs fn in a fresh read-write transaction: COMMIT when fn
// succeeds, ROLLBACK otherwise.
func withTx[T any](ctx context.Context, c *conn, op string, fn func(*oraTx) (T, error)) (T, error) {
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

// table is the target of a mutation: its quoted name and schema.
type table struct {
	name   string
	schema *driver.CollectionSchema
}

// target describes ref. Identifiers come only from Describe, never from the
// caller's text.
func (t *oraTx) target(ctx context.Context, op string, ref driver.CollectionRef) (*table, error) {
	schema, err := t.c.Describe(ctx, ref)
	if err != nil {
		var de *driver.Error
		if errors.As(err, &de) {
			de.Op = op
		}
		return nil, err
	}
	if schema.Type != driver.CollectionTable {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is a %s, not a table", schema.Ref.Name, schema.Type))
	}
	return &table{name: quoteIdent(schema.Ref.Namespace) + "." + quoteIdent(schema.Ref.Name), schema: schema}, nil
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

// stmt accumulates one statement's bind parameters (:1, :2 …).
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
	return ":" + strconv.Itoa(len(s.args)), nil
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

// inSavepoint runs fn inside a savepoint, rolled back to when fn fails, so a
// failed mutation leaves a caller's transaction as it was.
func (t *oraTx) inSavepoint(ctx context.Context, op string, fn func() error) error {
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+savepointMutation); err != nil {
		return mapError(ctx, op, err)
	}
	if err := fn(); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
		defer cancel()
		_, _ = t.tx.ExecContext(rctx, "ROLLBACK TO SAVEPOINT "+savepointMutation)
		return err
	}
	return nil
}

// mutation claims the transaction's session for a write, runs fn, and then
// proves the transaction is still the same one.
func (t *oraTx) mutation(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	cctx, release, err := t.claim(ctx, op, true)
	if err != nil {
		return err
	}
	err = fn(cctx)
	if perr := t.statementDone(ctx, false); err == nil {
		err = perr
	}
	release()
	return err
}

// Insert adds one row and returns its primary key, read back by the new
// row's ROWID (RETURNING ROWID INTO), so identity and default key values
// are included.
func (t *oraTx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	var key driver.Object
	err := t.mutation(ctx, op, func(ctx context.Context) error {
		tb, err := t.target(ctx, op, ref)
		if err != nil {
			return err
		}
		s := &stmt{op: op}
		cols := make([]string, len(values))
		marks := make([]string, len(values))
		for i, m := range values {
			name, err := field(op, tb.schema, m.Key)
			if err != nil {
				return err
			}
			cols[i] = quoteIdent(name)
			if marks[i], err = s.bind(m.Value); err != nil {
				return err
			}
		}
		text := "INSERT INTO " + tb.name
		if len(values) == 0 { // Oracle has no DEFAULT VALUES
			if len(tb.schema.Fields) == 0 {
				return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the table has no columns"))
			}
			text += " (" + quoteIdent(tb.schema.Fields[0].Name) + ") VALUES (DEFAULT)"
		} else {
			text += " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
		}
		var rowid string
		text += " RETURNING ROWID INTO :" + strconv.Itoa(len(s.args)+1)
		args := append(s.args, go_ora.Out{Dest: &rowid, Size: 64})
		return t.inSavepoint(ctx, op, func() error {
			if _, err := t.tx.ExecContext(ctx, text, args...); err != nil {
				return mapError(ctx, op, err)
			}
			if len(tb.schema.PrimaryKey) == 0 {
				return nil
			}
			pk := make([]string, len(tb.schema.PrimaryKey))
			for i, name := range tb.schema.PrimaryKey {
				pk[i] = quoteIdent(name)
			}
			rs, err := t.tx.QueryContext(ctx, "SELECT "+strings.Join(pk, ", ")+" FROM "+tb.name+" WHERE ROWID = CHARTOROWID(:1)", rowid)
			if err != nil {
				return mapError(ctx, op, err)
			}
			r, err := newResultSet(ctx, func() {}, rs, 0, time.Now(), nil)
			if err != nil {
				return err
			}
			row, ok, err := r.Next(ctx)
			_ = r.Close()
			if err != nil {
				return err
			}
			if !ok {
				return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("the inserted row could not be read back"))
			}
			for i, name := range tb.schema.PrimaryKey {
				key = append(key, driver.Member{Key: name, Value: row[i]})
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// Update changes the one row matching key. Oracle reports the rows an
// UPDATE matched, so "exactly one row" means rows matched: none is
// ErrNotFound; more than one is undone and reported as ErrConflict.
func (t *oraTx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	const op = "update"
	return t.mutation(ctx, op, func(ctx context.Context) error {
		tb, err := t.target(ctx, op, ref)
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
		where, err := s.whereKey(tb.schema, key)
		if err != nil {
			return err
		}
		return t.inSavepoint(ctx, op, func() error {
			res, err := t.tx.ExecContext(ctx, "UPDATE "+tb.name+" SET "+strings.Join(sets, ", ")+where, s.args...)
			if err != nil {
				return mapError(ctx, op, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return mapError(ctx, op, err)
			}
			return oneRow(op, n)
		})
	})
}

// Delete removes the one row matching key: none is ErrNotFound; more than
// one (it stops after two) is undone and reported as ErrConflict.
func (t *oraTx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	return t.mutation(ctx, op, func(ctx context.Context) error {
		tb, err := t.target(ctx, op, ref)
		if err != nil {
			return err
		}
		s := &stmt{op: op}
		where, err := s.whereKey(tb.schema, key)
		if err != nil {
			return err
		}
		return t.inSavepoint(ctx, op, func() error {
			res, err := t.tx.ExecContext(ctx, "DELETE FROM "+tb.name+where+" AND ROWNUM <= 2", s.args...)
			if err != nil {
				return mapError(ctx, op, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return mapError(ctx, op, err)
			}
			return oneRow(op, n)
		})
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
