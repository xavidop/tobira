package vertica

import (
	"context"
	"testing"
	"time"

	vlogger "github.com/vertica/vertica-sql-go/logger"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The driver never writes to stdout or stderr, which would draw over the
// TUI, even when the dial fails. vertica-sql-go logs to stdout at WARN by
// default; init turns that off.
func TestFailedDialWritesNothingToStderr(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := New().Open(ctx, driver.ConnConfig{
			DSN:      "vertica://u@127.0.0.1:1/db?tlsmode=none",
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

// Every vertica-sql-go logger is silenced: a FATAL message would otherwise
// also call os.Exit.
func TestSDKLoggingSilenced(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		vlogger.New("probe").Error("must not print %s", "this")
		vlogger.New("probe").Warn("nor this")
		vlogger.New("probe").Fatal("nor exit")
	})
	if out != "" {
		t.Errorf("vertica-sql-go logged:\n%s", out)
	}
}
