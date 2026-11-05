//go:build !race

package grid

import (
	"testing"
	"time"
)

// maxFrame is the budget for one 200x60 frame of a 10k-row page, which
// leaves headroom for 60 fps. The race detector skews timing, so this file
// is excluded under -race.
const maxFrame = 4 * time.Millisecond

func TestPerfGridView10k(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	// The fastest of a few runs: a loaded machine (the whole suite running
	// in parallel) slows any single run, but not what a frame costs.
	per := time.Duration(testing.Benchmark(BenchmarkGridView10k).NsPerOp())
	for i := 0; i < 2 && per > maxFrame; i++ {
		if d := time.Duration(testing.Benchmark(BenchmarkGridView10k).NsPerOp()); d < per {
			per = d
		}
	}
	t.Logf("GridView10k: %v/op (budget %v)", per, maxFrame)
	if per > maxFrame {
		t.Fatalf("frame took %v, want <= %v", per, maxFrame)
	}
}
