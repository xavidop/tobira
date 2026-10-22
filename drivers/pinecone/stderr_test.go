package pinecone

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

// The plaintext-key warning goes to ConnConfig.Logger, never to stderr,
// and it never carries the key.
func TestPlaintextKeyWarningGoesToConnLogger(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{
		Name:   "vec",
		DSN:    "pinecone://:s3cr3t-key@pinecone.invalid:5080?tls=false&allow_plaintext=1",
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		t.Errorf("ConnConfig.Logger got %q, want the plaintext-key warning", buf.String())
	}
	if strings.Contains(buf.String(), "s3cr3t-key") {
		t.Errorf("the warning carries the API key: %q", buf.String())
	}
}

// Nothing reaches stderr on a failed open to an index host either.
func TestIndexHostOpenIsSilent(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if c, err := New().Open(ctx, driver.ConnConfig{DSN: "pinecone://:k@?host=127.0.0.1:1&tls=false"}); err == nil {
			_ = c.Close()
		}
	})
	if out != "" {
		t.Errorf("Open wrote to stderr:\n%s", out)
	}
}
