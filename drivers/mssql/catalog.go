package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Namespaces are databases, then schemas. A schema of the login database
// (the database a fresh session starts in) is named by its bare name
// ("dbo"); a schema of another database is "database.schema"
// ("sales.dbo"). A part holding '.', '[' or ']' is bracket-quoted
// ("[my.db].dbo"). An empty namespace is the login database's default
// schema.
//
// Catalog reads come from the sys catalog views of the namespace's
// database, named with three-part names ([db].sys.objects). Names reach the
// server as parameters, except the database name, which is bracket-quoted.

// maxNamespaceDatabases caps how many databases Namespaces lists schemas
// for, so a server with thousands of databases stays responsive.
const maxNamespaceDatabases = 200

// nsPart quotes one part of a namespace name when it must be.
func nsPart(s string) string {
	if s == "" || strings.ContainsAny(s, ".[]") {
		return quoteIdent(s)
	}
	return s
}

// canonicalNS is the namespace name of schema in database db.
func (c *conn) canonicalNS(db, schema string) string {
	if strings.EqualFold(db, c.loginDB) {
		return nsPart(schema)
	}
	return nsPart(db) + "." + nsPart(schema)
}

// splitNS splits a namespace name into its parts: bare or bracket-quoted
// names separated by '.'.
func splitNS(ns string) ([]string, error) {
	var parts []string
	for i := 0; ; {
		var part string
		if i < len(ns) && ns[i] == '[' {
			j := i + 1
			var b strings.Builder
			for ; j < len(ns); j++ {
				if ns[j] == ']' {
					if j+1 < len(ns) && ns[j+1] == ']' {
						b.WriteByte(']')
						j++
						continue
					}
					break
				}
				b.WriteByte(ns[j])
			}
			if j >= len(ns) {
				return nil, errors.New("unterminated [ in the namespace")
			}
			part, i = b.String(), j+1
		} else {
			j := strings.IndexByte(ns[i:], '.')
			if j < 0 {
				j = len(ns) - i
			}
			part, i = ns[i:i+j], i+j
			if strings.ContainsAny(part, "[]") {
				return nil, errors.New("a namespace part holding [ or ] must be bracket-quoted")
			}
		}
		if part == "" {
			return nil, errors.New("empty part in the namespace")
		}
		parts = append(parts, part)
		if i == len(ns) {
			return parts, nil
		}
		if ns[i] != '.' {
			return nil, errors.New("expected '.' after a quoted part of the namespace")
		}
		i++
	}
}

// resolveNS returns the database and schema a namespace names.
func (c *conn) resolveNS(ns string) (db, schema string, err error) {
	if ns == "" {
		return c.loginDB, c.defaultSchema, nil
	}
	parts, err := splitNS(ns)
	if err != nil {
		return "", "", driver.Wrap(driver.ErrSyntax, Name, "namespace", fmt.Errorf("invalid namespace %q: %w", ns, err))
	}
	switch len(parts) {
	case 1:
		return c.loginDB, parts[0], nil
	case 2:
		return parts[0], parts[1], nil
	}
	return "", "", driver.Wrap(driver.ErrSyntax, Name, "namespace", fmt.Errorf("invalid namespace %q: want schema or database.schema", ns))
}

const databasesSQL = `
SELECT name FROM sys.databases
WHERE state = 0 AND HAS_DBACCESS(name) = 1 AND (database_id > 4 OR name = DB_NAME())
ORDER BY CASE WHEN name = DB_NAME() THEN 0 ELSE 1 END, name`

// schemasSQL lists a database's schemas, without sys, INFORMATION_SCHEMA,
// guest and the fixed database role schemas (schema_id 16384 and up).
const schemasSQL = `
SELECT name FROM %s.sys.schemas
WHERE schema_id < 16384 AND name NOT IN (N'sys', N'INFORMATION_SCHEMA', N'guest')
ORDER BY name`

func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	dbs, err := c.strings(ctx, databasesSQL)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if len(dbs) > maxNamespaceDatabases {
		dbs = dbs[:maxNamespaceDatabases]
	}
	var out []driver.Namespace
	for _, db := range dbs {
		schemas, err := c.strings(ctx, fmt.Sprintf(schemasSQL, quoteIdent(db)))
		if err != nil {
			if ctx.Err() != nil {
				return nil, mapError(ctx, op, err)
			}
			continue // a database that went offline or became inaccessible since the listing
		}
		login := strings.EqualFold(db, c.loginDB)
		for _, s := range schemas {
			ns := driver.Namespace{Name: c.canonicalNS(db, s), Label: "schema", Default: login && s == c.defaultSchema}
			if !login {
				ns.Comment = "database " + db
			}
			out = append(out, ns)
		}
	}
	return out, nil
}

