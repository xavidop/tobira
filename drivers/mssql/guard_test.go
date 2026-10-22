package mssql

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCheckReadOnlyAllowed(t *testing.T) {
	for _, text := range []string{
		"SELECT 1",
		"  select * from t",
		"\t\r\n\fSELECT 1",
		"/* x */ SELECT 1",
		"/* a /* nested */ b */ SELECT 1",
		"SELECT 1 /* a /* b */ DELETE FROM t */",
		"-- c\nSELECT 1",
		"-- c\r\nSELECT 1",
		"SELECT 1 --",
		"SELECT 1;",
		"SELECT 1; -- trailing",
		"SELECT 1 ;  /* c */ ;",
		"SELECT 'x' -- '; DELETE FROM t",
		"SELECT [delete], [insert into] FROM [update]",
		"SELECT \"exec\" FROM t",
		"SELECT N'DELETE FROM t' AS s",
		"SELECT 'it''s; DELETE' AS s",
		"SELECT [a]]; DELETE] FROM t",
		"SELECT TOP (10) * FROM dbo.t WITH (NOLOCK) WHERE a = @p1 ORDER BY b",
		"SELECT * FROM t WITH (NOLOCK, INDEX(ix_a))",
		"SELECT * FROM t WITH (READPAST, ROWLOCK)",
		"SELECT ALL a FROM t UNION ALL SELECT b FROM u",
		"SELECT * FROM t ORDER BY id OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY",
		"SELECT a FROM t UNION SELECT b FROM u",
		"SELECT a FROM t UNION ALL SELECT b FROM u EXCEPT SELECT c FROM v INTERSECT SELECT d FROM w",
		"(SELECT 1) UNION (SELECT 2)",
		"SELECT (SELECT MAX(x) FROM u) AS m, EXISTS_col FROM t WHERE id IN (SELECT id FROM v)",
		"WITH c AS (SELECT 1 AS n) SELECT * FROM c",
		"WITH c (n) AS (SELECT 1), d AS (SELECT 2 AS m) SELECT * FROM c, d",
		"WITH c AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM c WHERE n < 3) SELECT * FROM c OPTION (MAXRECURSION 10)",
		"WITH XMLNAMESPACES ('urn:x' AS x) SELECT 1 AS [x:a] FOR XML PATH",
		"WITH c AS (SELECT 1 AS n) (SELECT * FROM c)",
		"SELECT CASE WHEN a > 0 THEN 'p' ELSE 'n' END AS sign FROM t",
		"SELECT 0x1F, 1.5, 1e5, 1E-3, .5, 2. AS x",
		"SELECT $5 AS money_col",
		"SELECT @@VERSION, @@ROWCOUNT",
		"SELECT * FROM OPENJSON(N'[1,2]')",
		"SELECT * FROM t FOR JSON PATH",
		"SELECT x.update_count, delete_flag, sp_count_col FROM t AS x",
		"SELECT * FROM [srv].[db].[dbo].[t]",
		"select * from tobira_people where id = @p1",
		"EXEC sp_help 'dbo.t'",
		"exec sp_helptext N'dbo.v'",
		"EXECUTE sys.sp_helpindex @objname = N'dbo.t'",
		"sp_help",
		"sp_helpdb tobira;",
		"EXEC sp_helpconstraint t, 'nomsg'",
	} {
		if err := checkReadOnly(text); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want allowed", text, err)
		}
	}
}

