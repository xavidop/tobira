package vertica

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// A namespace is a schema of the connection's database, named as the
// catalog spells it. Catalog reads use v_catalog with values bound as
// parameters; listings stop at maxCatalogRows.

const maxCatalogRows = 10000

// resolve returns the schema a namespace names, "" meaning the
// connection's current schema.
func (c *conn) resolve(op, ns string) (string, error) {
	if ns == "" {
		if c.defaultSchema == "" {
			return "", driver.Wrap(driver.ErrNotFound, Name, op, errors.New("the connection has no current schema: name a namespace"))
		}
		return c.defaultSchema, nil
	}
	if err := checkSchemaName(op, ns); err != nil {
		return "", err
	}
	return ns, nil
}

func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	rows, err := c.collectStrings(ctx, op,
		"SELECT s.schema_name, c.comment FROM v_catalog.schemata s"+
			" LEFT JOIN v_catalog.comments c ON c.object_id = s.schema_id AND c.object_type = 'SCHEMA'"+
			" WHERE NOT s.is_system_schema ORDER BY s.schema_name LIMIT "+strconv.Itoa(maxCatalogRows))
	if err != nil {
		return nil, c.scrub(err)
	}
	out := make([]driver.Namespace, 0, len(rows))
	for _, r := range rows {
		out = append(out, driver.Namespace{Name: r[0], Label: "schema", Comment: r[1], Default: strings.EqualFold(r[0], c.defaultSchema)})
	}
	return out, nil
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	schema, err := c.resolve(op, ns.Name)
	if err != nil {
		return nil, err
	}
	rows, err := c.collectStrings(ctx, op,
		"SELECT x.table_name, x.kind, c.comment FROM ("+
			"SELECT table_id, table_name, 'TABLE' AS kind FROM v_catalog.tables WHERE LOWER(table_schema) = LOWER(?)"+
			" UNION ALL SELECT table_id, table_name, 'VIEW' FROM v_catalog.views WHERE LOWER(table_schema) = LOWER(?)) x"+
			" LEFT JOIN v_catalog.comments c ON c.object_id = x.table_id"+
			" ORDER BY x.table_name LIMIT "+strconv.Itoa(maxCatalogRows), schema, schema)
	if err != nil {
		return nil, c.scrub(err)
	}
	out := make([]driver.Collection, 0, len(rows))
	for _, r := range rows {
		typ := driver.CollectionTable
		if r[1] == "VIEW" {
			typ = driver.CollectionView
		}
		out = append(out, driver.Collection{
			Ref:         driver.CollectionRef{Namespace: schema, Name: r[0]},
			Type:        typ,
			RowEstimate: -1,
			Comment:     r[2],
		})
	}
	if len(out) == 0 {
		if err := c.requireSchema(ctx, op, schema); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// requireSchema returns ErrNotFound when schema does not exist.
func (c *conn) requireSchema(ctx context.Context, op, schema string) error {
	rows, err := c.collectStrings(ctx, op, "SELECT 1 FROM v_catalog.schemata WHERE LOWER(schema_name) = LOWER(?)", schema)
	if err != nil {
		return c.scrub(err)
	}
	if len(rows) == 0 {
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no schema %q", schema))
	}
	return nil
}

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	s, err := c.describe(ctx, op, ref)
	return s, c.scrub(err)
}

// describe reads a table's or view's columns and primary key. Names in the
// result are the catalog's spelling.
func (c *conn) describe(ctx context.Context, op string, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	schema, err := c.resolve(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	trows, err := c.collectStrings(ctx, op,
		"SELECT x.table_schema, x.table_name, x.kind, c.comment FROM ("+
			"SELECT table_id, table_schema, table_name, 'TABLE' AS kind FROM v_catalog.tables WHERE LOWER(table_schema) = LOWER(?) AND LOWER(table_name) = LOWER(?)"+
			" UNION ALL SELECT table_id, table_schema, table_name, 'VIEW' FROM v_catalog.views WHERE LOWER(table_schema) = LOWER(?) AND LOWER(table_name) = LOWER(?)) x"+
			" LEFT JOIN v_catalog.comments c ON c.object_id = x.table_id",
		schema, ref.Name, schema, ref.Name)
	if err != nil {
		return nil, err
	}
	if len(trows) == 0 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no table or view %q in %s", ref.Name, schema))
	}
	t := trows[0]
	out := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: t[0], Name: t[1]},
		Type:        driver.CollectionTable,
		RowEstimate: -1,
		SizeBytes:   -1,
		Comment:     t[3],
	}
	var crows [][]string
	if t[2] == "VIEW" {
		out.Type = driver.CollectionView
		crows, err = c.collectStrings(ctx, op,
			"SELECT column_name, data_type, 'true', '', 'false', numeric_scale FROM v_catalog.view_columns"+
				" WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", t[0], t[1])
	} else {
		crows, err = c.collectStrings(ctx, op,
			"SELECT column_name, data_type, is_nullable, column_default, is_identity, numeric_scale FROM v_catalog.columns"+
				" WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", t[0], t[1])
	}
	if err != nil {
		return nil, err
	}
	for _, r := range crows {
		def := r[3]
		out.Fields = append(out.Fields, driver.Field{
			Name:          r[0],
			NativeType:    r[1],
			Type:          columnType(r[1]),
			Nullable:      r[2] == "true",
			Default:       def,
			AutoIncrement: r[4] == "true" || strings.Contains(strings.ToLower(def), "nextval("),
		})
	}
	if out.Type == driver.CollectionTable {
		if out.PrimaryKey, err = c.primaryKey(ctx, op, t[0], t[1]); err != nil {
			return nil, err
		}
	}
	if len(out.PrimaryKey) > 0 {
		out.Indexes = []driver.Index{{Name: "PRIMARY KEY", Fields: out.PrimaryKey, Unique: true, Primary: true}}
	}
	return out, nil
}

// primaryKey reads a table's declared primary key, in key order. Vertica
// enforces it only when it is ENABLED; the Mutator does not rely on that.
func (c *conn) primaryKey(ctx context.Context, op, schema, table string) ([]string, error) {
	rows, err := c.collectStrings(ctx, op,
		"SELECT column_name FROM v_catalog.primary_keys WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position",
		schema, table)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r[0]
	}
	return out, nil
}
