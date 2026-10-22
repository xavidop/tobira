package mysql

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCheckReadOnlyAllowed(t *testing.T) {
	for _, text := range []string{
		"SELECT 1",
		"  select * from t",
		"\t\r\n\v\fSELECT 1",
		"/* x */ SELECT 1",
		"# c\nSELECT 1",
		"-- c\nSELECT 1",
		"--\tc\nSELECT 1",
		"SELECT 1 --",
		"SELECT 1;",
		"SELECT 1; -- trailing",
		"SELECT 1 ;  /* c */ ;",
		"SELECT 1 --x", // "--x" is not a comment: 1 - (-x)
		"SELECT 1 # ; DELETE FROM t",
		"SELECT 'x' -- '; DELETE FROM t",
		"WITH c AS (SELECT 1) SELECT * FROM c",
		"with recursive c(n) as (select 1 union all select n+1 from c where n<3) select * from c",
		"WITH a AS (SELECT 1), b (x) AS (SELECT 2) SELECT * FROM a, b",
		"WITH `x y` AS (SELECT 1) TABLE `x y`",
		"WITH c AS (SELECT 1) (SELECT * FROM c)",
		"WITH c AS (SELECT 1) VALUES ROW(1)",
		"VALUES ROW(1,2)",
		"TABLE t",
		"SHOW TABLES",
		"SHOW CREATE TABLE t",
		"DESCRIBE t",
		"DESC t",
		"desc t col",
		"DESC t 'c%'",
		"DESC db.t",
		"DESC `db`.`t`",
		"EXPLAIN t",
		"EXPLAIN SELECT 1",
		"EXPLAIN FORMAT=JSON SELECT 1",
		"EXPLAIN FORMAT = TREE SELECT 1",
		"EXPLAIN ANALYZE SELECT * FROM t",
		"EXPLAIN ANALYZE FORMAT=TREE SELECT * FROM t",
		"EXPLAIN FORMAT=JSON INTO @plan SELECT 1",
		"EXPLAIN TABLE t",
		"EXPLAIN (SELECT 1)",
		"EXPLAIN FOR CONNECTION 5",
		"DESC SELECT 1",
		"DESCRIBE WITH c AS (SELECT 1) SELECT * FROM c",
		"USE db",
		"USE `db`",
		"SELECT /*+ MAX_EXECUTION_TIME(1000) */ * FROM t",
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t FOR SHARE",
		"SELECT * FROM t LOCK IN SHARE MODE",
		"SELECT 1 INTO @v",
		"SELECT * FROM t WHERE a = 1 INTO @a",
		"SELECT 'a;b' AS x",
		`SELECT "a;b"`,
		"SELECT `a;b` FROM t",
		"SELECT `a``;b` FROM t",
		"SELECT 'it''s; DELETE'",
		`SELECT 'a\'; DELETE FROM t'`,
		`SELECT "a\"; DELETE FROM t"`,
		"SELECT '/*!50000 DROP TABLE t */'",
		"SELECT 'INTO OUTFILE'",
		"SELECT `outfile` FROM t",
		"(SELECT 1) UNION (SELECT 2)",
		"((SELECT 1))",
		"SELECT GET_LOCK('x', 0)",
		"SELECT x'4142', _utf8mb4'a;b', N'c'",
		"SELECT @a, @'b', @@session.sql_mode",
		"SELECT 1 /* a */ /* b */",
	} {
		if err := checkReadOnly(text, lexMode{}); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", text, err)
		}
	}
}

