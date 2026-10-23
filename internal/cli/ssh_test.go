package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/sshtunnel/sshtest"
)

// sshHarness writes a config whose "prod" connection goes through an
// in-process SSH server.
func sshHarness(t *testing.T) (*harness, *sshtest.Server) {
	t.Helper()
	h := newHarness(t)
	dir := t.TempDir()
	pub, key, err := sshtest.ClientKey(dir, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshtest.Start(sshtest.Options{Authorized: pub})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	kh := filepath.Join(dir, "known_hosts")
	if err := srv.WriteKnownHosts(kh); err != nil {
		t.Fatal(err)
	}
	body := "connections:\n  - name: prod\n    driver: fake\n    dsn: fake://app@db.internal:5432/app\n" +
		"    ssh:\n      host: " + srv.Addr + "\n      user: deploy\n      key: " + key + "\n" +
		"      passphrase: env:KEY_PASS\n      known_hosts: " + kh + "\n"
	if err := os.WriteFile(h.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Refs resolve through mamori, which reads the process environment.
	t.Setenv("KEY_PASS", "s3cret")
	return h, srv
}

func TestQueryThroughSSHTunnel(t *testing.T) {
	h, srv := sshHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "query", "prod", selectPeople, "--format", "ndjson")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(h.opened) != 1 || !strings.HasPrefix(h.opened[0].DSN, "fake://app@127.0.0.1:") || !strings.HasSuffix(h.opened[0].DSN, "/app") {
		t.Fatalf("driver saw %+v, want the tunnel's loopback address", h.opened)
	}
	if got := srv.Users(); len(got) != 1 || got[0] != "deploy" {
		t.Errorf("ssh users = %v", got)
	}

	// The wrong passphrase fails before the driver is reached, with exit 2
	// and no secret in the message.
	t.Setenv("KEY_PASS", "nope")
	h.opened = nil
	_, errOut, code = h.exec(t, context.Background(), "query", "prod", selectPeople)
	if code != 2 || !strings.Contains(errOut, "wrong passphrase") || strings.Contains(errOut, "nope") || strings.Contains(errOut, "s3cret") {
		t.Errorf("wrong passphrase: exit %d, stderr %q", code, errOut)
	}
	if len(h.opened) != 0 {
		t.Error("the driver was opened without a tunnel")
	}
}

func TestConnectionsListShowsSSH(t *testing.T) {
	h, srv := sshHarness(t)
	out, _, code := h.exec(t, context.Background(), "connections", "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rows []struct {
		Name string `json:"name"`
		SSH  string `json:"ssh"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SSH != "deploy@"+srv.Addr {
		t.Errorf("rows = %+v, want ssh deploy@%s", rows, srv.Addr)
	}
	h.tty = true
	out, _, _ = h.exec(t, context.Background(), "connections", "list")
	if !strings.Contains(out, "SSH") || !strings.Contains(out, "deploy@"+srv.Addr) {
		t.Errorf("table:\n%s", out)
	}
}

func TestDoctorChecksSSH(t *testing.T) {
	h, srv := sshHarness(t)
	out, _, code := h.exec(t, context.Background(), "doctor", "--no-ping")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "via ssh "+srv.Addr) {
		t.Errorf("doctor does not name the ssh server:\n%s", out)
	}
	if !strings.Contains(out, "ssh.passphrase") {
		t.Errorf("doctor does not check the ssh.passphrase ref:\n%s", out)
	}
	// With a ping, the connection is reached through the tunnel.
	out, _, code = h.exec(t, context.Background(), "doctor")
	if code != 0 || !strings.Contains(out, "reachable in") {
		t.Errorf("doctor ping through ssh: exit %d:\n%s", code, out)
	}
	// A missing known_hosts file fails the connection check before any network.
	body, _ := os.ReadFile(h.cfg)
	cfg := strings.Replace(string(body), "known_hosts: ", "known_hosts: /nonexistent/", 1)
	if err := os.WriteFile(h.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code = h.exec(t, context.Background(), "doctor", "--no-ping")
	if code == 0 || !strings.Contains(out, "ssh known_hosts") || !strings.Contains(out, "ssh-keyscan") {
		t.Errorf("missing known_hosts: exit %d:\n%s", code, out)
	}
}
