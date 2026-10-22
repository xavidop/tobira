package sqlscan

import (
	"fmt"
	"strings"
	"testing"
)

// dump renders the tokens of s as "Kind(text)" joined by spaces, leaving out
// Space tokens.
func dump(d Dialect, s string) string {
	var out []string
	Scan(d, s, func(t Token) bool {
		if t.Kind != Space {
			out = append(out, fmt.Sprintf("%v(%s)", t.Kind, s[t.Start:t.End]))
		}
		return true
	})
	return strings.Join(out, " ")
}

func TestScan(t *testing.T) {
	pgStd, pgBS := Postgres(true), Postgres(false)
	my, myNoBS, myANSI := MySQL(false, false), MySQL(true, false), MySQL(false, true)
	prefixed := MySQL(false, false)
	prefixed.PrefixedStrings = true
	unicode := Postgres(true)
	unicode.UnicodeEscapes = true
	ch := ClickHouse()
	for _, tc := range []struct {
		name string
		d    Dialect
		in   string
		want string
	}{
		{"words punct semicolon", Generic(), "select a.b, c;", "Word(select) Word(a) Punct(.) Word(b) Punct(,) Word(c) Semicolon(;)"},
		{"line comment stops before newline", Generic(), "a -- x\nb", "Word(a) Comment(-- x) Word(b)"},
		{"line comment to the end", Generic(), "a --x", "Word(a) Comment(--x)"},
		{"block comment", Generic(), "a /* x */ b", "Word(a) Comment(/* x */) Word(b)"},
		{"block comment does not nest", Generic(), "/* /* */ b */", "Comment(/* /* */) Word(b) Punct(*) Punct(/)"},
		{"unterminated block comment", Generic(), "a /* b", "Word(a) Unterminated(/* b)"},
		{"comment closer needs its own star", Generic(), "/*/ x", "Unterminated(/*/ x)"},
		{"string with doubled quote", Generic(), "'it''s' x", "String('it''s') Word(x)"},
		{"no backslash escapes by default", Generic(), `'a\' x`, `String('a\') Word(x)`},
		{"unterminated string", Generic(), "x 'abc", "Word(x) Unterminated('abc)"},
		{"double-quoted identifier", Generic(), `"a""b" c`, `QuotedIdent("a""b") Word(c)`},
		{"backticks off", Generic(), "`a`", "Punct(`) Word(a) Punct(`)"},
		{"generic number", Generic(), "1.5 0xFF 1.x", "Number(1.5) Number(0xFF) Number(1) Punct(.) Word(x)"},
		{"generic unicode word", Generic(), "é$1", "Word(é$1)"},

		{"sqlite brackets and backticks", SQLite(), "[a]]b] `x``y`", "QuotedIdent([a]) Punct(]) Word(b) Punct(]) QuotedIdent(`x``y`)"},
		{"sqlite bom is space", SQLite(), "\xEF\xBB\xBFselect", "Word(select)"},
		{"sqlite ascii words", SQLite(), "a$b é", "Word(a) Punct($) Word(b) Punct(\xc3) Punct(\xa9)"},
		{"sqlite number stops at dot", SQLite(), "9.5abc", "Number(9) Punct(.) Number(5abc)"},

		{"mysql dash needs space", my, "a --x\n--\tb\nc", "Word(a) Punct(-) Punct(-) Word(x) Comment(--\tb) Word(c)"},
		{"mysql dash at end", my, "a --", "Word(a) Comment(--)"},
		{"mysql hash comment", my, "a # x\nb", "Word(a) Comment(# x) Word(b)"},
		{"mysql exec comment", my, "/*!50000 x */ /*M! y */ /* a /*! b */ /* plain */", "ExecComment(/*!50000 x */) ExecComment(/*M! y */) ExecComment(/* a /*! b */) Comment(/* plain */)"},
		{"mysql backslash escapes", my, `'a\'b' "c\"d"`, `String('a\'b') String("c\"d")`},
		{"mysql no backslash escapes", myNoBS, `'a\' x`, `String('a\') Word(x)`},
		{"mysql ansi quotes", myANSI, `"a\" x`, `QuotedIdent("a\") Word(x)`},
		{"mysql dollar word", my, "$a 1.e5.x", "Word($a) Number(1.e5.x)"},
		{"mysql backtick", my, "`a``b`", "QuotedIdent(`a``b`)"},
		{"mysql introducers off by default", my, "_utf8'a' x'1f'", "Word(_utf8) String('a') Word(x) String('1f')"},
		{"prefixed strings", prefixed, `_utf8mb4'a' N'b' X'1f' b'01' n"c" _ 'd'`, `String(_utf8mb4'a') String(N'b') String(X'1f') String(b'01') String(n"c") Word(_) String('d')`},

		{"pg nested comment", pgStd, "/* a /* b */ c */ d", "Comment(/* a /* b */ c */) Word(d)"},
		{"pg unterminated nested comment", pgStd, "/* a /* b */ c", "Unterminated(/* a /* b */ c)"},
		{"pg standard strings", pgStd, `'a\' into`, `String('a\') Word(into)`},
		{"pg backslash strings", pgBS, `'a\' into`, `Unterminated('a\' into)`},
		{"pg E string", pgStd, `E'a\'b' e'c' x`, `String(E'a\'b') String(e'c') Word(x)`},
		{"pg dollar quotes", pgStd, "$$a$$ $t1$b$t$c$t1$ $1 $", "String($$a$$) String($t1$b$t$c$t1$) Punct($) Number(1) Punct($)"},
		{"pg dollar tag holds high bytes", pgStd, "$é$'$é$ x", "String($é$'$é$) Word(x)"},
		{"pg unterminated dollar", pgStd, "$a$ x", "Unterminated($a$ x)"},
		{"pg digits only", pgStd, "1into 1.5", "Number(1) Word(into) Number(1) Punct(.) Number(5)"},
		{"pg word with dollar", pgStd, "a$b$ c", "Word(a$b$) Word(c)"},
		{"pg unicode escapes off", pgStd, `U&"a"`, `Word(U) Punct(&) QuotedIdent("a")`},
		{"clickhouse nested comment", ch, "/* a /* b */ c */ d", "Comment(/* a /* b */ c */) Word(d)"},
		{"clickhouse comments", ch, "a -- x\n# y\n#!z\n--w", "Word(a) Comment(-- x) Comment(# y) Comment(#!z) Comment(--w)"},
		{"clickhouse backslash strings", ch, `'a\'b' 'c''d' x`, `String('a\'b') String('c''d') Word(x)`},
		{"clickhouse backslash identifiers", ch, "\"a\\\"b\" `c\\`d` \"e\"\"f\" x", "QuotedIdent(\"a\\\"b\") QuotedIdent(`c\\`d`) QuotedIdent(\"e\"\"f\") Word(x)"},
		{"clickhouse unterminated identifier escape", ch, `"a\" x`, `Unterminated("a\" x)`},
		{"clickhouse heredoc", ch, "$$a'b$$ $tag$x$tag$ y", "String($$a'b$$) String($tag$x$tag$) Word(y)"},
		{"clickhouse words", ch, "a$b é 1_000 0x1F 1.5e3", "Word(a$b) Word(é) Number(1_000) Number(0x1F) Number(1.5e3)"},
		{"unicode escapes", unicode, `U&"a\0061" u&'x\' UESCAPE '!'`, `QuotedIdent(U&"a\0061") String(u&'x\') Word(UESCAPE) String('!')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dump(tc.d, tc.in); got != tc.want {
				t.Errorf("Scan(%q)\n got %s\nwant %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestDollarTagASCII(t *testing.T) {
	d := Postgres(true)
	d.DollarTagHighBit = false
	if got, want := dump(d, "$é$x$é$"), "Punct($) Word(é$x$é$)"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestScanStops(t *testing.T) {
	n := 0
	Scan(Generic(), "a b c d", func(Token) bool {
		n++
		return n < 2
	})
	if n != 2 {
		t.Fatalf("callback ran %d times after returning false, want 2", n)
	}
}

func TestSpaceRuns(t *testing.T) {
	var toks []Token
	Scan(Generic(), " \t\n\r\f\vx", func(tk Token) bool { toks = append(toks, tk); return true })
	if len(toks) != 2 || toks[0] != (Token{Space, 0, 6}) {
		t.Fatalf("tokens = %v, want one Space run then the word", toks)
	}
}

func dialects() map[string]Dialect {
	// The pg guards' second reading: dollar quotes with ASCII-only tags.
	pgASCII := Postgres(true)
	pgASCII.DollarTagHighBit = false
	all := Dialect{
		DashCommentSpace: true, HashComments: true, NestedComments: true, ExecComments: true,
		BackslashEscapes: true, DoubleQuoteStrings: true, EStrings: true, DollarQuotes: true,
		DollarTagHighBit: true, PrefixedStrings: true, UnicodeEscapes: true, Backticks: true,
		Brackets: true, IdentDollar: true, IdentDollarStart: true, IdentHighBit: true,
		NumberIdentChars: true, NumberDot: DotAlways, BOMSpace: true, IdentBackslashEscapes: true,
	}
	return map[string]Dialect{
		"zero": {}, "generic": Generic(), "sqlite": SQLite(), "all": all,
		"mysql": MySQL(false, false), "mysql-nobs-ansi": MySQL(true, true),
		"pg-std": Postgres(true), "pg-bs": Postgres(false), "pg-ascii": pgASCII,
		"clickhouse": ClickHouse(),
	}
}

func FuzzScan(f *testing.F) {
	for _, s := range []string{
		"", "select 1", "a -- b\nc", "/* /* */ */", "'a''b'", `E'\''`, "$a$ $a$", "$$", "`a`", "[a]",
		"/*! x */", "_utf8'a'", `U&"a" UESCAPE '!'`, "--\x7f", "#x", "\xEF\xBB\xBF;", "1.e5.x", "\x00\xff",
	} {
		f.Add(s)
	}
	ds := dialects()
	f.Fuzz(func(t *testing.T, s string) {
		for name, d := range ds {
			pos, last := 0, Kind(255)
			Scan(d, s, func(tk Token) bool {
				if tk.Start != pos || tk.End <= tk.Start || tk.End > len(s) {
					t.Fatalf("%s: token %v after offset %d in %q: not contiguous or empty", name, tk, pos, s)
				}
				if last == Unterminated {
					t.Fatalf("%s: token %v follows an Unterminated token in %q", name, tk, s)
				}
				if tk.Kind == Unterminated && tk.End != len(s) {
					t.Fatalf("%s: Unterminated %v stops before the end of %q", name, tk, s)
				}
				pos, last = tk.End, tk.Kind
				return true
			})
			if pos != len(s) {
				t.Fatalf("%s: tokens cover [0,%d) of %d bytes in %q", name, pos, len(s), s)
			}
		}
	})
}
