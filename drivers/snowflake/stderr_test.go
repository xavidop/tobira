package snowflake

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
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Port 1 refuses connections; nil Logger means discard.
		c, err := New().Open(ctx, driver.ConnConfig{
			DSN:      "snowflake://u@acct?host=127.0.0.1&port=1&protocol=http",
			Password: driver.NewSecret("pw-123456"),
		})
		if err == nil {
			_ = c.Close()
			t.Error("Open on a closed port succeeded")
		}
	})
	if out != "" {
		t.Errorf("a failed dial wrote to stderr:\n%s", out)
	}
}
