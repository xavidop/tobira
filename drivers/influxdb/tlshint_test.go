package influxdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// TLS is the default for a host that is not loopback, so an upgraded DSN
// that reached a plain-HTTP server without tls= now fails the handshake.
// The error must say how to opt in.
func TestTLSDefaultedHint(t *testing.T) {
	for dsn, want := range map[string]bool{
		"influxdb3://db.example/x":                             true,
		"influxdb3://:tok@db.example/x":                        true,
		"influxdb3://db.example/x?tls=true":                    false,
		"influxdb3://db.example/x?tls=false&allow_plaintext=1": false,
		"influxdb3://localhost/x":                              false,
	} {
		s, err := parseDSN(driver.ConnConfig{DSN: dsn})
		if err != nil {
			t.Fatalf("parseDSN(%q): %v", dsn, err)
		}
		if s.tlsDefaulted != want {
			t.Errorf("parseDSN(%q).tlsDefaulted = %v, want %v", dsn, s.tlsDefaulted, want)
		}
	}

	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	set := settings{host: "https://" + plain.Listener.Addr().String(), tls: true, tlsDefaulted: true}
	c := &conn{http: &http.Client{}, set: set}
	_, err := c.get(context.Background(), "open", "/ping")
	if err == nil {
		t.Fatal("https to a plain server succeeded")
	}
	got := set.tlsHint(err)
	if !errors.Is(got, driver.ErrConnection) || !strings.Contains(got.Error(), "tls=false&allow_plaintext=1") {
		t.Errorf("tlsHint = %v, want ErrConnection with the allow_plaintext hint", got)
	}
	if strings.Count(got.Error(), "influxdb:") != 1 {
		t.Errorf("tlsHint repeats the driver prefix: %v", got)
	}

	set.tlsDefaulted = false
	if got := set.tlsHint(err); got != err {
		t.Errorf("explicit tls=true: tlsHint changed the error to %v", got)
	}
	other := driver.Wrap(driver.ErrAuth, Name, "open", errors.New("401 unauthorized"))
	set.tlsDefaulted = true
	if got := set.tlsHint(other); got != other {
		t.Errorf("non-TLS error: tlsHint changed it to %v", got)
	}
}