func TestCheckReadOnlyRefused(t *testing.T) {
	for _, text := range []string{
		"",
		"   ",
		";",
		"-- only a comment",
		"DELETE FROM t",
		"delete from tobira_people where id = 1",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"MERGE t USING u ON 1 = 1 WHEN MATCHED THEN DELETE;",
		"TRUNCATE TABLE t",
		"DROP TABLE t",
		"CREATE TABLE t (a int)",
		"ALTER TABLE t ADD b int",
		// several statements
		"SELECT 1; DELETE FROM t",
		"select 1; delete from tobira_people where id = 1",
		"SELECT 1 DELETE FROM t",
		"select 1 delete from tobira_people where id = 1",
		"SELECT 1 SELECT 2",
		"SELECT 1; SELECT 2",
		"(SELECT 1) SELECT 2",
		"SELECT 1 WITH c AS (SELECT 1) SELECT * FROM c",
		"SELECT 1\nGO\nDELETE FROM t",
		"SELECT 1 GO",
		// writable CTEs and SELECT INTO
		"WITH c AS (SELECT 1 AS n) DELETE FROM t",
		"with x as (select 1 as n) delete from tobira_people where id = 1",
		"WITH c AS (SELECT 1 AS n) UPDATE t SET a = 1",
		"SELECT * INTO t2 FROM t",
		"select * into tobira_copy from tobira_people",
		"SELECT * INTO #tmp FROM t",
		// procedures, dynamic SQL and server-side reads and writes
		"EXEC('DELETE FROM t')",
		"exec('delete from tobira_people where id = 1')",
		"EXECUTE sp_executesql N'DELETE FROM t'",
		"EXEC xp_cmdshell 'dir'",
		"xp_cmdshell 'dir'",
		"master..xp_cmdshell 'dir'",
		"[xp_cmdshell] 'dir'",
		"EXEC dbo.sp_help 't'",
		"EXEC sp_rename 't', 'u'",
		"sp_configure 'show advanced options', 1",
		"EXEC sp_help 't'; DELETE FROM t",
		"EXEC sp_help @objname = DELETE",
		"EXEC sp_help (SELECT 1)",
		"SELECT * FROM OPENROWSET('SQLNCLI', 'srv', 'DELETE FROM t')",
		"SELECT * FROM OPENQUERY(srv, 'DELETE FROM t')",
		"SELECT * FROM OPENDATASOURCE('SQLNCLI', 'x').db.dbo.t",
		"SELECT * FROM OPENROWSET(BULK 'c:\\x', SINGLE_CLOB) AS x",
		"SELECT xp_dirtree FROM t",
		"SELECT NEXT VALUE FOR dbo.seq",
		"BULK INSERT t FROM 'x'",
		// control flow, variables and session state
		"DECLARE @x int",
		"SET NOCOUNT ON",
		"SET TRANSACTION ISOLATION LEVEL READ UNCOMMITTED",
		"USE master",
		"BEGIN TRAN",
		"COMMIT",
		"ROLLBACK",
		"SAVE TRANSACTION s",
		"IF 1 = 1 SELECT 1",
		"WHILE 1 = 1 SELECT 1",
		"WAITFOR DELAY '00:00:10'",
		"SELECT 1 WAITFOR DELAY '00:00:10'",
		"PRINT 'x'",
		"RAISERROR('x', 16, 1)",
		"THROW 50000, 'x', 1",
		"DBCC CHECKDB",
		"KILL 52",
		"SHUTDOWN",
		"CHECKPOINT",
		"RECONFIGURE",
		"BACKUP DATABASE d TO DISK = 'x'",
		"GRANT SELECT ON t TO u",
		"REVOKE SELECT ON t TO u",
		"DENY SELECT ON t TO u",
		"OPEN c",
		"RECEIVE * FROM q",
		"SELECT 1 END CONVERSATION 'x'",
		"SELECT 1 SEND ON CONVERSATION 'x'",
		"SELECT 1 ENABLE TRIGGER tr ON t",
		"SELECT 1 OUTPUT",
		"REVERT",
		"SETUSER",
		// statements the allow-list does not know
		"VALUES (1)",
		"TABLE t",
		"dbo.proc",
		"[dbo].[proc] 1",
		"label: SELECT 1",
		// lexical tricks
		"SELECT 1 --x\rDELETE FROM t",
		"SELECT 1 -- x\r\n\rDELETE FROM t",
		"SELECT 0xINSERT t VALUES (1)",
		"SELECT 1eDELETE FROM t",
		"SELECT 1.eDELETE FROM t",
		"SELECT 1DELETE FROM t",
		"SELECT 1.5.6",
		"SELECT [a]]'] DELETE FROM t --'",
		"SELECT 'unterminated",
		"SELECT [unterminated",
		"SELECT \"unterminated",
		"SELECT 1 /* unterminated",
		"SELECT 1 /* a /* b */ unterminated",
		"SELECT 1\x00; DELETE FROM t",
		"SELECT 1 a#DELETE FROM t",
		// lock hints that hold exclusive or update locks while the result is open
		"SELECT * FROM t WITH (TABLOCKX)",
		"SELECT * FROM t WITH (TABLOCKX, HOLDLOCK)",
		"SELECT * FROM t WITH (XLOCK)",
		"SELECT * FROM t WITH (UPDLOCK)",
		"SELECT * FROM t WITH (holdlock)",
		"SELECT * FROM t WITH (PAGLOCK)",
		"SELECT * FROM t (XLOCK)",
		"SELECT * FROM t WITH (SERIALIZABLE)",
		"SELECT * FROM t WITH (repeatableread)",
		// reserved statement words
		"SELECT 1 DUMP DATABASE d TO x",
		"SELECT 1 LOAD DATABASE d FROM x",
		"SELECT 1 ALL SELECT 2",
	} {
		err := checkReadOnly(text)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", text, err)
		}
		var de *driver.Error
		if errors.As(err, &de) && de.Driver != Name {
			t.Errorf("checkReadOnly(%q): Driver = %q, want %q", text, de.Driver, Name)
		}
	}
}