// strings runs a one-column query of the driver's own and returns its
// values.
func (c *conn) strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// rowsSubquery estimates a table's rows from its heap or clustered index.
const rowsSubquery = `CAST(COALESCE((SELECT SUM(p.rows) FROM %[1]s.sys.partitions p WHERE p.object_id = o.object_id AND p.index_id IN (0, 1)), -1) AS bigint)`

// descriptionJoin reads an object's or column's MS_Description.
const descriptionJoin = `LEFT JOIN %[1]s.sys.extended_properties ep ON ep.class = 1 AND ep.major_id = %[2]s AND ep.minor_id = %[3]s AND ep.name = N'MS_Description'`

var collectionsSQL = `
SELECT o.name, o.type, ` + rowsSubquery + `, COALESCE(CAST(ep.value AS nvarchar(4000)), N'')
FROM %[1]s.sys.objects o
JOIN %[1]s.sys.schemas s ON s.schema_id = o.schema_id
` + fmt.Sprintf(descriptionJoin, "%[1]s", "o.object_id", "0") + `
WHERE s.name = @p1 AND o.type IN ('U', 'V') AND o.is_ms_shipped = 0
ORDER BY o.name`

// collectionType maps sys.objects.type.
func collectionType(objType string) driver.CollectionType {
	if strings.TrimSpace(objType) == "V" {
		return driver.CollectionView
	}
	return driver.CollectionTable
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	db, schema, err := c.resolveNS(ns.Name)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, fmt.Sprintf(collectionsSQL, quoteIdent(db)), schema)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Collection
	canonical := c.canonicalNS(db, schema)
	for rows.Next() {
		col := driver.Collection{Ref: driver.CollectionRef{Namespace: canonical}}
		var typ string
		if err := rows.Scan(&col.Ref.Name, &typ, &col.RowEstimate, &col.Comment); err != nil {
			return nil, mapError(ctx, op, err)
		}
		col.Type = collectionType(typ)
		if col.Type == driver.CollectionView {
			col.RowEstimate = -1
		}
		out = append(out, col)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return out, nil
}

var relationSQL = `
SELECT o.object_id, s.name, o.name, o.type, ` + rowsSubquery + `,
       CAST(COALESCE((SELECT SUM(a.used_pages) FROM %[1]s.sys.partitions p
                      JOIN %[1]s.sys.allocation_units a ON a.container_id = p.partition_id
                      WHERE p.object_id = o.object_id), -1) AS bigint),
       COALESCE(CAST(ep.value AS nvarchar(4000)), N'')
FROM %[1]s.sys.objects o
JOIN %[1]s.sys.schemas s ON s.schema_id = o.schema_id
` + fmt.Sprintf(descriptionJoin, "%[1]s", "o.object_id", "0") + `
WHERE s.name = @p1 AND o.name = @p2 AND o.type IN ('U', 'V')`

// fieldsSQL reads the columns of an object. The base type of an alias type
// decides the tobira type; a CLR type (geography, hierarchyid) keeps its
// own name.
var fieldsSQL = `
SELECT c.name, t.name,
       CASE WHEN t.is_user_defined = 1 AND t.is_assembly_type = 0 THEN bt.name ELSE t.name END,
       c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity, c.is_computed,
       COALESCE(cc.definition, N''), COALESCE(dc.definition, N''), COALESCE(CAST(ep.value AS nvarchar(4000)), N'')
FROM %[1]s.sys.columns c
JOIN %[1]s.sys.types t ON t.user_type_id = c.user_type_id
LEFT JOIN %[1]s.sys.types bt ON bt.user_type_id = t.system_type_id
LEFT JOIN %[1]s.sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
LEFT JOIN %[1]s.sys.default_constraints dc ON dc.object_id = c.default_object_id
` + fmt.Sprintf(descriptionJoin, "%[1]s", "c.object_id", "c.column_id") + `
WHERE c.object_id = @p1
ORDER BY c.column_id`

const indexesSQL = `
SELECT i.name, i.is_unique, i.is_primary_key, i.type_desc, c.name
FROM %[1]s.sys.indexes i
JOIN %[1]s.sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN %[1]s.sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = @p1 AND i.index_id > 0 AND ic.is_included_column = 0 AND ic.key_ordinal > 0
ORDER BY i.is_primary_key DESC, i.name, ic.key_ordinal`

