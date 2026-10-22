package bigquery

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCheckReadOnlyAllows(t *testing.T) {
	for _, q := range []string{
		"SELECT 1",
		"select * from tobira_people order by id",
		"  \n\tSELECT 1;",
		"SELECT 1;;  ",
		"SELECT 1; -- trailing comment",
		"WITH x AS (SELECT 1 AS a) SELECT a FROM x",
		"(SELECT 1) UNION ALL (SELECT 2)",
		"((SELECT 1))",
		"-- leading comment\nSELECT 1",
		"# hash comment\nSELECT 1",
		"/* block */ SELECT 1",
		"SELECT 'it''s', \"double\", '''triple ' quote''', \"\"\"triple \" dq\"\"\"",
		`SELECT r'raw \' still raw', b'bytes', rb'\x00', BR"x", R'''a'''`,
		"SELECT `update`, `delete` FROM `my-project.ds.t`",
		"SELECT `external_query_log`.x FROM `p.d.external_query_log`", // a name that only contains it
		"SELECT 'DELETE FROM t; DROP TABLE t'",
		"SELECT `a``b`", // a backtick name, then a second one (harmless)
		"SELECT * FROM t WHERE id = ? AND name = @name",
		"SELECT * FROM t FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP()",
		"SELECT REPEAT('a', 3), IF(true, 1, 2)",
		"SELECT 1.5e3, 0x1F, .5",
		"SELECT 'ünïcödé', `ünï`",
		"SELECT * FROM my-project.ds.t",
		// A backslash pairs with the next byte in a raw string too, so this
		// is one string, as ZetaSQL reads it (TestLexerAgreesWithServer).
		"SELECT r'\\'; DELETE FROM t; --'",
	} {
		if err := checkReadOnly(q); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", q, err)
		}
	}
}

func TestCheckReadOnlyRefuses(t *testing.T) {
	for _, q := range []string{
		"",
		"   ",
		"-- only a comment",
		"DELETE FROM tobira_people WHERE id = 1",
		"delete tobira_people where true",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1 WHERE true",
		"MERGE t USING s ON t.id = s.id WHEN MATCHED THEN DELETE",
		"TRUNCATE TABLE t",
		"CREATE TABLE t (a INT64)",
		"CREATE TEMP FUNCTION f() AS (1); SELECT f()",
		"DROP TABLE t",
		"ALTER TABLE t ADD COLUMN b INT64",
		"UNDROP SCHEMA ds",
		"EXPORT DATA OPTIONS(uri='gs://b/*.csv', format='CSV') AS SELECT 1",
		"LOAD DATA INTO t FROM FILES(format='CSV', uris=['gs://b/x'])",
		"DECLARE x INT64; SELECT x",
		"SET @@dataset_project_id = 'p'",
		"EXECUTE IMMEDIATE 'DELETE FROM t WHERE true'",
		"CALL ds.proc()",
		"BEGIN SELECT 1; END",
		"BEGIN TRANSACTION",
		"COMMIT",
		"ROLLBACK",
		"GRANT `roles/bigquery.dataViewer` ON TABLE t TO 'user:a@b.c'",
		"REVOKE `roles/bigquery.dataViewer` ON TABLE t FROM 'user:a@b.c'",
		"ASSERT (SELECT COUNT(*) FROM t) > 0",
		"RAISE USING MESSAGE = 'x'",
		"SELECT 1; DELETE FROM t WHERE true",
		"SELECT 1; SELECT 2",
		"SELECT 1 /* c */ ; /* c */ DELETE FROM t WHERE true",
		"WITH x AS (SELECT 1) DELETE FROM t WHERE true",
		"SELECT * FROM EXTERNAL_QUERY('conn', 'DELETE FROM x')",
		"SELECT * FROM t WHERE id IN (SELECT id FROM s); TRUNCATE TABLE t",
		"(DELETE FROM t WHERE true)",
		"SELECT 'unterminated",
		"SELECT \"unterminated",
		"SELECT '''unterminated'' '",
		"SELECT `unterminated",
		"SELECT 1 /* unterminated",
		"SELECT 'a\\'",                             // the backslash escapes the closing quote
		"SELECT 1 --x\r; DELETE FROM t WHERE true", // a carriage return ends a line comment
		"SELECT 1 #x\r; DELETE FROM t WHERE true",
		"#legacySQL\nSELECT * FROM [p:ds.t]",
		"  #LegacySql\nSELECT 1",
		"--legacySQL\nSELECT 1",
		"SELECT 1\x00",
		"VALUES (1)",
		"FROM t SELECT *", // pipe syntax starts with FROM: not on the allow-list
		"FROM t |> DELETE",
		// EXTERNAL_QUERY quoted, or as the last part of a quoted path: the
		// quotes change only parsing, so the server resolves the same TVF.
		"SELECT * FROM `EXTERNAL_QUERY`('conn', 'DELETE FROM x')",
		"SELECT * FROM `external_query`('conn', 'x')",
		"SELECT * FROM `External_Query`('conn', 'x')",
		"SELECT * FROM `region-us.EXTERNAL_QUERY`('conn', 'x')",
		"SELECT * FROM `region-us`.`EXTERNAL_QUERY`('conn', 'x')",
		"SELECT * FROM `my-project.region-us`.external_query('conn', 'x')",
		"SELECT * FROM `p.d.external_query`('conn', 'x')",
		"SELECT * FROM region-us.EXTERNAL_QUERY('conn', 'x')",
		"SELECT * FROM `EXTERNAL\\_QUERY`('conn', 'x')",   // a backslash escape inside the quotes
		"SELECT * FROM `\\x45XTERNAL_QUERY`('conn', 'x')", // \x45 is E
		"SELECT `a\\`b` FROM t",
	} {
		err := checkReadOnly(q)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", q, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkReadOnly(%q): error %v is not a bigquery driver.Error", q, err)
		}
	}
}

// A refusal names the reason, never the statement, which may hold values.
func TestCheckReadOnlyDoesNotEchoText(t *testing.T) {
	const secret = "s3cr3t-value-xyz"
	for _, q := range []string{
		"DELETE FROM t WHERE name = '" + secret + "'",
		"SELECT '" + secret + "'; DELETE FROM t WHERE true",
		"SELECT '" + secret,
	} {
		err := checkReadOnly(q)
		if err == nil {
			t.Fatalf("checkReadOnly(%q) accepted", q)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("refusal quotes the statement: %v", err)
		}
	}
}

func TestCheckStatementType(t *testing.T) {
	if err := checkStatementType("query", "SELECT"); err != nil {
		t.Errorf("SELECT refused: %v", err)
	}
	for _, st := range []string{"", "select", "SCRIPT", "INSERT", "UPDATE", "DELETE", "MERGE", "CREATE_TABLE",
		"CREATE_TABLE_AS_SELECT", "DROP_TABLE", "ALTER_TABLE", "EXPORT_DATA", "TRUNCATE_TABLE", "CALL", "LOAD_DATA"} {
		if err := checkStatementType("query", st); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("statement type %q: want ErrReadOnly, got %v", st, err)
		}
	}
}

func TestLexTokens(t *testing.T) {
	toks, err := lex("SELECT `a b`, 'x' 'y', r\"\\\"\", @p, ?, 1.5 -- c\n/* d */ FROM t;")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range toks.toks {
		got = append(got, tk.text)
	}
	want := []string{"SELECT", "`a b`", ",", "'x'", "'y'", ",", `r"\""`, ",", "@p", ",", "?", ",", "1.5", "FROM", "t", ";"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("tokens\n got %q\nwant %q", got, want)
	}
}
