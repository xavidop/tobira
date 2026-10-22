package redis

import (
	"context"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// go-redis logs every failed pool dial through its process-wide logger,
// which writes to stderr by default; the driver's init must silence it.
func TestFailedDialWritesNothingToStderr(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Port 1 refuses connections; nil Logger means discard.
		if c, err := New().Open(ctx, driver.ConnConfig{DSN: "redis://127.0.0.1:1/0"}); err == nil {
			_ = c.Close()
			t.Error("Open on a closed port succeeded")
		}
	})
	if out != "" {
		t.Errorf("a failed dial wrote to stderr:\n%s", out)
	}
}
