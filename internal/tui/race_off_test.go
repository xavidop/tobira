//go:build !race

package tui

import "time"

// keystrokeBudget is the most one key press may spend in Update.
const keystrokeBudget = 5 * time.Millisecond

// keystrokeCeiling is the most any single Update may take while the machine
// is loaded; a blocking driver call (200ms in the tests) is far above it.
const keystrokeCeiling = 50 * time.Millisecond
