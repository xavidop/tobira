package firestore

import (
	"context"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The driver never writes to stderr, which would draw over the TUI, even
// when the dial fails.
func TestFailedDialWritesNothingToStderr(t *testing.T) {
	defer func(d time.Duration) { openTimeout = d }(openTimeout)
	openTimeout = time.Second
	out := drivertest.Stderr(t, func() {
		if c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "firestore://p?emulator=127.0.0.1:1"}); err == nil {
			_ = c.Close()
			t.Error("Open on a closed port succeeded")
		}
	})
	if out != "" {
		t.Errorf("a failed dial wrote to stderr:\n%s", out)
	}
}
