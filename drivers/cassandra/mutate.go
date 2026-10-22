package cassandra

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// The Mutator writes one row by its whole primary key with lightweight
// transactions (Paxos at the connection's serial consistency), so a write
// never silently becomes another: INSERT is an upsert in CQL, so Insert
// adds IF NOT EXISTS and an existing row is ErrConstraint; Update and
// Delete add IF EXISTS and a missing row is ErrNotFound. A key must name
// every primary key column and nothing else, so it always matches exactly
// one row. Primary key columns cannot be updated (CQL has no such
// update). Materialized views and counter tables are refused.

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	sch, err := c.writable(ctx, "insert", ref)
	if err != nil {
		return nil, err
	}
	if err := checkNoTuple(sch, "insert", values); err != nil {
		return nil, err
	}
	text, args, key, err := insertStatement(sch, values)
	if err != nil {
		return nil, err
	}
	applied, err := c.cas(ctx, "insert", text, args)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, driver.Wrap(driver.ErrConstraint, Name, "insert", errors.New("a row with this primary key already exists"))
	}
	c.Invalidate()
	return key, nil
}

func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	sch, err := c.writable(ctx, "update", ref)
	if err != nil {
		return err
	}
	if err := checkNoTuple(sch, "update", changes); err != nil {
		return err
	}
	text, args, err := updateStatement(sch, key, changes)
	if err != nil {
		return err
	}
	return c.casExisting(ctx, "update", text, args)
}

func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	sch, err := c.writable(ctx, "delete", ref)
	if err != nil {
		return err
	}
	text, args, err := deleteStatement(sch, key)
	if err != nil {
		return err
	}
	return c.casExisting(ctx, "delete", text, args)
}

// writable refuses a write on a read-only connection, then describes ref
// and refuses a view.
func (c *conn) writable(ctx context.Context, op string, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.readOnly {
		return nil, driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
	}
	sch, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := checkWritable(sch); err != nil {
		return nil, err
	}
	return sch, nil
}

// checkWritable refuses a materialized view and a counter table: a
// counter table takes neither INSERT nor lightweight transactions, only
// UPDATE … SET c = c + n.
func checkWritable(sch *driver.CollectionSchema) error {
	if sch.Type == driver.CollectionMaterializedView {
		return driver.Wrap(driver.ErrUnsupported, Name, "mutate", fmt.Errorf("%s is a materialized view, which is read-only: edit its base table", sch.Ref.Name))
	}
	for _, f := range sch.Fields {
		if strings.EqualFold(f.NativeType, "counter") {
			return driver.Wrap(driver.ErrUnsupported, Name, "mutate", fmt.Errorf(
				"%s is a counter table, which cannot be edited row by row: run UPDATE … SET %s = %s + n in CQL", sch.Ref.Name, f.Name, f.Name))
		}
	}
	return nil
}

// checkNoTuple refuses a value for a top-level tuple column: gocql cannot
// bind one to a prepared statement (it counts the tuple's elements as
// separate placeholders). A tuple inside a collection or UDT is fine.
func checkNoTuple(sch *driver.CollectionSchema, op string, vals driver.Object) error {
	for _, m := range vals {
		f, ok := field(sch, m.Key)
		t := strings.ToLower(strings.TrimPrefix(f.NativeType, "frozen<"))
		if ok && strings.HasPrefix(t, "tuple<") {
			return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf(
				"%q is a tuple column, which the Go driver cannot bind: write it with a CQL statement", m.Key))
		}
	}
	return nil
}

// cas runs a conditional statement and reports whether it applied.
func (c *conn) cas(ctx context.Context, op, text string, args []param) (bool, error) {
	applied, err := c.session.Query(text, anyParams(args)...).MapScanCASContext(ctx, map[string]any{})
	if err != nil {
		return false, mapError(ctx, op, err)
	}
	return applied, nil
}

// casExisting runs an IF EXISTS statement: not applied is ErrNotFound.
func (c *conn) casExisting(ctx context.Context, op, text string, args []param) error {
	applied, err := c.cas(ctx, op, text, args)
	if err != nil {
		return err
	}
	if !applied {
		return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no row has this primary key"))
	}
	c.Invalidate()
	return nil
}

