package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

// Catalog reads always run in withReadOnlyTx, on read-only and writable
// connections alike: they never write, and one path means the read-only
// guard cannot be skipped by accident. Names reach the server only as
// parameters, never interpolated into SQL.

const namespacesSQL = `
select n.nspname,
       coalesce(obj_description(n.oid, 'pg_namespace'), ''),
       coalesce(n.nspname = current_schema(), false)
from pg_namespace n
where n.nspname <> 'pg_catalog'
  and n.nspname <> 'information_schema'
  and n.nspname not like 'pg\_toast%'
  and n.nspname not like 'pg\_temp\_%'
order by n.nspname`

func (c *conn) Namespaces(ctx context.Context) (_ []driver.Namespace, err error) {
	defer func() { err = c.named(err) }()
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	var out []driver.Namespace
	err = withReadOnlyTx(ctx, c.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, catalogFor(c.flavor).namespaces)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ns := driver.Namespace{Label: "schema"}
			if err := rows.Scan(&ns.Name, &ns.Comment, &ns.Default); err != nil {
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

// relkinds: r table, v view, m materialized view, p partitioned table.
const collectionsSQL = `
select n.nspname, c.relname, c.relkind::text, c.reltuples::int8,
       coalesce(obj_description(c.oid, 'pg_class'), '')
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where n.nspname = coalesce(nullif($1::text, ''), current_schema())
  and c.relkind in ('r', 'v', 'm', 'p')
order by c.relname`

func collectionType(relkind string) driver.CollectionType {
	switch relkind {
	case "v":
		return driver.CollectionView
	case "m":
		return driver.CollectionMaterializedView
	}
	return driver.CollectionTable
}

// estimate turns reltuples into a row estimate: negative means never analyzed.
func estimate(reltuples int64) int64 {
	if reltuples < 0 {
		return -1
	}
	return reltuples
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) (_ []driver.Collection, err error) {
	defer func() { err = c.named(err) }()
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	var out []driver.Collection
	err = withReadOnlyTx(ctx, c.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, catalogFor(c.flavor).collections, ns.Name)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var col driver.Collection
			var kind string
			var tuples int64
			if err := rows.Scan(&col.Ref.Namespace, &col.Ref.Name, &kind, &tuples, &col.Comment); err != nil {
				return err
			}
			col.Type, col.RowEstimate = collectionType(kind), estimate(tuples)
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
select c.oid, n.nspname, c.relkind::text, c.reltuples::int8,
       coalesce(obj_description(c.oid, 'pg_class'), ''),
       case when c.relkind in ('r', 'm', 'p') then pg_total_relation_size(c.oid) else -1 end
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where n.nspname = coalesce(nullif($1::text, ''), current_schema())
  and c.relname = $2
  and c.relkind in ('r', 'v', 'm', 'p')`

// fieldsSQL reads the fields. GENERATED ALWAYS AS IDENTITY ('a') cannot be
// written, so it is Generated as well as AutoIncrement; BY DEFAULT ('d')
// and nextval defaults (serial) are AutoIncrement only.
const fieldsSQL = `
select a.attname, format_type(a.atttypid, a.atttypmod), a.atttypid::int8, t.typname,
       not a.attnotnull,
       coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
       a.attgenerated::text <> '' or a.attidentity = 'a',
       a.attidentity in ('a', 'd') or coalesce(pg_get_expr(d.adbin, d.adrelid), '') like 'nextval(%',
       coalesce(col_description(a.attrelid, a.attnum), '')
from pg_attribute a
join pg_type t on t.oid = a.atttypid
left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
where a.attrelid = $1::oid and a.attnum > 0 and not a.attisdropped
order by a.attnum`

// An expression column of an index has attnum 0; it is shown as its
// expression. INCLUDE columns (past indnkeyatts) are not key fields.
const indexesSQL = `
select ic.relname, i.indisunique, i.indisprimary, am.amname,
       array(select coalesce(a.attname::text, pg_get_indexdef(i.indexrelid, k.ord::int, true))
             from unnest(i.indkey::int2[]) with ordinality as k(attnum, ord)
             left join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
             where k.ord <= i.indnkeyatts
             order by k.ord)
from pg_index i
join pg_class ic on ic.oid = i.indexrelid
join pg_am am on am.oid = ic.relam
where i.indrelid = $1::oid
order by i.indisprimary desc, ic.relname`

const foreignKeysSQL = `
select con.conname,
       array(select a.attname::text
             from unnest(con.conkey) with ordinality as k(attnum, ord)
             join pg_attribute a on a.attrelid = con.conrelid and a.attnum = k.attnum
             order by k.ord),
       fn.nspname, fc.relname,
       array(select a.attname::text
             from unnest(con.confkey) with ordinality as k(attnum, ord)
             join pg_attribute a on a.attrelid = con.confrelid and a.attnum = k.attnum
             order by k.ord)
from pg_constraint con
join pg_class fc on fc.oid = con.confrelid
join pg_namespace fn on fn.oid = fc.relnamespace
where con.conrelid = $1::oid and con.contype = 'f'
order by con.conname`

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (_ *driver.CollectionSchema, err error) {
	defer func() { err = c.named(err) }()
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	var s *driver.CollectionSchema
	err = withReadOnlyTx(ctx, c.pool, func(tx pgx.Tx) (err error) {
		s, err = describeIn(ctx, tx, c.flavor, op, ref)
		return err
	})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	return s, nil
}

// describeIn describes ref over tx, which may be a caller's open
// transaction, with the catalog queries of flavor f.
func describeIn(ctx context.Context, tx pgx.Tx, f flavor, op string, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	cat := catalogFor(f)
	var oid uint32
	var kind string
	var tuples int64
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Name: ref.Name}}
	err := tx.QueryRow(ctx, cat.relation, ref.Namespace, ref.Name).
		Scan(&oid, &s.Ref.Namespace, &kind, &tuples, &s.Comment, &s.SizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("%s does not exist", qualified(ref)))
	}
	if err != nil {
		return nil, err
	}
	s.Type, s.RowEstimate = collectionType(kind), estimate(tuples)

	if s.Fields, err = describeFields(ctx, tx, cat.fields, oid); err != nil {
		return nil, err
	}
	if cat.indexes != "" {
		if s.Indexes, err = describeIndexes(ctx, tx, cat.indexes, oid); err != nil {
			return nil, err
		}
	}
	for _, ix := range s.Indexes {
		if ix.Primary {
			s.PrimaryKey = ix.Fields
		}
	}
	if cat.primaryKey != "" {
		rows, err := tx.Query(ctx, cat.primaryKey, s.Ref.Namespace, s.Ref.Name)
		if err != nil {
			return nil, err
		}
		if s.PrimaryKey, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, err
		}
	}
	if cat.fkRows {
		s.ForeignKeys, err = describeForeignKeyRows(ctx, tx, cat.foreignKeys, s.Ref)
	} else {
		s.ForeignKeys, err = describeForeignKeys(ctx, tx, cat.foreignKeys, oid)
	}
	if err != nil {
		return nil, err
	}
	if cat.keys != "" {
		keys, err := describeKeys(ctx, tx, cat, oid, s.Ref)
		if err != nil {
			return nil, err
		}
		s.Indexes = append(s.Indexes, keys...)
	}
	return s, nil
}

func qualified(ref driver.CollectionRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "." + ref.Name
}

func describeFields(ctx context.Context, tx pgx.Tx, query string, oid uint32) ([]driver.Field, error) {
	rows, err := tx.Query(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tm := rows.Conn().TypeMap()
	var out []driver.Field
	for rows.Next() {
		var f driver.Field
		var typOID int64
		var typName string
		if err := rows.Scan(&f.Name, &f.NativeType, &typOID, &typName, &f.Nullable, &f.Default, &f.Generated, &f.AutoIncrement, &f.Comment); err != nil {
			return nil, err
		}
		f.Type = columnType(tm, uint32(typOID), typName)
		out = append(out, f)
	}
	return out, rows.Err()
}

func describeIndexes(ctx context.Context, tx pgx.Tx, query string, oid uint32) ([]driver.Index, error) {
	rows, err := tx.Query(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Index
	for rows.Next() {
		var ix driver.Index
		if err := rows.Scan(&ix.Name, &ix.Unique, &ix.Primary, &ix.Method, &ix.Fields); err != nil {
			return nil, err
		}
		out = append(out, ix)
	}
	return out, rows.Err()
}

func describeForeignKeys(ctx context.Context, tx pgx.Tx, query string, oid uint32) ([]driver.ForeignKey, error) {
	rows, err := tx.Query(ctx, query, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.ForeignKey
	for rows.Next() {
		var fk driver.ForeignKey
		if err := rows.Scan(&fk.Name, &fk.Fields, &fk.References.Namespace, &fk.References.Name, &fk.RefFields); err != nil {
			return nil, err
		}
		out = append(out, fk)
	}
	return out, rows.Err()
}

// describeForeignKeyRows reads foreign keys from one row per referencing
// column (constraint, column, referenced schema, table and column), in
// constraint then column order, for flavors without arrays (Redshift).
func describeForeignKeyRows(ctx context.Context, tx pgx.Tx, query string, ref driver.CollectionRef) ([]driver.ForeignKey, error) {
	rows, err := tx.Query(ctx, query, ref.Namespace, ref.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.ForeignKey
	for rows.Next() {
		var name, col, refNS, refName, refCol string
		if err := rows.Scan(&name, &col, &refNS, &refName, &refCol); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].Name != name {
			out = append(out, driver.ForeignKey{Name: name, References: driver.CollectionRef{Namespace: refNS, Name: refName}})
		}
		fk := &out[len(out)-1]
		fk.Fields, fk.RefFields = append(fk.Fields, col), append(fk.RefFields, refCol)
	}
	return out, rows.Err()
}
