package sqlcomplete

import (
	"strings"
	"testing"
)

func TestNeedsQuoteReservedWords(t *testing.T) {
	// PostgreSQL 17 appendix C: "reserved" and "reserved (can be function or
	// type)" keywords. Every one must be quoted when used as an identifier.
	pgReserved := `ALL ANALYSE ANALYZE AND ANY ARRAY AS ASC ASYMMETRIC BOTH CASE CAST CHECK COLLATE COLUMN CONSTRAINT
CREATE CURRENT_CATALOG CURRENT_DATE CURRENT_ROLE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER DEFAULT DEFERRABLE DESC
DISTINCT DO ELSE END EXCEPT FALSE FETCH FOR FOREIGN FROM GRANT GROUP HAVING IN INITIALLY INTERSECT INTO LATERAL
LEADING LIMIT LOCALTIME LOCALTIMESTAMP NOT NULL OFFSET ON ONLY OR ORDER PLACING PRIMARY REFERENCES RETURNING SELECT
SESSION_USER SOME SYMMETRIC SYSTEM_USER TABLE THEN TO TRAILING TRUE UNION UNIQUE USER USING VARIADIC WHEN WHERE
WINDOW WITH
AUTHORIZATION BINARY COLLATION CONCURRENTLY CROSS CURRENT_SCHEMA FREEZE FULL ILIKE INNER IS ISNULL JOIN LEFT LIKE
NATURAL NOTNULL OUTER OVERLAPS RIGHT SIMILAR TABLESAMPLE VERBOSE`
	// https://sqlite.org/lang_keywords.html
	sqliteKeywords := `ABORT ACTION ADD AFTER ALL ALTER ALWAYS ANALYZE AND AS ASC ATTACH AUTOINCREMENT BEFORE BEGIN
BETWEEN BY CASCADE CASE CAST CHECK COLLATE COLUMN COMMIT CONFLICT CONSTRAINT CREATE CROSS CURRENT CURRENT_DATE
CURRENT_TIME CURRENT_TIMESTAMP DATABASE DEFAULT DEFERRABLE DEFERRED DELETE DESC DETACH DISTINCT DO DROP EACH ELSE END
ESCAPE EXCEPT EXCLUDE EXCLUSIVE EXISTS EXPLAIN FAIL FILTER FIRST FOLLOWING FOR FOREIGN FROM FULL GENERATED GLOB GROUP
GROUPS HAVING IF IGNORE IMMEDIATE IN INDEX INDEXED INITIALLY INNER INSERT INSTEAD INTERSECT INTO IS ISNULL JOIN KEY
LAST LEFT LIKE LIMIT MATCH MATERIALIZED NATURAL NO NOT NOTHING NOTNULL NULL NULLS OF OFFSET ON OR ORDER OTHERS OUTER
OVER PARTITION PLAN PRAGMA PRECEDING PRIMARY QUERY RAISE RANGE RECURSIVE REFERENCES REGEXP REINDEX RELEASE RENAME
REPLACE RESTRICT RETURNING RIGHT ROLLBACK ROW ROWS SAVEPOINT SELECT SET TABLE TEMP TEMPORARY THEN TIES TO TRANSACTION
TRIGGER UNBOUNDED UNION UNIQUE UPDATE USING VACUUM VALUES VIEW VIRTUAL WHEN WHERE WINDOW WITH WITHOUT`
	for _, tc := range []struct {
		d     Dialect
		words string
	}{{Postgres(), pgReserved}, {SQLite(), sqliteKeywords}} {
		for _, w := range strings.Fields(tc.words) {
			for _, form := range []string{w, strings.ToLower(w)} {
				if !tc.d.needsQuote(form) {
					t.Errorf("%s: %q must be quoted", tc.d.Name, form)
				}
			}
		}
	}
}

func TestNeedsQuote(t *testing.T) {
	for _, tc := range []struct {
		d    Dialect
		name string
		want bool
	}{
		{Postgres(), "people", false},
		{Postgres(), "tobira_people", false},
		{Postgres(), "name", false},
		{Postgres(), "People", true},
		{Postgres(), "with space", true},
		{Postgres(), "1abc", true},
		{Postgres(), "", true},
		{Postgres(), "a$b", false},
		{Postgres(), "$a", true},
		{Postgres(), "naïve", false},
		{SQLite(), "People", false},
		{SQLite(), "with-dash", true},
		{SQLite(), "plain", false},
	} {
		if got := tc.d.needsQuote(tc.name); got != tc.want {
			t.Errorf("%s needsQuote(%q) = %v, want %v", tc.d.Name, tc.name, got, tc.want)
		}
	}
}

func TestMySQLNeedsQuote(t *testing.T) {
	d := MySQL()
	// A sample of MySQL 8.4 reserved words (https://dev.mysql.com/doc/refman/8.4/en/keywords.html),
	// including the window-function ones added in 8.0.
	for _, w := range strings.Fields(`SELECT FROM ORDER GROUP KEY KEYS RANGE RANK ROW_NUMBER WINDOW GROUPS LEAD LAG
INTERVAL MATCH READ WRITE CONDITION DUAL CHANGE DATABASE SCHEMA SEPARATOR SQL_BIG_RESULT USAGE LINES OPTION
MAXVALUE LOW_PRIORITY HIGH_PRIORITY STRAIGHT_JOIN FULLTEXT SPATIAL ZEROFILL UTC_TIMESTAMP LATERAL RECURSIVE`) {
		for _, form := range []string{w, strings.ToLower(w)} {
			if !d.needsQuote(form) {
				t.Errorf("mysql: %q must be quoted", form)
			}
		}
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"people", false},
		{"People", false}, // identifiers do not fold to lower case
		{"tobira_people", false},
		{"with space", true},
		{"with-dash", true},
		{"1abc", true},
		{"a$b", false},
		{"", true},
		{"status", false}, // a non-reserved keyword is a valid identifier
	} {
		if got := d.needsQuote(tc.name); got != tc.want {
			t.Errorf("mysql needsQuote(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClickHouseNeedsQuote(t *testing.T) {
	d := ClickHouse()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"people", false},
		{"People", false}, // identifiers are case-sensitive and never fold
		{"a$b", false},
		{"with space", true},
		{"1abc", true},
		{"", true},
		{"select", true},
		{"FROM", true},
		{"status", false},
	} {
		if got := d.needsQuote(tc.name); got != tc.want {
			t.Errorf("clickhouse needsQuote(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
