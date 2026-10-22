package driver_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestConnConfigLogNilDiscards(t *testing.T) {
	l := driver.ConnConfig{}.Log()
	if l == nil {
		t.Fatal("Log() on a zero ConnConfig = nil, want a discard logger")
	}
	if l.Handler().Enabled(t.Context(), slog.LevelError) {
		t.Error("the nil-Logger fallback is enabled; it must discard")
	}
}

func TestConnConfigLogUsesLogger(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	cfg.Log().Warn("hello", "k", "v")
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("log output = %q, want the warning", buf.String())
	}
}
