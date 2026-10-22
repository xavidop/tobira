package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A run that hits a driver's size limit says so and how to get under it.
func TestRunErrTextSizeLimit(t *testing.T) {
	h := newQueryHarness(t, nil)
	err := driver.Wrap(driver.ErrLimit, "fake", "query", errors.New("the result exceeds the memory budget"))
	got := h.m.runErrText(&resultTab{conn: "local"}, err)
	if !strings.Contains(got, "the result exceeds the memory budget") || !strings.Contains(got, "size limit") ||
		!strings.Contains(got, "narrow the query") {
		t.Errorf("runErrText = %q, want the error and a size-limit hint", got)
	}
}
