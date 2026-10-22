package tui

import (
	"fmt"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestTokenAt(t *testing.T) {
	tests := []struct {
		name   string
		line   string // "|" marks the cursor
		want   token
		wantOK bool
	}{
		{"empty", "|", token{ok: true}, true},
		{"word", "select us|", token{start: 7, end: 9, prefix: "us", ok: true}, true},
		{"mid word", "sel|ect x", token{start: 0, end: 6, prefix: "sel", ok: true}, true},
		{"after a space", "select |", token{start: 7, end: 7, ok: true}, true},
		{"after a dot", "select p.|", token{start: 9, end: 9, dot: true, qual: "p", ok: true}, true},
		{"after a dot with prefix", "from main.tob|", token{start: 10, end: 13, prefix: "tob", dot: true, qual: "main", ok: true}, true},
		{"quoted qualifier", `select "My T".na|`, token{start: 14, end: 16, prefix: "na", dot: true, qual: "My T", ok: true}, true},
		{"open double quote", `select "Us|`, token{start: 7, end: 10, prefix: "Us", quote: '"', ok: true}, true},
		{"closed double quote", `select "Us|ers" x`, token{start: 7, end: 14, prefix: "Us", quote: '"', ok: true}, true},
		{"backtick", "select `us|`", token{start: 7, end: 11, prefix: "us", quote: '`', ok: true}, true},
		{"bracket", "select [us|", token{start: 7, end: 10, prefix: "us", quote: '[', ok: true}, true},
		{"unicode", "select ñandú|", token{start: 7, end: 14, prefix: "ñandú", ok: true}, true},
		{"combining mark", "select café|", token{start: 7, end: 13, prefix: "café", ok: true}, true},
		{"cjk", "select 名前|", token{start: 7, end: 13, prefix: "名前", ok: true}, true},
		{"string literal", "where a = 'ab|", token{}, false},
		{"after a closed string", "where a = 'x' an|", token{start: 14, end: 16, prefix: "an", ok: true}, true},
		{"line comment", "select 1 -- us|", token{}, false},
		{"block comment", "select /* us|", token{}, false},
		{"after a block comment", "select /* c */ us|", token{start: 15, end: 17, prefix: "us", ok: true}, true},
		{"dollar sign", "select a$b|", token{start: 7, end: 10, prefix: "a$b", ok: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := indexOf(tt.line, "|")
			line := tt.line[:cur] + tt.line[cur+1:]
			got := tokenAt(line, cur, true, false)
			if got.ok != tt.wantOK {
				t.Fatalf("tokenAt(%q, %d).ok = %v, want %v", line, cur, got.ok, tt.wantOK)
			}
			if tt.wantOK && got != tt.want {
				t.Fatalf("tokenAt(%q, %d) = %+v, want %+v", line, cur, got, tt.want)
			}
		})
	}
}

func TestTokenAtWithoutSquareQuotes(t *testing.T) {
	// In postgres and mysql "[" is an array subscript, not a quote.
	got := tokenAt("select arr[us", 13, false, false)
	if want := (token{start: 11, end: 13, prefix: "us", ok: true}); got != want {
		t.Fatalf("tokenAt = %+v, want %+v", got, want)
	}
}

func indexOf(s, sub string) int {
	for i := range len(s) - len(sub) + 1 {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestAutoTrigger(t *testing.T) {
	tests := []struct {
		tok  token
		want bool
	}{
		{token{ok: true}, false},
		{token{ok: true, prefix: "u"}, false},
		{token{ok: true, prefix: "us"}, true},
		{token{ok: true, prefix: "ñа"}, true}, // two runes, more bytes
		{token{ok: true, prefix: "12"}, false},
		{token{ok: true, dot: true}, true},
		{token{ok: false, prefix: "us"}, false},
	}
	for _, tt := range tests {
		if got := tt.tok.autoOpens(); got != tt.want {
			t.Errorf("%+v.autoOpens() = %v, want %v", tt.tok, got, tt.want)
		}
	}
}

func TestMergeCompletions(t *testing.T) {
	comp := []driver.Completion{{Label: "users", Kind: "collection"}, {Label: "user_id", Kind: "field"}}
	local := []driver.Completion{{Label: "users", Kind: "collection", Detail: "main"}, {Label: "username", Kind: "field"}}
	got := mergeCompletions(comp, local)
	if len(got) != 3 || got[0] != comp[0] || got[1] != comp[1] || got[2].Label != "username" {
		t.Fatalf("merge = %+v", got)
	}
	var many []driver.Completion
	for i := range 500 {
		many = append(many, driver.Completion{Label: fmt.Sprintf("n%03d", i)})
	}
	if got := mergeCompletions(many, many); len(got) != maxCompletions {
		t.Fatalf("merge kept %d, want %d", len(got), maxCompletions)
	}
}

func TestSchemaSnapComplete(t *testing.T) {
	people := driver.CollectionRef{Namespace: "main", Name: "people"}
	snap := schemaSnap{
		namespaces:  []string{"main", "Archive"},
		collections: []driver.CollectionRef{people, {Namespace: "Archive", Name: "old_people"}},
		fields:      map[driver.CollectionRef][]string{people: {"id", "name", "Nickname"}},
	}
	labels := func(cs []driver.Completion) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Label+":"+c.Kind)
		}
		return out
	}
	check := func(tok token, want ...string) {
		t.Helper()
		got := labels(snap.complete(tok))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("complete(%+v) = %v, want %v", tok, got, want)
		}
	}
	check(token{ok: true, prefix: "N"}, "name:field", "Nickname:field")
	check(token{ok: true, prefix: "a"}, "Archive:namespace")
	check(token{ok: true, prefix: "OLD"}, "old_people:collection")
	check(token{ok: true, dot: true, qual: "MAIN"}, "people:collection")
	check(token{ok: true, dot: true, qual: "people", prefix: "i"}, "id:field")
	check(token{ok: true, dot: true, qual: "nothing"})
}

