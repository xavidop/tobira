package influxdb

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestGuardAllowsReads(t *testing.T) {
	for _, tc := range []struct {
		lang language
		text string
	}{
		{langSQL, "SELECT * FROM cpu"},
		{langSQL, "select * from cpu;"},
		{langSQL, "  -- leading comment\nSELECT 1 ;  "},
		{langSQL, "/* block */ SELECT 1"},
		{langSQL, "SHOW TABLES"},
		{langSQL, "show columns from cpu"},
		{langSQL, "EXPLAIN SELECT * FROM cpu"},
		{langSQL, "EXPLAIN ANALYZE VERBOSE SELECT 1"},
		{langSQL, "EXPLAIN FORMAT tree SELECT 1"},
		{langSQL, "SELECT 'it''s; INSERT INTO x' AS s"},
		{langSQL, `SELECT "INTO" FROM "my ""table"""`},
		{langSQL, `SELECT 'a\' AS s`},         // no backslash escape in plain strings
		{langSQL, `SELECT E'a\'; into' AS s`}, // E-string: backslash escapes
		{langSQL, "SELECT * FROM cpu WHERE id = $1"},
		{langSQL, "SELECT (SELECT max(x) FROM t) FROM u"},
		{langSQL, "WITH x AS (SELECT 1) SELECT * FROM x"},
		{langSQL, `SELECT "update", "delete" FROM t`},
		{langSQL, "with recursive r(n) as (select 1 union all select n + 1 from r where n < 3) select * from r"},
		{langSQL, `WITH a AS (SELECT 1), "b c" AS MATERIALIZED (SELECT 2), d AS NOT MATERIALIZED (VALUES (3)) SELECT * FROM a, "b c", d`},
		{langSQL, "WITH x AS ((SELECT 1)) SELECT * FROM x;"},
		{langSQL, "WITH x AS (WITH y AS (SELECT 1) SELECT * FROM y) SELECT * FROM x"},
		{langSQL, "EXPLAIN WITH x AS (SELECT 1) SELECT * FROM x"},
		{langSQL, "-- c\nWITH x AS (SELECT ')') SELECT * FROM x"},
		{langInfluxQL, "SELECT usage_user FROM cpu WHERE host =~ /web.*/"},
		{langInfluxQL, `SELECT usage_user FROM cpu WHERE host = 'a\'b; into'`},
		{langInfluxQL, "SHOW MEASUREMENTS"},
		{langInfluxQL, "SHOW TAG VALUES WITH KEY = host"},
		{langInfluxQL, "EXPLAIN ANALYZE SELECT usage_user FROM cpu"},
		{langInfluxQL, "SELECT usage_user / 2 FROM cpu"},
		{langInfluxQL, "SELECT /us/ FROM cpu"},
		{langInfluxQL, "SELECT count(/us/) FROM cpu"},
		{langInfluxQL, "SELECT mean(usage_user) FROM cpu GROUP BY /ho/"},
		{langSQL, "SELECT 'café ☕' AS s, \"naïve\" FROM t"},
		{langSQL, "SELECT $1, $2 FROM t"},
	} {
		if err := checkStatement(tc.text, tc.lang); err != nil {
			t.Errorf("checkStatement(%q, %v) = %v, want nil", tc.text, tc.lang, err)
		}
	}
}

