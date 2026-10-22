package sqlcomplete

import (
	"reflect"
	"testing"

	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// Lexical replaces the flag-derived rules; Oracle scans with
// sqlscan.ScanOracle and keeps a PL/SQL unit whole.
func TestSplitLexicalAndOracle(t *testing.T) {
	snow := sqlscan.Generic()
	snow.BackslashEscapes, snow.DollarQuotes = true, true
	gsql := sqlscan.MySQL(false, false)
	gsql.DashCommentSpace = false
	ora := Dialect{Name: "oracle", Oracle: true}
	for _, tc := range []struct {
		name string
		d    Dialect
		text string
		want []string
	}{
		{"lexical backslash", Dialect{Lexical: &snow}, `select 'a\';b'; select 2`, []string{`select 'a\';b'`, "select 2"}},
		{"lexical dollar", Dialect{Lexical: &snow}, "select $$a;b$$; select 2", []string{"select $$a;b$$", "select 2"}},
		{"lexical double-quoted string unterminated", Dialect{Lexical: &gsql}, `select 1; select "a;b`, []string{"select 1", `select "a;b`}},
		{"lexical dash comment without space", Dialect{Lexical: &gsql}, "select 1 --;\n; select 2", []string{"select 1 --;", "select 2"}},
		{"oracle q-quote", ora, "select q'[it's;]' from dual; select 2 from dual", []string{"select q'[it's;]' from dual", "select 2 from dual"}},
		{"oracle plain", ora, "select 1 from dual;\nselect 2 from dual;", []string{"select 1 from dual", "select 2 from dual"}},
		{"oracle anonymous block to the end", ora, "begin\n  x := 1;\n  y := 2;\nend;", []string{"begin\n  x := 1;\n  y := 2;\nend;"}},
		{"oracle block ended by slash", ora, "declare v number;\nbegin v := 1; end;\n/\nselect 1 from dual;",
			[]string{"declare v number;\nbegin v := 1; end;", "select 1 from dual"}},
		{"oracle create procedure", ora, "select 0 from dual;\nCREATE OR REPLACE PROCEDURE p IS BEGIN NULL; END;\n/\n",
			[]string{"select 0 from dual", "CREATE OR REPLACE PROCEDURE p IS BEGIN NULL; END;"}},
		{"oracle create editionable package body", ora, "create or replace editionable package body k as procedure p is begin null; end; end;",
			[]string{"create or replace editionable package body k as procedure p is begin null; end; end;"}},
		{"oracle slash separates sql too", ora, "select 1 from dual\n/\nselect 2 from dual", []string{"select 1 from dual", "select 2 from dual"}},
		{"oracle division is not a separator", ora, "select 4 / 2 from dual; select 1 from dual", []string{"select 4 / 2 from dual", "select 1 from dual"}},
		{"oracle create table is plain sql", ora, "create table t (a int); select 1 from dual", []string{"create table t (a int)", "select 1 from dual"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := texts(tc.text, Split(tc.d, tc.text))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Split(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}