// Every refusal names what it refused, so the user can fix the text.
func TestCheckReadOnlyReasons(t *testing.T) {
	for text, want := range map[string]string{
		"DELETE FROM t":                      "DELETE",
		"SELECT * INTO t2 FROM t":            "INTO",
		"SELECT 1 SELECT 2":                  "one statement",
		"SELECT 1; SELECT 2":                 "one statement",
		"SELECT 1 --x\rDELETE FROM t":        "carriage return",
		"SELECT 0xINSERT t VALUES (1)":       "number",
		"SELECT 'x":                          "unterminated",
		"EXEC xp_cmdshell 'dir'":             "EXEC",
		"SELECT NEXT VALUE FOR s":            "NEXT VALUE FOR",
		"SELECT 1\x00":                       "NUL",
		"SELECT xp_dirtree FROM t":           "XP_DIRTREE",
		"WITH c AS (SELECT 1) DELETE FROM t": "DELETE",
	} {
		err := checkReadOnly(text)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("checkReadOnly(%q) = %v, want a reason mentioning %q", text, err, want)
		}
	}
}

func TestExplainTarget(t *testing.T) {
	for _, text := range []string{
		"SELECT * FROM t",
		"WITH c AS (SELECT 1 AS n) SELECT * FROM c",
		"(SELECT 1)",
		"DELETE FROM t WHERE id = 1",
		"delete from tobira_people where id = 1",
		"INSERT INTO t (a) SELECT b FROM u",
		"INSERT INTO t OUTPUT INSERTED.a VALUES (1)",
		"UPDATE t SET a = 1 WHERE b = @p1",
		"UPDATE dbo.[t] WITH (ROWLOCK) SET a = 1",
		"MERGE t USING u ON t.a = u.a WHEN MATCHED THEN UPDATE SET b = u.b;",
		"MERGE t USING u ON t.a = u.a WHEN MATCHED THEN DELETE;",
		"WITH c AS (SELECT 1 AS n) DELETE FROM t",
		"SELECT * INTO t2 FROM t",
	} {
		if err := explainTarget(text); err != nil {
			t.Errorf("explainTarget(%q) = %v, want allowed", text, err)
		}
	}
	for _, text := range []string{
		"",
		"SET SHOWPLAN_XML OFF",
		"SELECT 1; SET SHOWPLAN_XML OFF",
		"SELECT 1 SET SHOWPLAN_XML OFF DELETE FROM t",
		"UPDATE t SET a = 1 SET SHOWPLAN_XML OFF DELETE FROM t",
		"UPDATE t SET a = 1 WHERE b = 2 SET SHOWPLAN_XML OFF",
		"SELECT 1\nGO\nDELETE FROM t",
		"EXEC('DELETE FROM t')",
		"EXEC sp_help 't'",
		"BEGIN TRAN",
		"COMMIT",
		"SELECT 1 COMMIT",
		"DECLARE @x int",
		"USE master",
		"CREATE TABLE t (a int)",
		"DROP TABLE t",
		"TRUNCATE TABLE t",
		"SELECT 1; SELECT 2",
		"SELECT 1 --x\rCOMMIT",
		"SELECT 'x",
		"BULK INSERT t FROM 'x'",
		"SELECT * FROM OPENROWSET('a', 'b', 'c')",
		"xp_cmdshell 'dir'",
	} {
		if err := explainTarget(text); err == nil {
			t.Errorf("explainTarget(%q) = nil, want refused", text)
		}
	}
}

