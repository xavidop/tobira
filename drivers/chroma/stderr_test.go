package chroma

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
		DSN:    "chroma://:s3cr3t-tok@chroma.invalid:8000?tls=false&allow_plaintext=1",
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
		t.Errorf("ConnConfig.Logger got %q, want the plaintext-token warning", buf.String())
	}
	if strings.Contains(buf.String(), "s3cr3t-tok") {
		t.Errorf("the warning carries the token: %q", buf.String())
	}
}

// A whole session against the fake writes nothing to stderr.
func TestSessionIsSilent(t *testing.T) {
	f := newFake(t)
	c := f.addColl("default_database", "things", "l2", 2)
	c.add("1", []float64{1, 0}, "one", map[string]any{"n": 1})
	out := drivertest.Stderr(t, func() {
		conn := openFake(t, f, driver.ConnConfig{})
		rs, err := conn.Query(t.Context(), driver.Query{Text: `{"collection":"things"}`})
		if err != nil {
			t.Error(err)
			return
		}
		_, _, _ = driver.Collect(t.Context(), rs, 0)
	})
	if out != "" {
		t.Errorf("a session wrote to stderr:\n%s", out)
	}
}