func TestCheckReadOnlyRefused(t *testing.T) {
	for _, text := range []string{
		"",
		"   ",
		"-- only a comment",
		"DELETE FROM t",
		"UPDATE t SET a = 1",
		"INSERT INTO t VALUES (1)",
		"REPLACE INTO t VALUES (1)",
		"CREATE TABLE x (a int)",
		"DROP TABLE t",
		"ALTER TABLE t ADD c int",
		"TRUNCATE t",
		"RENAME TABLE t TO u",
		"SET autocommit = 1",
		"SET SESSION TRANSACTION READ WRITE",
		"SET TRANSACTION READ WRITE",
		"LOCK TABLES t WRITE",
		"UNLOCK TABLES",
		"COMMIT",
		"ROLLBACK",
		"START TRANSACTION",
		"BEGIN",
		"XA START 'x'",
		"SAVEPOINT s",
		"CALL p()",
		"DO GET_LOCK('x', 0)",
		"HANDLER t OPEN",
		"LOAD DATA INFILE 'x' INTO TABLE t",
		"PREPARE s FROM 'DELETE FROM t'",
		"EXECUTE s",
		"GRANT ALL ON *.* TO x",
		"FLUSH TABLES",
		"KILL 1",
		"ANALYZE TABLE t",
		"OPTIMIZE TABLE t",
		"HELP 'x'",
		"/* x */ DELETE FROM t",
		"# x\nDELETE FROM t",
		"-- x\nDELETE FROM t",
		"--x\nDELETE FROM t",
		"/*!50000 DELETE FROM t */",
		"/*!DELETE FROM t */",
		"/*M!100000 DELETE FROM t */",
		"SELECT 1 /*!50000 , 2 */",
		"SELECT /*! STRAIGHT_JOIN */ 1",
		"SELECT 1 /* /*!50000 x */",
		"SELECT /*+ /*!50000 x */ 1",
		"WITH c AS (SELECT 1) DELETE FROM t",
		"WITH c AS (SELECT 1) UPDATE t SET a = 1",
		"WITH RECURSIVE c AS (SELECT 1) DELETE t FROM t",
		"WITH a AS (SELECT 1), b AS (SELECT 2) DELETE FROM t",
		"WITH c AS (SELECT 1)",
		"WITH",
		"WITH c (SELECT 1) SELECT 1",
		"WITH c AS SELECT 1",
		"EXPLAIN ANALYZE DELETE FROM t",
		"EXPLAIN DELETE FROM t",
		"EXPLAIN FORMAT=TREE UPDATE t SET a = 1",
		"DESC UPDATE t SET a = 1",
		"DESCRIBE INSERT INTO t VALUES (1)",
		"DESCRIBE REPLACE INTO t VALUES (1)",
		"EXPLAIN ANALYZE WITH c AS (SELECT 1) DELETE FROM t",
		"EXPLAIN (DELETE FROM t)",
		"EXPLAIN",
		"EXPLAIN ANALYZE",
		"DESC t col extra",
		"EXPLAIN FOR",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"SELECT * INTO DUMPFILE '/tmp/x' FROM t",
		"select * from t into outfile '/tmp/x' fields terminated by ','",
		"TABLE t INTO OUTFILE '/tmp/x'",
		"EXPLAIN ANALYZE SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"WITH c AS (SELECT 1) SELECT * FROM c INTO OUTFILE '/tmp/x'",
		"SELECT 1; DELETE FROM t",
		"SELECT 1;DELETE FROM t",
		"DESC t; DROP TABLE t",
		"SELECT 'unterminated",
		`SELECT "unterminated`,
		"SELECT `unterminated",
		"/* unterminated SELECT 1",
		"SELECT 1 /* unterminated",
		"SELECT 1\x00",
		"`select` 1",
		"'SELECT' 1",
		"(DELETE FROM t)",
		"(",
		"; SELECT 1",
	} {
		err := checkReadOnly(text, lexMode{})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Op != "query" {
			t.Errorf("checkReadOnly(%q) = %#v, want a *driver.Error from %s/query", text, err, Name)
		}
	}
}

func TestCheckReadOnlyMultipleReads(t *testing.T) {
	for _, text := range []string{
		"SELECT 1; SELECT 2",
		"SHOW TABLES; SELECT 1;",
		"SELECT 1;\nSELECT 2 -- c",
	} {
		err := checkReadOnly(text, lexMode{})
		if !errors.Is(err, driver.ErrUnsupported) || errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrUnsupported only", text, err)
		}
	}
}

