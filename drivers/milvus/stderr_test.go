package milvus

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

// The plaintext-token warning goes to ConnConfig.Logger, never to stderr or
// the default slog logger, and it never carries the token.
func TestPlaintextTokenWarningGoesToConnLogger(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{
		Name:   "vec",
		DSN:    "milvus://root:s3cr3t-tok@milvus.invalid:19530?tls=false&allow_plaintext=1",
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
		t.Errorf("ConnConfig.Logger got %q, want the plaintext-token warning", buf.String())
	}
	if strings.Contains(buf.String(), "s3cr3t-tok") {
		t.Errorf("the warning carries the token: %q", buf.String())
	}
}

// With no Logger the warning is discarded, not written to stderr.
func TestPlaintextTokenWarningNilLoggerIsSilent(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if c, err := New().Open(ctx, driver.ConnConfig{DSN: "milvus://:k@milvus.invalid:19530?tls=false&allow_plaintext=1"}); err == nil {
			_ = c.Close()
		}
	})
	if out != "" {
		t.Errorf("Open wrote to stderr:\n%s", out)
	}
}
