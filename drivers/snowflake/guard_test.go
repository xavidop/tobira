package snowflake

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// The read-only guard is lexical and runs before anything is sent, so these
// tests need no server: whatever a fake would answer, a refused text never
// leaves the driver (TestReadOnlyRefusalSendsNothing checks that end to end).

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
		"SHOW TABLES",
		"show terse schemas in account",
		"SHOW PRIMARY KEYS IN TABLE db.s.t",
		"DESC TABLE t",
		"DESCRIBE TABLE db.s.\"t\"",
		"describe view v",
		"EXPLAIN USING TEXT SELECT * FROM t",
		"explain select 1",
		// Strings, quoted names and comments hide what they hold.
		"SELECT 'delete from t; drop table t' AS s",
		`SELECT 'it\'s; still one string' AS s`,
		"SELECT 'doubled '' quote; delete' AS s",
		"SELECT $$ dollar ; delete from t $$ AS s",
		`SELECT "update", "delete" FROM "insert"`,
		`SELECT "a""b" FROM t`,
		"SELECT 1 -- ; delete from t",
		"SELECT 1 // ; delete from t",
		"SELECT /* ; delete from t */ 1",
		"-- leading comment\nSELECT 1",
		"/* leading */ SELECT 1",
		// Session variables, lambdas, JSON paths, casts, time travel.
		"SELECT $myvar, $1x FROM t",
		"SELECT FILTER(arr, a -> a > 1) FROM t",
		"SELECT v:a.b[0]::STRING FROM t",
		"SELECT * FROM t AT(OFFSET => -60)",
		"SELECT * FROM t WHERE id = ? AND name = :2",
		"SELECT SYSTEM$TYPEOF(v) FROM t",
		"SELECT x FROM t QUALIFY ROW_NUMBER() OVER (ORDER BY x) = 1 LIMIT 10 OFFSET 5",
		"SELECT ARRAY_CONSTRUCT(1, 2), OBJECT_CONSTRUCT('a', 1)",
		"SELECT * FROM TABLE(GENERATOR(ROWCOUNT => 10))",
		"SELECT 1.5e3, .5, 0x1F",
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
		// Writes, DDL, session and scripting statements.
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"MERGE INTO t USING u ON t.id = u.id WHEN MATCHED THEN DELETE",
		"TRUNCATE TABLE t",
		"CREATE TABLE t2 AS SELECT * FROM t",
		"create or replace table t2 as select 1",
		"ALTER SESSION SET TRANSACTION_READ_ONLY = TRUE",
		"DROP TABLE t",
		"UNDROP TABLE t",
		"GRANT SELECT ON t TO ROLE r",
		"USE SCHEMA s",
		"SET x = 1",
		"UNSET x",
		"BEGIN",
		"BEGIN TRANSACTION",
		"COMMIT",
		"CALL my_proc()",
		"EXECUTE IMMEDIATE 'DELETE FROM t'",
		"EXECUTE IMMEDIATE $$ BEGIN DELETE FROM t; END; $$",
		"DECLARE x INT; BEGIN RETURN 1; END",
		"COPY INTO t FROM @s",
		"COPY INTO @s FROM t",
		"PUT file:///tmp/x @s",
		"GET @s file:///tmp",
		"REMOVE @s",
		"LIST @s",
		"COMMENT ON TABLE t IS 'x'",
		// Several statements in one text.
		"SELECT 1; DELETE FROM t",
		"SELECT 1; SELECT 2",
		"SELECT 1 ; -- trailing\n DELETE FROM t",
		"SHOW TABLES; DROP TABLE t",
		// The flow operator chains statements.
		"SHOW TABLES ->> DELETE FROM t",
		"SELECT 1 ->> SELECT * FROM $1",
		"SHOW TABLES->>SELECT 1",
		// Stages: @name, @~ and @%table, and functions that read stage files.
		"SELECT $1 FROM @mystage",
		"SELECT * FROM @~/f.csv",
		"SELECT * FROM @%t",
		"SELECT * FROM DIRECTORY(@s)",
		"SELECT * FROM TABLE(INFER_SCHEMA(LOCATION => '@s', FILE_FORMAT => 'f'))",
		"SELECT GET_PRESIGNED_URL(@s, 'f')",
		"SELECT BUILD_SCOPED_FILE_URL(@s, 'f')",
		// Anonymous procedures, sequences, locks, INTO.
		"WITH p AS PROCEDURE () RETURNS INT LANGUAGE SQL AS $$ BEGIN DELETE FROM t; RETURN 1; END $$ CALL p()",
		"WITH c AS (SELECT 1) DELETE FROM t",
		"WITH c AS (SELECT 1) INSERT INTO t SELECT * FROM c",
		"SELECT seq1.NEXTVAL",
		"SELECT * FROM t FOR UPDATE",
		"SELECT x INTO :v FROM t",
		// System functions with side effects.
		"SELECT SYSTEM$CANCEL_ALL_QUERIES(1)",
		"SELECT SYSTEM$ABORT_SESSION(1)",
		"select system$wait(10)",
		"SELECT SYSTEM$SET_RETURN_VALUE('x')",
		// EXPLAIN of a write is refused too (fail closed).
		"EXPLAIN DELETE FROM t",
		"EXPLAIN USING TEXT INSERT INTO t VALUES (1)",
		// Parentheses only around queries.
		"(SHOW TABLES)",
		"(DELETE FROM t)",
		// Text the lexer cannot read.
		"SELECT 'unterminated",
		`SELECT 'escaped quote at the end\'`,
		`SELECT "unterminated`,
		"SELECT $$ unterminated",
		"SELECT 1 /* unterminated",
		"SELECT 1 /* nested /* opener */ ; DELETE FROM t */",
		"SELECT 1\x00",
		// A line comment that also ends at a carriage return.
		"SELECT 1 -- x\rDELETE FROM t",
		"SELECT 1 // x\rDELETE FROM t",
		// A keyword the server could read past a byte the lexer splits on.
		"SELECT 1; DELETE FROM t",
		" DELETE FROM t",
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

