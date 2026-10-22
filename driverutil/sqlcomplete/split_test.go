package sqlcomplete

import (
	"reflect"
	"testing"
)

// texts returns the text of every span.
func texts(text string, spans []Span) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = text[s.Start:s.End]
	}
	return out
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Dialect
		text string
		want []string
	}{
		{"empty", SQLite(), "", []string{}},
		{"blank and semicolons", SQLite(), "  ;\n ; ", []string{}},
		{"one, no semicolon", SQLite(), "  select 1  ", []string{"select 1"}},
		{"two", SQLite(), "select 1;\nselect 2;", []string{"select 1", "select 2"}},
		{"trailing comment only is dropped", SQLite(), "select 1; -- done", []string{"select 1"}},
		{"leading comment kept", SQLite(), "-- first\nselect 1; select 2", []string{"-- first\nselect 1", "select 2"}},
		{"semicolon in string", SQLite(), "select ';'; select 2", []string{"select ';'", "select 2"}},
		{"semicolon in quoted ident", SQLite(), `select "a;b" from t; select 2`, []string{`select "a;b" from t`, "select 2"}},
		{"semicolon in block comment", SQLite(), "select /* ; */ 1; select 2", []string{"select /* ; */ 1", "select 2"}},
		{"semicolon in line comment", SQLite(), "select 1 -- ;\n; select 2", []string{"select 1 -- ;", "select 2"}},
		{"sqlite backtick and bracket", SQLite(), "select `a;` , [b;] from t;select 2", []string{"select `a;` , [b;] from t", "select 2"}},
		{"pg dollar quote", Postgres(), "do $$ begin; end $$; select 2", []string{"do $$ begin; end $$", "select 2"}},
		{"pg tagged dollar quote", Postgres(), "select $f$;$f$; select 2", []string{"select $f$;$f$", "select 2"}},
		{"pg E string", Postgres(), `select E'\';'; select 2`, []string{`select E'\';'`, "select 2"}},
		{"pg nested comment", Postgres(), "select /* /* ; */ ; */ 1; select 2", []string{"select /* /* ; */ ; */ 1", "select 2"}},
		{"mysql backtick", MySQL(), "select `a;b` from t; select 2", []string{"select `a;b` from t", "select 2"}},
		{"mysql hash comment", MySQL(), "select 1 # ;\n; select 2", []string{"select 1 # ;", "select 2"}},
		{"mysql double-quoted string", MySQL(), `select "a;b"; select 2`, []string{`select "a;b"`, "select 2"}},
		{"mysql backslash escape", MySQL(), `select 'a\';b'; select 2`, []string{`select 'a\';b'`, "select 2"}},
		{"clickhouse heredoc", ClickHouse(), "select $$a;b$$; select 2", []string{"select $$a;b$$", "select 2"}},
		{"clickhouse nested comment", ClickHouse(), "select /* /* ; */ ; */ 1; select 2", []string{"select /* /* ; */ ; */ 1", "select 2"}},
		{"clickhouse identifier escape", ClickHouse(), "select `a\\`;b` from t; select 2", []string{"select `a\\`;b` from t", "select 2"}},
		{"unterminated string runs to the end", SQLite(), "select 1; select 'a;b", []string{"select 1", "select 'a;b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := texts(tc.text, Split(tc.d, tc.text))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Split(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestSplitOffsetsAreHalfOpenAndTrimmed(t *testing.T) {
	text := "  select 1 ;\n\n  select 2\n"
	got := Split(SQLite(), text)
	want := []Span{{2, 10}, {16, 24}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Split = %v, want %v", got, want)
	}
}