const foreignKeysSQL = `
SELECT fk.name, pc.name, rs.name, ro.name, rc.name
FROM %[1]s.sys.foreign_keys fk
JOIN %[1]s.sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
JOIN %[1]s.sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
JOIN %[1]s.sys.objects ro ON ro.object_id = fkc.referenced_object_id
JOIN %[1]s.sys.schemas rs ON rs.schema_id = ro.schema_id
JOIN %[1]s.sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
WHERE fk.parent_object_id = @p1
ORDER BY fk.name, fkc.constraint_column_id`

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	t, err := c.describeIn(ctx, c.db, op, ref)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	return t.schema, nil
}

// queryer is what the catalog reads run on: the pool, or the *sql.Conn of
// a transaction or mutation.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// table is a described collection: its schema plus what the driver needs
// to address it.
type table struct {
	schema   *driver.CollectionSchema
	db       string // database name
	owner    string // schema name
	objectID int64
}

// name is the table's three-part, bracket-quoted name.
func (t *table) name() string {
	return quoteIdent(t.db) + "." + quoteIdent(t.owner) + "." + quoteIdent(t.schema.Ref.Name)
}

// describeIn describes ref over q.
func (c *conn) describeIn(ctx context.Context, q queryer, op string, ref driver.CollectionRef) (*table, error) {
	db, schema, err := c.resolveNS(ref.Namespace)
	if err != nil {
		return nil, err
	}
	dbq := quoteIdent(db)
	t := &table{db: db, schema: &driver.CollectionSchema{}}
	s := t.schema
	var typ string
	err = q.QueryRowContext(ctx, fmt.Sprintf(relationSQL, dbq), schema, ref.Name).
		Scan(&t.objectID, &t.owner, &s.Ref.Name, &typ, &s.RowEstimate, &s.SizeBytes, &s.Comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("%s does not exist", qualified(ref)))
	}
	if err != nil {
		return nil, err
	}
	s.Ref.Namespace = c.canonicalNS(db, t.owner)
	s.Type = collectionType(typ)
	if s.SizeBytes >= 0 {
		s.SizeBytes *= 8192
	}
	if s.Type == driver.CollectionView {
		s.RowEstimate, s.SizeBytes = -1, -1
	}
	if s.Fields, err = describeFields(ctx, q, dbq, t.objectID); err != nil {
		return nil, err
	}
	if s.Indexes, err = describeIndexes(ctx, q, dbq, t.objectID); err != nil {
		return nil, err
	}
	for _, ix := range s.Indexes {
		if ix.Primary {
			s.PrimaryKey = ix.Fields
		}
	}
	if s.ForeignKeys, err = c.describeForeignKeys(ctx, q, db, t.objectID); err != nil {
		return nil, err
	}
	return t, nil
}

func qualified(ref driver.CollectionRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "." + ref.Name
}

func describeFields(ctx context.Context, q queryer, dbq string, objectID int64) ([]driver.Field, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(fieldsSQL, dbq), objectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Field
	for rows.Next() {
		var f driver.Field
		var typeName, baseName, computed, def string
		var maxLen, precision, scale int
		if err := rows.Scan(&f.Name, &typeName, &baseName, &maxLen, &precision, &scale, &f.Nullable,
			&f.AutoIncrement, &f.Generated, &computed, &def, &f.Comment); err != nil {
			return nil, err
		}
		f.NativeType = nativeType(typeName, maxLen, precision, scale)
		f.Type = fieldType(baseName)
		switch {
		case f.Generated:
			f.Default = computed
		default:
			f.Default = def
		}
		if strings.EqualFold(baseName, "timestamp") || strings.EqualFold(baseName, "rowversion") {
			f.Generated = true // the server sets it on every write
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func describeIndexes(ctx context.Context, q queryer, dbq string, objectID int64) ([]driver.Index, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(indexesSQL, dbq), objectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Index
	for rows.Next() {
		var name, method, field string
		var unique, primary bool
		if err := rows.Scan(&name, &unique, &primary, &method, &field); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, driver.Index{Name: name, Unique: unique, Primary: primary, Method: strings.ToLower(method)})
		}
		ix := &out[len(out)-1]
		ix.Fields = append(ix.Fields, field)
	}
	return out, rows.Err()
}

func (c *conn) describeForeignKeys(ctx context.Context, q queryer, db string, objectID int64) ([]driver.ForeignKey, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(foreignKeysSQL, quoteIdent(db)), objectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.ForeignKey
	for rows.Next() {
		var name, field, refSchema, refTable, refField string
		if err := rows.Scan(&name, &field, &refSchema, &refTable, &refField); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, driver.ForeignKey{Name: name, References: driver.CollectionRef{Namespace: c.canonicalNS(db, refSchema), Name: refTable}})
		}
		fk := &out[len(out)-1]
		fk.Fields = append(fk.Fields, field)
		fk.RefFields = append(fk.RefFields, refField)
	}
	return out, rows.Err()
}
