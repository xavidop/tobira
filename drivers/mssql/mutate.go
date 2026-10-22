package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

const savepointMutation = "tobira_mutation"

// Insert, Update and Delete each run in their own transaction on a
// dedicated connection, so a conflicting match rolls back before anything is
// visible. On a read-only connection they fail with ErrReadOnly before any
// statement is sent.

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	return withTx(ctx, c, "insert", func(t *msTx) (driver.Object, error) { return t.Insert(ctx, ref, values) })
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	_, err := withTx(ctx, c, "update", func(t *msTx) (driver.Object, error) { return nil, t.Update(ctx, ref, key, changes) })
	return err
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	_, err := withTx(ctx, c, "delete", func(t *msTx) (driver.Object, error) { return nil, t.Delete(ctx, ref, key) })
	return err
}

// withTx runs fn in a fresh read-write transaction: COMMIT when fn succeeds,
// ROLLBACK otherwise.
func withTx[T any](ctx context.Context, c *conn, op string, fn func(*msTx) (T, error)) (T, error) {
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

// target describes ref over sc for a mutation. Identifiers come only from
// the catalog, never from the caller's text.
func (c *conn) target(ctx context.Context, sc *sql.Conn, op string, ref driver.CollectionRef) (*table, error) {
	t, err := c.describeIn(ctx, sc, op, ref)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if t.schema.Type != driver.CollectionTable {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is a %s, not a table", t.schema.Ref.Name, t.schema.Type))
	}
	return t, nil
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

// stmt accumulates one statement's parameters (@p1, @p2 …).
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
	return "@p" + strconv.Itoa(len(s.args)), nil
}

// whereKey renders the WHERE clause matching key; a null value matches IS
// NULL.
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
// failed mutation leaves a caller's transaction as it was. When the
// transaction can no longer commit after the failure, the Tx is dead.
func (t *msTx) inSavepoint(ctx context.Context, sc *sql.Conn, op string, fn func() error) error {
	if _, err := sc.ExecContext(ctx, "SAVE TRANSACTION "+savepointMutation); err != nil {
		return mapError(ctx, op, err)
	}
	if err := fn(); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_, _ = sc.ExecContext(rctx, "IF XACT_STATE() = 1 ROLLBACK TRANSACTION "+savepointMutation)
		if t.proveOpen(rctx) != nil {
			t.mu.Lock()
			t.dead = true
			t.mu.Unlock()
		}
		return err
	}
	return nil
}

// Insert adds one row and returns its primary key, read back with OUTPUT
// INSERTED … INTO a table variable (OUTPUT without INTO fails on a table
// with triggers). An identity column supplied as NULL is left out, so the
// server assigns it; one supplied with a value is inserted under SET
// IDENTITY_INSERT, which the statement turns off again (and which ends with
// the statement's sp_executesql batch in any case).
func (t *msTx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (_ driver.Object, err error) {
	const op = "insert"
	sc, ctx, release, err := t.use(ctx, op, true)
	if err != nil {
		return nil, err
	}
	defer release()
	tb, err := t.c.target(ctx, sc, op, ref)
	if err != nil {
		return nil, err
	}
	s := &stmt{op: op}
	var cols, marks []string
	identityInsert := false
	for _, m := range values {
		name, err := field(op, tb.schema, m.Key)
		if err != nil {
			return nil, err
		}
		if f := schemaField(tb.schema, name); f != nil && f.AutoIncrement {
			if m.Value.IsNull() {
				continue
			}
			identityInsert = true
		}
		ph, err := s.bind(m.Value)
		if err != nil {
			return nil, err
		}
		cols = append(cols, quoteIdent(name))
		marks = append(marks, ph)
	}
	var b strings.Builder
	pk := tb.schema.PrimaryKey
	if len(pk) > 0 {
		decl := make([]string, len(pk))
		for i, name := range pk {
			f := schemaField(tb.schema, name)
			if f == nil {
				return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("primary key column %q not described", name))
			}
			decl[i] = quoteIdent(name) + " " + keyColumnType(f.NativeType)
		}
		b.WriteString("DECLARE @tobira_key TABLE (" + strings.Join(decl, ", ") + "); ")
	}
	if identityInsert {
		b.WriteString("SET IDENTITY_INSERT " + tb.name() + " ON; ")
	}
	b.WriteString("INSERT INTO " + tb.name())
	if len(cols) > 0 {
		b.WriteString(" (" + strings.Join(cols, ", ") + ")")
	}
	if len(pk) > 0 {
		out := make([]string, len(pk))
		for i, name := range pk {
			out[i] = "INSERTED." + quoteIdent(name)
		}
		b.WriteString(" OUTPUT " + strings.Join(out, ", ") + " INTO @tobira_key")
	}
	if len(cols) > 0 {
		b.WriteString(" VALUES (" + strings.Join(marks, ", ") + ")")
	} else {
		b.WriteString(" DEFAULT VALUES")
	}
	b.WriteString(";")
	if identityInsert {
		b.WriteString(" SET IDENTITY_INSERT " + tb.name() + " OFF;")
	}
	if len(pk) > 0 {
		b.WriteString(" SELECT * FROM @tobira_key;")
	}
	var key driver.Object
	defer func() {
		if identityInsert && err != nil {
			// A batch-aborting error skips the statement's own OFF; make
			// sure the setting does not outlive the failed insert.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
			defer cancel()
			_, _ = sc.ExecContext(rctx, "SET IDENTITY_INSERT "+tb.name()+" OFF")
		}
	}()
	err = t.inSavepoint(ctx, sc, op, func() error {
		if len(pk) == 0 {
			_, err := sc.ExecContext(ctx, b.String(), s.args...)
			return mapError(ctx, op, err)
		}
		rows, err := sc.QueryContext(ctx, b.String(), s.args...)
		if err != nil {
			return mapError(ctx, op, err)
		}
		defer func() { _ = rows.Close() }()
		cts, err := rows.ColumnTypes()
		if err != nil {
			return mapError(ctx, op, err)
		}
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return mapError(ctx, op, err)
			}
			return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("the insert returned no key"))
		}
		raw := make([]any, len(cts))
		ptrs := make([]any, len(raw))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return mapError(ctx, op, err)
		}
		for i, ct := range cts {
			key = append(key, driver.Member{Key: ct.Name(), Value: convertValue(ct.DatabaseTypeName(), raw[i])})
		}
		for rows.Next() { // drain, so a server error after the row surfaces
		}
		if err := rows.Err(); err != nil {
			return mapError(ctx, op, err)
		}
		return mapError(ctx, op, rows.Close())
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// keyColumnType is the type a key column takes in the table variable that
// receives the inserted key: its own, except rowversion, which a table
// variable cannot hold as such.
func keyColumnType(native string) string {
	switch baseOf(native) {
	case "timestamp", "rowversion":
		return "binary(8)"
	}
	return native
}

