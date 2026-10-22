package vertica

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// The read-only guard is lexical and runs before anything is sent, so these
// tests need no server. The integration suite repeats the dangerous ones
// against Vertica and checks that the data did not change.

func TestCheckReadOnlyAllows(t *testing.T) {
	for _, text := range []string{
		"SELECT 1",
		"select * from t",
		"  \n\tSELECT 1;",
		"SELECT 1;;  ;",
		"(SELECT 1)",
		"((select a from t)) union all (select b from u)",
		"WITH c AS (SELECT 1 AS x) SELECT x FROM c",
		"with recursive r (n) as (select 1 union all select n + 1 from r where n < 3) select n from r",
		"SHOW ALL",
		"show search_path",
		"EXPLAIN SELECT * FROM t",
		"explain verbose select 1",
		"explain json select 1",
		// Strings, quoted names and comments hide what they hold.
		"SELECT 'delete from t; drop table t' AS s",
		"SELECT 'doubled '' quote; delete' AS s",
		`SELECT E'it\'s; still one string' AS s`,
		"SELECT $$ dollar ; delete from t $$ AS s",
		"SELECT $q$ tagged ; delete from t $q$ AS s",
		`SELECT "update", "delete" FROM "insert"`,
		`SELECT "a""b" FROM t`,
		"SELECT 1 -- ; delete from t",
		"SELECT /* ; delete from t */ 1",
		"-- leading comment\nSELECT 1",
		"-- crlf comment\r\nSELECT 1",
		"/* leading */ SELECT 1",
		// Built-in functions, casts, analytics and Vertica clauses.
		"SELECT COUNT(*), SUM(score), AVG(age), MAX(created_at) FROM t GROUP BY ROLLUP(a, b)",
		"SELECT LOWER(name), SUBSTR(email, 1, 5), LENGTH(name) || 'x' FROM t",
		"SELECT CAST(score AS NUMERIC(10,2)), score::VARCHAR(20), age::INT FROM t",
		"SELECT ROW_NUMBER() OVER (PARTITION BY a ORDER BY b DESC) FROM t",
		"SELECT DATE_TRUNC('day', created_at), DATEDIFF('day', a, b), NOW(), CURRENT_DATE FROM t",
		"SELECT COALESCE(a, 0), NULLIF(a, b), DECODE(a, 1, 'x', 'y'), CASE WHEN a > 1 THEN 1 ELSE 0 END FROM t",
		"SELECT * FROM t WHERE id IN (1, 2) AND name LIKE 'A%' AND EXISTS (SELECT 1 FROM u WHERE u.id = t.id)",
		"SELECT * FROM t WHERE id = ? AND name = ?",
		"SELECT * FROM t ORDER BY id LIMIT 10 OFFSET 5",
		"SELECT ts, TS_FIRST_VALUE(v) FROM t TIMESERIES ts AS '1 hour' OVER (PARTITION BY k ORDER BY t)",
		"SELECT * FROM t AT EPOCH LATEST",
		"SELECT ARRAY[1, 2], ROW(1, 2), 1.5e3, .5, 0x1F",
		"SELECT version(), current_user, current_schema(), current_database()",
		"SELECT * FROM v_catalog.tables WHERE table_schema = 'public'",
		"SELECT x FROM t WHERE x BETWEEN 1 AND 2 OR x ILIKE 'a%'",
		"SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY x) OVER () FROM t",
		"SELECT LISTAGG(name) FROM t",
		"SELECT * FROM t1 JOIN (SELECT * FROM t2) s ON t1.id = s.id",
		"SELECT * FROM t1 LEFT JOIN t2 USING (id)",
		`SELECT E'\\'`,
		// A quoted column whose name is a denied word is just a column.
		`SELECT "insert" FROM t`,
	} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
}

