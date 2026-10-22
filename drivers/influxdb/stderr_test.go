package influxdb

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The tls=false warning goes to ConnConfig.Logger, never to stderr or the
// default slog logger, and it never carries the token.
func TestTLSFalseWarningGoesToConnLogger(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{
		DSN:    "influxdb3://:s3cr3t-token@influx.invalid:8181/db?tls=false&allow_plaintext=1",
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if c, err := New().Open(ctx, cfg); err == nil {
			_ = c.Close()
			t.Error("Open on an unresolvable host succeeded")
		}
	})
	if out != "" {
		t.Errorf("a failed dial wrote to stderr:\n%s", out)
	}
	if !strings.Contains(buf.String(), "cleartext") {
		t.Errorf("ConnConfig.Logger got %q, want the tls=false warning", buf.String())
	}
	if strings.Contains(buf.String(), "s3cr3t-token") {
		t.Errorf("the warning carries the token: %q", buf.String())
	}
}
