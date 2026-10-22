package sqlcomplete

import (
	"strings"

	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// Dialect describes the SQL flavour a connection speaks: the keywords to
// offer, how identifiers are quoted and folded, and which lexical extras the
// tokenizer must understand. Dialect values are cheap; build them with
// SQLite, Postgres or MySQL.
type Dialect struct {
	Name string
	// Keywords are offered in keyword contexts, upper case.
	Keywords []string
	// Reserved words must be quoted when used as an identifier, upper case.
	Reserved []string
	// LowerFold reports that unquoted identifiers fold to lower case, so an
	// identifier with an upper-case letter needs quoting (postgres).
	LowerFold bool
	// PGSyntax enables dollar-quoted strings, E'' strings and nested block
	// comments.
	PGSyntax bool
	// MySQLSyntax enables MySQL's lexical rules: "#" line comments, "--"
	// comments only when followed by whitespace or a control character,
	// double-quoted strings (ANSI_QUOTES off), and backslash escapes in
	// strings.
	MySQLSyntax bool
	// CHSyntax enables ClickHouse's lexical rules: "#" line comments,
	// nesting block comments, backslash escapes in strings and in quoted
	// identifiers, and $tag$ … $tag$ heredoc strings.
	CHSyntax bool
	// Backticks and Brackets enable `ident` and [ident] quoting (sqlite).
	Backticks, Brackets bool
	// IdentQuote is the character completions are quoted with when a name
	// needs quoting and the user opened no quote: '"' when zero.
	IdentQuote byte
	// Lexical, when set, replaces the lexical rules the flags above choose
	// (PGSyntax, MySQLSyntax, CHSyntax, Backticks, Brackets) with these, for
	// a dialect whose rules no flag names: GoogleSQL, T-SQL, Snowflake and
	// so on.
	Lexical *sqlscan.Dialect
	// Oracle reads the text with sqlscan.ScanOracle (q'…' strings), and
	// makes Split keep a PL/SQL unit whole: a statement that starts with
	// BEGIN, DECLARE or CREATE [OR REPLACE] [EDITIONABLE|NONEDITIONABLE]
	// FUNCTION, PROCEDURE, PACKAGE, TRIGGER or TYPE runs, semicolons
	// included, to a line holding only "/" or to the end of the text. A
	// line holding only "/" also ends any other statement, as in SQL*Plus.
	// It overrides Lexical.
	Oracle bool
}

const commonKeywords = `SELECT FROM WHERE AND OR NOT NULL IS IN LIKE BETWEEN EXISTS AS ON USING
JOIN INNER LEFT RIGHT FULL OUTER CROSS NATURAL GROUP BY ORDER HAVING LIMIT OFFSET UNION ALL
INTERSECT EXCEPT DISTINCT ASC DESC INSERT INTO VALUES UPDATE SET DELETE CREATE ALTER DROP TABLE
VIEW INDEX UNIQUE PRIMARY KEY FOREIGN REFERENCES DEFAULT CHECK CONSTRAINT CASE WHEN THEN ELSE END
CAST WITH RECURSIVE BEGIN COMMIT ROLLBACK EXPLAIN COUNT SUM AVG MIN MAX COALESCE TRUE FALSE
RETURNING IF ADD COLUMN RENAME TO`

// sqliteReserved is every keyword on https://sqlite.org/lang_keywords.html.
// SQLite accepts many of them as bare identifiers; quoting them is always safe.
const sqliteReserved = `ABORT ACTION ADD AFTER ALL ALTER ALWAYS ANALYZE AND AS ASC ATTACH AUTOINCREMENT BEFORE BEGIN
BETWEEN BY CASCADE CASE CAST CHECK COLLATE COLUMN COMMIT CONFLICT CONSTRAINT CREATE CROSS CURRENT CURRENT_DATE
CURRENT_TIME CURRENT_TIMESTAMP DATABASE DEFAULT DEFERRABLE DEFERRED DELETE DESC DETACH DISTINCT DO DROP EACH ELSE END
ESCAPE EXCEPT EXCLUDE EXCLUSIVE EXISTS EXPLAIN FAIL FILTER FIRST FOLLOWING FOR FOREIGN FROM FULL GENERATED GLOB GROUP
GROUPS HAVING IF IGNORE IMMEDIATE IN INDEX INDEXED INITIALLY INNER INSERT INSTEAD INTERSECT INTO IS ISNULL JOIN KEY
LAST LEFT LIKE LIMIT MATCH MATERIALIZED NATURAL NO NOT NOTHING NOTNULL NULL NULLS OF OFFSET ON OR ORDER OTHERS OUTER
OVER PARTITION PLAN PRAGMA PRECEDING PRIMARY QUERY RAISE RANGE RECURSIVE REFERENCES REGEXP REINDEX RELEASE RENAME
REPLACE RESTRICT RETURNING RIGHT ROLLBACK ROW ROWS SAVEPOINT SELECT SET TABLE TEMP TEMPORARY THEN TIES TO TRANSACTION
TRIGGER UNBOUNDED UNION UNIQUE UPDATE USING VACUUM VALUES VIEW VIRTUAL WHEN WHERE WINDOW WITH WITHOUT`

// postgresReserved is the PostgreSQL 17 "reserved" and "reserved (can be
// function or type)" keywords (appendix C), plus common statement words that
// are harmless to quote.
const postgresReserved = `ALL ANALYSE ANALYZE AND ANY ARRAY AS ASC ASYMMETRIC BOTH CASE CAST CHECK COLLATE COLUMN CONSTRAINT
CREATE CURRENT_CATALOG CURRENT_DATE CURRENT_ROLE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER DEFAULT DEFERRABLE DESC
DISTINCT DO ELSE END EXCEPT FALSE FETCH FOR FOREIGN FROM GRANT GROUP HAVING IN INITIALLY INTERSECT INTO LATERAL
LEADING LIMIT LOCALTIME LOCALTIMESTAMP NOT NULL OFFSET ON ONLY OR ORDER PLACING PRIMARY REFERENCES RETURNING SELECT
SESSION_USER SOME SYMMETRIC SYSTEM_USER TABLE THEN TO TRAILING TRUE UNION UNIQUE USER USING VARIADIC WHEN WHERE
WINDOW WITH
AUTHORIZATION BINARY COLLATION CONCURRENTLY CROSS CURRENT_SCHEMA FREEZE FULL ILIKE INNER IS ISNULL JOIN LEFT LIKE
NATURAL NOTNULL OUTER OVERLAPS RIGHT SIMILAR TABLESAMPLE VERBOSE
BETWEEN EXISTS VALUES INSERT UPDATE DELETE ALTER DROP INDEX BEGIN COMMIT ROLLBACK`

// mysqlReserved is the MySQL 8.4 reserved word list
// (https://dev.mysql.com/doc/refman/8.4/en/keywords.html, the words marked
// R), which must be quoted when used as an identifier.
const mysqlReserved = `ACCESSIBLE ADD ALL ALTER ANALYZE AND AS ASC ASENSITIVE BEFORE BETWEEN BIGINT BINARY BLOB BOTH BY
CALL CASCADE CASE CHANGE CHAR CHARACTER CHECK COLLATE COLUMN CONDITION CONSTRAINT CONTINUE CONVERT CREATE CROSS CUBE
CUME_DIST CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER CURSOR DATABASE DATABASES DAY_HOUR DAY_MICROSECOND
DAY_MINUTE DAY_SECOND DEC DECIMAL DECLARE DEFAULT DELAYED DELETE DENSE_RANK DESC DESCRIBE DETERMINISTIC DISTINCT
DISTINCTROW DIV DOUBLE DROP DUAL EACH ELSE ELSEIF EMPTY ENCLOSED ESCAPED EXCEPT EXISTS EXIT EXPLAIN FALSE FETCH
FIRST_VALUE FLOAT FLOAT4 FLOAT8 FOR FORCE FOREIGN FROM FULLTEXT FUNCTION GENERATED GET GRANT GROUP GROUPING GROUPS
HAVING HIGH_PRIORITY HOUR_MICROSECOND HOUR_MINUTE HOUR_SECOND IF IGNORE IN INDEX INFILE INNER INOUT INSENSITIVE INSERT
INT INT1 INT2 INT3 INT4 INT8 INTEGER INTERSECT INTERVAL INTO IO_AFTER_GTIDS IO_BEFORE_GTIDS IS ITERATE JOIN JSON_TABLE
KEY KEYS KILL LAG LAST_VALUE LATERAL LEAD LEADING LEAVE LEFT LIKE LIMIT LINEAR LINES LOAD LOCALTIME LOCALTIMESTAMP
LOCK LONG LONGBLOB LONGTEXT LOOP LOW_PRIORITY MANUAL MASTER_BIND MASTER_SSL_VERIFY_SERVER_CERT MATCH MAXVALUE
MEDIUMBLOB MEDIUMINT MEDIUMTEXT MIDDLEINT MINUTE_MICROSECOND MINUTE_SECOND MOD MODIFIES NATURAL NOT NO_WRITE_TO_BINLOG
NTH_VALUE NTILE NULL NUMERIC OF ON OPTIMIZE OPTIMIZER_COSTS OPTION OPTIONALLY OR ORDER OUT OUTER OUTFILE OVER
PARALLEL PARTITION PERCENT_RANK PRECISION PRIMARY PROCEDURE PURGE QUALIFY RANGE RANK READ READS READ_WRITE REAL
RECURSIVE REFERENCES REGEXP RELEASE RENAME REPEAT REPLACE REQUIRE RESIGNAL RESTRICT RETURN REVOKE RIGHT RLIKE ROW
ROWS ROW_NUMBER SCHEMA SCHEMAS SECOND_MICROSECOND SELECT SENSITIVE SEPARATOR SET SHOW SIGNAL SMALLINT SPATIAL
SPECIFIC SQL SQLEXCEPTION SQLSTATE SQLWARNING SQL_BIG_RESULT SQL_CALC_FOUND_ROWS SQL_SMALL_RESULT SSL STARTING STORED
STRAIGHT_JOIN SYSTEM TABLE TABLESAMPLE TERMINATED THEN TINYBLOB TINYINT TINYTEXT TO TRAILING TRIGGER TRUE UNDO UNION
UNIQUE UNLOCK UNSIGNED UPDATE USAGE USE USING UTC_DATE UTC_TIME UTC_TIMESTAMP VALUES VARBINARY VARCHAR VARCHARACTER
VARYING VIRTUAL WHEN WHERE WHILE WINDOW WITH WRITE XOR YEAR_MONTH ZEROFILL`

// clickhouseReserved holds the clause and operator keywords that would be
// misread as an identifier in a ClickHouse query. ClickHouse reserves almost
// nothing outright; quoting these is always safe.
const clickhouseReserved = `ALL AND ANTI ANY ARRAY AS ASC ASOF BETWEEN BY CASE CAST CROSS DESC DISTINCT ELSE END
EXCEPT EXISTS FALSE FINAL FORMAT FROM FULL GLOBAL GROUP HAVING ILIKE IN INNER INTERSECT INTERVAL INTO IS JOIN LEFT
LIKE LIMIT NOT NULL OFFSET ON OR ORDER OUTER PREWHERE QUALIFY RIGHT SAMPLE SELECT SEMI SETTINGS THEN TRUE UNION
USING WHEN WHERE WINDOW WITH`

// SQLite returns the sqlite dialect.
func SQLite() Dialect {
	return Dialect{
		Name: "sqlite",
		Keywords: strings.Fields(commonKeywords + ` AUTOINCREMENT PRAGMA VACUUM ATTACH DETACH GLOB REPLACE
IGNORE ABORT FAIL ROWID WITHOUT TRIGGER VIRTUAL TEMP TEMPORARY ANALYZE REINDEX`),
		Reserved:  strings.Fields(sqliteReserved),
		Backticks: true,
		Brackets:  true,
	}
}

// Postgres returns the postgres dialect.
func Postgres() Dialect {
	return Dialect{
		Name: "postgres",
		Keywords: strings.Fields(commonKeywords + ` ILIKE SIMILAR LATERAL ONLY SERIAL BIGSERIAL
TRUNCATE VACUUM ANALYZE SCHEMA SEQUENCE EXTENSION GRANT REVOKE CASCADE RESTRICT CONFLICT DO NOTHING
OVER PARTITION WINDOW FETCH NEXT ROWS FILTER ARRAY INTERVAL TIMESTAMP NOTIFY LISTEN UNLISTEN COPY`),
		Reserved:  strings.Fields(postgresReserved),
		LowerFold: true,
		PGSyntax:  true,
	}
}

// MySQL returns the mysql dialect (MySQL 8, default sql_mode).
func MySQL() Dialect {
	return Dialect{
		Name: "mysql",
		Keywords: strings.Fields(commonKeywords + ` SHOW DESCRIBE USE DATABASE DATABASES TABLES REPLACE IGNORE
DUPLICATE AUTO_INCREMENT ENGINE CHARSET COLLATE TRUNCATE STRAIGHT_JOIN REGEXP RLIKE DIV MOD XOR INTERVAL LOCK UNLOCK
FOR SHARE NOWAIT SKIP LOCKED OVER PARTITION WINDOW DUAL START TRANSACTION`),
		Reserved:    strings.Fields(mysqlReserved),
		MySQLSyntax: true,
		Backticks:   true,
		IdentQuote:  '`',
	}
}

// ClickHouse returns the clickhouse dialect. Identifiers are
// case-sensitive and are quoted with backticks.
func ClickHouse() Dialect {
	return Dialect{
		Name: "clickhouse",
		Keywords: strings.Fields(commonKeywords + ` SHOW DESCRIBE DATABASE DATABASES TABLES DICTIONARIES
PREWHERE FINAL SAMPLE ARRAY GLOBAL ANY ASOF SEMI ANTI SETTINGS FORMAT ENGINE PARTITION TTL TOTALS ROLLUP CUBE
ILIKE INTERVAL OVER WINDOW QUALIFY FILL INTERPOLATE CODEC MATERIALIZED`),
		Reserved:   strings.Fields(clickhouseReserved),
		CHSyntax:   true,
		Backticks:  true,
		IdentQuote: '`',
	}
}

// needsQuote reports whether name must be quoted to survive as an identifier.
func (d Dialect) needsQuote(name string) bool {
	if name == "" {
		return true
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_', c >= 0x80:
		case c >= 'A' && c <= 'Z':
			if d.LowerFold {
				return true
			}
		case (c >= '0' && c <= '9') || c == '$':
			if i == 0 {
				return true
			}
		default:
			return true
		}
	}
	up := strings.ToUpper(name)
	for _, r := range d.Reserved {
		if r == up {
			return true
		}
	}
	return false
}

// quote wraps name in the quote character open (", ` or [), escaping it.
// Under CHSyntax a backslash, which escapes inside quotes, is doubled too.
func (d Dialect) quote(name string, open byte) string {
	if d.CHSyntax && open != '[' {
		name = strings.ReplaceAll(name, `\`, `\\`)
	}
	switch open {
	case '[':
		return "[" + name + "]"
	case '`':
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	default:
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}
