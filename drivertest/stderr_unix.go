//go:build unix && !solaris

package drivertest

import (
	"io"
	"os"
	"syscall"
	"testing"
)

// Stderr runs fn with file descriptor 2 redirected to a pipe and returns
// what was written to it. It captures the descriptor, not the os.Stderr
// variable, so it also sees loggers that kept a reference to the original
// *os.File (go-redis's default logger, the log package). Drivers use it to
// prove they never write to the terminal. Tests that call it must not run
// in parallel with tests that write to stderr.
func Stderr(t testing.TB, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("drivertest.Stderr: pipe: %v", err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatalf("drivertest.Stderr: dup: %v", err)
	}
	if err := dup2(int(w.Fd()), 2); err != nil {
		t.Fatalf("drivertest.Stderr: dup2: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		_ = r.Close()
		done <- string(b)
	}()
	restore := func() {
		_ = dup2(saved, 2)
		_ = syscall.Close(saved)
		_ = w.Close()
	}
	func() {
		defer restore()
		fn()
	}()
	return <-done
}
