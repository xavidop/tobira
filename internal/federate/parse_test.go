package federate

import (
	"errors"
	"strings"
	"testing"
)

func TestParseReadsSourcesAndFinal(t *testing.T) {
	text := strings.Join([]string{
		`-- @source orders = local-pg/public: select id, user_id, total from orders`,
		`--   where created_at > now() - interval '7 days'`,
		`-- @source issues = gh: {"collection":"issues","repo":"acme/app","state":"open"}`,
		``,
		`select o.user_id, count(*) as open_issues`,
		`from orders o join issues i on i.author_id = o.user_id -- a comment inside the final statement`,
		`group by o.user_id`,
	}, "\n")
	st, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sources) != 2 {
		t.Fatalf("sources: got %d, want 2", len(st.Sources))
	}
	o := st.Sources[0]
	if o.Alias != "orders" || o.Conn != "local-pg" || o.Namespace != "public" || o.Line != 1 {
		t.Errorf("first source: %+v", o)
	}
	wantQuery := "select id, user_id, total from orders\n  where created_at > now() - interval '7 days'"
	if o.Query != wantQuery {
		t.Errorf("first query:\n%q\nwant\n%q", o.Query, wantQuery)
	}
	i := st.Sources[1]
	if i.Alias != "issues" || i.Conn != "gh" || i.Namespace != "" || i.Line != 3 {
		t.Errorf("second source: %+v", i)
	}
	if i.Query != `{"collection":"issues","repo":"acme/app","state":"open"}` {
		t.Errorf("second query: %q", i.Query)
	}
	if !strings.HasPrefix(st.Final, "select o.user_id") || !strings.HasSuffix(st.Final, "group by o.user_id") {
		t.Errorf("final: %q", st.Final)
	}
	if !strings.Contains(st.Final, "-- a comment inside the final statement") {
		t.Errorf("final lost its own comment: %q", st.Final)
	}
	if st.Sink != nil {
		t.Errorf("sink: got %+v, want none", st.Sink)
	}
}

func TestParseQuotedNamesAndSink(t *testing.T) {
	text := strings.Join([]string{
		`-- @source a = "prod db"/"my schema": select 1 as n`,
		`-- @sink "staging mysql"/app: user_activity`,
		`select n from a`,
	}, "\n")
	st, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Sources[0]; got.Conn != "prod db" || got.Namespace != "my schema" || got.Query != "select 1 as n" {
		t.Errorf("source: %+v", got)
	}
	if st.Sink == nil || st.Sink.Conn != "staging mysql" || st.Sink.Namespace != "app" || st.Sink.Collection != "user_activity" || st.Sink.Line != 2 {
		t.Errorf("sink: %+v", st.Sink)
	}
}

func TestParseKeepsWindowsLineEndings(t *testing.T) {
	st, err := Parse("-- @source a = c: select 1\r\nselect * from a\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if st.Sources[0].Query != "select 1" || st.Final != "select * from a" {
		t.Errorf("%+v", st)
	}
}

func TestParseDirectiveSpacing(t *testing.T) {
	for _, text := range []string{
		"--@source a = c: select 1\nselect * from a",
		"  --  @source   a   =   c :  select 1\nselect * from a",
		"-- @source a=c:select 1\nselect * from a",
	} {
		st, err := Parse(text)
		if err != nil {
			t.Errorf("%q: %v", text, err)
			continue
		}
		if s := st.Sources[0]; s.Alias != "a" || s.Conn != "c" || s.Query != "select 1" {
			t.Errorf("%q: %+v", text, s)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, text string
		line       int
		msg        string
	}{
		{"no source", "select 1", 1, "no @source"},
		{"no final", "-- @source a = c: select 1\n-- just a comment", 2, "no final statement"},
		{"bad alias", "-- @source 1a = c: select 1\nselect 1", 1, "must be an identifier"},
		{"dashed alias", "-- @source my-alias = c: select 1\nselect 1", 1, "must be an identifier"},
		{"missing equals", "-- @source a c: select 1\nselect 1", 1, "expected @source"},
		{"missing colon", "-- @source a = c select 1\nselect 1", 1, `expected ":"`},
		{"empty connection", "-- @source a = : select 1\nselect 1", 1, "connection name is empty"},
		{"unclosed quote", `-- @source a = "c: select 1` + "\nselect 1", 1, "closing quote"},
		{"empty query", "-- @source a = c:\nselect 1", 1, "has no query"},
		{"duplicate alias", "-- @source a = c: select 1\n-- @source a = d: select 2\nselect 1", 2, "already defined on line 1"},
		{"two sinks", "-- @source a = c: select 1\n-- @sink d: t\n-- @sink e: u\nselect 1", 3, "one sink"},
		{"sink without collection", "-- @source a = c: select 1\n-- @sink d:\nselect 1", 2, "no collection"},
		{"unknown directive", "-- @source a = c: select 1\n-- @join b\nselect 1", 2, "unknown directive @join"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.text)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("got %v, want a ParseError", err)
			}
			if pe.Line != tc.line || !strings.Contains(pe.Msg, tc.msg) {
				t.Errorf("got %q, want line %d containing %q", pe, tc.line, tc.msg)
			}
		})
	}
}

func TestIsFederated(t *testing.T) {
	if !IsFederated("-- @source a = c: select 1\nselect * from a") {
		t.Error("a buffer with a source directive is federated")
	}
	for _, text := range []string{
		"select 1",
		"-- a comment mentioning @source\nselect 1",
		"-- @sink c: t\nselect 1", // a sink alone is not federated; Parse reports the missing source
		"select '-- @source a = c: x'",
	} {
		if IsFederated(text) {
			t.Errorf("%q is not federated", text)
		}
	}
}
