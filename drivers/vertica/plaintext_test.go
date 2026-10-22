package vertica

import (
	"strings"
	"testing"
)

// Off loopback the default tlsmode is server-strict, and the modes that do
// not verify the server (server, prefer, none) need allow_plaintext=1.
// Loopback keeps prefer.
func TestTLSModePlaintextRule(t *testing.T) {
	for dsn, want := range map[string]string{
		"vertica://u@db.example.com/d":                                   "server-strict",
		"vertica://u@db.example.com/d?tlsmode=server&allow_plaintext=1":  "server",
		"vertica://u@localhost/d?tlsmode=server":                         "server",
		"vertica://u@db.example.com/d?tlsmode=prefer&allow_plaintext=1":  "prefer",
		"vertica://u@db.example.com/d?tlsmode=none&allow_plaintext=true": "none",
		"vertica://u@db.example.com/d?allow_plaintext=1":                 "server-strict", // alone it keeps TLS
		"vertica://u@localhost/d":                                        "prefer",
		"vertica://u@127.0.0.1/d?tlsmode=none":                           "none",
		"vertica://u@[::1]/d":                                            "prefer",
		"vertica://u@localhost/d?backup_server_node=db2.example.com":     "server-strict",
	} {
		d, err := parseDSN(dsn)
		if err != nil || d.tlsMode != want {
			t.Errorf("parseDSN(%q) = tlsmode %q, %v; want %q", dsn, d.tlsMode, err, want)
		}
	}
	for _, dsn := range []string{
		"vertica://u@db.example.com/d?tlsmode=prefer",
		"vertica://u@db.example.com/d?tlsmode=server",
		"vertica://u@db.example.com/d?tlsmode=none",
		"vertica://u@db.example.com/d?tlsmode=none&allow_plaintext=0",
		"vertica://u@localhost/d?tlsmode=prefer&backup_server_node=db2.example.com",
	} {
		if _, err := parseDSN(dsn); err == nil || !strings.Contains(err.Error(), "allow_plaintext=1") {
			t.Errorf("parseDSN(%q) = %v, want a refusal naming allow_plaintext=1", dsn, err)
		}
	}
	if _, err := parseDSN("vertica://u@h/d?allow_plaintext=maybe"); err == nil {
		t.Error("allow_plaintext=maybe was accepted")
	}
}
