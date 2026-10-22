//go:build unix && !solaris

package drivertest

import (
	"os"
	"testing"
)

// dup2 onto the same descriptor succeeds and leaves it open, as dup2(2)
// does; dup3, which Linux uses, refuses that case with EINVAL.
func TestDup2SameDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	fd := int(w.Fd())
	if err := dup2(fd, fd); err != nil {
		t.Fatalf("dup2(%d, %d) = %v, want nil", fd, fd, err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write after dup2 onto itself: %v", err)
	}
}
