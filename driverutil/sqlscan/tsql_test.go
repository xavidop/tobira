package sqlscan

import "testing"

func TestScanTSQL(t *testing.T) {
	ts := TSQL()
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"brackets double their closer", "[a]]b] c", "QuotedIdent([a]]b]) Word(c)"},
		{"bracket holding a quote", "[a]]'] x --'", "QuotedIdent([a]]']) Word(x) Comment(--')"},
		{"unterminated bracket", "[a]]", "Unterminated([a]])"},
		{"double-quoted identifier", `"a""b" c`, `QuotedIdent("a""b") Word(c)`},
		{"string with doubled quote", "'it''s' x", "String('it''s') Word(x)"},
		{"no backslash escapes", `'a\' x`, `String('a\') Word(x)`},
		{"national string is a word then a string", "N'é' n'x'", "Word(N) String('é') Word(n) String('x')"},
		{"nested comments", "/* a /* b */ c */ d", "Comment(/* a /* b */ c */) Word(d)"},
		{"unterminated nested comment", "/* a /* b */ c", "Unterminated(/* a /* b */ c)"},
		{"dash comment needs no space", "a --x\nb", "Word(a) Comment(--x) Word(b)"},
		{"no hash comments", "#t", "Punct(#) Word(t)"},
		{"variables are punctuation then a word", "@p1 @@rowcount", "Punct(@) Word(p1) Punct(@) Punct(@) Word(rowcount)"},
		{"dollar continues a word", "a$b $action", "Word(a$b) Punct($) Word(action)"},
		{"numbers swallow word bytes and dots", "1 1.5 0x1F 1e5 1.eINSERT 1INTO", "Number(1) Number(1.5) Number(0x1F) Number(1e5) Number(1.eINSERT) Number(1INTO)"},
		{"high bytes are punctuation", "aé", "Word(a) Punct(\xc3) Punct(\xa9)"},
		{"backticks are punctuation", "`a`", "Punct(`) Word(a) Punct(`)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dump(ts, tc.in); got != tc.want {
				t.Errorf("Scan(%q)\n got  %s\n want %s", tc.in, got, tc.want)
			}
		})
	}
}

// BracketDoubling changes nothing for a dialect that leaves it off.
func TestBracketDoublingOffKeepsSQLite(t *testing.T) {
	if got, want := dump(SQLite(), "[a]]b]"), "QuotedIdent([a]) Punct(]) Word(b) Punct(])"; got != want {
		t.Errorf("SQLite brackets changed: got %s, want %s", got, want)
	}
}