// The refusal names the reason, never the statement, which could hold a
// pasted secret.
func TestCheckReadOnlyErrorDoesNotQuoteText(t *testing.T) {
	const secret = "s3cr3t-value"
	for _, text := range []string{
		"INSERT INTO t VALUES ('" + secret + "')",
		"SELECT '" + secret,
		"SELECT 1; SELECT '" + secret + "'",
		"SELECT * FROM @" + secret,
	} {
		err := checkReadOnly(text)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("checkReadOnly(%q) = %v: want a refusal that does not quote the text", text, err)
		}
	}
}

func TestLexTokens(t *testing.T) {
	toks, err := lex(`SELECT "a""b", 'x\'y', $$z$$, $v, SYSTEM$TYPEOF(1), @s, 1.5 // c` + "\n-- d\n/* e */ ;")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range toks {
		got = append(got, tk.kind.String()+":"+tk.text)
	}
	want := []string{
		"word:SELECT", `quoted:"a""b"`, "punct:,", `string:'x\'y'`, "punct:,", "string:$$z$$", "punct:,",
		"var:$v", "punct:,", "word:SYSTEM$TYPEOF", "punct:(", "number:1", "punct:)", "punct:,", "punct:@", "word:s",
		"punct:,", "number:1.5", "punct:;",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("lex tokens:\n got %v\nwant %v", got, want)
	}
}

