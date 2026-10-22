package cassandra

import (
	"slices"
	"strings"
	"testing"
)

func lexTexts(t *testing.T, text string) []string {
	t.Helper()
	toks, bad := lexCQL(text)
	if bad != "" {
		t.Fatalf("lexCQL(%q): unexpected refusal %q", text, bad)
	}
	out := make([]string, len(toks))
	for i, tk := range toks {
		out[i] = tk.text
	}
	return out
}

func TestLexTokens(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"SELECT * FROM t", []string{"SELECT", "*", "FROM", "t"}},
		{"select 'it''s' , $$a;b$$ ;", []string{"select", "'it''s'", ",", "$$a;b$$", ";"}},
		{`"quoted""id" x`, []string{`"quoted""id"`, "x"}},
		{"-- one\n// two\n/* three; */ SELECT", []string{"SELECT"}},
		{"a=? AND b=:name", []string{"a", "=", "?", "AND", "b", "=", ":name"}},
		{"0xCAFE 1.5e10 42", []string{"0xCAFE", "1.5e10", "42"}},
		{"ks.t", []string{"ks", ".", "t"}},
		{"'héllo' \"Zoë\"", []string{"'héllo'", `"Zoë"`}},
		{"x\t\r\ny", []string{"x", "y"}},
	} {
		if got := lexTexts(t, tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("lexCQL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLexKinds(t *testing.T) {
	toks, bad := lexCQL(`SELECT "Q" 'S' $$D$$ 1 ? :n ( ;`)
	if bad != "" {
		t.Fatal(bad)
	}
	want := []tokKind{tWord, tQuoted, tString, tString, tNumber, tParam, tParam, tPunct, tSemicolon}
	var got []tokKind
	for _, tk := range toks {
		got = append(got, tk.kind)
	}
	if !slices.Equal(got, want) {
		t.Errorf("kinds = %v, want %v", got, want)
	}
	if toks[0].low != "select" {
		t.Errorf("word low = %q, want select", toks[0].low)
	}
}

func TestLexRefusals(t *testing.T) {
	for _, in := range []string{
		"SELECT 'abc",
		"SELECT $$abc",
		"SELECT /* abc",
		`SELECT "abc`,
		"SELECT \x00",
		"SELECT é FROM t", // a non-ASCII byte outside a literal
		"SELECT \x01",     // a control character
		"SELECT $abc",     // a lone '$'
	} {
		if _, bad := lexCQL(in); bad == "" {
			t.Errorf("lexCQL(%q): accepted, want a refusal", in)
		}
	}
}

// A comment that would hide a statement from a naive scanner is a
// comment, and a string that looks like a statement is one token.
func TestLexHidesNothing(t *testing.T) {
	got := strings.Join(lexTexts(t, "SELECT * FROM t WHERE s = 'x; DELETE FROM t' /* ; DROP */"), " ")
	if got != "SELECT * FROM t WHERE s = 'x; DELETE FROM t'" {
		t.Errorf("got %q", got)
	}
}
