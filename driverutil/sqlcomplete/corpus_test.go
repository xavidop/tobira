package sqlcomplete

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/lex_corpus.txt from the current tokenizer")

// tokenDump renders the tokenizer's view of text under d: kind, offsets,
// value and open flag of every token.
func tokenDump(text string, d Dialect) string {
	var b strings.Builder
	for _, t := range tokenize(text, d) {
		fmt.Fprintf(&b, "%d:%d-%d", t.kind, t.start, t.end)
		if t.text != "" {
			b.WriteString(":" + strconv.Quote(t.text))
		}
		if t.open {
			b.WriteString("!")
		}
		b.WriteString(" ")
	}
	return b.String()
}

// TestLexCorpus is the regression test for the move onto driverutil/sqlscan.
// testdata/lex_corpus.txt holds tricky inputs (the string literals of the
// driver and completion tests plus seeded random interleavings of quotes,
// comments, dollar tags, backslashes and Unicode), each followed by the
// tokens the pre-sqlscan tokenizer produced for every dialect. The tokens
// must stay identical.
func TestLexCorpus(t *testing.T) {
	dialects := []struct {
		name string
		d    Dialect
	}{{"generic", Dialect{}}, {"sqlite", SQLite()}, {"postgres", Postgres()}, {"mysql", MySQL()}}
	data, err := os.ReadFile("testdata/lex_corpus.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\t") {
			continue // a recorded result
		}
		text, err := strconv.Unquote(line)
		if err != nil {
			t.Fatalf("bad corpus line %q: %v", line, err)
		}
		n++
		out.WriteString(line + "\n")
		for _, dl := range dialects {
			got := "\t" + dl.name + " " + tokenDump(text, dl.d)
			out.WriteString(got + "\n")
			if *update {
				continue
			}
			if !sc.Scan() || sc.Text() != got {
				t.Fatalf("input %s, dialect %s:\n got %q\nwant %q", line, dl.name, got, sc.Text())
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 300 {
		t.Fatalf("corpus has %d inputs, want at least 300", n)
	}
	if *update {
		if err := os.WriteFile("testdata/lex_corpus.txt", []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
