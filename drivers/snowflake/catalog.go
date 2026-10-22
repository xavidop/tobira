package snowflake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// A namespace is a schema of a database, named "database.schema" with the
// catalog's own spelling of both names; a part holding '.' or '"' is
// written in double quotes ("my.db".PUBLIC). Catalog reads use SHOW and
// INFORMATION_SCHEMA with fully qualified, quoted names; values reach the
// server only as bound parameters. Listings stop at maxCatalogRows.

const maxCatalogRows = 10000

// namespaceName names schema in db.
func namespaceName(db, schema string) string {
	part := func(s string) string {
		if s == "" || strings.ContainsAny(s, `."`) {
			return quoteIdent(s)
		}
		return s
	}
	if schema == "" {
		return part(db)
	}
	return part(db) + "." + part(schema)
}

// splitNamespace parses a namespace name: one or two parts, each bare or
// in double quotes.
func splitNamespace(ns string) (db, schema string, err error) {
	bad := errors.New("invalid namespace: want database.schema")
	var parts []string
	for i := 0; ; {
		var p string
		if i < len(ns) && ns[i] == '"' {
			end, err := quotedEnd(ns, i+1)
			if err != nil {
				return "", "", bad
			}
			p, i = unquote(ns[i:end]), end
		} else {
			j := strings.IndexByte(ns[i:], '.')
			if j < 0 {
				j = len(ns) - i
			}
			p = ns[i : i+j]
			if p == "" || strings.Contains(p, `"`) {
				return "", "", bad
			}
			i += j
		}
		parts = append(parts, p)
		if i == len(ns) {
			break
		}
		if ns[i] != '.' || i+1 == len(ns) {
			return "", "", bad
		}
		i++
	}
	switch len(parts) {
	case 1:
		return parts[0], "", nil
	case 2:
		return parts[0], parts[1], nil
	}
	return "", "", bad
}