func TestGuardRefusesWrites(t *testing.T) {
	for _, tc := range []struct {
		lang language
		text string
	}{
		// Every write family InfluxDB 3's SQL engine has a statement for.
		{langSQL, "INSERT INTO cpu (time, host) VALUES (now(), 'x')"},
		{langSQL, "UPDATE cpu SET usage_user = 1"},
		{langSQL, "DELETE FROM cpu"},
		{langSQL, "TRUNCATE cpu"},
		{langSQL, "DROP TABLE cpu"},
		{langSQL, "CREATE TABLE t AS SELECT 1"},
		{langSQL, "CREATE EXTERNAL TABLE t STORED AS CSV LOCATION '/tmp/x'"},
		{langSQL, "COPY (SELECT * FROM cpu) TO '/tmp/x.csv'"},
		{langSQL, "ALTER TABLE cpu ADD COLUMN x INT"},
		{langSQL, "SET datafusion.execution.batch_size = 1"},
		{langSQL, "PREPARE p AS SELECT 1"},
		{langSQL, "EXECUTE p"},
		{langSQL, "WITH x AS (SELECT 1) DELETE FROM cpu"},
		{langSQL, "WITH x AS (SELECT 1) UPDATE cpu SET a = 1"},
		{langSQL, "WITH x AS (SELECT 1) INSERT INTO cpu SELECT * FROM x"},
		{langSQL, "WITH d AS (DELETE FROM cpu RETURNING *) SELECT * FROM d"},
		{langSQL, "WITH d AS (UPDATE cpu SET a = 1 RETURNING *) SELECT * FROM d"},
		{langSQL, "WITH x AS (SELECT 1) SELECT * FROM x WHERE (DELETE FROM cpu) IS NULL"},
		{langSQL, "WITH x AS (SELECT 1)"},
		{langSQL, "SELECT * FROM (DELETE FROM cpu RETURNING *) d"},
		{langSQL, "SELECT update FROM t"},
		{langSQL, "WITH x (SELECT 1) SELECT 1"},
		{langSQL, "WITH x AS (SELECT 1 SELECT 1"},
		{langSQL, "WITH x AS SELECT 1 SELECT 1"},
		{langSQL, "WITH x AS (SELECT 1), SELECT 1"},
		{langSQL, "WITH x AS (SELECT 1) VALUES (1)"},
		{langSQL, "WITH x AS (SELECT 1) SELECT 1; DELETE FROM cpu"},
		{langSQL, "WITH x AS (SELECT 1) COPY (SELECT 1) TO '/tmp/x'"},
		{langSQL, "EXPLAIN WITH x AS (SELECT 1) DELETE FROM cpu"},
		{langInfluxQL, "WITH x AS (SELECT 1) SELECT 1"},
		{langSQL, "DESCRIBE cpu"},
		// Compound and hidden forms.
		{langSQL, "SELECT 1; DELETE FROM cpu"},
		{langSQL, "SELECT 1;; SELECT 2"},
		{langSQL, "SELECT * INTO t2 FROM cpu"},
		{langSQL, "select 1 into t2"},
		{langSQL, "EXPLAIN DELETE FROM cpu"},
		{langSQL, "EXPLAIN ANALYZE INSERT INTO cpu VALUES (1)"},
		{langSQL, "EXPLAIN"},
		{langSQL, "EXPLAIN FORMAT"},
		{langSQL, "/* SELECT */ DELETE FROM cpu"},
		{langSQL, "-- SELECT\nDELETE FROM cpu"},
		{langSQL, "SELECT 'unterminated"},
		{langSQL, "SELECT \"unterminated"},
		{langSQL, "SELECT 1 /* unterminated"},
		{langSQL, "SELECT 1 /* a /* nested */ ; DELETE FROM cpu */"},
		{langSQL, "SELECT $$ ' $$ INTO t, '"},
		{langSQL, "SELECT $tag$ x $tag$"},
		// Tagged dollar quotes starting with a digit or a
		// non-ASCII letter, Unicode whitespace, and InfluxQL regex positions.
		{langSQL, "SELECT $1$'$1$ FROM (DELETE FROM cpu RETURNING *) d --'"},
		{langSQL, "SELECT $1$'$1$ INTO x FROM cpu --'"},
		{langSQL, "SELECT $é$'$é$ INTO x FROM cpu --'"},
		{langSQL, "SELECT $_1$ x $_1$"},
		{langSQL, "SELECT * FROM (\u00a0DELETE FROM cpu RETURNING *) d"},
		{langSQL, "SELECT *\u00a0INTO\u00a0x FROM cpu"},
		{langSQL, "SELECT *\u2003INTO x FROM cpu"},
		{langSQL, "SELECT * FROM café"},
		{langSQL, "-- café\nSELECT 1"},
		{langInfluxQL, "SELECT /'/ INTO x FROM cpu WHERE t = 'a' --'"},
		{langInfluxQL, "SELECT * FROM cpu GROUP BY /'/ INTO x --'"},
		{langInfluxQL, "SELECT count(/'/) FROM cpu WHERE t = 'a'; DROP MEASUREMENT cpu --'"},
		{langInfluxQL, "SELECT DISTINCT /'/ INTO x FROM cpu --'"},
		{langInfluxQL, "SELECT * FROM cpu WHERE host = 'x'\u00a0INTO y"},
		{langSQL, "SELECT `'` INTO t, '`'"},
		{langSQL, "SELECT 1\x00; DELETE FROM cpu"},
		{langSQL, `SELECT 'a\' INTO t, '\'`}, // plain string ends at the second quote
		{langInfluxQL, "SELECT * INTO cpu_copy FROM cpu"},
		{langInfluxQL, "DROP MEASUREMENT cpu"},
		{langInfluxQL, "DELETE FROM cpu"},
		{langInfluxQL, "CREATE DATABASE x"},
		{langInfluxQL, "CREATE RETENTION POLICY rp ON db DURATION 1d REPLICATION 1"},
		{langInfluxQL, "GRANT ALL TO u"},
		{langInfluxQL, "KILL QUERY 1"},
		{langInfluxQL, "SELECT 1; DROP MEASUREMENT cpu"},
		{langInfluxQL, `SELECT * FROM cpu WHERE host = 'a\\' INTO x`}, // escaped backslash, then the quote closes
		{langInfluxQL, "SELECT * FROM cpu WHERE host =~ /'/; DROP MEASUREMENT cpu '"},
		{langInfluxQL, "SELECT * FROM cpu WHERE host =~ /x;/"},
		{langInfluxQL, "SELECT * FROM cpu WHERE host =~ /unterminated"},
	} {
		err := checkStatement(tc.text, tc.lang)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkStatement(%q, %v) = %v, want ErrReadOnly", tc.text, tc.lang, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkStatement(%q): %v is not a *driver.Error from %s", tc.text, err, Name)
		}
	}
}

func TestGuardEmptyIsSyntax(t *testing.T) {
	for _, text := range []string{"", "   ", "-- only a comment", ";"} {
		if err := checkStatement(text, langSQL); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkStatement(%q) = %v, want ErrSyntax", text, err)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, ok := range []string{"usage_user > 1", "(a = 1) OR (b = 'x)')", "host IN ('a', 'b')"} {
		if err := checkRawFilter(ok); err != nil {
			t.Errorf("checkRawFilter(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"1 = 1) OR (1 = 1",
		"a = 1; DELETE FROM cpu",
		"(a = 1",
		"a = 'x",
		"a = 1 -- trailing comment",
		"a = 1 /* c */",
	} {
		if err := checkRawFilter(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}
