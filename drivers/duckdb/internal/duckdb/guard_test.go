package duckdb

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// The read-only guard's lexical layer: what it lets through and what it
// refuses. The second layers (access_mode=READ_ONLY, external access off,
// a READ ONLY transaction and the parser's statement type) are covered by
// the open and query tests against a real database.

func TestCheckReadOnlyAllows(t *testing.T) {
	for _, q := range []string{
		"select 1",
		"SELECT * FROM t WHERE a = 1;",
		"select * from t; -- trailing comment",
		"select * from t; /* trailing */ ",
		"  (select 1) union all (select 2)",
		"with x as (select 1 as a) select * from x",
		"values (1, 'a'), (2, 'b')",
		"from t select a, b",
		"from t",
		"table t",
		"show tables",
		"show databases",
		"describe t",
		"desc t",
		"describe select 1",
		"summarize t",
		"pivot t on b using sum(a)",
		"explain select * from t",
		"pragma version",
		"PRAGMA table_info('t')",
		"pragma table_info(\"main\".\"t\")",
		"pragma show_tables",
		"pragma database_size",
		"select * from range(10)",
		"select * from generate_series(1, 3) g(x)",
		"select * from t, unnest(t.l) u(x)",
		"select * from duckdb_tables()",
		"select * from main.duckdb_columns()",
		"select * from pragma_table_info('t')",
		"select * from \"main\".\"t\"",
		"select * from \"My Table\"",
		"select * from \"main\".\"my.table\"",
		"select * from t where a in ('x', 'y')",
		"select lower('ABC'), coalesce(a, 'z') from t",
		"select * from t pivot (sum(a) for b in ('x', 'y'))",
		"select * from t order by a desc, b",
		"select * from t fetch first 1 rows only",
		"select * from (select 'a/b.csv' as p) s",
		"select 'a/b.csv' as path",
		"select * from t where name like '%.csv'",
		"select * from t where a in (1, 2, 3)",
		"select 1.5e3, 0x1F, 1_000, 1e-5, 0b101, 2E10",
		"select $$it's$$",
		"select E'a\\'b'",
		"select \"select\" from t",
		"select * from t join u on t.a = u.a, v",
		"select * from t join u using (a)",
		"select list_transform([1, 2], x -> x + 1)",
		"select struct_insert({'a': 1}, b := 2)",
		"select starts_with('ab', 'a')",
		"select * replace (a + 1 as a) from t",
		"select * from t order by \"update\"",
		"select case when a then 1 else 0 end from t",
		"select /* a /* nested */ comment */ 1",
		"select * from t at (version => 1)",
		"select * from t qualify row_number() over () = 1",
		"select * from read_t",   // a table named read_t, not a call
		"select query from logs", // a column named query, not a call
		"select * from glob",     // a table named glob
	} {
		if err := checkReadOnly(q); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", q, err)
		}
	}
}

