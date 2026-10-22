package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// Catalog reads come from the system tables and always run read-only
// (readonly=1, or the server's own readonly). Listings stop at 10000 rows. Names reach the server only as bound parameters. An empty
// namespace means the connection's default database.

const namespacesSQL = `
SELECT name, comment
FROM system.databases
WHERE name NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema')
ORDER BY name
LIMIT 10000`

func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	dctx, done := detach(ctx)
	defer done()
	rows, err := c.db.Query(c.roCtx(dctx), namespacesSQL)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Namespace
	for rows.Next() {
		ns := driver.Namespace{Label: "database"}
		if err := rows.Scan(&ns.Name, &ns.Comment); err != nil {
			return nil, mapError(ctx, op, err)
		}
		ns.Default = ns.Name == c.defaultNS
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return out, nil
}

const collectionsSQL = `
SELECT name, engine, total_rows, comment
FROM system.tables
WHERE database = ? AND NOT is_temporary
ORDER BY name
LIMIT 10000`

// collectionType maps a table engine to a collection type.
func collectionType(engine string) driver.CollectionType {
	switch engine {
	case "View", "LiveView", "WindowView":
		return driver.CollectionView
	case "MaterializedView":
		return driver.CollectionMaterializedView
	case "Dictionary":
		return driver.CollectionOther
	}
	return driver.CollectionTable
}

func (c *conn) namespace(ns string) string {
	if ns == "" {
		return c.defaultNS
	}
	return ns
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	db := c.namespace(ns.Name)
	dctx, done := detach(ctx)
	defer done()
	rows, err := c.db.Query(c.roCtx(dctx), collectionsSQL, db)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Collection
	for rows.Next() {
		var name, engine, comment string
		var total *uint64
		if err := rows.Scan(&name, &engine, &total, &comment); err != nil {
			return nil, mapError(ctx, op, err)
		}
		out = append(out, driver.Collection{
			Ref:         driver.CollectionRef{Namespace: db, Name: name},
			Type:        collectionType(engine),
			RowEstimate: estimate(total),
			Comment:     comment,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	if out == nil {
		if err := c.requireDatabase(ctx, op, db); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// requireDatabase returns ErrNotFound when database db does not exist.
func (c *conn) requireDatabase(ctx context.Context, op, db string) error {
	var n uint64
	dctx, done := detach(ctx)
	defer done()
	if err := c.db.QueryRow(c.roCtx(dctx), "SELECT count() FROM system.databases WHERE name = ?", db).Scan(&n); err != nil {
		return mapError(ctx, op, err)
	}
	if n == 0 {
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("database %q does not exist", db))
	}
	return nil
}

func estimate(n *uint64) int64 {
	if n == nil || *n > 1<<62 {
		return -1
	}
	return int64(*n)
}

const tableSQL = `
SELECT engine, sorting_key, primary_key, total_rows, total_bytes, comment, create_table_query
FROM system.tables
WHERE database = ? AND name = ?`

const columnsSQL = `
SELECT name, type, default_kind, default_expression, comment
FROM system.columns
WHERE database = ? AND table = ?
ORDER BY position`

const skipIndexesSQL = `
SELECT name, type, expr
FROM system.data_skipping_indices
WHERE database = ? AND table = ?
ORDER BY name`

// Describe reads a table from system.tables, system.columns and
// system.data_skipping_indices. The sorting key is reported as the primary
// key when every part of it is a plain column (ClickHouse keys may be
// expressions, and need not be unique). The primary index and the
// data-skipping indexes are listed as indexes; ClickHouse has no foreign
// keys.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	s, _, err := c.describe(ctx, "describe", ref)
	return s, err
}

// describe is Describe, plus the table's sorting key expression as the
// server formats it ("" when it has none).
func (c *conn) describe(ctx context.Context, op string, ref driver.CollectionRef) (*driver.CollectionSchema, string, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, "", err
	}
	db := c.namespace(ref.Namespace)
	dctx, done := detach(ctx)
	defer done()
	rctx := c.roCtx(dctx)
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Namespace: db, Name: ref.Name}}
	var engine, sortingKey, primaryKey string
	var total, bytes *uint64
	rows, err := c.db.Query(rctx, tableSQL, db, ref.Name)
	if err != nil {
		return nil, "", mapError(ctx, op, err)
	}
	found := rows.Next()
	if found {
		err = rows.Scan(&engine, &sortingKey, &primaryKey, &total, &bytes, &s.Comment, &s.DDL)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return nil, "", mapError(ctx, op, err)
	}
	if !found {
		return nil, "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("table %s.%s does not exist", db, ref.Name))
	}
	s.Type = collectionType(engine)
	s.RowEstimate = estimate(total)
	s.SizeBytes = estimate(bytes)

	if s.Fields, err = c.describeColumns(rctx, db, ref.Name); err != nil {
		return nil, "", mapError(ctx, op, err)
	}
	s.PrimaryKey = keyColumns(sortingKey, s.Fields)
	if pk := keyParts(primaryKey); len(pk) > 0 {
		s.Indexes = append(s.Indexes, driver.Index{Name: "primary", Fields: pk, Primary: true, Method: "sparse"})
	}
	idx, err := c.db.Query(rctx, skipIndexesSQL, db, ref.Name)
	if err != nil {
		return nil, "", mapError(ctx, op, err)
	}
	defer func() { _ = idx.Close() }()
	for idx.Next() {
		var name, typ, expr string
		if err := idx.Scan(&name, &typ, &expr); err != nil {
			return nil, "", mapError(ctx, op, err)
		}
		s.Indexes = append(s.Indexes, driver.Index{Name: name, Fields: []string{expr}, Method: typ})
	}
	if err := idx.Err(); err != nil {
		return nil, "", mapError(ctx, op, err)
	}
	return s, sortingKey, nil
}

