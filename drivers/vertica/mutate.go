package vertica

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Mutator = (*conn)(nil)

// Vertica records PRIMARY KEY constraints but enforces them only when they
// are ENABLED, so the Mutator enforces its contract itself, each call in a
// transaction of its own:
//
//   - Insert needs every primary key value (ErrSyntax otherwise), except a
//     single AUTO_INCREMENT or IDENTITY key, and first counts the rows with
//     the same primary key: when there is
//     one, nothing is inserted and the error is ErrConstraint. Two
//     concurrent inserts of one key can still both succeed; the check is not
//     a lock. The returned key is the primary key's values as given; a
//     single AUTO_INCREMENT or IDENTITY key left out is read back with
//     LAST_INSERT_ID() on the same session.
//   - Update and Delete must touch exactly one row: none is ErrNotFound;
//     more than one is rolled back and reported as ErrConflict.
//
// Names come only from Describe, never from the caller's text. Values are
// bound; a time is sent as text in the column's own form.
//
// A Tx is not a Mutator.

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	schema, err := c.mutationTarget(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("there are no values"))
	}
	s := &stmt{op: op}
	cols := make([]string, len(values))
	marks := make([]string, len(values))
	given := map[string]driver.Value{}
	for i, m := range values {
		f, err := findField(op, schema, m.Key)
		if err != nil {
			return nil, err
		}
		cols[i] = quoteIdent(f.Name)
		if marks[i], err = s.bind(f, m.Value); err != nil {
			return nil, err
		}
		given[f.Name] = m.Value
	}
	var key driver.Object
	var autoKey *driver.Field
	for _, pk := range schema.PrimaryKey {
		v, ok := given[pk]
		if !ok || v.IsNull() {
			if f, err := findField(op, schema, pk); err == nil && f.AutoIncrement && len(schema.PrimaryKey) == 1 {
				autoKey = &f
				key = nil
				break
			}
			// Without the whole key the duplicate check cannot run and the
			// row could not be found again.
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf(
				"a value for primary key column %q is required: Vertica does not enforce primary keys, so the driver checks for a duplicate by the whole key", pk))
		}
		key = append(key, driver.Member{Key: pk, Value: v})
	}
	text := "INSERT INTO " + tableName(schema) + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	err = c.inTx(ctx, op, func(tx *session) error {
		if key != nil {
			ks := &stmt{op: op}
			where, err := ks.whereKey(schema, key)
			if err != nil {
				return err
			}
			var n int64
			if err := tx.queryRow(ctx, "SELECT COUNT(*) FROM "+tableName(schema)+where, ks.args, &n); err != nil {
				return c.scrub(mapError(ctx, op, err))
			}
			if n > 0 {
				return driver.Wrap(driver.ErrConstraint, Name, op, errors.New("a row with this primary key already exists"))
			}
		}
		if _, err := tx.exec(ctx, text, s.args...); err != nil {
			return c.scrub(mapError(ctx, op, err))
		}
		if autoKey != nil {
			var id int64
			if err := tx.queryRow(ctx, "SELECT LAST_INSERT_ID()", nil, &id); err != nil {
				return c.scrub(mapError(ctx, op, err))
			}
			key = driver.Object{{Key: autoKey.Name, Value: driver.Int(id)}}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	const op = "update"
	schema, err := c.mutationTarget(ctx, op, ref)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("there are no changes"))
	}
	s := &stmt{op: op}
	sets := make([]string, len(changes))
	for i, m := range changes {
		f, err := findField(op, schema, m.Key)
		if err != nil {
			return err
		}
		mark, err := s.bind(f, m.Value)
		if err != nil {
			return err
		}
		sets[i] = quoteIdent(f.Name) + " = " + mark
	}
	where, err := s.whereKey(schema, key)
	if err != nil {
		return err
	}
	return c.execOne(ctx, op, "UPDATE "+tableName(schema)+" SET "+strings.Join(sets, ", ")+where, s.args)
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	schema, err := c.mutationTarget(ctx, op, ref)
	if err != nil {
		return err
	}
	s := &stmt{op: op}
	where, err := s.whereKey(schema, key)
	if err != nil {
		return err
	}
	return c.execOne(ctx, op, "DELETE FROM "+tableName(schema)+where, s.args)
}

