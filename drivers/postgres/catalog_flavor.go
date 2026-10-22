package postgres

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

// catalogSQL is the set of catalog queries of one flavor. Every query keeps
// the stock query's parameters and result columns, so one Go path reads
// them all, except where a field below says otherwise.
type catalogSQL struct {
	namespaces, collections, relation, fields string
	// indexes is empty when the flavor has no indexes (Redshift).
	indexes string
	// foreignKeys returns one row per foreign key with array columns, or,
	// with fkRows set, one row per column pair, keyed by schema and name.
	foreignKeys string
	fkRows      bool
	// primaryKey, when set, reads the primary key columns in order (by
	// schema and name) instead of taking them from the primary index.
	primaryKey string
	// keys reads the flavor's data placement keys (Greenplum distribution,
	// Redshift distribution and sort keys), shown as extra indexes. keysPath
	// runs first with the schema as $1 (Redshift's PG_TABLE_DEF lists only
	// tables on search_path).
	keys, keysPath string
}

// catalogFor returns the catalog queries of f.
func catalogFor(f flavor) catalogSQL {
	stock := catalogSQL{
		namespaces: namespacesSQL, collections: collectionsSQL, relation: relationSQL,
		fields: fieldsSQL, indexes: indexesSQL, foreignKeys: foreignKeysSQL,
	}
	switch f {
	case flavorCockroach:
		stock.namespaces, stock.collections, stock.relation = cockroachNamespacesSQL, cockroachCollectionsSQL, cockroachRelationSQL
	case flavorGreenplum:
		stock.namespaces, stock.fields, stock.indexes, stock.keys = greenplumNamespacesSQL, greenplumFieldsSQL, greenplumIndexesSQL, greenplumKeysSQL
	case flavorRedshift:
		return catalogSQL{
			namespaces: redshiftNamespacesSQL, collections: redshiftCollectionsSQL, relation: redshiftRelationSQL,
			fields: redshiftFieldsSQL, foreignKeys: redshiftForeignKeysSQL, fkRows: true, primaryKey: redshiftPrimaryKeySQL,
			keys: redshiftKeysSQL, keysPath: "select set_config('search_path', $1, true)",
		}
	}
	return stock
}

// CockroachDB: crdb_internal and pg_extension are system schemas, reltuples
// is NULL until statistics exist, and there is no pg_total_relation_size.
const cockroachNamespacesSQL = `
select n.nspname,
       coalesce(obj_description(n.oid, 'pg_namespace'), ''),
       coalesce(n.nspname = current_schema(), false)
from pg_namespace n
where n.nspname not in ('pg_catalog', 'information_schema', 'crdb_internal', 'pg_extension')
  and n.nspname not like 'pg\_temp\_%'
order by n.nspname`

const cockroachCollectionsSQL = `
select n.nspname, c.relname, c.relkind::text, coalesce(c.reltuples, -1)::int8,
       coalesce(obj_description(c.oid, 'pg_class'), '')
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where n.nspname = coalesce(nullif($1::text, ''), current_schema())
  and c.relkind in ('r', 'v', 'm', 'p')
order by c.relname`

const cockroachRelationSQL = `
select c.oid, n.nspname, c.relkind::text, coalesce(c.reltuples, -1)::int8,
       coalesce(obj_description(c.oid, 'pg_class'), ''),
       -1::int8
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where n.nspname = coalesce(nullif($1::text, ''), current_schema())
  and c.relname = $2
  and c.relkind in ('r', 'v', 'm', 'p')`

// Greenplum: the queries must run on Greenplum 6 (PostgreSQL 9.4) and 7
// (PostgreSQL 12), so no attgenerated, attidentity or indnkeyatts.
const greenplumNamespacesSQL = `
select n.nspname,
       coalesce(obj_description(n.oid, 'pg_namespace'), ''),
       coalesce(n.nspname = current_schema(), false)
from pg_namespace n
where n.nspname not in ('pg_catalog', 'information_schema', 'gp_toolkit', 'pg_aoseg', 'pg_bitmapindex', 'pg_ext_aux')
  and n.nspname not like 'pg\_toast%'
  and n.nspname not like 'pg\_temp\_%'
order by n.nspname`