func TestCheckTxStatement(t *testing.T) {
	for _, text := range []string{
		"UPDATE t SET a = 1",
		"update tobira_people set name = 'tx' where id = 1",
		"INSERT INTO t VALUES (1)",
		"DELETE FROM t",
		"SELECT * FROM t",
		"WITH c AS (SELECT 1 AS n) UPDATE t SET a = 1",
		"CREATE TABLE t (a int)",
		"DECLARE @x int = 1",
		"SET IDENTITY_INSERT t ON",
		"SAVE TRANSACTION s",
		"SELECT 'COMMIT' AS [rollback]",
		"BEGIN TRY SELECT 1 END TRY BEGIN CATCH SELECT 2 END CATCH",
		"UPDATE t SET a = 1;",
	} {
		if err := checkTxStatement(text); err != nil {
			t.Errorf("checkTxStatement(%q) = %v, want allowed", text, err)
		}
	}
	for _, text := range []string{
		"COMMIT",
		"COMMIT TRANSACTION",
		"UPDATE t SET a = 1 COMMIT",
		"ROLLBACK",
		"ROLLBACK TRAN s",
		"BEGIN TRAN",
		"BEGIN TRANSACTION",
		"BEGIN DISTRIBUTED TRANSACTION",
		"SELECT 1 BEGIN TRAN",
		"EXEC dbo.p",
		"EXECUTE('COMMIT')",
		"dbo.p",
		"sp_who",
		"[p] 1",
		"SET IMPLICIT_TRANSACTIONS ON",
		"SELECT 1\nGO\nCOMMIT",
		"UPDATE t SET a = 1; COMMIT",
		"UPDATE t SET a = 1; SELECT 1",
		"SELECT 1 --x\rCOMMIT",
		"SELECT 'x",
		"",
	} {
		err := checkTxStatement(text)
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("checkTxStatement(%q) = %v, want ErrUnsupported", text, err)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, raw := range []string{
		"age > 30",
		"(a = 1 OR b = 2) AND c IN (SELECT x FROM y)",
		"name LIKE N'%é%'",
		"a = ')' -- comment",
		"[weird)] = 1",
	} {
		if err := checkRawFilter(raw); err != nil {
			t.Errorf("checkRawFilter(%q) = %v, want allowed", raw, err)
		}
	}
	for _, raw := range []string{
		"1 = 1) DELETE FROM t WHERE (1 = 1",
		"a = 1)",
		"(a = 1",
		"a = 1; DELETE FROM t",
		"a = 'x",
		"a = 1 /* x",
		"a = 1 --x\r) OR (1 = 1",
		"a = 0xINSERT",
	} {
		err := checkRawFilter(raw)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", raw, err)
		}
	}
}

func TestUseExec(t *testing.T) {
	for text, want := range map[string]bool{
		"DELETE FROM t":                               true,
		"insert into t values (1)":                    true,
		"INSERT INTO t SELECT * FROM u":               true,
		"UPDATE t SET a = 1":                          true,
		"WITH c AS (SELECT 1 AS n) DELETE FROM t":     true,
		"CREATE TABLE t (a int)":                      true,
		"TRUNCATE TABLE t":                            true,
		"INSERT INTO t OUTPUT INSERTED.id VALUES (1)": false,
		"DELETE FROM t OUTPUT DELETED.*":              false,
		"SELECT 1":                                    false,
		"WITH c AS (SELECT 1 AS n) SELECT * FROM c":   false,
		"EXEC sp_help":                                false,
		"DELETE FROM t; SELECT 1":                     false,
		"WAITFOR DELAY '00:00:01'":                    false,
	} {
		if got := useExec(lex(text).toks); got != want {
			t.Errorf("useExec(%q) = %v, want %v", text, got, want)
		}
	}
	for text, want := range map[string]bool{
		"DELETE FROM t": true, "WITH c AS (SELECT 1 AS n) UPDATE t SET a = 1": true, "MERGE t USING u ON 1 = 1 WHEN MATCHED THEN DELETE;": true,
		"CREATE PROCEDURE p AS SELECT 1": false, "TRUNCATE TABLE t": false, "SELECT 1": false,
	} {
		if got := isDML(lex(text).toks); got != want {
			t.Errorf("isDML(%q) = %v, want %v", text, got, want)
		}
	}
}
