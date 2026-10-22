package export

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// During a rewrite the progress byte count never goes back: it holds at
// the first pass's total until the copy passes it, and ends at the size of
// the file written.
func TestReplaceProgressMonotonic(t *testing.T) {
	dir := t.TempDir()
	f, err := createTemp(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.Repeat([]byte("a"), 1000)
	pr := &progress{}
	out := &tempOut{dir: dir, f: f, cw: &countWriter{w: f, p: pr}}
	if _, err := out.cw.Write(first); err != nil {
		t.Fatal(err)
	}
	if pr.bytes != 1000 {
		t.Fatalf("first pass bytes = %d", pr.bytes)
	}
	var seen []int64
	err = out.replace(pr, func(w io.Writer) error {
		for range 12 {
			if _, err := w.Write(bytes.Repeat([]byte("b"), 100)); err != nil {
				return err
			}
			seen = append(seen, pr.bytes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	prev := int64(1000)
	for i, b := range seen {
		if b < prev {
			t.Fatalf("progress went back at write %d: %d after %d (all %v)", i, b, prev, seen)
		}
		prev = b
	}
	if pr.bytes != 1200 || out.cw.n != 1200 {
		t.Fatalf("final bytes = %d, file count %d, want 1200", pr.bytes, out.cw.n)
	}
	out.discard()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("left: %v", ents)
	}
}
