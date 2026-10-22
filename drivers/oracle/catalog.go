package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Catalog reads come from the ALL_* dictionary views, so they list what the
// login user can see. Names reach the server only as bind parameters. An
// empty namespace means the login schema. Names are matched exactly as the
// dictionary stores them; when nothing matches, the upper-cased name (how
// Oracle folds an unquoted identifier) is tried.

const namespacesSQL = `
SELECT username, CASE WHEN username = SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') THEN 1 ELSE 0 END
FROM all_users
WHERE oracle_maintained = 'N' OR username = SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')
ORDER BY username`

// Namespaces lists the schemas (users) the login user can see, without the
// ones Oracle maintains (SYS, SYSTEM, XDB…) unless it is the current one.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, namespacesSQL)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Namespace
	for rows.Next() {
		ns := driver.Namespace{Label: "schema"}
		var def int64
		if err := rows.Scan(&ns.Name, &def); err != nil {
			return nil, mapError(ctx, op, err)
		}
		ns.Default = def == 1
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return out, nil
}

const collectionsSQL = `
SELECT t.table_name,
       CASE WHEN m.mview_name IS NOT NULL THEN 'MVIEW' ELSE 'TABLE' END,
       t.num_rows, c.comments
FROM all_tables t
LEFT JOIN all_mviews m ON m.owner = t.owner AND m.mview_name = t.table_name
LEFT JOIN all_tab_comments c ON c.owner = t.owner AND c.table_name = t.table_name
WHERE t.owner = :1 AND t.nested = 'NO' AND t.secondary = 'N' AND t.dropped = 'NO'
  AND (t.iot_type IS NULL OR t.iot_type = 'IOT')
UNION ALL
SELECT v.view_name, 'VIEW', NULL, c.comments
FROM all_views v
LEFT JOIN all_tab_comments c ON c.owner = v.owner AND c.table_name = v.view_name
WHERE v.owner = :2
ORDER BY 1`

func collectionType(t string) driver.CollectionType {
	switch t {
	case "VIEW":
		return driver.CollectionView
	case "MVIEW":
		return driver.CollectionMaterializedView
	}
	return driver.CollectionTable
}

// Collections lists the tables, views and materialized views of a schema.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	owner, err := c.resolveSchema(ctx, op, ns.Name)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, collectionsSQL, owner, owner)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Collection
	for rows.Next() {
		col := driver.Collection{Ref: driver.CollectionRef{Namespace: owner}}
		var typ string
		var est sql.NullInt64
		var comment sql.NullString
		if err := rows.Scan(&col.Ref.Name, &typ, &est, &comment); err != nil {
			return nil, mapError(ctx, op, err)
		}
		col.Type = collectionType(typ)
		col.RowEstimate = -1
		if est.Valid {
			col.RowEstimate = est.Int64
		}
		col.Comment = comment.String
		out = append(out, col)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return out, nil
}

// resolveSchema returns the schema name as the dictionary stores it: the
// login schema for "", name itself when such a user exists, else its
// upper-cased form when that exists. An unknown schema is ErrNotFound.
func (c *conn) resolveSchema(ctx context.Context, op, name string) (string, error) {
	if name == "" {
		return c.user, nil
	}
	var got string
	err := c.db.QueryRowContext(ctx,
		"SELECT username FROM all_users WHERE username IN (:1, :2) ORDER BY CASE WHEN username = :3 THEN 0 ELSE 1 END FETCH FIRST 1 ROWS ONLY",
		name, strings.ToUpper(name), name).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no schema %q", name))
	}
	if err != nil {
		return "", mapError(ctx, op, err)
	}
	return got, nil
}

