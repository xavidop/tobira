package sqlite

import (
	"bufio"
	"bytes"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/guard_corpus.txt from the current guards")

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// guardDecisions renders every lexical guard decision on text.
func guardDecisions(text string) string {
	return "checkReadOnly=" + strconv.Quote(errText(checkReadOnly(text))) +
		" checkSingle=" + strconv.Quote(errText(checkSingle(text))) +
		" returnsRows=" + strconv.FormatBool(returnsRows(text))
}

// TestGuardCorpus is the regression test for the move onto
// driverutil/sqlscan. testdata/guard_corpus.txt holds tricky inputs (the
// string literals of the driver tests plus seeded random interleavings of
// quotes, comments, brackets, backslashes, BOMs and Unicode), each followed
// by the decisions the pre-sqlscan guards made on it. They must stay
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