func TestLexErrors(t *testing.T) {
	for _, text := range []string{"'x", `"x`, "$$x", "/* x", "a\x00", "/* a /* b */ */"} {
		if _, err := lex(text); err == nil {
			t.Errorf("lex(%q) = nil error, want one", text)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, ok := range []string{"a = 1", "(a = 1) OR (b = 'x)')", "name LIKE 'A%' -- trailing comment", "CONTAINS(name, 'x')"} {
		if err := checkRawFilter(ok); err != nil {
			t.Errorf("checkRawFilter(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"a = 1) OR (1 = 1", "a = 1; DELETE FROM t", "(a = 1", "'unterminated", "a = 1 /* x"} {
		if err := checkRawFilter(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"tobira_people": `"tobira_people"`,
		`a"b`:           `"a""b"`,
		"TABLE":         `"TABLE"`,
	} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

// Inside a transaction, statements that end it or that Snowflake commits on
// its own (every DDL statement commits the open transaction) are refused.
func TestTxStatementRefusal(t *testing.T) {
	for _, text := range []string{
		"BEGIN", "begin transaction", "START TRANSACTION", "COMMIT", "ROLLBACK", "COMMIT WORK",
		"CREATE TABLE x (a INT)", "ALTER TABLE t ADD COLUMN b INT", "DROP TABLE t", "UNDROP TABLE t",
		"GRANT SELECT ON t TO ROLE r", "REVOKE SELECT ON t FROM ROLE r", "COMMENT ON TABLE t IS 'x'",
		"TRUNCATE TABLE t", "USE SCHEMA s", "(COMMIT)", "-- c\nCOMMIT", "'unterminated",
	} {
		if err := txStatementRefusal(text); err == nil {
			t.Errorf("txStatementRefusal(%q) = nil, want a refusal", text)
		}
	}
	for _, text := range []string{"SELECT 1", "INSERT INTO t VALUES (1)", "UPDATE t SET a = 1", "DELETE FROM t", "MERGE INTO t USING u ON true WHEN MATCHED THEN DELETE"} {
		if err := txStatementRefusal(text); err != nil {
			t.Errorf("txStatementRefusal(%q) = %v, want nil", text, err)
		}
	}
}

// A number token must not swallow letters or a sign that is
// not part of an exponent, so a keyword or the flow operator glued to a
// digit stays visible to the guard.
func TestGuardRefusesKeywordsGluedToNumbers(t *testing.T) {
	for _, text := range []string{
		"SELECT 1e->>COMMENT ON TABLE t IS 'x'",
		"SELECT 0xe->> COMMENT ON TABLE t IS 'x'",
		"SELECT 1e->>START TRANSACTION",
		"SELECT 1DELETE",
		"SELECT 1.5e->>SELECT 1",
		"SELECT 1 COMMENT",
		"SELECT 1 FROM t WHERE 2START",
	} {
		if err := checkReadOnly(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
		}
	}
	for _, text := range []string{"SELECT 1e10, 1.5E-3, 2e+5, .25, 7.", `SELECT "COMMENT", "START" FROM t`} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
}

func TestLexNumberTokensStopAtLetters(t *testing.T) {
	for in, want := range map[string]string{
		"1DELETE": "number:1 word:DELETE",
		"1e->>":   "number:1 word:e punct:- punct:> punct:>",
		"0xe":     "number:0 word:xe",
		"1.5e3":   "number:1.5e3",
		"1.5E-3x": "number:1.5E-3 word:x",
		"2e+":     "number:2 word:e punct:+",
		".5":      "number:.5",
		"7.e":     "number:7. word:e",
		"12_3":    "number:12 word:_3",
		"3.4.5":   "number:3.4 number:.5",
	} {
		toks, err := lex(in)
		if err != nil {
			t.Fatalf("lex(%q): %v", in, err)
		}
		var got []string
		for _, tk := range toks {
			got = append(got, tk.kind.String()+":"+tk.text)
		}
		if g := strings.Join(got, " "); g != want {
			t.Errorf("lex(%q) = %s, want %s", in, g, want)
		}
	}
}

// Table functions on a read-only path are an allow-list of built-ins: a
// stored procedure that returns a table can be selected FROM TABLE(...) and
// may run DML, and a user-defined table function cannot be told from one.
func TestReadOnlyTableFunctionAllowList(t *testing.T) {
	for _, text := range []string{
		"SELECT * FROM TABLE(my_proc(1))",
		"SELECT * FROM TABLE(db.s.my_proc(1))",
		`SELECT * FROM TABLE("my_proc"(1))`,
		"SELECT * FROM t, LATERAL my_udtf(t.v)",
		"SELECT * FROM my_proc(1)",
		"SELECT * FROM t JOIN my_proc(1) ON true",
		"SELECT * FROM TABLE(db.public.QUERY_HISTORY())",
		"SELECT * FROM TABLE(INFORMATION_SCHEMA)",
	} {
		err := checkReadOnly(text)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
			continue
		}
		if text != "SELECT * FROM TABLE(INFORMATION_SCHEMA)" && !strings.Contains(err.Error(), "role") {
			t.Errorf("checkReadOnly(%q) = %v: want a hint about a SELECT-only role", text, err)
		}
	}
	for _, text := range []string{
		"SELECT * FROM TABLE(GENERATOR(ROWCOUNT => 10))",
		"select * from table(result_scan(last_query_id()))",
		"SELECT f.value FROM t, TABLE(FLATTEN(input => t.v)) f",
		"SELECT f.value FROM t, LATERAL FLATTEN(input => t.v) f",
		"SELECT * FROM TABLE(SPLIT_TO_TABLE('a,b', ','))",
		"SELECT * FROM TABLE(STRTOK_SPLIT_TO_TABLE('a b', ' '))",
		"SELECT * FROM TABLE(INFORMATION_SCHEMA.QUERY_HISTORY())",
		"SELECT * FROM TABLE(SALES.INFORMATION_SCHEMA.TASK_HISTORY(RESULT_LIMIT => 10))",
		"SELECT * FROM TABLE($tname)",
		"SELECT * FROM TABLE('t')",
		"SELECT * FROM (SELECT 1) x JOIN (SELECT 2) y ON true",
		"SELECT COUNT(*) FROM t",
		"SELECT EXTRACT(year FROM TO_DATE('2026-01-02'))",
		"SELECT TRIM(' ' FROM UPPER(name)) FROM t",
		"SELECT * FROM (SELECT x FROM TABLE(GENERATOR(ROWCOUNT => 2))) JOIN t ON true",
	} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
}

// Inside a transaction every word is checked, not only the first, and the
// flow operator is refused: a chained or embedded DDL would still commit.
func TestTxStatementChecksEveryWord(t *testing.T) {
	for _, text := range []string{
		"SELECT 1 ->> CREATE TABLE x(a INT)",
		"WITH p AS PROCEDURE () RETURNS INT LANGUAGE SQL AS $$ BEGIN RETURN 1; END $$ CALL p()",
		"SELECT 1->>SELECT 2",
		"SELECT 1e->>DROP TABLE t",
	} {
		if err := txStatementRefusal(text); err == nil {
			t.Errorf("txStatementRefusal(%q) = nil, want a refusal", text)
		}
	}
}

// Quoted names are case-sensitive user objects ("generator"
// is not GENERATOR, "information_schema" is not INFORMATION_SCHEMA), so a
// quoted part in a table-function name is refused; and a call after a
// comma in a FROM clause is judged like one after FROM.
func TestReadOnlyRefusesQuotedTableFunctionNames(t *testing.T) {
	for _, text := range []string{
		`SELECT * FROM TABLE("generator"())`,
		`SELECT * FROM TABLE("result_scan"('x'))`,
		`SELECT * FROM TABLE(db."information_schema".evil())`,
		`SELECT * FROM TABLE("information_schema".evil())`,
		`SELECT * FROM TABLE("GENERATOR"(ROWCOUNT => 1))`,
		`SELECT * FROM TABLE("SALES".INFORMATION_SCHEMA.COPY_HISTORY(TABLE_NAME => 'T'))`,
		`SELECT * FROM t, LATERAL "flatten"(input => t.v)`,
		`SELECT * FROM t, evil(t.x)`,
		`SELECT * FROM t, u, db.s.evil(1)`,
		`SELECT * FROM (SELECT 1 AS x) a, evil(a.x)`,
		`SELECT x FROM (SELECT * FROM t, evil(1))`,
		`SELECT * FROM t JOIN u ON t.a = u.a, evil(1)`,
	} {
		if err := checkReadOnly(text); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
		}
	}
	for _, text := range []string{
		`SELECT * FROM t, u WHERE f(t.a, u.b) = 1`,
		`SELECT f(a, b), g(c) FROM t, u ORDER BY h(a, b)`,
		`SELECT * FROM t, TABLE(FLATTEN(input => t.v)) f, u`,
		`SELECT * FROM t, LATERAL FLATTEN(input => t.v) f`,
		`SELECT * FROM t WHERE x IN (1, 2) GROUP BY a, b`,
		`SELECT "a", "b" FROM "t", "u"`,
		`SELECT * FROM t, u QUALIFY ROW_NUMBER() OVER (PARTITION BY a, b ORDER BY c) = 1`,
		`SELECT * FROM t, (SELECT 1) s, u`,
		`SELECT COALESCE(a, b) FROM t, u LIMIT 10`,
		`SELECT * FROM t JOIN u USING (a, b), v`,
		`SELECT * FROM t JOIN u ON t.a IN (1, 2), v`,
	} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
}
