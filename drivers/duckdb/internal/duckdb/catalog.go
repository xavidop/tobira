package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// quoteIdent double-quotes an identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// queryer is the read half of *sql.DB and *sql.Conn.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Namespaces lists the schemas of the current database, without DuckDB's
// information_schema and pg_catalog. The default is current_schema().
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT schema_name, schema_name = current_schema() FROM duckdb_schemas()
		WHERE database_name = current_database() AND NOT internal ORDER BY schema_name`)
	if err != nil {
		return nil, mapError(op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Namespace
	for rows.Next() {
		var name string
		var def bool
		if err := rows.Scan(&name, &def); err != nil {
			return nil, mapError(op, err)
		}
		out = append(out, driver.Namespace{Name: name, Label: "schema", Default: def})
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(op, err)
	}
	if len(out) == 0 || !hasDefault(out) { // main is internal in some versions
		out = append([]driver.Namespace{{Name: "main", Label: "schema", Default: true}}, out...)
	}
	return out, nil
}

func hasDefault(nss []driver.Namespace) bool {
	for _, n := range nss {
		if n.Default {
			return true
		}
	}
	return false
}

// schemaName is ns, or the current schema when ns is empty.
func schemaName(ctx context.Context, db queryer, ns string) (string, error) {
	if ns != "" {
		return ns, nil
	}
	var s string
	err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&s)
	return s, err
}

// Collections lists the tables and views of schema ns.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	schema, err := schemaName(ctx, c.db, ns.Name)
	if err != nil {
		return nil, mapError(op, err)
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT table_name, 'table', coalesce(estimated_size, -1) FROM duckdb_tables()
		WHERE database_name = current_database() AND schema_name = ? AND NOT internal
		UNION ALL
		SELECT view_name, 'view', -1 FROM duckdb_views()
		WHERE database_name = current_database() AND schema_name = ? AND NOT internal
		ORDER BY 1`, schema, schema)
	if err != nil {
		return nil, mapError(op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Collection
	for rows.Next() {
		var name, typ string
		var est int64
		if err := rows.Scan(&name, &typ, &est); err != nil {
			return nil, mapError(op, err)
		}
		ct := driver.CollectionTable
		if typ == "view" {
			ct = driver.CollectionView
		}
		out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: schema, Name: name}, Type: ct, RowEstimate: est})
	}
	return out, mapError(op, rows.Err())
}

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	return describeOn(ctx, c.db, ref)
}