func TestCheckReadOnlyRefuses(t *testing.T) {
	for _, q := range []string{
		// Writes and DDL.
		"insert into t values (1)",
		"update t set a = 1",
		"delete from t",
		"merge into t using u on t.a = u.a when matched then delete",
		"truncate t",
		"create table x (a int)",
		"create temp table x (a int)",
		"create table x as select 1",
		"create or replace macro m() as 1",
		"create secret s (type s3, key_id 'k', secret 's')",
		"drop table t",
		"alter table t add column b int",
		"comment on table t is 'x'",
		"with x as (select 1) insert into t select * from x",
		"with x as (delete from t returning *) select * from x",
		"insert or replace into t values (1)",
		"select 1 delete from t",
		"select 1delete from t",
		"select 1e5delete from t",
		"select 1e5e3delete from t",
		"select 1returning from t",
		"select 0xdelete from t", // DuckDB reads 0 AS xdelete, but a number glued to a word is refused
		"select 1 /* */ delete from t",
		"select nextval('s')",
		"select setval('s', 1)",
		"select * from t for update",
		"select a into b from t",
		// Files, extensions and settings.
		"attach 'x.db' as x",
		"attach ':memory:' as m",
		"detach x",
		"use x",
		"copy t to 'out.csv'",
		"copy t from 'in.csv'",
		"copy (select 1) to 'out.parquet' (format parquet)",
		"export database 'dir'",
		"import database 'dir'",
		"install httpfs",
		"force install httpfs",
		"load httpfs",
		"load '/tmp/evil.duckdb_extension'",
		"set enable_external_access = true",
		"set allow_unsigned_extensions = true",
		"set global enable_external_access = true",
		"reset enable_external_access",
		"set lock_configuration = false",
		"set variable x = 1",
		"pragma enable_profiling",
		"pragma threads = 4",
		"pragma memory_limit='1GB'",
		"pragma unknown_pragma",
		"pragma table_info('t') ; select 1",
		"select * from pragma_table_info('t') where pragma",
		"call pragma_version()",
		"checkpoint",
		"force checkpoint",
		"vacuum",
		"analyze",
		"explain analyze select 1",
		"explain analyse select 1",
		"explain (format json) select 1",
		"explain delete from t",
		"explain select * from read_csv('x.csv')",
		"prepare p as select 1",
		"execute p",
		"deallocate p",
		"begin",
		"begin transaction",
		"commit",
		"rollback",
		"abort",
		"start transaction",
		"grant select on t to u",
		// Table functions that read files, URLs or run SQL.
		"select * from read_csv('/etc/passwd')",
		"select * from read_csv_auto('x.csv')",
		"select * from read_parquet('s3://bucket/x.parquet')",
		"select * from read_json('http://169.254.169.254/latest/meta-data')",
		"select * from read_json_auto('x.json')",
		"select * from read_ndjson('x.json')",
		"select * from read_text('/etc/hosts')",
		"select * from read_blob('/etc/hosts')",
		"select * from parquet_scan('x.parquet')",
		"select * from parquet_metadata('x.parquet')",
		"select * from glob('/etc/*')",
		"select * from sniff_csv('x.csv')",
		"select * from main.read_csv('x.csv')",
		"select * from system.main.read_csv('x.csv')",
		"select * from \"read_csv\"('x.csv')",
		"select * from \"READ_CSV\"('x.csv')",
		"select * from READ_CSV ( 'x.csv' )",
		"select * from read_csv/**/('x.csv')",
		"select * from sqlite_scan('x.db', 't')",
		"select * from postgres_scan('host=h', 'public', 't')",
		"select * from postgres_execute('db', 'drop table t')",
		"select * from iceberg_scan('s3://b/t')",
		"select * from delta_scan('s3://b/t')",
		"select * from st_read('x.shp')",
		"select * from query('delete from t')",
		"select * from query_table('t')",
		"select * from duckdb_secrets()",
		"select * from which_secret('s3://x', 's3')",
		"select json_execute_serialized_sql('...')",
		"select (select count(*) from read_csv('x.csv'))",
		"select * from t where a in (select a from read_csv('x.csv'))",
		"select * from t, read_csv('x.csv')",
		"select * from t join read_csv('x.csv') r on true",
		"select * from t, lateral read_csv(t.path)",
		"select * from my_table_macro()",
		"select * from main.my_table_macro(1)",
		"select * from t, my_table_macro(t.a)",
		"select * from t join u on t.a = u.a, my_table_macro()",
		"select * from t join u on t.a = 1, 'x.csv'",
		// A string or path-like quoted name used as a table reads a file.
		"select * from 'x.csv'",
		"select * from 'https://example.com/x.parquet'",
		"from 'x.csv'",
		"from 'x.csv' select *",
		"select * from t, 'x.csv'",
		"select * from t join 'x.csv' on true",
		"select * from \"x.csv\"",
		"select * from \"/etc/passwd\"",
		"select * from \"s3://bucket/x.parquet\"",
		"select * from \"data/*.parquet\"",
		"select * from ('x.csv')",
		"select * from e'x.csv'",
		"select * from $$x.csv$$",
		"select * from $q$x.csv$q$",
		`select * from U&"\0072ead_csv"('x.csv')`,
		`select U&'\0061'`,
		"select * from (from 'x.csv')",
		"table 'x.csv'",
		"describe 'x.csv'",
		"summarize 'x.csv'",
		"pivot 'x.csv' on a using sum(b)",
		// Table positions inside subqueries, set operations, joins and ONLY:
		// each reads the file on a shared instance.
		"SELECT * FROM (SUMMARIZE '/abs/x.csv')",
		"SELECT * FROM (DESCRIBE '/abs/x.csv')",
		"SELECT * FROM (DESC '/abs/x.csv')",
		"SELECT * FROM (SHOW '/abs/x.csv')",
		"SELECT * FROM (PIVOT '/abs/x.csv' ON a USING count(*))",
		"SELECT * FROM (UNPIVOT '/abs/x.csv' ON a)",
		"SELECT * FROM ONLY '/abs/x.csv'",
		"SELECT * FROM t JOIN ONLY '/abs/x.csv' ON true",
		`SELECT * FROM t UNION ALL TABLE "/abs/x.csv"`,
		`SELECT * FROM (TABLE "/abs/x.csv")`,
		"SELECT * FROM t, LATERAL (SUMMARIZE '/abs/x.csv')",
		"SELECT (SELECT count(*) FROM (DESCRIBE '/abs/x.csv'))",
		"FROM t SELECT * UNION ALL BY NAME (SUMMARIZE '/abs/x.csv')",
		"SELECT * FROM (SUMMARIZE 'https://example.com/x.parquet')",
		"SELECT * FROM ((('/abs/x.csv')))",
		"SELECT * FROM t JOIN ('/abs/x.csv') ON true",
		`SELECT * FROM t JOIN (("/abs/x.csv")) ON true`,
		// Multiple statements and unlexable text.
		"select 1; select 2",
		"select 1; delete from t",
		"select 1;;",
		"select 'unterminated",
		"select \"unterminated",
		"select /* unterminated",
		"select $$unterminated",
		"select 1 \x00",
		"select '\xff\xfe'",
		"",
		"   ",
		"-- only a comment",
		";",
		"update",
		"(delete from t)",
		"((select 1)) ; insert into t values (1)",
	} {
		err := checkReadOnly(q)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", q, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkReadOnly(%q): error %v is not a *driver.Error from %q", q, err, Name)
		}
	}
}

