package session

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/internal/config"
)

func TestSetReadOnlyOverride(t *testing.T) {
	m, _, rec := newManager(t, false, fakeConn)
	if m.State("dev").ReadOnly {
		t.Fatal("dev starts read-only")
	}
	if err := m.SetReadOnly("dev", true); err != nil {
		t.Fatal(err)
	}
	if !m.State("dev").ReadOnly {
		t.Fatal("on: State.ReadOnly = false")
	}
	// The override is per call: the connection itself still opens writable,
	// so turning it off needs no reopen.
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if rec.cfgs[0].ReadOnly {
		t.Fatal("the override opened the connection read-only")
	}
	if err := m.SetReadOnly("dev", false); err != nil {
		t.Fatal(err)
	}
	if m.State("dev").ReadOnly {
		t.Fatal("off: State.ReadOnly = true")
	}
	t.Cleanup(func() { _ = m.CloseAll() })
}

func TestSetReadOnlyOffRefusedWhenEnforced(t *testing.T) {
	cfgRO := fakeConn
	cfgRO.ReadOnly = true
	for _, tt := range []struct {
		name string
		flag bool
		conn config.Connection
	}{
		{"config", false, cfgRO},
		{"flag", true, fakeConn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := newManager(t, tt.flag, tt.conn)
			err := m.SetReadOnly("dev", false)
			if !errors.Is(err, ErrReadOnlyEnforced) {
				t.Fatalf("off = %v, want ErrReadOnlyEnforced", err)
			}
			if err.Error() != "read-only is enforced by config/flag" {
				t.Fatalf("message = %q", err.Error())
			}
			if !m.State("dev").ReadOnly {
				t.Fatal("refused off still cleared read-only")
			}
			if err := m.SetReadOnly("dev", true); err != nil {
				t.Fatalf("on = %v", err)
			}
		})
	}
}

func TestSetReadOnlyUnknownAndReload(t *testing.T) {
	m, _, _ := newManager(t, false, fakeConn)
	if err := m.SetReadOnly("nope", true); err == nil {
		t.Fatal("unknown connection accepted")
	}
	if err := m.SetReadOnly("dev", true); err != nil {
		t.Fatal(err)
	}
	// A config change to the entry keeps the override: it only ever adds
	// a guard.
	changed := fakeConn
	changed.Color = "red"
	m.Reload(&config.Config{Connections: []config.Connection{changed}})
	if !m.State("dev").ReadOnly {
		t.Fatal("reload dropped the override")
	}
	// A removed and re-added entry starts clean.
	m.Reload(&config.Config{})
	m.Reload(&config.Config{Connections: []config.Connection{fakeConn}})
	if m.State("dev").ReadOnly {
		t.Fatal("a re-added connection kept the old override")
	}
}

func TestReadOnlyOverrideIsReportedApart(t *testing.T) {
	cfgRO := fakeConn
	cfgRO.ReadOnly = true
	m, _, _ := newManager(t, false, cfgRO)
	if m.ReadOnlyOverride("dev") {
		t.Fatal("a config read-only entry reported an override")
	}
	if err := m.SetReadOnly("dev", true); err != nil {
		t.Fatal(err)
	}
	if !m.ReadOnlyOverride("dev") || m.ReadOnlyOverride("nope") {
		t.Fatal("override not reported")
	}
}
