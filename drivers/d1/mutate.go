package d1

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// The Mutator works by key through single statements, since D1's REST API
// has no transactions or savepoints: identifiers come only from Describe and
// values are bound. An UPDATE or DELETE carries its own guard, "AND (SELECT
// count(*) … WHERE <key>) = 1", so SQLite changes nothing unless the key
// matches exactly one row, atomically within the statement.

var (
	errReadOnlyMutation = errors.New("the connection is read-only")
	errChangesUnknown   = errors.New("the server did not report a change count; the statement may have run")
)

// target returns the quoted table of ref with its schema; a view is
// ErrUnsupported.
func (c *conn) target(ctx context.Context, op string, ref driver.CollectionRef) (string, *driver.CollectionSchema, error) {
	if err := c.check(ctx, op); err != nil {
		return "", nil, err
	}
	if c.readOnly {
		return "", nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyMutation)
	}
	schema, err := c.Describe(ctx, ref)
	if err != nil {
		return "", nil, err
	}
	if schema.Type != driver.CollectionTable {
		return "", nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is a %s, not a table", schema.Ref.Name, schema.Type))
	}
	return sqlitecore.QuoteIdent(schema.Ref.Namespace) + "." + sqlitecore.QuoteIdent(schema.Ref.Name), schema, nil
}

// run sends one generated statement.
func (c *conn) run(ctx context.Context, op, text string, vals []driver.Value) (*stmtResult, error) {
	args, err := params(op, vals)
	if err != nil {
		return nil, err
	}
	r, err := c.client.exec(ctx, text, args)
	if err != nil {
		return nil, mapError(op, err)
	}
	return r, nil
}

// Insert inserts values and returns the stored primary key (supplied,
// defaulted or the generated rowid) from RETURNING; a table without one
// returns a nil key.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	table, schema, err := c.target(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	text := "INSERT INTO " + table + " DEFAULT VALUES"
	var vals []driver.Value
	if len(values) > 0 {
		cols := make([]string, len(values))
		marks := make([]string, len(values))
		for i, m := range values {
			if cols[i], err = sqlitecore.Column(Name, op, schema, m.Key); err != nil {
				return nil, err
			}
			marks[i] = "?"
			vals = append(vals, m.Value)
		}
		text = "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	}
	if len(schema.PrimaryKey) > 0 {
		quoted := make([]string, len(schema.PrimaryKey))
		for i, pk := range schema.PrimaryKey {
			quoted[i] = sqlitecore.QuoteIdent(pk)
		}
		text += " RETURNING " + strings.Join(quoted, ", ")
	}
	r, err := c.run(ctx, op, text, vals)
	if err != nil {
		return nil, err
	}
	if len(schema.PrimaryKey) == 0 {
		return nil, nil
	}
	if len(r.Rows) == 0 || len(r.Rows[0]) != len(schema.PrimaryKey) {
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
		key = append(key, driver.Member{Key: pk, Value: convert(declared, sqlitecore.ColumnType(declared, r.Rows[0][i]), r.Rows[0][i])})
	}
	return key, nil
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	const op = "update"
	table, schema, err := c.target(ctx, op, ref)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("there are no changes"))
	}
	sets := make([]string, len(changes))
	var vals []driver.Value
	for i, m := range changes {
		col, err := sqlitecore.Column(Name, op, schema, m.Key)
		if err != nil {
			return err
		}
		sets[i] = col + " = ?"
		vals = append(vals, m.Value)
	}
	where, kp, err := sqlitecore.WhereKey(Name, op, schema, key)
	if err != nil {
		return err
	}
	return c.affectOne(ctx, op, table, "UPDATE "+table+" SET "+strings.Join(sets, ", ")+where, vals, where, kp)
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	table, schema, err := c.target(ctx, op, ref)
	if err != nil {
		return err
	}
	where, kp, err := sqlitecore.WhereKey(Name, op, schema, key)
	if err != nil {
		return err
	}
	return c.affectOne(ctx, op, table, "DELETE FROM "+table+where, nil, where, kp)
}

// affectOne runs stmt (an UPDATE or DELETE ending in where) guarded so it
// changes rows only when where matches exactly one. When nothing changed it
// counts the matches to tell ErrNotFound (none) from ErrConflict (several);
// that count is a second request, so it reports what it saw a moment later.
func (c *conn) affectOne(ctx context.Context, op, table, stmt string, vals []driver.Value, where string, kp []driver.Value) error {
	guarded := stmt + " AND (SELECT count(*) FROM " + table + where + ") = 1"
	args := append(append(append([]driver.Value(nil), vals...), kp...), kp...)
	r, err := c.run(ctx, op, guarded, args)
	if err != nil {
		return err
	}
	if !r.Meta.ChangesKnown {
		// The statement may have run: never guess "not found".
		return driver.Wrap(driver.ErrConnection, Name, op, errChangesUnknown)
	}
	if r.Meta.Changes > 0 {
		return nil
	}
	cr, err := c.run(ctx, op, "SELECT count(*) FROM "+table+where, kp)
	if err != nil {
		return err
	}
	var n int64
	if len(cr.Rows) == 1 && len(cr.Rows[0]) == 1 {
		n = sqlitecore.AsInt(cr.Rows[0][0])
	}
	if n > 1 {
		return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("the key matches %d rows; nothing was changed", n))
	}
	return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row matches the key"))
}