// describeOn describes ref over db: the pool, or a transaction's own
// connection so it sees what the transaction created. A name matches
// exactly, else case-insensitively when that is unique (DuckDB resolves
// names case-insensitively).
func describeOn(ctx context.Context, db queryer, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	schema, err := schemaName(ctx, db, ref.Namespace)
	if err != nil {
		return nil, mapError(op, err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT table_name, 'table', coalesce(sql, ''), coalesce(estimated_size, -1) FROM duckdb_tables()
		WHERE database_name = current_database() AND lower(schema_name) = lower(?) AND lower(table_name) = lower(?)
		UNION ALL
		SELECT view_name, 'view', coalesce(sql, ''), -1 FROM duckdb_views()
		WHERE database_name = current_database() AND lower(schema_name) = lower(?) AND lower(view_name) = lower(?) AND NOT internal`,
		schema, ref.Name, schema, ref.Name)
	if err != nil {
		return nil, mapError(op, err)
	}
	type match struct {
		name, typ, ddl string
		est            int64
	}
	var matches []match
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.name, &m.typ, &m.ddl, &m.est); err != nil {
			_ = rows.Close()
			return nil, mapError(op, err)
		}
		matches = append(matches, m)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapError(op, err)
	}
	var found *match
	for i := range matches {
		if matches[i].name == ref.Name {
			found = &matches[i]
		}
	}
	if found == nil && len(matches) == 1 {
		found = &matches[0]
	}
	if found == nil {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no such table or view: %s.%s", schema, ref.Name))
	}
	s := &driver.CollectionSchema{
		Ref: driver.CollectionRef{Namespace: schema, Name: found.name}, Type: driver.CollectionTable,
		DDL: found.ddl, RowEstimate: found.est, SizeBytes: -1,
	}
	if found.typ == "view" {
		s.Type = driver.CollectionView
	}
	if err := describeFields(ctx, db, s); err != nil {
		return nil, mapError(op, err)
	}
	if err := describeConstraints(ctx, db, s); err != nil {
		return nil, mapError(op, err)
	}
	if err := describeIndexes(ctx, db, s); err != nil {
		return nil, mapError(op, err)
	}
	return s, nil
}

func describeFields(ctx context.Context, db queryer, s *driver.CollectionSchema) error {
	rows, err := db.QueryContext(ctx, `SELECT column_name, data_type, is_nullable, coalesce(column_default, '')
		FROM duckdb_columns() WHERE database_name = current_database() AND schema_name = ? AND table_name = ?
		ORDER BY column_index`, s.Ref.Namespace, s.Ref.Name)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, typ, def string
		var nullable bool
		if err := rows.Scan(&name, &typ, &nullable, &def); err != nil {
			return err
		}
		s.Fields = append(s.Fields, driver.Field{
			Name: name, NativeType: typ, Type: mapType(typ), Nullable: nullable, Default: def,
			AutoIncrement: strings.HasPrefix(strings.ToLower(def), "nextval("),
		})
	}
	return rows.Err()
}

// describeConstraints reads the primary key and foreign keys.
func describeConstraints(ctx context.Context, db queryer, s *driver.CollectionSchema) error {
	rows, err := db.QueryContext(ctx, `SELECT constraint_type, constraint_column_names, coalesce(referenced_table, ''),
		coalesce(referenced_column_names, []::VARCHAR[])
		FROM duckdb_constraints() WHERE database_name = current_database() AND schema_name = ? AND table_name = ?
		AND constraint_type IN ('PRIMARY KEY', 'FOREIGN KEY') ORDER BY constraint_index`, s.Ref.Namespace, s.Ref.Name)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var typ, refTable string
		var cols, refCols any
		if err := rows.Scan(&typ, &cols, &refTable, &refCols); err != nil {
			return err
		}
		switch typ {
		case "PRIMARY KEY":
			s.PrimaryKey = stringList(cols)
		case "FOREIGN KEY":
			s.ForeignKeys = append(s.ForeignKeys, driver.ForeignKey{
				Fields: stringList(cols), References: driver.CollectionRef{Namespace: s.Ref.Namespace, Name: refTable},
				RefFields: stringList(refCols),
			})
		}
	}
	return rows.Err()
}

// stringList converts a scanned VARCHAR[] to a []string.
func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func describeIndexes(ctx context.Context, db queryer, s *driver.CollectionSchema) error {
	rows, err := db.QueryContext(ctx, `SELECT index_name, is_unique, is_primary, coalesce(expressions, '')
		FROM duckdb_indexes() WHERE database_name = current_database() AND schema_name = ? AND table_name = ?
		ORDER BY index_name`, s.Ref.Namespace, s.Ref.Name)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, exprs string
		var unique, primary bool
		if err := rows.Scan(&name, &unique, &primary, &exprs); err != nil {
			return err
		}
		s.Indexes = append(s.Indexes, driver.Index{Name: name, Unique: unique, Primary: primary, Method: "art", Fields: indexFields(exprs)})
	}
	return rows.Err()
}

// indexFields reads duckdb_indexes().expressions, a list such as
// "[a, \"b c\"]", into its column names.
func indexFields(exprs string) []string {
	exprs = strings.TrimSpace(exprs)
	exprs = strings.TrimSuffix(strings.TrimPrefix(exprs, "["), "]")
	if exprs == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(exprs, ",") {
		p = strings.Trim(strings.TrimSpace(p), `'"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var errReadOnlyConn = errors.New("the connection is read-only")
