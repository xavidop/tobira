package weaviate

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

// The plaintext-key warning goes to ConnConfig.Logger, never to stderr, and
// it never carries the key.
func TestPlaintextKeyWarningGoesToConnLogger(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{
		Name:   "vec",
		DSN:    "weaviate://:s3cr3t-key@weaviate.invalid:8080?scheme=http&allow_plaintext=1",
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if c, err := New().Open(ctx, cfg); err == nil {
			_ = c.Close()
			t.Error("Open on an unresolvable host succeeded")
		}
	})
	if out != "" {
		t.Errorf("a failed dial wrote to stderr:\n%s", out)
	}
	if !strings.Contains(buf.String(), "without TLS") {
		t.Errorf("ConnConfig.Logger got %q, want the plaintext-key warning", buf.String())
	}
	if strings.Contains(buf.String(), "s3cr3t-key") {
		t.Errorf("the warning carries the API key: %q", buf.String())
	}
}
