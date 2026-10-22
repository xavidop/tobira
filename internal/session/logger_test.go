package session

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

// WithLogger hands the host's --debug log to every Open (and Test) as
// ConnConfig.Logger, so driver warnings reach it.
func TestWithLoggerReachesOpen(t *testing.T) {
	rec := &recorder{Driver: drivertest.NewFake().Driver()}
	reg := driver.NewRegistry()
	if err := reg.Add(rec); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	m := New(&config.Config{Connections: []config.Connection{fakeConn}}, reg, secrets.Resolver{}, false, WithLogger(log))
	defer func() { _ = m.CloseAll() }()
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if err := m.Test(context.Background(), fakeConn); err != nil {
		t.Fatal(err)
	}
	if len(rec.cfgs) != 2 {
		t.Fatalf("%d opens", len(rec.cfgs))
	}
	for i, cfg := range rec.cfgs {
		if cfg.Logger != log {
			t.Errorf("open %d: Logger = %v, want the manager's", i, cfg.Logger)
		}
	}
}

// Without WithLogger (no --debug), drivers get a logger that discards.
func TestNoLoggerMeansDiscard(t *testing.T) {
	m, _, rec := newManager(t, false, fakeConn)
	defer func() { _ = m.CloseAll() }()
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	l := rec.cfgs[0].Logger
	if l == nil {
		t.Fatal("Logger = nil, want a discard logger")
	}
	if l.Handler().Enabled(context.Background(), slog.LevelError) {
		t.Error("the default Logger is enabled; it must discard")
	}
}
