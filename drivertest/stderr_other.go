//go:build !unix || solaris

package drivertest

import "testing"

// Stderr is not supported on this platform (no unix descriptors, or no
// dup2 in package syscall, as on Solaris and illumos): it skips the test.
func Stderr(t testing.TB, fn func()) string {
	t.Helper()
	t.Skip("drivertest.Stderr needs a unix file descriptor 2")
	return ""
}
