package qdrant

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// TLS is the default for a host that is not loopback, so an upgraded DSN
// that reached a plain gRPC server without tls= now fails the handshake.
// The error must say how to opt in.
func TestTLSDefaultedHint(t *testing.T) {
	for dsn, want := range map[string]bool{
		"qdrant://db.example":                             true,
		"qdrant://:key@db.example":                        true,
		"qdrant://db.example?tls=true":                    false,
		"qdrant://db.example?tls=false&allow_plaintext=1": false,
		"qdrant://localhost":                              false,
	} {
		d, err := parseDSN(driver.ConnConfig{DSN: dsn})
		if err != nil {
			t.Fatalf("parseDSN(%q): %v", dsn, err)
		}
		if d.tlsDefaulted != want {
			t.Errorf("parseDSN(%q).tlsDefaulted = %v, want %v", dsn, d.tlsDefaulted, want)
		}
	}

	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	host, port, _ := net.SplitHostPort(plain.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	d := dsnConfig{host: host, port: p, tls: true, tlsDefaulted: true}
	c, err := newConn(d, driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = c.Ping(ctx)
	if err == nil {
		t.Fatal("TLS to a plain server succeeded")
	}
	got := d.tlsHint(err)
	if !errors.Is(got, driver.ErrConnection) || !strings.Contains(got.Error(), "tls=false&allow_plaintext=1") {
		t.Errorf("tlsHint(%v) = %v, want ErrConnection with the allow_plaintext hint", err, got)
	}

	d.tlsDefaulted = false
	if got := d.tlsHint(err); got != err {
		t.Errorf("explicit tls=true: tlsHint changed the error to %v", got)
	}
}
