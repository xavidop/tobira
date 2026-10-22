package elasticsearch

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

// The plaintext-credential warning goes to ConnConfig.Logger, never to
// stderr or the default slog logger, and it never carries the secret.
func TestPlaintextCredentialWarningGoesToConnLogger(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{
		Name:   "search",
		DSN:    "elasticsearch://elastic:s3cr3t-pw@es.invalid:9200?tls=false&allow_plaintext=1",
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
		t.Errorf("a failed open wrote to stderr:\n%s", out)
	}
	if !strings.Contains(buf.String(), "without TLS") {
		t.Errorf("ConnConfig.Logger got %q, want the plaintext warning", buf.String())
	}
	if strings.Contains(buf.String(), "s3cr3t-pw") {
		t.Errorf("the warning carries the password: %q", buf.String())
	}
}

// With no Logger the warning is discarded, not written to stderr.
func TestPlaintextCredentialWarningNilLoggerIsSilent(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if c, err := New().Open(ctx, driver.ConnConfig{DSN: "elasticsearch://:k3y@es.invalid:9200?tls=false&allow_plaintext=1"}); err == nil {
			_ = c.Close()
		}
	})
	if out != "" {
		t.Errorf("Open wrote to stderr:\n%s", out)
	}
}