// mutationTarget checks the connection may write and describes ref, which
// must be a table.
func (c *conn) mutationTarget(ctx context.Context, op string, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyDB)
	}
	schema, err := c.describe(ctx, op, ref)
	if err != nil {
		return nil, c.scrub(err)
	}
	if schema.Type != driver.CollectionTable {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is a %s, not a table", schema.Ref.Name, schema.Type))
	}
	return schema, nil
}

// execOne runs one UPDATE or DELETE in a transaction of its own and commits
// only when it touched exactly one row.
func (c *conn) execOne(ctx context.Context, op, text string, args []any) error {
	return c.inTx(ctx, op, func(tx *session) error {
		n, err := tx.exec(ctx, text, args...)
		if err != nil {
			return c.scrub(mapError(ctx, op, err))
		}
		switch {
		case n == 0:
			return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
		case n > 1:
			return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
		}
		return nil
	})
}

// inTx runs fn in a transaction of its own: COMMIT when fn succeeds,
// ROLLBACK otherwise.
func (c *conn) inTx(ctx context.Context, op string, fn func(*session) error) error {
	t, err := c.begin(ctx, op, driver.TxOptions{})
	if err != nil {
		return err
	}
	if err := fn(t.s); err != nil {
		_ = t.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return t.Commit(ctx)
}

// stmt builds one statement with ? parameters.
type stmt struct {
	op   string
	args []any
}

// bind adds v as a parameter for column f and returns its placeholder.
func (s *stmt) bind(f driver.Field, v driver.Value) (string, error) {
	var arg any
	var err error
	if t, ok := v.AsTime(); ok {
		arg = timeText(f.NativeType, t)
	} else if arg, err = bindValue(v); err != nil {
		return "", driver.Wrap(driver.ErrUnsupported, Name, s.op, fmt.Errorf("field %q: %w", f.Name, err))
	}
	s.args = append(s.args, arg)
	return "?", nil
}

// timeText renders t for a column of the given catalog type: a TIMESTAMPTZ
// as RFC 3339 (the instant), a DATE as its day, a TIME as its time of day
// and a TIMESTAMP as its UTC wall clock, matching how values are read.
func timeText(native string, t time.Time) string {
	switch baseType(native) {
	case "TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE":
		return t.Format(time.RFC3339Nano)
	case "DATE":
		return t.UTC().Format(time.DateOnly)
	case "TIME":
		return t.UTC().Format("15:04:05.999999")
	case "TIMETZ", "TIME WITH TIME ZONE":
		return t.Format("15:04:05.999999-07:00")
	case "TIMESTAMP", "DATETIME", "SMALLDATETIME":
		return t.UTC().Format("2006-01-02 15:04:05.999999")
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// whereKey renders the WHERE clause matching key; a null value matches IS
// NULL.
func (s *stmt) whereKey(schema *driver.CollectionSchema, key driver.Object) (string, error) {
	if len(key) == 0 {
		return "", driver.Wrap(driver.ErrSyntax, Name, s.op, errors.New("the key is empty"))
	}
	parts := make([]string, len(key))
	for i, m := range key {
		f, err := findField(s.op, schema, m.Key)
		if err != nil {
			return "", err
		}
		col := quoteIdent(f.Name)
		if m.Value.IsNull() {
			parts[i] = col + " IS NULL"
			continue
		}
		mark, err := s.bind(f, m.Value)
		if err != nil {
			return "", err
		}
		parts[i] = col + " = " + mark
	}
	return " WHERE " + strings.Join(parts, " AND "), nil
}