func TestQuoteLabel(t *testing.T) {
	tests := []struct {
		label string
		q     byte
		want  string
	}{
		{"My Table", '"', `"My Table"`},
		{`a"b`, '"', `"a""b"`},
		{"a`b", '`', "`a``b`"},
		{"a]b", '[', "[a]]b]"},
	}
	for _, tt := range tests {
		if got := quoteLabel(tt.label, tt.q); got != tt.want {
			t.Errorf("quoteLabel(%q, %q) = %q, want %q", tt.label, tt.q, got, tt.want)
		}
	}
}

// In mysql (without ANSI_QUOTES) "..." is a string, not a name: no
// completion inside it, and a name after it is completed as usual.
func TestTokenAtMySQLDoubleQuoteIsAString(t *testing.T) {
	if got := tokenAt(`where a = "ab`, 13, false, true); got.ok {
		t.Fatalf("inside a mysql double-quoted string: %+v", got)
	}
	got := tokenAt(`where a = "x" an`, 16, false, true)
	if want := (token{start: 14, end: 16, prefix: "an", ok: true}); got != want {
		t.Fatalf("after a mysql string: %+v, want %+v", got, want)
	}
	got = tokenAt(`select "a".na`, 13, false, true)
	if got.qual != "" || !got.dot {
		t.Fatalf("a mysql string is not a qualifier: %+v", got)
	}
	got = tokenAt("select `us", 10, false, true)
	if want := (token{start: 7, end: 10, prefix: "us", quote: '`', ok: true}); got != want {
		t.Fatalf("mysql backtick name: %+v, want %+v", got, want)
	}
}

func TestDoubleQuoteString(t *testing.T) {
	for d, want := range map[string]bool{"mysql": true, "MariaDB": true,
		"sqlite": false, "postgresql": false, "postgres": false, "fake": false, "": false} {
		if got := doubleQuoteString(d); got != want {
			t.Errorf("doubleQuoteString(%q) = %v, want %v", d, got, want)
		}
	}
}

func TestSquareQuotes(t *testing.T) {
	for d, want := range map[string]bool{"sqlite": true, "fake": true, "mssql": true, "": true,
		"postgresql": false, "postgres": false, "mysql": false, "MariaDB": false} {
		if got := squareQuotes(d); got != want {
			t.Errorf("squareQuotes(%q) = %v, want %v", d, got, want)
		}
	}
}
