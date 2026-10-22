package snowflake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Mutator = (*conn)(nil)

// Snowflake records PRIMARY KEY and UNIQUE constraints but does not enforce
// them on standard tables, so the Mutator enforces its contract itself:
//
//   - Insert writes INSERT … SELECT … WHERE NOT EXISTS (a row with the same
//     primary key): when one exists nothing is inserted and the error is
//     ErrConstraint. Two concurrent inserts of one key can still both
//     succeed; the check is not a lock. The returned key is the primary
//     key's values as given; when a key column is left to the database
//     (AUTOINCREMENT/IDENTITY or a default) the key is nil, because
//     Snowflake has no RETURNING and a follow-up read could pick another
//     session's row. Give the key explicitly to get it back.
//   - Update and Delete run in a transaction of their own and must touch
//     exactly one row: none is ErrNotFound; more than one is rolled back
//     and reported as ErrConflict.
//
// Names come only from Describe, never from the caller's text. Values are
// bound: a JSON value into a VARIANT, OBJECT or ARRAY column goes through
// PARSE_JSON, and a time is sent as text in the column's own form.
//
// A Tx is not a Mutator: Snowflake has no savepoints, so a conflicting
// match could not be undone without ending the caller's transaction.

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
	exprs := make([]string, len(values))
	given := map[string]driver.Value{}
	for i, m := range values {
		f, err := findField(op, schema, m.Key)
		if err != nil {
			return nil, err
		}
		cols[i] = quoteIdent(f.Name)
		if exprs[i], err = s.bind(f, m.Value); err != nil {
			return nil, err
		}
		given[f.Name] = m.Value
	}
	text := "INSERT INTO " + tableName(schema) + " (" + strings.Join(cols, ", ") + ") SELECT " + strings.Join(exprs, ", ")
	var key driver.Object
	for _, pk := range schema.PrimaryKey {
		v, ok := given[pk]
		if !ok || v.IsNull() {
			key = nil
			break
		}
		key = append(key, driver.Member{Key: pk, Value: v})
	}
	if key != nil {
		where, err := s.whereKey(schema, key)
		if err != nil {
			return nil, err
		}
		text += " WHERE NOT EXISTS (SELECT 1 FROM " + tableName(schema) + where + ")"
	}
	n, err := c.inTx(ctx, op, text, s.args, func(n int64) error { return nil })
	if err != nil {
		return nil, err
	}
	if n == 0 && key != nil {
		return nil, driver.Wrap(driver.ErrConstraint, Name, op, errors.New("a row with this primary key already exists"))
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
		expr, err := s.bind(f, m.Value)
		if err != nil {
			return err
		}
		sets[i] = quoteIdent(f.Name) + " = " + expr
	}
	where, err := s.whereKey(schema, key)
	if err != nil {
		return err
	}
	_, err = c.inTx(ctx, op, "UPDATE "+tableName(schema)+" SET "+strings.Join(sets, ", ")+where, s.args, exactlyOne(op))
	return err
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
	_, err = c.inTx(ctx, op, "DELETE FROM "+tableName(schema)+where, s.args, exactlyOne(op))
	return err
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

// exactlyOne is the check Update and Delete run on their row count.
func exactlyOne(op string) func(int64) error {
	return func(n int64) error {
		switch {
		case n == 0:
			return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
		case n > 1:
			return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
		}
		return nil
	}
}

// inTx runs one DML statement in a transaction of its own: COMMIT when
// check accepts its row count, ROLLBACK otherwise.
func (c *conn) inTx(ctx context.Context, op, text string, args []any, check func(int64) error) (int64, error) {
	t, err := c.begin(ctx, op, driver.TxOptions{})
	if err != nil {
		return 0, err
	}
	res, err := t.tx.ExecContext(sf.WithMultiStatement(ctx, 1), text, args...)
	if err != nil {
		_ = t.Rollback(context.WithoutCancel(ctx))
		return 0, c.scrub(mapError(ctx, op, err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		_ = t.Rollback(context.WithoutCancel(ctx))
		return 0, c.scrub(mapError(ctx, op, err))
	}
	if err := check(n); err != nil {
		_ = t.Rollback(context.WithoutCancel(ctx))
		return 0, err
	}
	if err := t.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

// tableName is the fully qualified, quoted name of a described table.
func tableName(schema *driver.CollectionSchema) string {
	db, sch, _ := splitNamespace(schema.Ref.Namespace)
	return quoteIdent(db) + "." + quoteIdent(sch) + "." + quoteIdent(schema.Ref.Name)
}

// stmt builds one statement with ? parameters.
type stmt struct {
	op   string
	args []any
}

// bind adds v as a parameter for column f and returns the expression that
// reads it.
func (s *stmt) bind(f driver.Field, v driver.Value) (string, error) {
	native := strings.ToUpper(f.NativeType)
	var arg any
	var err error
	if t, ok := v.AsTime(); ok {
		arg = timeText(native, t)
	} else if arg, err = bindValue(v); err != nil {
		return "", driver.Wrap(driver.ErrUnsupported, Name, s.op, fmt.Errorf("field %q: %w", f.Name, err))
	}
	s.args = append(s.args, arg)
	if v.Type() != driver.TypeJSON {
		return "?", nil
	}
	switch {
	case strings.HasPrefix(native, "OBJECT"):
		return "PARSE_JSON(?)::OBJECT", nil
	case strings.HasPrefix(native, "ARRAY"):
		return "PARSE_JSON(?)::ARRAY", nil
	}
	return "PARSE_JSON(?)", nil
}

// timeText renders t for a column of the given native type: a
// TIMESTAMP_TZ as RFC 3339 with t's own offset (the column stores it), a
// DATE as its day, a TIME as its time of day, a TIMESTAMP_NTZ as its UTC
// wall time, and anything else as RFC 3339 in UTC.
func timeText(native string, t time.Time) string {
	if strings.HasPrefix(native, "TIMESTAMP_TZ") {
		return t.Format(time.RFC3339Nano)
	}
	t = t.UTC()
	switch {
	case native == "DATE":
		return t.Format(time.DateOnly)
	case native == "TIME" || strings.HasPrefix(native, "TIME("):
		return t.Format("15:04:05.999999999")
	case strings.HasPrefix(native, "TIMESTAMP_NTZ") || native == "DATETIME":
		return t.Format("2006-01-02 15:04:05.999999999")
	}
	return t.Format(time.RFC3339Nano)
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
		expr, err := s.bind(f, m.Value)
		if err != nil {
			return "", err
		}
		parts[i] = col + " = " + expr
	}
	return " WHERE " + strings.Join(parts, " AND "), nil
}