func (c *conn) describeColumns(ctx context.Context, db, table string) ([]driver.Field, error) {
	rows, err := c.db.Query(ctx, columnsSQL, db, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Field
	for rows.Next() {
		var f driver.Field
		var kind, expr string
		if err := rows.Scan(&f.Name, &f.NativeType, &kind, &expr, &f.Comment); err != nil {
			return nil, err
		}
		f.Type = columnType(f.NativeType)
		f.Nullable = nullable(f.NativeType)
		switch kind {
		case "DEFAULT":
			f.Default = expr
		case "MATERIALIZED", "ALIAS":
			f.Default, f.Generated = expr, true
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// keyParts splits a key expression list ("a, toDate(b)") at its top-level
// commas.
func keyParts(key string) []string {
	var parts []string
	depth, start := 0, 0
	sqlscan.Scan(sqlscan.ClickHouse(), key, func(t sqlscan.Token) bool {
		if t.Kind != sqlscan.Punct {
			return true
		}
		switch key[t.Start] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(key[start:t.Start]))
				start = t.End
			}
		}
		return true
	})
	if last := strings.TrimSpace(key[start:]); last != "" {
		parts = append(parts, last)
	}
	return parts
}

// keyColumns returns the sorting key as column names when every part of it
// is a plain column, and nil otherwise.
func keyColumns(key string, fields []driver.Field) []string {
	parts := keyParts(key)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name := p
		if len(p) >= 2 && (p[0] == '`' || p[0] == '"') && p[len(p)-1] == p[0] {
			name = unquoteIdent(p)
		}
		known := false
		for _, f := range fields {
			if f.Name == name {
				known = true
				break
			}
		}
		if !known {
			return nil
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unquoteIdent undoes quoteIdent for a `…` or "…" identifier.
func unquoteIdent(q string) string {
	body := q[1 : len(q)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if (body[i] == '\\' || body[i] == q[0]) && i+1 < len(body) {
			i++
		}
		b.WriteByte(body[i])
	}
	return b.String()
}
