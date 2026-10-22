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
	res := testing.Benchmark(BenchmarkGridView10k)
	per := time.Duration(res.NsPerOp())
	t.Logf("GridView10k: %v/op (budget %v)", per, maxFrame)
	if per > maxFrame {
		t.Fatalf("frame took %v, want <= %v", per, maxFrame)
	}
}
