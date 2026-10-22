package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Catalog reads come from information_schema and always run in
// withReadOnlyTx. Names reach the server only as parameters. An empty
// namespace means the connection's default database (DATABASE()).

// namespacesSQL lists the databases, without the flavor's system ones.
func namespacesSQL(f flavor) string {
	system := "'information_schema', 'mysql', 'performance_schema', 'sys'"
	if f == flavorSingleStore {
		system = "'information_schema', 'memsql', 'cluster'"
	}
	return `
SELECT SCHEMA_NAME, COALESCE(SCHEMA_NAME = DATABASE(), FALSE)
FROM information_schema.SCHEMATA
WHERE SCHEMA_NAME NOT IN (` + system + `)
ORDER BY SCHEMA_NAME`
}

func (c *conn) Namespaces(ctx context.Context) (_ []driver.Namespace, err error) {
	defer func() { err = c.named(err) }()
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	var out []driver.Namespace
	err = c.withReadOnlyTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, namespacesSQL(c.syntaxFlavor()))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			ns := driver.Namespace{Label: "database"}
			if err := rows.Scan(&ns.Name, &ns.Default); err != nil {
				return err
			}
			out = append(out, ns)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapError(ctx, "namespaces", err)
	}
	return out, nil
}

const collectionsSQL = `
SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, TABLE_ROWS, COALESCE(TABLE_COMMENT, '')
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())
ORDER BY TABLE_NAME`

// collectionType maps information_schema TABLE_TYPE.
func collectionType(tableType string) driver.CollectionType {
	if strings.Contains(tableType, "VIEW") { // VIEW, SYSTEM VIEW
		return driver.CollectionView
	}
	return driver.CollectionTable
}

// tableComment drops the "VIEW" placeholder MySQL reports as a view's comment.
func tableComment(typ driver.CollectionType, comment string) string {
	if typ == driver.CollectionView && comment == "VIEW" {
		return ""
	}
	return comment
}

func nullOr(n sql.NullInt64, def int64) int64 {
	if !n.Valid {
		return def
	}
	return n.Int64
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) (_ []driver.Collection, err error) {
	defer func() { err = c.named(err) }()
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	var out []driver.Collection
	err = c.withReadOnlyTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, collectionsSQL, ns.Name)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var col driver.Collection
			var typ string
			var est sql.NullInt64
			if err := rows.Scan(&col.Ref.Namespace, &col.Ref.Name, &typ, &est, &col.Comment); err != nil {
				return err
			}
			col.Type = collectionType(typ)
			col.Comment = tableComment(col.Type, col.Comment)
			col.RowEstimate = nullOr(est, -1)
			if col.Type == driver.CollectionView {
				col.RowEstimate = -1
			}
			out = append(out, col)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapError(ctx, "collections", err)
	}
	return out, nil
}

const relationSQL = `
SELECT TABLE_SCHEMA, TABLE_TYPE, TABLE_ROWS, COALESCE(TABLE_COMMENT, ''), DATA_LENGTH + INDEX_LENGTH
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE()) AND TABLE_NAME = ?`

const fieldsSQL = `
SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA,
       COALESCE(GENERATION_EXPRESSION, ''), COALESCE(COLUMN_COMMENT, '')
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY ORDINAL_POSITION`

// indexesSQL lists a table's index columns in order, primary key first.
//
//   - MySQL: a functional key part has no COLUMN_NAME; it is shown as its
//     EXPRESSION.
//   - MariaDB: STATISTICS has no EXPRESSION column.
//   - SingleStore: an index can appear once per index type (a columnstore
//     primary key is a SHARD row and a COLUMNSTORE HASH row), so the rows
//     are folded per column; the shard key's own rows (type SHARD, such as
//     __SHARDKEY) are left out.
func indexesSQL(f flavor) string {
	switch f {
	case flavorMariaDB:
		return `
SELECT INDEX_NAME, NON_UNIQUE, INDEX_TYPE, COALESCE(COLUMN_NAME, '')
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY INDEX_NAME = 'PRIMARY' DESC, INDEX_NAME, SEQ_IN_INDEX`
	case flavorSingleStore:
		return `
SELECT INDEX_NAME, MIN(NON_UNIQUE), MIN(INDEX_TYPE), COALESCE(COLUMN_NAME, '')
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_TYPE <> 'SHARD'
GROUP BY INDEX_NAME, SEQ_IN_INDEX, COLUMN_NAME
ORDER BY INDEX_NAME = 'PRIMARY' DESC, INDEX_NAME, SEQ_IN_INDEX`
	}
	return `
SELECT INDEX_NAME, NON_UNIQUE, INDEX_TYPE, COALESCE(COLUMN_NAME, EXPRESSION, '')
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY INDEX_NAME = 'PRIMARY' DESC, INDEX_NAME, SEQ_IN_INDEX`
}

const foreignKeysSQL = `
SELECT CONSTRAINT_NAME, COLUMN_NAME, REFERENCED_TABLE_SCHEMA, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
FROM information_schema.KEY_COLUMN_USAGE
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND REFERENCED_TABLE_NAME IS NOT NULL
ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION`

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (_ *driver.CollectionSchema, err error) {
	defer func() { err = c.named(err) }()
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	var s *driver.CollectionSchema
	err = c.withReadOnlyTx(ctx, func(tx *sql.Tx) (err error) {
		s, err = describeIn(ctx, c.syntaxFlavor(), tx, op, ref)
		return err
	})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	return s, nil
}

