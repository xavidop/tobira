//go:build unix

package drivertest_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/xavidop/tobira/drivertest"
)

func TestStderrCapturesFD2(t *testing.T) {
	stderr := os.NewFile(2, "fd2") // the descriptor, not the os.Stderr variable
	got := drivertest.Stderr(t, func() { _, _ = fmt.Fprint(stderr, "boom") })
	if got != "boom" {
		t.Errorf("Stderr captured %q, want %q", got, "boom")
	}
	if got := drivertest.Stderr(t, func() {}); got != "" {
		t.Errorf("Stderr captured %q from a silent func", got)
	}
}