const relationSQL = `
SELECT object_name, type, num_rows, comments FROM (
  SELECT t.table_name AS object_name,
         CASE WHEN m.mview_name IS NOT NULL THEN 'MVIEW' ELSE 'TABLE' END AS type,
         t.num_rows, c.comments
  FROM all_tables t
  LEFT JOIN all_mviews m ON m.owner = t.owner AND m.mview_name = t.table_name
  LEFT JOIN all_tab_comments c ON c.owner = t.owner AND c.table_name = t.table_name
  WHERE t.owner = :1 AND t.table_name IN (:2, :3)
  UNION ALL
  SELECT v.view_name, 'VIEW', NULL, c.comments
  FROM all_views v
  LEFT JOIN all_tab_comments c ON c.owner = v.owner AND c.table_name = v.view_name
  WHERE v.owner = :4 AND v.view_name IN (:5, :6)
)
ORDER BY CASE WHEN object_name = :7 THEN 0 ELSE 1 END
FETCH FIRST 1 ROWS ONLY`

const fieldsSQL = `
SELECT c.column_name, c.data_type, c.data_precision, c.data_scale, c.char_length, c.char_used,
       c.nullable, c.data_default, c.virtual_column, c.identity_column, cc.comments
FROM all_tab_cols c
LEFT JOIN all_col_comments cc ON cc.owner = c.owner AND cc.table_name = c.table_name AND cc.column_name = c.column_name
WHERE c.owner = :1 AND c.table_name = :2 AND c.hidden_column = 'NO'
ORDER BY c.column_id`

const constraintColsSQL = `
SELECT k.constraint_name, k.constraint_type, cc.column_name, k.r_owner, k.r_constraint_name
FROM all_constraints k
JOIN all_cons_columns cc ON cc.owner = k.owner AND cc.constraint_name = k.constraint_name AND cc.table_name = k.table_name
WHERE k.owner = :1 AND k.table_name = :2 AND k.constraint_type IN ('P', 'R')
ORDER BY k.constraint_type, k.constraint_name, cc.position`

const refColsSQL = `
SELECT table_name, column_name FROM all_cons_columns
WHERE owner = :1 AND constraint_name = :2
ORDER BY position`

const indexesSQL = `
SELECT i.index_name, i.uniqueness, i.index_type, ic.column_name,
       CASE WHEN EXISTS (SELECT 1 FROM all_constraints k WHERE k.owner = i.table_owner AND k.table_name = i.table_name
                          AND k.constraint_type = 'P' AND k.index_name = i.index_name) THEN 1 ELSE 0 END
FROM all_indexes i
JOIN all_ind_columns ic ON ic.index_owner = i.owner AND ic.index_name = i.index_name
WHERE i.table_owner = :1 AND i.table_name = :2 AND i.index_type <> 'LOB'
ORDER BY i.index_name, ic.column_position`

// Describe returns a table's or view's columns (ALL_TAB_COLS, without hidden
// columns), primary key, indexes, foreign keys, comments and the optimizer's
// row estimate. A missing collection is ErrNotFound.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	owner, err := c.resolveSchema(ctx, op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Namespace: owner}, RowEstimate: -1, SizeBytes: -1}
	var typ string
	var est sql.NullInt64
	var comment sql.NullString
	up := strings.ToUpper(ref.Name)
	err = c.db.QueryRowContext(ctx, relationSQL, owner, ref.Name, up, owner, ref.Name, up, ref.Name).Scan(&s.Ref.Name, &typ, &est, &comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no table or view %q in schema %q", ref.Name, owner))
	}
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	s.Type = collectionType(typ)
	if est.Valid {
		s.RowEstimate = est.Int64
	}
	s.Comment = comment.String
	if err := c.describeFields(ctx, s); err != nil {
		return nil, mapError(ctx, op, err)
	}
	if err := c.describeConstraints(ctx, s); err != nil {
		return nil, mapError(ctx, op, err)
	}
	if err := c.describeIndexes(ctx, s); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return s, nil
}

func (c *conn) describeFields(ctx context.Context, s *driver.CollectionSchema) error {
	rows, err := c.db.QueryContext(ctx, fieldsSQL, s.Ref.Namespace, s.Ref.Name)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, dataType, nullable string
		var precision, scale, charLen sql.NullInt64
		var charUsed, def, virtual, identity, comment sql.NullString
		if err := rows.Scan(&name, &dataType, &precision, &scale, &charLen, &charUsed, &nullable, &def, &virtual, &identity, &comment); err != nil {
			return err
		}
		f := driver.Field{
			Name:          name,
			NativeType:    nativeFieldType(dataType, precision, scale, charLen, charUsed.String),
			Type:          fieldType(dataType, precision, scale),
			Nullable:      nullable == "Y",
			Generated:     virtual.String == "YES",
			AutoIncrement: identity.String == "YES",
			Comment:       comment.String,
		}
		if !f.Generated && !f.AutoIncrement {
			f.Default = strings.TrimSpace(def.String)
		}
		s.Fields = append(s.Fields, f)
	}
	return rows.Err()
}

