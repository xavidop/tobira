package postgres

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

// guardDecisions renders the word scans and every lexical guard decision on
// text.
func guardDecisions(text string) string {
	var b strings.Builder
	var first string
	for i, words := range append(lexings(text, false), lexings(text, true)...) {
		var l strings.Builder
		for _, w := range words {
			fmt.Fprintf(&l, "%q:%d,", w.text, w.depth)
		}
		ws := l.String()
		if i == 0 {
			first = ws
		} else if ws == first {
			ws = "same"
		}
		fmt.Fprintf(&b, "words%d=%s ", i, ws)
	}
	g, gok := changesStringGUC(text)
	f, fok := serverFileFunction(text)
	exp, err := exportQuery(text)
	fmt.Fprintf(&b, "kw=%q prepTx=%v chain=%v copy=%v do=%v guc=%q,%v file=%q,%v into=%v txControl=%v rbTo=%v export=%q,%s",
		leadingKeyword(text), isPrepareTransaction(text), endsWithChain(text), isCopy(text), isDO(text),
		g, gok, f, fok, hasTopLevelInto(text), isTxControl(text), rollsBackToSavepoint(text), exp, errText(err))
	return b.String()
}

// TestGuardCorpus is the regression test for the move onto
// driverutil/sqlscan. testdata/guard_corpus.txt holds tricky inputs (the
// string literals of the driver tests plus seeded random interleavings of
// quotes, comments, dollar tags, backslashes, U&"…" UESCAPE and
// Unicode), each followed
// by the words (every guard lexing) and decisions the guards produce for
// it. It was recorded from the pre-sqlscan guards, then regenerated once when
// dollar-quote tags were widened to bytes >= 0x80. That regeneration changed
// no decision for the inputs already there, only the recorded word dumps;
// the six inputs it added are new refusals.
// Words and decisions must stay identical.
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
