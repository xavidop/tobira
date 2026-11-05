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
	// The fastest of a few runs: a loaded machine (the whole suite running
	// in parallel) slows any single run, but not what a frame costs.
	per := time.Duration(testing.Benchmark(BenchmarkEditorView).NsPerOp())
	for i := 0; i < 2 && per > maxFrame; i++ {
		if d := time.Duration(testing.Benchmark(BenchmarkEditorView).NsPerOp()); d < per {
			per = d
		}
	}
	t.Logf("EditorView 5k lines: %v/op (budget %v)", per, maxFrame)
	if per > maxFrame {
		t.Fatalf("frame took %v, want <= %v", per, maxFrame)
	}
}