// Update changes the one row matching key. It first counts the matching rows
// under UPDLOCK and HOLDLOCK, so "exactly one row" holds until the update:
// none matching is ErrNotFound and more than one is ErrConflict; nothing is
// changed then.
func (t *msTx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	const op = "update"
	sc, ctx, release, err := t.use(ctx, op, true)
	if err != nil {
		return err
	}
	defer release()
	tb, err := t.c.target(ctx, sc, op, ref)
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
	return t.inSavepoint(ctx, sc, op, func() error {
		var n int64
		count := "SELECT COUNT_BIG(*) FROM " + tb.name() + " WITH (UPDLOCK, HOLDLOCK)" + where
		if err := sc.QueryRowContext(ctx, count, s.args...).Scan(&n); err != nil {
			return mapError(ctx, op, err)
		}
		if err := oneRow(op, n); err != nil {
			return err
		}
		n, err := rowCount(ctx, sc, "UPDATE "+tb.name()+" SET "+strings.Join(sets, ", ")+where, s.args...)
		if err != nil {
			return mapError(ctx, op, err)
		}
		if n > 1 { // cannot happen under the lock; undo rather than trust it
			return oneRow(op, n)
		}
		return nil
	})
}

// Delete removes the one row matching key: none is ErrNotFound; more than
// one (it stops after two) is undone and reported as ErrConflict.
func (t *msTx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	sc, ctx, release, err := t.use(ctx, op, true)
	if err != nil {
		return err
	}
	defer release()
	tb, err := t.c.target(ctx, sc, op, ref)
	if err != nil {
		return err
	}
	s := &stmt{op: op}
	where, err := s.whereKey(tb.schema, key)
	if err != nil {
		return err
	}
	return t.inSavepoint(ctx, sc, op, func() error {
		// TOP (2) is enough to tell one matching row from several.
		n, err := rowCount(ctx, sc, "DELETE TOP (2) FROM "+tb.name()+where, s.args...)
		if err != nil {
			return mapError(ctx, op, err)
		}
		return oneRow(op, n)
	})
}

// rowCount runs one data change and returns the rows it affected, read
// with SELECT @@ROWCOUNT in the same batch. go-mssqldb's RowsAffected sums
// every count the server reports, a trigger's statements included, so an
// audit trigger would make one row look like several.
func rowCount(ctx context.Context, q queryer, stmt string, args ...any) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, stmt+"\n;SELECT CAST(@@ROWCOUNT AS bigint)", args...).Scan(&n)
	return n, err
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