// field returns sch's field called name.
func field(sch *driver.CollectionSchema, name string) (driver.Field, bool) {
	i := slices.IndexFunc(sch.Fields, func(f driver.Field) bool { return f.Name == name })
	if i < 0 {
		return driver.Field{}, false
	}
	return sch.Fields[i], true
}

// table is sch's qualified, quoted name.
func table(sch *driver.CollectionSchema) string {
	return quoteIdent(sch.Ref.Namespace) + "." + quoteIdent(sch.Ref.Name)
}

func insertStatement(sch *driver.CollectionSchema, values driver.Object) (string, []param, driver.Object, error) {
	fail := func(kind error, format string, args ...any) (string, []param, driver.Object, error) {
		return "", nil, nil, driver.Wrap(kind, Name, "insert", fmt.Errorf(format, args...))
	}
	var (
		cols  []string
		marks []string
		args  []param
		seen  = map[string]bool{}
	)
	for _, m := range values {
		if _, ok := field(sch, m.Key); !ok {
			return fail(driver.ErrNotFound, "no column %q", m.Key)
		}
		if seen[m.Key] {
			return fail(driver.ErrSyntax, "column %q is given twice", m.Key)
		}
		seen[m.Key] = true
		cols = append(cols, quoteIdent(m.Key))
		marks = append(marks, "?")
		args = append(args, param{m.Value})
	}
	key := make(driver.Object, 0, len(sch.PrimaryKey))
	for _, k := range sch.PrimaryKey {
		v, ok := values.Get(k)
		if !ok || v.IsNull() {
			return fail(driver.ErrSyntax, "the primary key column %q needs a value", k)
		}
		key = append(key, driver.Member{Key: k, Value: v})
	}
	text := "INSERT INTO " + table(sch) + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ") IF NOT EXISTS"
	return text, args, key, nil
}

// keyWhere builds the WHERE body for a whole primary key, in key order.
func keyWhere(sch *driver.CollectionSchema, op string, key driver.Object) (string, []param, error) {
	bad := func(format string, args ...any) (string, []param, error) {
		return "", nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf(format, args...))
	}
	if len(key) != len(sch.PrimaryKey) {
		return bad("the key must name exactly the primary key columns %q", sch.PrimaryKey)
	}
	var (
		parts []string
		args  []param
	)
	for _, k := range sch.PrimaryKey {
		v, ok := key.Get(k)
		if !ok || v.IsNull() {
			return bad("the key must name exactly the primary key columns %q", sch.PrimaryKey)
		}
		parts = append(parts, quoteIdent(k)+" = ?")
		args = append(args, param{v})
	}
	return strings.Join(parts, " AND "), args, nil
}

func updateStatement(sch *driver.CollectionSchema, key, changes driver.Object) (string, []param, error) {
	if len(changes) == 0 {
		return "", nil, driver.Wrap(driver.ErrSyntax, Name, "update", errors.New("nothing to change"))
	}
	var (
		sets []string
		args []param
	)
	for _, m := range changes {
		if _, ok := field(sch, m.Key); !ok {
			return "", nil, driver.Wrap(driver.ErrNotFound, Name, "update", fmt.Errorf("no column %q", m.Key))
		}
		if slices.Contains(sch.PrimaryKey, m.Key) {
			return "", nil, driver.Wrap(driver.ErrUnsupported, Name, "update", fmt.Errorf(
				"%q is part of the primary key, which CQL cannot update: insert a new row and delete this one", m.Key))
		}
		sets = append(sets, quoteIdent(m.Key)+" = ?")
		args = append(args, param{m.Value})
	}
	where, kargs, err := keyWhere(sch, "update", key)
	if err != nil {
		return "", nil, err
	}
	return "UPDATE " + table(sch) + " SET " + strings.Join(sets, ", ") + " WHERE " + where + " IF EXISTS", append(args, kargs...), nil
}

func deleteStatement(sch *driver.CollectionSchema, key driver.Object) (string, []param, error) {
	where, args, err := keyWhere(sch, "delete", key)
	if err != nil {
		return "", nil, err
	}
	return "DELETE FROM " + table(sch) + " WHERE " + where + " IF EXISTS", args, nil
}