// TestCheckReadOnlySQLModes checks that the guard lexes strings the way the
// server will: with NO_BACKSLASH_ESCAPES a backslash does not escape a quote,
// and with ANSI_QUOTES "…" is an identifier, where it never does.
func TestCheckReadOnlySQLModes(t *testing.T) {
	tests := []struct {
		name string
		text string
		mode lexMode
		ok   bool
	}{
		{"default mode reads a string", `SELECT 'a\' INTO OUTFILE '/tmp/x' -- '`, lexMode{}, true},
		{"no backslash escapes exposes INTO OUTFILE", `SELECT 'a\' INTO OUTFILE '/tmp/x' -- '`, lexMode{noBackslash: true}, false},
		{"no backslash escapes exposes a second statement", `SELECT 'a\'; DELETE FROM t -- '`, lexMode{noBackslash: true}, false},
		{"ansi quotes identifier ignores backslash", `SELECT "a\" INTO OUTFILE '/tmp/x' -- "`, lexMode{ansiQuotes: true}, false},
		{"ansi quotes doubled quote", `SELECT "a""b" FROM t`, lexMode{ansiQuotes: true}, true},
		{"no backslash plain string", `SELECT 'a\b'`, lexMode{noBackslash: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkReadOnly(tt.text, tt.mode)
			if tt.ok && err != nil {
				t.Fatalf("checkReadOnly = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, driver.ErrReadOnly) {
				t.Fatalf("checkReadOnly = %v, want ErrReadOnly", err)
			}
		})
	}
}

func TestModeFromSQLMode(t *testing.T) {
	tests := []struct {
		sqlMode string
		want    lexMode
	}{
		{"", lexMode{}},
		{"STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION", lexMode{}},
		{"NO_BACKSLASH_ESCAPES", lexMode{noBackslash: true}},
		{"ansi_quotes,no_backslash_escapes", lexMode{noBackslash: true, ansiQuotes: true}},
		{"ANSI", lexMode{ansiQuotes: true}}, // ANSI expands to ANSI_QUOTES on the server; keep the raw name too
	}
	for _, tt := range tests {
		if got := modeFromSQLMode(tt.sqlMode); got != tt.want {
			t.Errorf("modeFromSQLMode(%q) = %+v, want %+v", tt.sqlMode, got, tt.want)
		}
	}
}

func TestExplainTargetAllowed(t *testing.T) {
	for _, stmt := range []string{
		"SELECT * FROM t",
		"select 1;",
		"(SELECT 1)",
		"WITH c AS (SELECT 1) SELECT * FROM c",
		"TABLE t",
		"VALUES ROW(1)",
		"DELETE FROM t WHERE id = 1",
		"UPDATE t SET a = 1 WHERE id = 2",
		"INSERT INTO t (a) VALUES (1)",
		"REPLACE INTO t (a) VALUES (1)",
		"WITH c AS (SELECT 1) UPDATE t SET a = 1",
		"WITH c AS (SELECT 1) DELETE FROM t",
		"/* c */ SELECT 1",
		"SELECT '\\'' FROM t",
	} {
		for _, format := range []string{"JSON", "TREE", "TRADITIONAL"} {
			text := explainText(format, stmt)
			if err := explainTarget(text, lexMode{}); err != nil {
				t.Errorf("explainTarget(%q) = %v, want nil", text, err)
			}
		}
	}
}

func TestExplainTargetRefused(t *testing.T) {
	for _, stmt := range []string{
		"",
		"-- only a comment",
		"ANALYZE SELECT 1",
		"ANALYZE TABLE t",
		"FOR CONNECTION 5",
		"CREATE TABLE x (a int)",
		"DROP TABLE t",
		"SET autocommit = 1",
		"COMMIT",
		"CALL p()",
		"SHOW TABLES",
		"DESCRIBE t",
		"EXPLAIN SELECT 1",
		"SELECT 1; DELETE FROM t",
		"SELECT 1; SELECT 2",
		"SELECT /*! 1; DROP TABLE t */",
		"SELECT 1 /*!50000 , 2 */",
		"SELECT 'unterminated",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"WITH c AS (SELECT 1) CREATE TABLE x AS SELECT 1",
		"DO 1",
		"LOAD DATA INFILE 'x' INTO TABLE t",
	} {
		for _, format := range []string{fmtJSON, fmtTree, fmtTraditional, fmtBareJSON, fmtBare} {
			text := explainText(format, stmt)
			if err := explainTarget(text, lexMode{}); err == nil {
				t.Errorf("explainTarget(%q) = nil, want a refusal", text)
			}
		}
	}
	// The prefix must be the driver's own. (Plain EXPLAIN SELECT is
	// SingleStore's text form, which only plans, so it is accepted.)
	for _, text := range []string{"SELECT 1", "EXPLAIN ANALYZE SELECT 1", "EXPLAIN FORMAT=XML SELECT 1", "EXPLAIN EXTENDED SELECT 1"} {
		if err := explainTarget(text, lexMode{}); err == nil {
			t.Errorf("explainTarget(%q) = nil, want a refusal", text)
		}
	}
	// Under NO_BACKSLASH_ESCAPES the backslash ends the string: a second statement follows.
	text := explainText("JSON", `SELECT 'a\'; DELETE FROM t; -- '`)
	if err := explainTarget(text, lexMode{}); err != nil {
		t.Fatalf("default mode: %v", err)
	}
	if err := explainTarget(text, lexMode{noBackslash: true}); err == nil {
		t.Errorf("NO_BACKSLASH_ESCAPES: %q accepted", text)
	}
}

func TestCheckTxStatementAllowed(t *testing.T) {
	for _, text := range []string{
		"SELECT 1",
		"UPDATE t SET a = 1 WHERE id = 1",
		"DELETE FROM t",
		"INSERT INTO t VALUES (1)",
		"REPLACE INTO t VALUES (1)",
		"WITH c AS (SELECT 1) UPDATE t SET a = 1",
		"SET @x = 1",
		"SET SESSION sql_mode = ''",
		"SHOW TABLES",
		"DESCRIBE t",
		"EXPLAIN SELECT 1",
		"USE tobira",
		"DO SLEEP(0)",
		"SELECT * FROM t FOR UPDATE",
		"TABLE t",
		"VALUES ROW(1)",
		"SELECT 'COMMIT'",
		"SELECT `commit` FROM t",
		"/* COMMIT */ SELECT 1",
		"SELECT 1;",
		"LOAD DATA LOCAL INFILE 'x' INTO TABLE t",
	} {
		if err := checkTxStatement(text, lexMode{}); err != nil {
			t.Errorf("checkTxStatement(%q) = %v, want nil", text, err)
		}
	}
}

func TestCheckTxStatementRefused(t *testing.T) {
	for _, text := range []string{
		"CREATE TABLE x (a int)",
		"create temporary table x (a int)",
		"ALTER TABLE t ADD c int",
		"DROP TABLE t",
		"RENAME TABLE t TO u",
		"TRUNCATE TABLE t",
		"COMMIT",
		"commit work",
		"ROLLBACK",
		"ROLLBACK TO SAVEPOINT s",
		"START TRANSACTION",
		"BEGIN",
		"BEGIN WORK",
		"SAVEPOINT s",
		"RELEASE SAVEPOINT s",
		"XA START 'x'",
		"LOCK TABLES t WRITE",
		"LOCK INSTANCE FOR BACKUP",
		"UNLOCK TABLES",
		"SET autocommit = 1",
		"set AUTOCOMMIT=0",
		"SET @@autocommit = 1",
		"SET @@session.autocommit = 1",
		"SET SESSION autocommit = ON",
		"SET @x = 1, autocommit = 1",
		"SET PASSWORD = 'x'",
		"SET DEFAULT ROLE ALL TO u",
		"GRANT SELECT ON t TO u",
		"REVOKE SELECT ON t FROM u",
		"CREATE USER u",
		"ANALYZE TABLE t",
		"OPTIMIZE TABLE t",
		"REPAIR TABLE t",
		"CHECK TABLE t",
		"FLUSH TABLES",
		"CACHE INDEX t IN c",
		"LOAD INDEX INTO CACHE t",
		"RESET MASTER",
		"INSTALL PLUGIN p SONAME 'x'",
		"UNINSTALL PLUGIN p",
		"IMPORT TABLE FROM 'x'",
		"CHANGE REPLICATION SOURCE TO SOURCE_HOST='x'",
		"STOP REPLICA",
		"CALL p()",
		"PREPARE s FROM 'COMMIT'",
		"EXECUTE s",
		"DEALLOCATE PREPARE s",
		"/* x */ COMMIT",
		"-- x\nCOMMIT",
		"# x\nCOMMIT",
		";COMMIT",
		"SELECT 1; COMMIT",
		"SELECT /*! 1; COMMIT */",
		"/*!50000 COMMIT */",
		"SELECT 'unterminated",
		"COMMIT AND CHAIN",
		"COMMIT WORK AND CHAIN",
		"COMMIT WORK AND NO CHAIN",
		"commit and chain no release",
		"COMMIT AND NO CHAIN RELEASE",
		"ROLLBACK AND CHAIN",
		"ROLLBACK WORK AND CHAIN",
		"rollback work and no chain",
		"SET `autocommit` = 0",
		"SET `AutoCommit` = 1",
		"SET @@`autocommit` = 0",
		"SET @@session.`autocommit` = 1",
		"SET @@SESSION.autocommit = 1",
		"SET SESSION `autocommit` = 1",
		"SET @x = 1, `autocommit` = 1",
		"SET `auto``commit` = 1, `autocommit` = 1",
	} {
		err := checkTxStatement(text, lexMode{})
		if err == nil {
			t.Errorf("checkTxStatement(%q) = nil, want a refusal", text)
			continue
		}
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("checkTxStatement(%q) = %v, want ErrUnsupported", text, err)
		}
	}
	// Under NO_BACKSLASH_ESCAPES the backslash ends the string and COMMIT is a statement.
	text := `SELECT 'a\'; COMMIT; -- '`
	if err := checkTxStatement(text, lexMode{noBackslash: true}); err == nil {
		t.Errorf("NO_BACKSLASH_ESCAPES: %q accepted", text)
	}
}

