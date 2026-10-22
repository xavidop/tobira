package dynamodb

import (
	"math"
	"testing"
)

func TestStatementLimit(t *testing.T) {
	for in, want := range map[int]int32{1: 2, 10: 11, 999: 1000, 1000: 1000, 5000: 1000, math.MaxInt: 1000, math.MaxInt32: 1000} {
		if got := statementLimit(in); got != want {
			t.Errorf("statementLimit(%d) = %d, want %d", in, got, want)
		}
	}
}
