package drivertest

import (
	"testing"

	"go.uber.org/goleak"
)

// checkNoLeak fails when goroutines started since the before snapshot are
// still running. goleak retries for a short while, so goroutines that are
// already shutting down do not count.
func checkNoLeak(t *testing.T, before goleak.Option) {
	t.Helper()
	if err := goleak.Find(before); err != nil {
		t.Errorf("goroutines leaked by the driver (close what Open, Query, Watch and Begin start): %v", err)
	}
}
