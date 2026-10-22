//go:build race

package tui

import "time"

// keystrokeBudget is looser under the race detector, which skews timing;
// the !race build enforces the real 5 ms budget.
const keystrokeBudget = 50 * time.Millisecond

// keystrokeCeiling is the race build's ceiling for any single Update; a
// blocking driver call (200ms in the tests) is still above it.
const keystrokeCeiling = 150 * time.Millisecond