func TestCheckReadOnlyRefuses(t *testing.T) {
	for _, text := range []string{
		"",
		"   ",
		"-- only a comment",
		";",
		// Writes, DDL, session and transaction statements.
		"INSERT INTO t VALUES (1)",
		"insert /*+ direct */ into t select * from u",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"MERGE INTO t USING u ON t.id = u.id WHEN MATCHED THEN UPDATE SET a = 1",
		"TRUNCATE TABLE t",
		"CREATE TABLE t2 AS SELECT * FROM t",
		"CREATE LOCAL TEMP TABLE x (a int)",
		"ALTER TABLE t ADD COLUMN c int",
		"DROP TABLE t",
		"GRANT SELECT ON t TO u",
		"REVOKE SELECT ON t FROM u",
		"COMMENT ON TABLE t IS 'x'",
		"SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE",
		"SET SEARCH_PATH TO x",
		"START TRANSACTION",
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
		"END",
		"SAVEPOINT s",
		"RELEASE SAVEPOINT s",
		"LOCK TABLE t IN EXCLUSIVE MODE",
		"CALL p()",
		"EXECUTE DATA LOADER l",
		"PROFILE SELECT 1",
		"ACTIVATE DIRECTED QUERY q",
		"SAVE QUERY SELECT 1",
		// COPY, EXPORT and CONNECT, in every spelling.
		"COPY t FROM STDIN",
		"COPY t FROM LOCAL '/etc/passwd'",
		"copy t from '/data/x.csv'",
		"COPY t FROM VERTICA other.public.t",
		"EXPORT TO PARQUET(directory = '/tmp/x') AS SELECT * FROM t",
		"EXPORT TO VERTICA other.public.t AS SELECT * FROM t",
		"CONNECT TO VERTICA other USER u PASSWORD 'p' ON 'h', 5433",
		"DISCONNECT other",
		// Writes hidden in an allowed statement.
		"SELECT 1; DELETE FROM t",
		"SELECT 1; SELECT 2",
		"select 1 delete from t",
		"WITH c AS (SELECT 1) DELETE FROM t",
		"WITH c AS (SELECT 1) INSERT INTO t SELECT * FROM c",
		"SELECT * INTO t2 FROM t",
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t FOR UPDATE OF t",
		"EXPLAIN ANALYZE SELECT 1",
		"EXPLAIN DELETE FROM t",
		"EXPLAIN INSERT INTO t SELECT 1",
		"(DELETE FROM t)",
		"SELECT NEXTVAL('s')",
		"SELECT s.NEXTVAL",
		`SELECT s."NEXTVAL"`,
		"SELECT CURRVAL('s')",
		// Meta-functions run actions from a SELECT, even in a READ ONLY
		// transaction: only allow-listed functions may be called.
		"SELECT SET_CONFIG_PARAMETER('MaxClientSessions', 0)",
		"SELECT SHUTDOWN()",
		"SELECT CLOSE_ALL_SESSIONS()",
		"SELECT ANALYZE_STATISTICS('t')",
		"SELECT PURGE()",
		"SELECT EXPORT_TABLES('', 't')",
		"SELECT DROP_PARTITIONS('t', 1, 2)",
		"SELECT DO_TM_TASK('mergeout')",
		"SELECT INTERRUPT_STATEMENT('s', 1)",
		"SELECT MAKE_AHM_NOW()",
		"SELECT AUDIT('')",
		"select v_catalog.anything(1)",
		"SELECT public.my_udf(x) FROM t",
		`SELECT "SHUTDOWN"()`,
		`SELECT "lower"(name) FROM t`,
		"SELECT my_udtf(x) OVER (PARTITION BEST) FROM t",
		"SELECT COUNT(*) FROM t WHERE SET_CONFIG_PARAMETER('x', 1) = 0",
		"SELECT * FROM t WHERE a IN (SELECT shutdown())",
		"SELECT lower (shutdown ())",
		// Lexer gaps that hid keywords elsewhere.
		"SELECT 1DELETE FROM t",
		"SELECT 0x1INTO t2 FROM t",
		"SELECT 1e5DELETE",
		"SELECT 1 -- c\rDELETE FROM t",
		"SELECT /* a /* b */ DELETE FROM t */ 1",
		"SELECT 'unterminated",
		"SELECT 1\x00",
		"SELECT 'a\xff'",
		"SELECT\u00a0DELETE", // NBSP: two words to the guard, refused either way
		// A backslash in a plain string is read differently when
		// StandardConformingStrings is off.
		`SELECT '\'' ; DELETE FROM t --'`,
		`SELECT 'C:\path'`,
	} {
		err := checkReadOnly(text)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkReadOnly(%q) = %#v, want a *driver.Error from %s", text, err, Name)
		}
	}
}

