package mysql

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/guard_corpus.txt from the current guards")

// errText is "ok" for nil, else a short digest of the error message, which
// keeps the corpus file small while still pinning the exact message.
func errText(err error) string {
	if err == nil {
		return "ok"
	}
	sum := sha256.Sum256([]byte(err.Error()))
	return hex.EncodeToString(sum[:4])
}

// lexModes are every combination of the sql_mode flags the lexer reads.
var lexModes = []lexMode{{}, {noBackslash: true}, {ansiQuotes: true}, {noBackslash: true, ansiQuotes: true}}

// guardDecisions renders the token stream and every lexical guard decision
// on text, under every lexMode.
func guardDecisions(text string) string {
	var b strings.Builder
	var first string
	for i, m := range lexModes {
		lr := lex(text, m)
		var l strings.Builder
		fmt.Fprintf(&l, "%v,%q", lr.execComment, lr.bad)
		for _, t := range lr.toks {
			fmt.Fprintf(&l, ",%d:%d:%q", t.kind, t.pos, t.text)
		}
		toks := l.String()
		if i == 0 {
			first = toks
		} else if toks == first {
			toks = "same"
		}
		fmt.Fprintf(&b, "[%d] lex=%s ro=%s explain=%s explainBuilt=%s tx=%s raw=%s ", i, toks,
			errText(checkReadOnly(text, m)), errText(explainTarget(text, m)),
			errText(explainTarget(explainText("JSON", text), m)), errText(checkTxStatement(text, m)),
			errText(checkRawFilter(text, m)))
	}
	return b.String()
}

// TestGuardCorpus is the regression test for the move onto
// driverutil/sqlscan. testdata/guard_corpus.txt holds tricky inputs (the
// string literals of the driver tests plus seeded random interleavings of
// quotes, comments, executable comments, backslashes, NUL bytes and
// Unicode), each followed
// by the tokens and decisions the pre-sqlscan guards produced for it under
// every sql_mode. They must stay
// identical.
func TestGuardCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/guard_corpus.txt")
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
		got := "\t" + guardDecisions(text)
		out.WriteString(line + "\n" + got + "\n")
		if *update {
			continue
		}
		if !sc.Scan() || sc.Text() != got {
			t.Errorf("input %s:\n got %s\nwant %s", line, got, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 300 {
		t.Fatalf("corpus has %d inputs, want at least 300", n)
	}
	if *update {
		if err := os.WriteFile("testdata/guard_corpus.txt", []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