// queryer is what the catalog reads run on: a *sql.Tx, or the *sql.Conn of
// a transaction or mutation.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// describeIn describes ref over tx, with flavor f's catalog SQL.
func describeIn(ctx context.Context, f flavor, tx queryer, op string, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Name: ref.Name}}
	var typ string
	var est, size sql.NullInt64
	err := tx.QueryRowContext(ctx, relationSQL, ref.Namespace, ref.Name).Scan(&s.Ref.Namespace, &typ, &est, &s.Comment, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("%s does not exist", qualified(ref)))
	}
	if err != nil {
		return nil, err
	}
	s.Type = collectionType(typ)
	s.Comment = tableComment(s.Type, s.Comment)
	s.RowEstimate, s.SizeBytes = nullOr(est, -1), nullOr(size, -1)
	if s.Type == driver.CollectionView {
		s.RowEstimate, s.SizeBytes = -1, -1
	}
	if s.Fields, err = describeFields(ctx, f, tx, s.Ref); err != nil {
		return nil, err
	}
	if s.Indexes, err = describeIndexes(ctx, f, tx, s.Ref); err != nil {
		return nil, err
	}
	for _, ix := range s.Indexes {
		if ix.Primary {
			s.PrimaryKey = ix.Fields
		}
	}
	if s.ForeignKeys, err = describeForeignKeys(ctx, tx, s.Ref); err != nil {
		return nil, err
	}
	return s, nil
}

func qualified(ref driver.CollectionRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "." + ref.Name
}

func describeFields(ctx context.Context, f flavor, tx queryer, ref driver.CollectionRef) ([]driver.Field, error) {
	rows, err := tx.QueryContext(ctx, fieldsSQL, ref.Namespace, ref.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Field
	for rows.Next() {
		var fd driver.Field
		var dataType, nullable, extra, genExpr string
		var def sql.NullString
		if err := rows.Scan(&fd.Name, &dataType, &fd.NativeType, &nullable, &def, &extra, &genExpr, &fd.Comment); err != nil {
			return nil, err
		}
		fd.Type = fieldType(dataType, fd.NativeType)
		fd.Nullable = nullable == "YES"
		extra = strings.ToUpper(extra)
		// SingleStore marks a computed (PERSISTED) column "computed".
		fd.Generated = strings.Contains(extra, "VIRTUAL GENERATED") || strings.Contains(extra, "STORED GENERATED") || extra == "COMPUTED"
		fd.AutoIncrement = strings.Contains(extra, "AUTO_INCREMENT")
		switch {
		case fd.Generated:
			fd.Default = genExpr
		case def.Valid:
			fd.Default = defaultExpr(f, def.String, fd.Type, extra)
		}
		out = append(out, fd)
	}
	return out, rows.Err()
}

// defaultExpr renders information_schema COLUMN_DEFAULT (def, with the
// column's upper-cased EXTRA) as a native default expression.
//
//   - MySQL stores a literal default unquoted, so a non-numeric literal is
//     quoted; an expression default (EXTRA has DEFAULT_GENERATED) is kept.
//   - MariaDB already stores an expression: quoted literals, numbers and
//     expressions such as current_timestamp(). The text NULL is its "no
//     default other than NULL", which is no default.
//   - SingleStore stores literals unquoted like MySQL but never says
//     DEFAULT_GENERATED, so CURRENT_TIMESTAMP and NOW() on a time column are
//     taken as expressions.
func defaultExpr(f flavor, def string, typ driver.Type, extra string) string {
	switch f {
	case flavorMariaDB:
		if def == "NULL" {
			return ""
		}
		return def
	case flavorSingleStore:
		u := strings.ToUpper(def)
		if typ == driver.TypeTime && (strings.HasPrefix(u, "CURRENT_TIMESTAMP") || strings.HasPrefix(u, "NOW(")) {
			return def
		}
	}
	switch {
	case strings.Contains(extra, "DEFAULT_GENERATED"):
		return def
	case typ == driver.TypeInt || typ == driver.TypeFloat || typ == driver.TypeDecimal || typ == driver.TypeBool:
		return def
	}
	return "'" + strings.ReplaceAll(def, "'", "''") + "'"
}

func describeIndexes(ctx context.Context, f flavor, tx queryer, ref driver.CollectionRef) ([]driver.Index, error) {
	rows, err := tx.QueryContext(ctx, indexesSQL(f), ref.Namespace, ref.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Index
	for rows.Next() {
		var name, method, field string
		var nonUnique int64
		if err := rows.Scan(&name, &nonUnique, &method, &field); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, driver.Index{Name: name, Unique: nonUnique == 0, Primary: name == "PRIMARY", Method: strings.ToLower(method)})
		}
		ix := &out[len(out)-1]
		ix.Fields = append(ix.Fields, field)
	}
	return out, rows.Err()
}

func describeForeignKeys(ctx context.Context, tx queryer, ref driver.CollectionRef) ([]driver.ForeignKey, error) {
	rows, err := tx.QueryContext(ctx, foreignKeysSQL, ref.Namespace, ref.Name)
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
			out = append(out, driver.ForeignKey{Name: name, References: driver.CollectionRef{Namespace: refSchema, Name: refTable}})
		}
		fk := &out[len(out)-1]
		fk.Fields = append(fk.Fields, field)
		fk.RefFields = append(fk.RefFields, refField)
	}
	return out, rows.Err()
}
