package sqlscan

import (
	"fmt"
	"strings"
	"testing"
)

// dumpOracle renders the tokens ScanOracle makes of s as "Kind(text)",
// leaving out Space tokens.
func dumpOracle(s string) string {
	var out []string
	ScanOracle(s, func(t Token) bool {
		if t.Kind != Space {
			out = append(out, fmt.Sprintf("%v(%s)", t.Kind, s[t.Start:t.End]))
		}
		return true
	})
	return strings.Join(out, " ")
}

func TestScanOracle(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"words punct semicolon", "select a.b, c;", "Word(select) Word(a) Punct(.) Word(b) Punct(,) Word(c) Semicolon(;)"},
		{"dollar and hash continue words", "v$session a#b x$", "Word(v$session) Word(a#b) Word(x$)"},
		{"dollar and hash do not start words", "$a #b", "Punct($) Word(a) Punct(#) Word(b)"},
		{"non-ascii letters are word bytes", "café ñ", "Word(café) Word(ñ)"},
		{"line comment stops before newline", "a -- x\nb", "Word(a) Comment(-- x) Word(b)"},
		{"line comment needs no space", "a --x", "Word(a) Comment(--x)"},
		{"line comment runs over a lone CR", "a -- x\rb", "Word(a) Comment(-- x\rb)"},
		{"block comment does not nest", "/* /* */ b */", "Comment(/* /* */) Word(b) Punct(*) Punct(/)"},
		{"hint is a comment", "select /*+ full(t) */ 1", "Word(select) Comment(/*+ full(t) */) Number(1)"},
		{"unterminated block comment", "a /* b", "Word(a) Unterminated(/* b)"},
		{"string with doubled quote", "'it''s' x", "String('it''s') Word(x)"},
		{"no backslash escapes", `'a\' x`, `String('a\') Word(x)`},
		{"unterminated string", "x 'abc", "Word(x) Unterminated('abc)"},
		{"national string splits", "N'a'", "Word(N) String('a')"},
		{"quoted identifier ends at the first quote", `"a""b" c`, `QuotedIdent("a") QuotedIdent("b") Word(c)`},
		{"unterminated identifier", `"abc`, `Unterminated("abc)`},
		{"q quote brackets", "q'[it's]' x", "String(q'[it's]') Word(x)"},
		{"q quote braces parens angles", "Q'{a}' q'(b)' q'<c>'", "String(Q'{a}') String(q'(b)') String(q'<c>')"},
		{"q quote same delimiter", "q'!a'b!' x", "String(q'!a'b!') Word(x)"},
		{"q quote hides a quote and keywords", "q'# '; delete from t; '#'", "String(q'# '; delete from t; '#')"},
		{"q quote needs the delimiter before the quote", "q'[a]b]'", "String(q'[a]b]')"},
		{"nq quote", "nq'[x]' NQ'|y|'", "String(nq'[x]') String(NQ'|y|')"},
		{"q quote space delimiter is unterminated", "q' a ' x", "Unterminated(q' a ' x)"},
		{"q quote quote delimiter is unterminated", "q''a''", "Unterminated(q''a'')"},
		{"unterminated q quote", "q'[abc'", "Unterminated(q'[abc')"},
		{"q quote multibyte delimiter is unterminated", "q'éaé' x", "Unterminated(q'éaé' x)"},
		{"q quote high byte delimiter is unterminated", "q'\xffa\xff' x", "Unterminated(q'\xffa\xff' x)"},
		{"q quote at the end is unterminated", "q'", "Unterminated(q')"},
		{"q word without quote", "q, nq x", "Word(q) Punct(,) Word(nq) Word(x)"},
		{"longer words are not q quotes", "aq'x' qq'y'", "Word(aq) String('x') Word(qq) String('y')"},
		{"numbers", "1 1.5 1. 1e5 1E-3 2.5e+10 1.5f 2d 7F 8D", "Number(1) Number(1.5) Number(1.) Number(1e5) Number(1E-3) Number(2.5e+10) Number(1.5f) Number(2d) Number(7F) Number(8D)"},
		{"number runs on through word bytes", "1delete 1e 1into 1.5x", "Number(1delete) Number(1e) Number(1into) Number(1.5x)"},
		{"number then dot word", "1.x", "Number(1.x)"},
		{"leading dot", ".5", "Punct(.) Number(5)"},
		{"bind variables", ":1 :name", "Punct(:) Number(1) Punct(:) Word(name)"},
		{"dblink", "t@remote", "Word(t) Punct(@) Word(remote)"},
		{"operators", "a||b != c => d", "Word(a) Punct(|) Punct(|) Word(b) Punct(!) Punct(=) Word(c) Punct(=) Punct(>) Word(d)"},
		{"nul is punct", "a\x00b", "Word(a) Punct(\x00) Word(b)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dumpOracle(tc.in); got != tc.want {
				t.Errorf("ScanOracle(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

// Tokens cover the text contiguously, are never empty, and an
// Unterminated token is always the last one.
func TestScanOracleCoversText(t *testing.T) {
	for _, s := range []string{
		"", "select 1 from dual", "q'[a", "'x", "/* a", `"b`, "1e+", "a--b\n'c''d' q'<e>'",
		"\xff\xfe", "nq'", "q'\n'", "-- c",
	} {
		pos := 0
		ScanOracle(s, func(tok Token) bool {
			if tok.Start != pos || tok.End <= tok.Start {
				t.Fatalf("ScanOracle(%q): token %v at %d..%d, want start %d and non-empty", s, tok.Kind, tok.Start, tok.End, pos)
			}
			if pos != 0 && tok.Kind == Unterminated && tok.End != len(s) {
				t.Fatalf("ScanOracle(%q): Unterminated token does not run to the end", s)
			}
			pos = tok.End
			return true
		})
		if pos != len(s) {
			t.Errorf("ScanOracle(%q): tokens end at %d, want %d", s, pos, len(s))
		}
	}
}

func TestScanOracleStopsEarly(t *testing.T) {
	n := 0
	ScanOracle("a b c", func(Token) bool { n++; return n < 2 })
	if n != 2 {
		t.Errorf("ScanOracle called fn %d times after it returned false, want 2", n)
	}
}

// The other dialects are untouched by the Oracle scanner.
func TestScanOracleLeavesDialectsAlone(t *testing.T) {
	if got, want := dump(Generic(), `q'[a]'`), "Word(q) String('[a]')"; got != want {
		t.Errorf("Generic q-quote: got %s, want %s", got, want)
	}
}