// The refusal names the reason, never the statement, which may hold data.
func TestCheckReadOnlyErrorOmitsText(t *testing.T) {
	err := checkReadOnly("DELETE FROM t WHERE secret = 'hunter2-value'")
	if err == nil || strings.Contains(err.Error(), "hunter2-value") {
		t.Errorf("checkReadOnly error = %v, want a refusal that does not quote the text", err)
	}
}

// checkShape runs on every path, read-only or not: exactly one statement,
// the same count vertica-sql-go's client-side splitter sees (it would run
// each piece on its own), and no COPY that reads client files or stdin.
func TestCheckShape(t *testing.T) {
	for _, text := range []string{
		"SELECT 1",
		"DELETE FROM t WHERE id = 1;",
		"CREATE TABLE t (a int)",
		"COPY t FROM '/data/server-side.csv'",
		"SELECT LOCALTIMESTAMP, 'copy' AS s",
		"SELECT $$a;b$$",
	} {
		if err := checkShape("query", text); err != nil {
			t.Errorf("checkShape(%q) = %v, want nil", text, err)
		}
	}
	for _, text := range []string{
		"",
		"SELECT 1; SELECT 2",
		"SELECT 1; DELETE FROM t",
		"select begin from t; delete from t", // the client splitter sees one, the server two
		`SELECT E'\';'`,                      // the guard sees one statement, the client splitter two
		"COPY t FROM LOCAL '/etc/passwd'",
		"copy t from local stdin",
		"COPY t FROM STDIN",
		"copy t (a) from stdin delimiter ','",
		"COPY t FROM LOCAL '/a' , '/b'",
		`COPY t FROM E'\' LOCAL '/etc/passwd' --'`, // vertica-sql-go's tokenizer sees LOCAL here
		"SELECT 'copy local' AS s",                 // refused: words are read ignoring quotes
		"SELECT 'open",
	} {
		err := checkShape("query", text)
		if err == nil {
			t.Errorf("checkShape(%q) = nil, want a refusal", text)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkShape(%q) = %#v, want a *driver.Error", text, err)
		}
	}
}

// explainTarget keeps EXPLAIN to a plan: on any path the explained text
// must start a statement Vertica can explain, so EXPLAIN ANALYZE (which
// runs it) cannot be smuggled in through the text.
func TestExplainTarget(t *testing.T) {
	for _, text := range []string{"SELECT 1", "with c as (select 1) select * from c", "(select 1)",
		"INSERT INTO t SELECT 1", "update t set a = 1", "DELETE FROM t", "MERGE INTO t USING u ON 1=1 WHEN MATCHED THEN DELETE",
		"-- c\nSELECT 1"} {
		if err := explainTarget("explain", text); err != nil {
			t.Errorf("explainTarget(%q) = %v, want nil", text, err)
		}
	}
	for _, text := range []string{"ANALYZE SELECT 1", "analyze delete from t", "VERBOSE ANALYZE SELECT 1", "LOCAL SELECT 1",
		"CREATE TABLE t (a int)", "COPY t FROM STDIN", "", "SELECT 1; SELECT 2"} {
		if err := explainTarget("explain", text); err == nil {
			t.Errorf("explainTarget(%q) = nil, want a refusal", text)
		}
	}
}