// nativeFieldType spells a column type as Oracle's DDL does.
func nativeFieldType(dataType string, precision, scale, charLen sql.NullInt64, charUsed string) string {
	switch dataType {
	case "NUMBER":
		switch {
		case precision.Valid && scale.Valid && scale.Int64 != 0:
			return fmt.Sprintf("NUMBER(%d,%d)", precision.Int64, scale.Int64)
		case precision.Valid:
			return fmt.Sprintf("NUMBER(%d)", precision.Int64)
		case scale.Valid && scale.Int64 == 0:
			return "INTEGER"
		}
		return "NUMBER"
	case "FLOAT":
		if precision.Valid {
			return fmt.Sprintf("FLOAT(%d)", precision.Int64)
		}
	case "VARCHAR2", "CHAR":
		if charLen.Valid {
			unit := " BYTE"
			if charUsed == "C" {
				unit = " CHAR"
			}
			return fmt.Sprintf("%s(%d%s)", dataType, charLen.Int64, unit)
		}
	case "NVARCHAR2", "NCHAR":
		if charLen.Valid {
			return fmt.Sprintf("%s(%d)", dataType, charLen.Int64)
		}
	}
	return dataType
}

func (c *conn) describeConstraints(ctx context.Context, s *driver.CollectionSchema) error {
	type fk struct {
		name, rOwner, rName string
		cols                []string
	}
	var fks []*fk
	rows, err := c.db.QueryContext(ctx, constraintColsSQL, s.Ref.Namespace, s.Ref.Name)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, typ, col string
		var rOwner, rName sql.NullString
		if err := rows.Scan(&name, &typ, &col, &rOwner, &rName); err != nil {
			_ = rows.Close()
			return err
		}
		switch typ {
		case "P":
			s.PrimaryKey = append(s.PrimaryKey, col)
		case "R":
			if len(fks) == 0 || fks[len(fks)-1].name != name {
				fks = append(fks, &fk{name: name, rOwner: rOwner.String, rName: rName.String})
			}
			fks[len(fks)-1].cols = append(fks[len(fks)-1].cols, col)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, k := range fks {
		out := driver.ForeignKey{Name: k.name, Fields: k.cols, References: driver.CollectionRef{Namespace: k.rOwner}}
		refRows, err := c.db.QueryContext(ctx, refColsSQL, k.rOwner, k.rName)
		if err != nil {
			return err
		}
		for refRows.Next() {
			var tbl, col string
			if err := refRows.Scan(&tbl, &col); err != nil {
				_ = refRows.Close()
				return err
			}
			out.References.Name = tbl
			out.RefFields = append(out.RefFields, col)
		}
		err = refRows.Err()
		_ = refRows.Close()
		if err != nil {
			return err
		}
		s.ForeignKeys = append(s.ForeignKeys, out)
	}
	return nil
}

func (c *conn) describeIndexes(ctx context.Context, s *driver.CollectionSchema) error {
	rows, err := c.db.QueryContext(ctx, indexesSQL, s.Ref.Namespace, s.Ref.Name)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, uniq, method, col string
		var primary int64
		if err := rows.Scan(&name, &uniq, &method, &col, &primary); err != nil {
			return err
		}
		if n := len(s.Indexes); n == 0 || s.Indexes[n-1].Name != name {
			s.Indexes = append(s.Indexes, driver.Index{Name: name, Unique: uniq == "UNIQUE", Primary: primary == 1, Method: method})
		}
		s.Indexes[len(s.Indexes)-1].Fields = append(s.Indexes[len(s.Indexes)-1].Fields, col)
	}
	return rows.Err()
}
