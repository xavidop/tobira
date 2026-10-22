//go:build !race

package editor

import (
	"testing"
	"time"
)

// maxFrame is the budget for one frame of a 5k-line buffer. The race
// detector skews timing, so this file is excluded under -race.
const maxFrame = 4 * time.Millisecond

func TestPerfEditorView5k(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	res := testing.Benchmark(BenchmarkEditorView)
	per := time.Duration(res.NsPerOp())
	t.Logf("EditorView 5k lines: %v/op (budget %v)", per, maxFrame)
	if per > maxFrame {
		t.Fatalf("frame took %v, want <= %v", per, maxFrame)
	}
}