// resolve returns the database and schema a namespace names, "" meaning
// the connection's default.
func (c *conn) resolve(op, ns string) (db, schema string, err error) {
	if ns == "" {
		if c.defaultDB == "" || c.defaultSchema == "" {
			return "", "", driver.Wrap(driver.ErrNotFound, Name, op, errors.New("the connection has no current schema: name a namespace (database.schema)"))
		}
		return c.defaultDB, c.defaultSchema, nil
	}
	db, schema, err = splitNamespace(ns)
	if err == nil && schema == "" {
		err = errors.New("invalid namespace: want database.schema")
	}
	if err != nil {
		return "", "", driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	return db, schema, nil
}

// column returns the index of a SHOW result column, case-insensitively.
func column(names []string, name string) int {
	return slices.IndexFunc(names, func(n string) bool { return strings.EqualFold(n, name) })
}

func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	names, rows, err := collectStrings(ctx, op, c.db, "SHOW SCHEMAS IN ACCOUNT")
	if err != nil {
		return nil, c.scrub(err)
	}
	ni, di, ci := column(names, "name"), column(names, "database_name"), column(names, "comment")
	if ni < 0 || di < 0 {
		return nil, driver.Wrap(nil, Name, op, errors.New("SHOW SCHEMAS returned no name or database_name column"))
	}
	def := c.defaultNamespace()
	var out []driver.Namespace
	for _, r := range rows {
		if strings.EqualFold(r[ni], "INFORMATION_SCHEMA") {
			continue
		}
		ns := driver.Namespace{Name: namespaceName(r[di], r[ni]), Label: "schema"}
		if ci >= 0 {
			ns.Comment = r[ci]
		}
		ns.Default = ns.Name == def
		out = append(out, ns)
		if len(out) == maxCatalogRows {
			break
		}
	}
	slices.SortFunc(out, func(a, b driver.Namespace) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// infoSchema returns the quoted INFORMATION_SCHEMA view of database db.
func infoSchema(db, view string) string { return quoteIdent(db) + ".INFORMATION_SCHEMA." + view }

// collectionType maps an INFORMATION_SCHEMA.TABLES table_type.
func collectionType(t string) driver.CollectionType {
	switch t = strings.ToUpper(t); {
	case t == "VIEW":
		return driver.CollectionView
	case t == "MATERIALIZED VIEW":
		return driver.CollectionMaterializedView
	case strings.Contains(t, "TABLE"), t == "LOCAL TEMPORARY":
		return driver.CollectionTable
	}
	return driver.CollectionOther
}

func estimate(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return -1
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	db, schema, err := c.resolve(op, ns.Name)
	if err != nil {
		return nil, err
	}
	_, rows, err := collectStrings(ctx, op, c.db,
		"SELECT table_name, table_type, row_count, comment FROM "+infoSchema(db, "TABLES")+
			" WHERE table_schema = ? ORDER BY table_name LIMIT "+strconv.Itoa(maxCatalogRows), schema)
	if err != nil {
		return nil, c.scrub(err)
	}
	name := namespaceName(db, schema)
	out := make([]driver.Collection, 0, len(rows))
	for _, r := range rows {
		out = append(out, driver.Collection{
			Ref:         driver.CollectionRef{Namespace: name, Name: r[0]},
			Type:        collectionType(r[1]),
			RowEstimate: estimate(r[2]),
			Comment:     r[3],
		})
	}
	if len(out) == 0 {
		if err := c.requireSchema(ctx, op, db, schema); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// requireSchema returns ErrNotFound when schema does not exist in db.
func (c *conn) requireSchema(ctx context.Context, op, db, schema string) error {
	names, rows, err := collectStrings(ctx, op, c.db, "SHOW SCHEMAS IN DATABASE "+quoteIdent(db))
	if err != nil {
		return c.scrub(err)
	}
	if ni := column(names, "name"); ni >= 0 {
		for _, r := range rows {
			if r[ni] == schema {
				return nil
			}
		}
	}
	return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no schema %q in database %q", schema, db))
}

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	s, err := c.describe(ctx, op, ref)
	return s, c.scrub(err)
}

// describe reads a table's or view's columns and keys.
func (c *conn) describe(ctx context.Context, op string, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	db, schema, err := c.resolve(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	_, trows, err := collectStrings(ctx, op, c.db,
		"SELECT table_type, row_count, bytes, comment FROM "+infoSchema(db, "TABLES")+
			" WHERE table_schema = ? AND table_name = ?", schema, ref.Name)
	if err != nil {
		return nil, err
	}
	if len(trows) == 0 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no table or view %q in %s", ref.Name, namespaceName(db, schema)))
	}
	out := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: namespaceName(db, schema), Name: ref.Name},
		Type:        collectionType(trows[0][0]),
		RowEstimate: estimate(trows[0][1]),
		SizeBytes:   estimate(trows[0][2]),
		Comment:     trows[0][3],
	}
	_, crows, err := collectStrings(ctx, op, c.db,
		"SELECT column_name, data_type, is_nullable, column_default, comment, numeric_precision, numeric_scale, is_identity FROM "+
			infoSchema(db, "COLUMNS")+" WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", schema, ref.Name)
	if err != nil {
		return nil, err
	}
	for _, r := range crows {
		scale, _ := strconv.ParseInt(r[6], 10, 64)
		native := strings.ToUpper(r[1])
		if native == "NUMBER" && r[5] != "" {
			native = fmt.Sprintf("NUMBER(%s,%d)", r[5], scale)
		}
		def := r[3]
		out.Fields = append(out.Fields, driver.Field{
			Name:       r[0],
			NativeType: native,
			Type:       columnType(r[1], scale),
			Nullable:   strings.EqualFold(r[2], "YES"),
			Default:    def,
			Comment:    r[4],
			AutoIncrement: strings.EqualFold(r[7], "YES") || strings.EqualFold(r[7], "true") ||
				strings.Contains(strings.ToUpper(def), ".NEXTVAL") || strings.Contains(strings.ToUpper(def), "IDENTITY") ||
				strings.Contains(strings.ToUpper(def), "AUTOINCREMENT"),
		})
	}
	if out.Type == driver.CollectionTable {
		if out.PrimaryKey, err = c.primaryKey(ctx, op, db, schema, ref.Name); err != nil {
			return nil, err
		}
	}
	if len(out.PrimaryKey) > 0 {
		out.Indexes = []driver.Index{{Name: "PRIMARY KEY", Fields: out.PrimaryKey, Unique: true, Primary: true}}
	}
	return out, nil
}

// primaryKey reads a table's declared primary key, in key order. Snowflake
// records it but does not enforce it on standard tables (see Mutator).
func (c *conn) primaryKey(ctx context.Context, op, db, schema, table string) ([]string, error) {
	names, rows, err := collectStrings(ctx, op, c.db,
		"SHOW PRIMARY KEYS IN TABLE "+quoteIdent(db)+"."+quoteIdent(schema)+"."+quoteIdent(table))
	if err != nil {
		return nil, err
	}
	ci, si := column(names, "column_name"), column(names, "key_sequence")
	if ci < 0 {
		return nil, nil
	}
	// Keep only the rows of this table: fakesnow answers with every
	// schema's table of that name.
	owner := map[int]string{column(names, "database_name"): db, column(names, "schema_name"): schema, column(names, "table_name"): table}
	type keyCol struct {
		name string
		seq  int64
	}
	var cols []keyCol
rows:
	for _, r := range rows {
		for i, want := range owner {
			if i >= 0 && r[i] != want {
				continue rows
			}
		}
		k := keyCol{name: r[ci]}
		if si >= 0 {
			k.seq, _ = strconv.ParseInt(r[si], 10, 64)
		}
		cols = append(cols, k)
	}
	slices.SortStableFunc(cols, func(a, b keyCol) int { return int(a.seq - b.seq) })
	out := make([]string, len(cols))
	for i, k := range cols {
		out[i] = k.name
	}
	return out, nil
}
