package postgres

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestExportQueryAccepts(t *testing.T) {
	for text, want := range map[string]string{
		"select 1":                             "select 1",
		"  SELECT * FROM t;  ":                 "  SELECT * FROM t",
		"select 1;;":                           "select 1",
		"values (1), (2)":                      "values (1), (2)",
		"table t":                              "table t",
		"(select 1) union (select 2)":          "(select 1) union (select 2)",
		"with x as (select 1) select * from x": "with x as (select 1) select * from x",
		"select ';' , ')' , \"a)\" , $$)$$":    "select ';' , ')' , \"a)\" , $$)$$",
		"select E'\\')' ":                      "select E'\\')' ",
		"select 1 /* ) */ -- );\n":             "select 1 /* ) */ -- );\n",
		"/* lead */ select 1 -- tail":          "/* lead */ select 1 -- tail",
		"select /* a /* nested ) */ ) */ 1":    "select /* a /* nested ) */ ) */ 1",
	} {
		got, err := exportQuery(text)
		if err != nil {
			t.Errorf("exportQuery(%q): %v", text, err)
			continue
		}
		if got != want {
			t.Errorf("exportQuery(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestExportQueryRefuses(t *testing.T) {
	for _, text := range []string{
		"",
		"   ",
		"delete from t",
		"insert into t values (1)",
		"update t set a = 1",
		"create table x (a int)",
		"copy t to stdout",
		"explain select 1",
		"select 1; select 2",
		"select 1; delete from t",
		"; select 1",
		// parenthesis and comment tricks
		"select 1) TO PROGRAM 'x' --",
		"select 1) to program 'touch /tmp/x'; --",
		"select 1) TO STDOUT WITH (FORMAT binary) --",
		"select 1 ) --",
		")",
		"select (1",
		"select 1 /* unterminated",
		"select 1 /* a /* b */",
		"select 'open",
		"select \"open",
		"select $$open",
		"select $a$ x $$",
		"select E'bs\\'",
		"select (1; select 2)",
		"select 1)) TO PROGRAM 'x'; (select (1",
		"select 1) to program (x", // balanced overall, but closes our parenthesis
	} {
		_, err := exportQuery(text)
		if err == nil {
			t.Errorf("exportQuery(%q) accepted", text)
			continue
		}
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("exportQuery(%q) = %v, want ErrUnsupported", text, err)
		}
	}
}

// Export runs inside BEGIN READ ONLY, which does not stop server-side file
// access, so it refuses what the read-only query path refuses before
// building the COPY, whatever the connection's mode. No server is needed:
// the refusal comes before any statement.
func TestExportRefusesServerFileAccess(t *testing.T) {
	for _, text := range []string{
		"select lo_export(16384, '/tmp/x')",
		`select "pg_catalog"."lo_export"(16384, '/tmp/x')`,
		`select u&"lo!005fexport" uescape '!' (16384, '/tmp/x')`,
		"select $é$'$é$, lo_export(16384, '/tmp/x') --'",
		"with x as (select lo_import('/etc/passwd')) select * from x",
		"select set_config('standard_conforming_strings', 'off', true)",
	} {
		err := (&conn{}).Export(context.Background(), driver.Query{Text: text}, "csv", io.Discard)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Export(%q) = %v, want ErrReadOnly", text, err)
		}
	}
}

// When the two dollar-tag readings accept different statements, the
// refusal says so instead of blaming a second statement.
func TestExportQueryReadingsDisagree(t *testing.T) {
	// With high-bit tags "$é$ -- $é$" is a string and ";" ends the
	// statement; with ASCII tags "--" starts a comment that swallows it.
	_, err := exportQuery("select $é$ -- $é$;")
	if err == nil || !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("err = %v, want an ErrUnsupported refusal", err)
	}
	if want := "the statement reads differently under PostgreSQL's string modes; simplify the quoting"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %q, want %q", err, want)
	}
}
