package weaviate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "weaviate" || info.Kind != driver.KindVector || info.Dialect != "weaviate" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "weaviate" {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := map[driver.Capability]bool{driver.CapBrowser: true, driver.CapMutator: true, driver.CapVectorSearcher: true, driver.CapReadOnly: true}
	if len(info.Capabilities) != len(want) {
		t.Fatalf("caps = %v", info.Capabilities)
	}
	for _, c := range info.Capabilities {
		if !want[c] {
			t.Errorf("unexpected capability %v", c)
		}
	}
	if _, ok := driver.Default.Lookup(Name); !ok {
		t.Error("not registered in driver.Default")
	}
	if d, ok := driver.Default.ForScheme("weaviate"); !ok || d.Info().Name != Name {
		t.Error("weaviate scheme not routed to the driver")
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		name string
		cfg  driver.ConnConfig
		want dsnConfig
	}{
		{"remote defaults to https", driver.ConnConfig{DSN: "weaviate://db.example"}, dsnConfig{scheme: "https", host: "db.example"}},
		{"loopback defaults to http:8080", driver.ConnConfig{DSN: "weaviate://localhost"}, dsnConfig{scheme: "http", host: "localhost", port: 8080}},
		{"empty host", driver.ConnConfig{DSN: "weaviate://"}, dsnConfig{scheme: "http", host: "localhost", port: 8080}},
		{"port", driver.ConnConfig{DSN: "weaviate://db.example:9000"}, dsnConfig{scheme: "https", host: "db.example", port: 9000}},
		{"scheme http", driver.ConnConfig{DSN: "weaviate://db.example?scheme=http&allow_plaintext=1"}, dsnConfig{scheme: "http", host: "db.example", port: 8080}},
		{"allow_plaintext as an option", driver.ConnConfig{DSN: "weaviate://db.example", Options: map[string]string{"scheme": "http", "allow_plaintext": "true"}},
			dsnConfig{scheme: "http", host: "db.example", port: 8080}},
		{"allow_plaintext alone keeps https", driver.ConnConfig{DSN: "weaviate://db.example?allow_plaintext=1"}, dsnConfig{scheme: "https", host: "db.example"}},
		{"scheme option", driver.ConnConfig{DSN: "weaviate://127.0.0.1:7", Options: map[string]string{"scheme": "https"}}, dsnConfig{scheme: "https", host: "127.0.0.1", port: 7}},
		{"ipv6", driver.ConnConfig{DSN: "weaviate://[::1]:8081"}, dsnConfig{scheme: "http", host: "::1", port: 8081}},
		{"key in password", driver.ConnConfig{DSN: "weaviate://:s3cret@db.example"}, dsnConfig{scheme: "https", host: "db.example", apiKey: "s3cret"}},
		{"key in api_key", driver.ConnConfig{DSN: "weaviate://db.example?api_key=s3cret"}, dsnConfig{scheme: "https", host: "db.example", apiKey: "s3cret"}},
		{"key in apikey", driver.ConnConfig{DSN: "weaviate://db.example?apikey=s3cret"}, dsnConfig{scheme: "https", host: "db.example", apiKey: "s3cret"}},
		{"password ref wins", driver.ConnConfig{DSN: "weaviate://:fromdsn@db.example", Password: driver.NewSecret("override")},
			dsnConfig{scheme: "https", host: "db.example", apiKey: "override"}},
		{"password ref wins over api_key", driver.ConnConfig{DSN: "weaviate://db.example?api_key=fromdsn", Password: driver.NewSecret("override")},
			dsnConfig{scheme: "https", host: "db.example", apiKey: "override"}},
		{"key over plain http to a remote host", driver.ConnConfig{DSN: "weaviate://:k@db.example?scheme=http&allow_plaintext=1"},
			dsnConfig{scheme: "http", host: "db.example", port: 8080, apiKey: "k", plaintextKey: true}},
		{"key over http to loopback", driver.ConnConfig{DSN: "weaviate://:k@127.0.0.1:9"}, dsnConfig{scheme: "http", host: "127.0.0.1", port: 9, apiKey: "k"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDSN(tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("parseDSN = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDSNHostPort(t *testing.T) {
	for _, tt := range []struct {
		d    dsnConfig
		want string
	}{
		{dsnConfig{scheme: "https", host: "db.example"}, "db.example"},
		{dsnConfig{scheme: "http", host: "localhost", port: 8080}, "localhost:8080"},
		{dsnConfig{scheme: "http", host: "::1", port: 8080}, "[::1]:8080"},
	} {
		if got := tt.d.hostPort(); got != tt.want {
			t.Errorf("hostPort(%+v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestParseDSNRefusals(t *testing.T) {
	const key = "sup3r-s3cret-key"
	for _, tt := range []struct {
		name string
		cfg  driver.ConnConfig
	}{
		{"wrong scheme", driver.ConnConfig{DSN: "http://db.example"}},
		{"path", driver.ConnConfig{DSN: "weaviate://db.example/Class"}},
		{"bad port", driver.ConnConfig{DSN: "weaviate://db.example:0"}},
		{"bad scheme param", driver.ConnConfig{DSN: "weaviate://db.example?scheme=ftp"}},
		{"unknown param", driver.ConnConfig{DSN: "weaviate://db.example?api_key=" + key + "&grpc=1"}},
		{"unknown option", driver.ConnConfig{DSN: "weaviate://db.example", Options: map[string]string{"tenant": "a"}}},
		{"user name", driver.ConnConfig{DSN: "weaviate://bob:" + key + "@db.example"}},
		{"key twice", driver.ConnConfig{DSN: "weaviate://:" + key + "@db.example?api_key=" + key}},
		{"key param twice", driver.ConnConfig{DSN: "weaviate://db.example?api_key=" + key + "&apikey=" + key}},
		{"unparsable", driver.ConnConfig{DSN: "weaviate://:" + key + "@db.example:%zz"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseDSN(tt.cfg)
			if !errors.Is(err, driver.ErrSyntax) {
				t.Fatalf("parseDSN(%q): want ErrSyntax, got %v", tt.cfg.DSN, err)
			}
			if err := redact(err, tt.cfg, ""); strings.Contains(err.Error(), key) {
				t.Errorf("the error carries the key: %v", err)
			}
		})
	}
}

// Plain http to a host that is not loopback needs allow_plaintext=1, and
// Weaviate Cloud is never reached in clear.
func TestParseDSNPlaintextRule(t *testing.T) {
	for _, tt := range []struct{ dsn, want string }{
		{"weaviate://db.example?scheme=http", "allow_plaintext=1"},
		{"weaviate://:k@db.example?scheme=http&allow_plaintext=0", "allow_plaintext=1"},
		{"weaviate://db.example?allow_plaintext=maybe", "allow_plaintext must be"},
		{"weaviate://abc.c0.europe-west3.gcp.weaviate.cloud?scheme=http&allow_plaintext=1", "Weaviate Cloud"},
		{"weaviate://Sandbox.Weaviate.Network.?scheme=http&allow_plaintext=1", "Weaviate Cloud"},
		{"weaviate://weaviate.cloud?scheme=http&allow_plaintext=1", "Weaviate Cloud"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tt.dsn})
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("parseDSN(%q) = %v, want ErrSyntax containing %q", tt.dsn, err, tt.want)
		}
	}
}

// A redirect is never followed: the API key must never reach the second
// server, whatever its host or scheme.
func TestRedirectIsNotFollowed(t *testing.T) {
	var hits, keyed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			keyed.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.30.0"}`))
	}))
	t.Cleanup(target.Close)
	// The same host name on another port: Go keeps Authorization there.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	dsn := "weaviate://" + strings.TrimPrefix(redirector.URL, "http://")
	c, err := New().Open(t.Context(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("s3cr3t-k3y")})
	if err == nil {
		_ = c.Close()
		t.Error("Open through a redirect succeeded")
	}
	if err != nil && strings.Contains(err.Error(), "s3cr3t-k3y") {
		t.Errorf("the error carries the key: %v", err)
	}
	if n := keyed.Load(); n != 0 {
		t.Errorf("the redirect target got the API key %d times", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

// The DSN redactor masks the key wherever the DSN can carry it.
func TestRedactDSNMasksKey(t *testing.T) {
	const key = "sup3r-s3cret-key"
	for _, dsn := range []string{
		"weaviate://db.example?api_key=" + key,
		"weaviate://db.example?apikey=" + key,
		"weaviate://db.example?scheme=https&apikey=" + key,
		"weaviate://:" + key + "@db.example",
	} {
		if got := driver.RedactDSN(dsn); strings.Contains(got, key) {
			t.Errorf("RedactDSN(%q) = %q, still has the key", dsn, got)
		}
		msg := redact(errors.New("dial "+dsn+" failed: Bearer "+key), driver.ConnConfig{DSN: dsn}, key).Error()
		if strings.Contains(msg, key) {
			t.Errorf("redact kept the key: %q", msg)
		}
	}
}

// Open against a closed port fails with ErrConnection, never with the key.
func TestOpenUnreachable(t *testing.T) {
	const key = "sup3r-s3cret-key"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "weaviate://127.0.0.1:1?apikey=" + key},
		{DSN: "weaviate://127.0.0.1:1", Password: driver.NewSecret(key)},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Fatal("Open on a closed port succeeded")
		}
		if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("want ErrConnection, got %v", err)
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("error carries the key: %v", err)
		}
	}
}

func TestOpenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: "weaviate://127.0.0.1:1"})
	if err == nil {
		_ = c.Close()
		t.Fatal("Open with a cancelled context succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}