func TestCheckSingle(t *testing.T) {
	for _, q := range []string{"select 1", "select 1;", "insert into t values (1); -- done", "select ';'"} {
		if err := checkSingle(q); err != nil {
			t.Errorf("checkSingle(%q) = %v, want nil", q, err)
		}
	}
	for _, q := range []string{"select 1; select 2", "select 1; delete from t", "select 'x", "", "-- c", "select \x00"} {
		if err := checkSingle(q); err == nil {
			t.Errorf("checkSingle(%q) = nil, want an error", q)
		}
	}
}

func TestHasReturning(t *testing.T) {
	cases := map[string]bool{
		"insert into t values (1) returning id": true,
		"delete from t RETURNING *":             true,
		"insert into t values ('returning')":    false,
		"update t set \"returning\" = 1":        false,
		"update t set a = 1 -- returning":       false,
	}
	for q, want := range cases {
		if got := hasReturning(q); got != want {
			t.Errorf("hasReturning(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestIsTxControl(t *testing.T) {
	cases := map[string]bool{
		"begin": true, "BEGIN TRANSACTION": true, "commit": true, "rollback": true, "abort": true,
		"end": true, "start transaction": true, " /* c */ commit;": true,
		"select 1": false, "select 'commit'": false, "insert into t values (1)": false,
	}
	for q, want := range cases {
		if got := isTxControl(q); got != want {
			t.Errorf("isTxControl(%q) = %v, want %v", q, got, want)
		}
	}
}
