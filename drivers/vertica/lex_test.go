package vertica

import (
	"errors"
	"slices"
	"testing"
)

func kinds(toks []token) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.kind.String() + ":" + t.text
	}
	return out
}

func TestLexTokens(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"SELECT a, b FROM t;", []string{"word:SELECT", "word:a", "punct:,", "word:b", "word:FROM", "word:t", "punct:;"}},
		{`select "a""b" from "T"`, []string{"word:select", `quoted:"a""b"`, "word:from", `quoted:"T"`}},
		{"select 'it''s'", []string{"word:select", "string:'it''s'"}},
		{`select E'a\'b', e'\\'`, []string{"word:select", `string:E'a\'b'`, "punct:,", `string:e'\\'`}},
		{`select U&'d\0061t'`, []string{"word:select", "word:U", "punct:&", `string:'d\0061t'`}},
		{"select $$a;b$$, $tag$ x $ y $tag$", []string{"word:select", "string:$$a;b$$", "punct:,", "string:$tag$ x $ y $tag$"}},
		{"select 1 -- c\nfrom t", []string{"word:select", "number:1", "word:from", "word:t"}},
		{"select /* c */ 1", []string{"word:select", "number:1"}},
		{"select 1.5e3, .5, 2e-3, 0x1F, 0XfF", []string{"word:select", "number:1.5e3", "punct:,", "number:.5", "punct:,", "number:2e-3", "punct:,", "number:0x1F", "punct:,", "number:0XfF"}},
		{"select a::int", []string{"word:select", "word:a", "punct::", "punct::", "word:int"}},
		{"select ?, :p", []string{"word:select", "punct:?", "punct:,", "punct::", "word:p"}},
		// A byte >= 0x80 outside quotes splits words.
		{"select a b", []string{"word:select", "word:a", "punct:\xc2", "punct:\xa0", "word:b"}},
		// CRLF ends a line comment.
		{"select 1 -- c\r\nfrom t", []string{"word:select", "number:1", "word:from", "word:t"}},
	}
	for _, tt := range tests {
		toks, err := lex(tt.in)
		if err != nil {
			t.Errorf("lex(%q): %v", tt.in, err)
			continue
		}
		if got := kinds(toks); !slices.Equal(got, tt.want) {
			t.Errorf("lex(%q) =\n  %q\nwant\n  %q", tt.in, got, tt.want)
		}
	}
}

// The lexer fails closed: text it cannot read the way the server does is
// an error, never a guess.
func TestLexRefuses(t *testing.T) {
	tests := map[string]error{
		"select 'open":                errUnterminated,
		`select "open`:                errUnterminated,
		"select /* open":              errUnterminated,
		"select $$ open":              errUnterminated,
		"select $t$ open $u$":         errUnterminated,
		`select E'open\'`:             errUnterminated,
		"select 1 /* a /* b */ */":    errNested, // Vertica nests block comments
		"select 1\x00":                errNUL,
		"select 'a\xff'":              errUTF8,
		"select \xff":                 errUTF8,
		"select 1 -- c\rdelete":       errLoneCR, // a lone CR: does it end the comment?
		"select 1delete":              errGlued,
		"select 0x1insert into t":     errGlued, // hex 0x1, then INSERT
		"select 1e5e":                 errGlued,
		"select 1.5x":                 errGlued,
		"select 0xg":                  errGlued, // 0 glued to xg
		"select 1e":                   errGlued,
		"select 2.e":                  errGlued,
		"select U&'x' UESCAPE '\xff'": errUTF8,
	}
	for in, want := range tests {
		_, err := lex(in)
		if !errors.Is(err, want) {
			t.Errorf("lex(%q) = %v, want %v", in, err, want)
		}
	}
}

func TestNumberEnd(t *testing.T) {
	for in, want := range map[string]int{
		"1": 1, "12 ": 2, "1.5": 3, ".5": 2, "1.": 2, "1e5": 3, "1E+5": 4, "1e-5x": 4,
		"0x1F": 4, "0xfF ": 4, "0x": 1, "1e": 1, "1e+": 1, "007": 3,
	} {
		if got := numberEnd(in, 0); got != want {
			t.Errorf("numberEnd(%q) = %d, want %d", in, got, want)
		}
	}
}
