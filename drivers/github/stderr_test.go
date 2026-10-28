package github

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

// A failed Open writes nothing to the terminal.
func TestFailedOpenIsSilent(t *testing.T) {
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if c, err := New().Open(ctx, driver.ConnConfig{DSN: "github://octo/widgets?endpoint=http://127.0.0.1:1", Password: driver.NewSecret(fakeToken)}); err == nil {
			_ = c.Close()
			t.Error("Open on a closed port succeeded")
		}
	})
	if out != "" {
		t.Errorf("a failed open wrote to stderr:\n%s", out)
	}
}

// The no-token warning goes to ConnConfig.Logger, never to stderr, and a
// working connection stays silent too.
func TestNoTokenWarningGoesToConnLogger(t *testing.T) {
	f := newFakeGitHub(t)
	var buf bytes.Buffer
	cfg := driver.ConnConfig{Name: "gh", DSN: f.dsn("octo", "widgets"), Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	out := drivertest.Stderr(t, func() {
		c, err := New().Open(context.Background(), cfg)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`}); err != nil {
			t.Error(err)
		}
	})
	if out != "" {
		t.Errorf("the driver wrote to stderr:\n%s", out)
	}
	if !strings.Contains(buf.String(), "no token") {
		t.Errorf("ConnConfig.Logger got %q, want the no-token warning", buf.String())
	}
	buf.Reset()
	c, err := New().Open(context.Background(), driver.ConnConfig{Name: "gh", DSN: f.dsn("octo", "widgets"), Password: driver.NewSecret(fakeToken), Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if buf.Len() != 0 {
		t.Errorf("a connection with a token logged %q", buf.String())
	}
}

// Sending the token in clear to a remote host (an explicit http:// endpoint
// with allow_plaintext) is warned about through the logger, without the
// token; a loopback endpoint needs no opt-in and draws no warning.
func TestPlaintextTokenWarningGoesToConnLogger(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{
		Name:     "gh",
		DSN:      "github://octo/widgets?endpoint=http://192.0.2.1:9&allow_plaintext=1&timeout=200ms",
		Password: driver.NewSecret(fakeToken),
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	out := drivertest.Stderr(t, func() {
		if c, err := New().Open(context.Background(), cfg); err == nil {
			_ = c.Close()
			t.Error("Open on an unroutable host succeeded")
		}
	})
	if out != "" {
		t.Errorf("a failed open wrote to stderr:\n%s", out)
	}
	if !strings.Contains(buf.String(), "without TLS") {
		t.Errorf("ConnConfig.Logger got %q, want the plaintext warning", buf.String())
	}
	if strings.Contains(buf.String(), fakeToken) {
		t.Errorf("the warning carries the token: %q", buf.String())
	}
	f := newFakeGitHub(t)
	buf.Reset()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: f.dsn("octo", "widgets"), Password: driver.NewSecret(fakeToken), Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if buf.Len() != 0 {
		t.Errorf("a loopback endpoint logged %q", buf.String())
	}
}