// Inside a transaction, statements that would end it or commit it
// implicitly (every DDL statement in Vertica) are refused, anywhere in the
// text.
func TestTxStatementRefusal(t *testing.T) {
	for _, text := range []string{"SELECT 1", "UPDATE t SET a = 1", "insert into t values (1)", "DELETE FROM t", `SELECT "create" FROM t`} {
		if err := txStatementRefusal(text); err != nil {
			t.Errorf("txStatementRefusal(%q) = %v, want nil", text, err)
		}
	}
	for _, text := range []string{"COMMIT", "ROLLBACK", "BEGIN", "START TRANSACTION", "END", "SAVEPOINT x",
		"CREATE TABLE x (a int)", "ALTER TABLE t ADD COLUMN c int", "DROP TABLE t", "TRUNCATE TABLE t",
		"GRANT SELECT ON t TO u", "COMMENT ON TABLE t IS 'x'", "SET SESSION AUTOCOMMIT TO ON", "COPY t FROM '/x'",
		"select 1 commit", "SELECT 'open"} {
		if err := txStatementRefusal(text); err == nil {
			t.Errorf("txStatementRefusal(%q) = nil, want a refusal", text)
		}
	}
}

// A plain Query refuses transaction control: on a pooled session it would
// leave a transaction open for the next caller. Begin/Commit/Rollback are
// the way.
func TestTxControlRefusal(t *testing.T) {
	for _, text := range []string{"SELECT 1", "UPDATE t SET a = 1", "CREATE TABLE x (a int)"} {
		if err := txControlRefusal(text); err != nil {
			t.Errorf("txControlRefusal(%q) = %v, want nil", text, err)
		}
	}
	for _, text := range []string{"COMMIT", "rollback", "BEGIN", "START TRANSACTION", "END", "SAVEPOINT x",
		"RELEASE SAVEPOINT x", "ROLLBACK TO SAVEPOINT x", "SET SESSION AUTOCOMMIT TO OFF", "set session autocommit to off"} {
		if err := txControlRefusal(text); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("txControlRefusal(%q) = %v, want ErrUnsupported", text, err)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, raw := range []string{"a = 1", "name LIKE 'x%' AND (b > 2 OR c IS NULL)", "a = 1 -- trailing"} {
		if err := checkRawFilter(raw); err != nil {
			t.Errorf("checkRawFilter(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"a = 1) OR (1 = 1", "a = 1; DELETE FROM t", "(a = 1", "'open", "a = 1)"} {
		if err := checkRawFilter(raw); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", raw, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{"t": `"t"`, `a"b`: `"a""b"`, "Mixed Case": `"Mixed Case"`} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// A namespace reaches SET SEARCH_PATH on a pooled session, outside the
// statement guard, so its name is checked before it is quoted.
func TestCheckSchemaName(t *testing.T) {
	for _, ok := range []string{"public", "Mixed Case", `we"ird`, "ü"} {
		if err := checkSchemaName("query", ok); err != nil {
			t.Errorf("checkSchemaName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "a\x00b", "a\xffb", strings.Repeat("x", 200)} {
		if err := checkSchemaName("query", bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkSchemaName(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}

// A CTE column list is not a call; nothing else shaped like one passes.
func TestCTEColumnsAreNotCalls(t *testing.T) {
	for _, text := range []string{
		"WITH c (a, b) AS (SELECT 1, 2) SELECT * FROM c",
		`WITH x AS (SELECT 1), c ("a") AS (SELECT 2) SELECT * FROM c`,
	} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
	for _, text := range []string{
		"WITH c (shutdown()) AS (SELECT 1) SELECT * FROM c",
		"SELECT shutdown(x) AS (y)",
		"WITH c AS (SELECT 1) SELECT shutdown(a) AS (b) FROM c",
		"SELECT 1, shutdown(a) AS (b)",
	} {
		if err := checkReadOnly(text); err == nil {
			t.Errorf("checkReadOnly(%q) = nil, want a refusal", text)
		}
	}
}

// Every name before '(' that is not an allow-listed built-in
// is judged, whatever its spelling: non-ASCII letters (a UDx may be named
// malé), and words the guard used to skip as clause or type words, which
// are not reserved in Vertica and can name a UDx.
func TestReadOnlyJudgesEveryCallName(t *testing.T) {
	for _, text := range []string{
		"SELECT malé(1)",
		"SELECT x_é(1) FROM t",
		"SELECT x.yé(1)",
		"SELECT 'ok', é",
		"SELECT ñ FROM t",
		"SELECT pattern(1)", "SELECT define(1)", "SELECT cube(1)", "SELECT rollup(1)", "SELECT sets(1)",
		"SELECT return(1)", "SELECT escape(1)", "SELECT money(1)", "SELECT raw(1)", "SELECT number(1)",
		"SELECT bytea(1)", "SELECT varying(1)", "SELECT lateral(1)", "SELECT values(1)", "SELECT partition(1)",
		"SELECT over(1)", "SELECT varchar(1)", "SELECT numeric(1)", "SELECT timestamp(1)",
		"SELECT * FROM t TABLESAMPLE(10)",
		// CTE scan stops at the main query, parenthesised or not.
		"WITH a AS (SELECT 1) (SELECT 1) UNION ALL (SELECT 2) ORDER BY 1, evil(a) AS (b)",
		"WITH a AS (SELECT 1) (SELECT 1), evil(a) AS (b)",
	} {
		if err := checkReadOnly(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
		}
	}
	for _, text := range []string{
		"SELECT 'malé' AS s, \"é\" FROM t -- é",
		"SELECT /* é */ 1",
		"SELECT CAST(x AS NUMERIC(10,2)), y::VARCHAR(20), z::TIMESTAMP(3) FROM t",
		"SELECT a FROM t GROUP BY ROLLUP(a, b)",
		"SELECT a FROM t GROUP BY CUBE(a)",
		"SELECT a FROM t GROUP BY GROUPING SETS((a), (b))",
		"SELECT ROW_NUMBER() OVER (ORDER BY a) FROM t",
		"SELECT ts FROM t TIMESERIES ts AS '1 hour' OVER (PARTITION BY k ORDER BY t)",
		"WITH a AS (SELECT 1), b (x) AS (SELECT 2) SELECT * FROM a, b",
	} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
}

// Grouping calls (rollup, cube) count only inside GROUP BY, keywords written
// as calls (by(1), exists(1) without a subquery) and a cast split as ": :"
// are refused, while the legitimate forms still pass.
func TestReadOnlyRefusesCallsOutsideTheirClause(t *testing.T) {
	for _, text := range []string{
		"SELECT 1 FROM t ORDER BY rollup(1)",
		"SELECT 1 FROM t ORDER BY cube(1)",
		"SELECT max(x) OVER (PARTITION BY cube(x)) FROM t",
		"SELECT by(1)", "SELECT is(1)", "SELECT between(1)", "SELECT exists(1)",
		"SELECT 1 WHERE EXISTS (1)",
		"SELECT x: :raw(1) FROM t",
		"SELECT x: :varchar(3) FROM t",
	} {
		if err := checkReadOnly(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
		}
	}
	for _, text := range []string{
		"SELECT 1 FROM t WHERE EXISTS (SELECT 1)",
		"SELECT 1 FROM t WHERE NOT EXISTS (WITH c AS (SELECT 1) SELECT * FROM c)",
		"SELECT a FROM t GROUP BY ROLLUP(a, b)",
		"SELECT a FROM t GROUP BY CUBE(a)",
		"SELECT x::VARCHAR(20) FROM t",
		`WITH "q" (x) AS (SELECT 1) SELECT * FROM q`,
	} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
}
