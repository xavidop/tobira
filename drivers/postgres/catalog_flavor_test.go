package postgres

import (
	"reflect"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// sqlOf returns every statement of a catalog set, by name.
func sqlOf(s catalogSQL) map[string]string {
	return map[string]string{
		"namespaces": s.namespaces, "collections": s.collections, "relation": s.relation,
		"fields": s.fields, "indexes": s.indexes, "foreignKeys": s.foreignKeys, "primaryKey": s.primaryKey,
		"keys": s.keys,
	}
}

func TestCatalogForPostgresAndCockroachIsTheStockSQL(t *testing.T) {
	pg := catalogFor(flavorPostgres)
	if pg.fields != fieldsSQL || pg.indexes != indexesSQL || pg.foreignKeys != foreignKeysSQL || pg.collections != collectionsSQL || pg.relation != relationSQL {
		t.Error("postgres must keep the stock catalog SQL")
	}
	if pg.keys != "" || pg.primaryKey != "" {
		t.Error("postgres has no flavor key query")
	}
	crdb := catalogFor(flavorCockroach)
	if crdb.fields != fieldsSQL || crdb.indexes != indexesSQL || crdb.foreignKeys != foreignKeysSQL {
		t.Error("cockroachdb reads the stock pg_catalog field, index and foreign key SQL")
	}
	for _, internal := range []string{"crdb_internal", "pg_extension"} {
		if !strings.Contains(crdb.namespaces, "'"+internal+"'") {
			t.Errorf("cockroachdb namespaces must hide %s", internal)
		}
	}
	if strings.Contains(crdb.relation, "pg_total_relation_size") {
		t.Error("cockroachdb has no pg_total_relation_size")
	}
}

// TestRedshiftCatalogSQL pins the Redshift catalog queries to what the
// Redshift leader node offers: PostgreSQL 8.0-era pg_catalog plus SVV_TABLE_INFO
// and PG_TABLE_DEF, no arrays, no unnest, no identity or generated columns.
func TestRedshiftCatalogSQL(t *testing.T) {
	rs := catalogFor(flavorRedshift)
	for name, q := range sqlOf(rs) {
		lower := strings.ToLower(q)
		for _, bad := range []string{"unnest", "ordinality", "attgenerated", "attidentity", "indnkeyatts",
			"pg_total_relation_size", "obj_description", "col_description", "array(", "jsonb", "coalesce(n.nspname = current_schema()"} {
			if strings.Contains(lower, bad) {
				t.Errorf("redshift %s SQL uses %q, which Redshift lacks", name, bad)
			}
		}
	}
	for name, want := range map[string]string{
		"collections": "svv_table_info", "relation": "svv_table_info", "keys": "pg_table_def",
		"primaryKey": "information_schema.key_column_usage", "foreignKeys": "information_schema.referential_constraints",
	} {
		if !strings.Contains(strings.ToLower(sqlOf(rs)[name]), want) {
			t.Errorf("redshift %s SQL should read %s", name, want)
		}
	}
	if rs.indexes != "" {
		t.Error("redshift has no indexes: the index query must be empty")
	}
	for _, internal := range []string{"pg_internal", "pg_automv", "catalog_history", "information_schema", "pg_catalog"} {
		if !strings.Contains(rs.namespaces, "'"+internal+"'") {
			t.Errorf("redshift namespaces must hide %s", internal)
		}
	}
	// PG_TABLE_DEF only lists tables on search_path, so the key query runs
	// after a transaction-local search_path of the table's schema.
	if !strings.Contains(rs.keysPath, "set_config('search_path'") || !strings.Contains(rs.keysPath, ", true)") {
		t.Errorf("redshift keys need a transaction-local search_path: %q", rs.keysPath)
	}
}

// TestGreenplumCatalogSQL pins the Greenplum queries to what Greenplum 6
// (PostgreSQL 9.4) and 7 (PostgreSQL 12) both have.
func TestGreenplumCatalogSQL(t *testing.T) {
	gp := catalogFor(flavorGreenplum)
	for name, q := range sqlOf(gp) {
		for _, bad := range []string{"attgenerated", "attidentity", "indnkeyatts"} {
			if strings.Contains(q, bad) {
				t.Errorf("greenplum %s SQL uses %s, which Greenplum 6 lacks", name, bad)
			}
		}
	}
	if !strings.Contains(gp.keys, "gp_distribution_policy") {
		t.Error("greenplum keys must read gp_distribution_policy")
	}
	for _, internal := range []string{"gp_toolkit", "pg_aoseg", "pg_bitmapindex"} {
		if !strings.Contains(gp.namespaces, "'"+internal+"'") {
			t.Errorf("greenplum namespaces must hide %s", internal)
		}
	}
}

func TestRedshiftKeyIndexes(t *testing.T) {
	cols := []redshiftKeyColumn{
		{name: "id", sortKey: 2},
		{name: "tenant", distKey: true, sortKey: 1},
		{name: "note"},
	}
	want := []driver.Index{
		{Name: "distkey", Fields: []string{"tenant"}, Method: "distkey"},
		{Name: "sortkey", Fields: []string{"tenant", "id"}, Method: "compound sortkey"},
	}
	if got := redshiftKeyIndexes(cols); !reflect.DeepEqual(got, want) {
		t.Errorf("compound: got %+v, want %+v", got, want)
	}
	inter := []redshiftKeyColumn{{name: "a", sortKey: -2}, {name: "b", sortKey: 1}}
	want = []driver.Index{{Name: "sortkey", Fields: []string{"b", "a"}, Method: "interleaved sortkey"}}
	if got := redshiftKeyIndexes(inter); !reflect.DeepEqual(got, want) {
		t.Errorf("interleaved: got %+v, want %+v", got, want)
	}
	if got := redshiftKeyIndexes([]redshiftKeyColumn{{name: "x"}}); got != nil {
		t.Errorf("no keys: got %+v", got)
	}
}

func TestGreenplumDistribution(t *testing.T) {
	if got := greenplumDistribution("p", []string{"tenant", "id"}); !reflect.DeepEqual(got, []driver.Index{{Name: "distributed by", Fields: []string{"tenant", "id"}, Method: "distribution"}}) {
		t.Errorf("hash: %+v", got)
	}
	for _, tt := range []struct {
		policy string
		cols   []string
	}{{"p", nil}, {"r", nil}, {"", nil}} {
		if got := greenplumDistribution(tt.policy, tt.cols); got != nil {
			t.Errorf("%q: %+v, want none", tt.policy, got)
		}
	}
}