const greenplumFieldsSQL = `
select a.attname, format_type(a.atttypid, a.atttypmod), a.atttypid::int8, t.typname,
       not a.attnotnull,
       coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
       false,
       coalesce(pg_get_expr(d.adbin, d.adrelid), '') like 'nextval(%',
       coalesce(col_description(a.attrelid, a.attnum), '')
from pg_attribute a
join pg_type t on t.oid = a.atttypid
left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
where a.attrelid = $1::oid and a.attnum > 0 and not a.attisdropped
order by a.attnum`

const greenplumIndexesSQL = `
select ic.relname, i.indisunique, i.indisprimary, am.amname,
       array(select coalesce(a.attname::text, pg_get_indexdef(i.indexrelid, k.ord::int, true))
             from unnest(i.indkey::int2[]) with ordinality as k(attnum, ord)
             left join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
             where k.ord <= i.indnatts
             order by k.ord)
from pg_index i
join pg_class ic on ic.oid = i.indexrelid
join pg_am am on am.oid = ic.relam
where i.indrelid = $1::oid
order by i.indisprimary desc, ic.relname`

// greenplumKeysSQL reads the distribution policy (Greenplum 6 and 7:
// distkey int2vector; 'p' hashed or random, 'r' replicated).
const greenplumKeysSQL = `
select p.policytype::text,
       array(select a.attname::text
             from unnest(p.distkey::int2[]) with ordinality as k(attnum, ord)
             join pg_attribute a on a.attrelid = p.localoid and a.attnum = k.attnum
             order by k.ord)
from gp_distribution_policy p
where p.localoid = $1::oid`

// Redshift: the leader node's catalog is PostgreSQL 8.0's plus Redshift
// views. There are no arrays in results, no unnest, no obj_description, no
// indexes (keys are informational constraints) and no
// pg_total_relation_size; sizes and row counts come from SVV_TABLE_INFO,
// which lists only tables that hold data.
const redshiftNamespacesSQL = `
select n.nspname,
       coalesce(d.description, ''),
       n.nspname = coalesce(current_schema(), '')
from pg_namespace n
left join pg_description d on d.objoid = n.oid and d.objsubid = 0
where n.nspname not in ('pg_catalog', 'information_schema', 'pg_internal', 'pg_automv', 'catalog_history', 'pg_auto_copy', 'pg_mv', 'pg_s3')
  and n.nspname not like 'pg\_toast%'
  and n.nspname not like 'pg\_temp\_%'
order by n.nspname`

const redshiftCollectionsSQL = `
select n.nspname, c.relname, c.relkind::text, coalesce(i.tbl_rows::int8, -1),
       coalesce(d.description, '')
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
left join svv_table_info i on i.table_id = c.oid
left join pg_description d on d.objoid = c.oid and d.objsubid = 0
where n.nspname = coalesce(nullif($1::text, ''), current_schema())
  and c.relkind in ('r', 'v')
order by c.relname`

// redshiftRelationSQL: SVV_TABLE_INFO.size is in 1 MB blocks.
const redshiftRelationSQL = `
select c.oid, n.nspname, c.relkind::text, coalesce(i.tbl_rows::int8, -1),
       coalesce(d.description, ''),
       coalesce(i.size::int8 * 1048576, -1)
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
left join svv_table_info i on i.table_id = c.oid
left join pg_description d on d.objoid = c.oid and d.objsubid = 0
where n.nspname = coalesce(nullif($1::text, ''), current_schema())
  and c.relname = $2
  and c.relkind in ('r', 'v')`

// redshiftFieldsSQL: an IDENTITY column's default reads "identity"(…).
const redshiftFieldsSQL = `
select a.attname, format_type(a.atttypid, a.atttypmod), a.atttypid::int8, t.typname,
       not a.attnotnull,
       coalesce(pg_get_expr(ad.adbin, ad.adrelid), ''),
       false,
       coalesce(pg_get_expr(ad.adbin, ad.adrelid), '') like '%identity%'
         or coalesce(pg_get_expr(ad.adbin, ad.adrelid), '') like 'nextval(%',
       coalesce(d.description, '')
from pg_attribute a
join pg_type t on t.oid = a.atttypid
left join pg_attrdef ad on ad.adrelid = a.attrelid and ad.adnum = a.attnum
left join pg_description d on d.objoid = a.attrelid and d.objsubid = a.attnum
where a.attrelid = $1::oid and a.attnum > 0 and not a.attisdropped
order by a.attnum`