func TestCheckTxStatementQuotedIdentifiers(t *testing.T) {
	// Under ANSI_QUOTES "autocommit" is an identifier too.
	if err := checkTxStatement(`SET "autocommit" = 1`, lexMode{ansiQuotes: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("ANSI_QUOTES SET \"autocommit\": want ErrUnsupported, got %v", err)
	}
	for _, text := range []string{"SET @v = 'autocommit'", "SET `auto commit` = 1", "SELECT `autocommit` FROM t", "SET @`x` = 1"} {
		if err := checkTxStatement(text, lexMode{}); err != nil {
			t.Errorf("checkTxStatement(%q) = %v, want nil", text, err)
		}
	}
}

func TestCheckTxStatementOneStatement(t *testing.T) {
	// Statements the deny-list allows one at a time are still refused together.
	for _, text := range []string{"SELECT 1; SELECT 2", "UPDATE t SET a = 1; DELETE FROM t", "SELECT ';'; SELECT 2 -- ;"} {
		if err := checkTxStatement(text, lexMode{}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("checkTxStatement(%q) = %v, want ErrUnsupported", text, err)
		}
	}
	for _, text := range []string{"SELECT 1;", "SELECT ';' ; ", "SELECT 1; -- x"} {
		if err := checkTxStatement(text, lexMode{}); err != nil {
			t.Errorf("checkTxStatement(%q) = %v, want nil", text, err)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, raw := range []string{
		"age > 3",
		"(a = 1 OR b = 2) AND c IN (1, 2)",
		"name = ')' AND x = '('",
		"name = ';'",
		"`we)ird` = 1",
		"f(g(1), (2)) = 3",
		"age > 3 -- ) trailing",
		"age > 3 # (",
		"/* ) */ age > 3",
	} {
		if err := checkRawFilter(raw, lexMode{}); err != nil {
			t.Errorf("checkRawFilter(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{
		"1) OR (1",
		"1) OR 1=1 OR (1",
		"(1",
		"1)",
		"1; DELETE FROM t",
		"1 ; ",
		"name = 'unterminated",
		"/* unterminated",
		")(",
	} {
		if err := checkRawFilter(raw, lexMode{}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", raw, err)
		}
	}
}

func TestCheckRawFilterSessionMode(t *testing.T) {
	raw := `name = 'x\') OR (1=1 -- '`
	if err := checkRawFilter(raw, lexMode{}); err != nil {
		t.Fatalf("default mode: %v", err)
	}
	if err := checkRawFilter(raw, lexMode{noBackslash: true}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("NO_BACKSLASH_ESCAPES: want ErrSyntax, got %v", err)
	}
}
