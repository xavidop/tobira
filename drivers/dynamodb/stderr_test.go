package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The driver never writes to stderr, which would draw over the TUI, even
// when the dial fails and the SDK retries.
func TestFailedDialWritesNothingToStderr(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Port 1 refuses connections; nil Logger means discard.
		c, err := New().Open(ctx, driver.ConnConfig{
			DSN:      "dynamodb://AKIDEXAMPLE@us-east-1?endpoint=http://127.0.0.1:1",
			Password: driver.NewSecret("not-a-real-secret"),
		})
		if err == nil {
			_ = c.Close()
			t.Error("Open on a closed port succeeded")
		} else if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("want ErrConnection, got %v", err)
		}
	})
	if out != "" {
		t.Errorf("a failed dial wrote to stderr:\n%s", out)
	}
}