const redshiftPrimaryKeySQL = `
select k.column_name::text
from information_schema.table_constraints tc
join information_schema.key_column_usage k
  on k.constraint_schema = tc.constraint_schema and k.constraint_name = tc.constraint_name
 and k.table_schema = tc.table_schema and k.table_name = tc.table_name
where tc.constraint_type = 'PRIMARY KEY' and tc.table_schema = $1 and tc.table_name = $2
order by k.ordinal_position`

// redshiftForeignKeysSQL returns one row per referencing column.
const redshiftForeignKeysSQL = `
select rc.constraint_name::text, k.column_name::text, u.table_schema::text, u.table_name::text, u.column_name::text
from information_schema.referential_constraints rc
join information_schema.key_column_usage k
  on k.constraint_schema = rc.constraint_schema and k.constraint_name = rc.constraint_name
join information_schema.key_column_usage u
  on u.constraint_schema = rc.unique_constraint_schema and u.constraint_name = rc.unique_constraint_name
 and u.ordinal_position = k.position_in_unique_constraint
where k.table_schema = $1 and k.table_name = $2
order by rc.constraint_name, k.ordinal_position`

// redshiftKeysSQL reads the distribution and sort key columns. PG_TABLE_DEF
// lists only tables on search_path, so keysPath sets it first.
const redshiftKeysSQL = `
select "column"::text, distkey, sortkey::int4
from pg_table_def
where schemaname = $1 and tablename = $2`

// redshiftKeyColumn is one PG_TABLE_DEF row.
type redshiftKeyColumn struct {
	name    string
	distKey bool
	sortKey int32 // position in the sort key; negative positions mark an interleaved key
}

// redshiftKeyIndexes turns PG_TABLE_DEF rows into the distkey and sortkey
// pseudo-indexes (nil when the table has neither).
func redshiftKeyIndexes(cols []redshiftKeyColumn) []driver.Index {
	var out []driver.Index
	var sorted []redshiftKeyColumn
	interleaved := false
	for _, c := range cols {
		if c.distKey {
			out = append(out, driver.Index{Name: "distkey", Fields: []string{c.name}, Method: "distkey"})
		}
		if c.sortKey != 0 {
			sorted = append(sorted, c)
			interleaved = interleaved || c.sortKey < 0
		}
	}
	if len(sorted) == 0 {
		return out
	}
	abs := func(n int32) int32 { return max(n, -n) }
	slices.SortFunc(sorted, func(a, b redshiftKeyColumn) int { return cmp.Compare(abs(a.sortKey), abs(b.sortKey)) })
	ix := driver.Index{Name: "sortkey", Method: "compound sortkey"}
	if interleaved {
		ix.Method = "interleaved sortkey"
	}
	for _, c := range sorted {
		ix.Fields = append(ix.Fields, c.name)
	}
	return append(out, ix)
}

// greenplumDistribution turns a distribution policy into the "distributed
// by" pseudo-index. Random ('p' without columns) and replicated ('r')
// tables have no distribution columns, so no index.
func greenplumDistribution(policy string, cols []string) []driver.Index {
	if policy != "p" || len(cols) == 0 {
		return nil
	}
	return []driver.Index{{Name: "distributed by", Fields: cols, Method: "distribution"}}
}

// describeKeys reads the flavor's placement keys of the relation oid.
func describeKeys(ctx context.Context, tx pgx.Tx, cat catalogSQL, oid uint32, ref driver.CollectionRef) ([]driver.Index, error) {
	if cat.keysPath != "" {
		if _, err := tx.Exec(ctx, cat.keysPath, quoteIdent(ref.Namespace)); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, cat.keys, ref.Namespace, ref.Name)
		if err != nil {
			return nil, err
		}
		cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (redshiftKeyColumn, error) {
			var c redshiftKeyColumn
			err := r.Scan(&c.name, &c.distKey, &c.sortKey)
			return c, err
		})
		if err != nil {
			return nil, err
		}
		return redshiftKeyIndexes(cols), nil
	}
	var policy string
	var cols []string
	err := tx.QueryRow(ctx, cat.keys, oid).Scan(&policy, &cols)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return greenplumDistribution(policy, cols), nil
}
