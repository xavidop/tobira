package session

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/sshtunnel/sshtest"
)

// sshConn is a connection through an in-process SSH server, with the key
// and known_hosts files the server trusts and is trusted by.
func sshConn(t *testing.T) (config.Connection, *sshtest.Server) {
	t.Helper()
	dir := t.TempDir()
	pub, key, err := sshtest.ClientKey(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshtest.Start(sshtest.Options{Authorized: pub})
	if err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, "known_hosts")
	if err := srv.WriteKnownHosts(kh); err != nil {
		t.Fatal(err)
	}
	return config.Connection{Name: "prod", Driver: "fake", DSN: "fake://app@db.internal:5432/app?x=1",
		SSH: &config.SSH{Host: srv.Addr, User: "deploy", Key: key, KnownHosts: kh}}, srv
}

func TestOpenThroughSSHTunnel(t *testing.T) {
	defer goleak.VerifyNone(t)
	c, srv := sshConn(t)
	defer srv.Close() // before the leak check: the server's goroutines are its own
	m, _, rec := newManager(t, false, c)
	m.res.Getenv = func(string) string { return "" } // no agent, no password override
	if _, err := m.Open(context.Background(), "prod"); err != nil {
		t.Fatal(err)
	}
	if len(rec.cfgs) != 1 {
		t.Fatalf("%d opens", len(rec.cfgs))
	}
	dsn := rec.cfgs[0].DSN
	if !strings.HasPrefix(dsn, "fake://app@127.0.0.1:") || !strings.HasSuffix(dsn, "/app?x=1") {
		t.Errorf("driver saw %q, want the loopback tunnel address", dsn)
	}
	// The loopback port is open while the connection is, and gone after.
	hostport := strings.TrimSuffix(strings.TrimPrefix(dsn, "fake://app@"), "/app?x=1")
	if conn, err := net.Dial("tcp", hostport); err != nil {
		t.Errorf("tunnel port: %v", err)
	} else {
		_ = conn.Close()
	}
	if err := m.Close("prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := net.Dial("tcp", hostport); err == nil {
		t.Error("the tunnel port still accepts after Close")
	}
	// Test probes through a tunnel too, and leaves nothing behind.
	if err := m.Test(context.Background(), c); err != nil {
		t.Errorf("Test: %v", err)
	}
	_ = m.CloseAll()
}

func TestSSHTunnelFailureIsReported(t *testing.T) {
	defer goleak.VerifyNone(t)
	c, srv := sshConn(t)
	defer srv.Close()
	c.SSH.KnownHosts = filepath.Join(t.TempDir(), "missing")
	m, f, _ := newManager(t, false, c)
	m.res.Getenv = func(string) string { return "" }
	_, err := m.Open(context.Background(), "prod")
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("err = %v, want a known_hosts error", err)
	}
	if opens(f) != 0 {
		t.Error("the driver was opened although the tunnel failed")
	}
	if st := m.State("prod"); st.Status != Failed {
		t.Errorf("status = %v, want Failed", st.Status)
	}
}
