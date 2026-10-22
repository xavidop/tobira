package qdrant

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "qdrant" || info.Kind != driver.KindVector || info.Dialect != "qdrant" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "qdrant" {
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
	if d, ok := driver.Default.ForScheme("qdrant"); !ok || d.Info().Name != Name {
		t.Error("qdrant scheme not routed to the driver")
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		name string
		cfg  driver.ConnConfig
		want dsnConfig
	}{
		// TLS is on off loopback unless tls=false and allow_plaintext=1 say otherwise.
		{"defaults", driver.ConnConfig{DSN: "qdrant://db.example"}, dsnConfig{host: "db.example", port: 6334, tls: true, tlsDefaulted: true}},
		{"port", driver.ConnConfig{DSN: "qdrant://db.example:7000"}, dsnConfig{host: "db.example", port: 7000, tls: true, tlsDefaulted: true}},
		{"empty host", driver.ConnConfig{DSN: "qdrant://"}, dsnConfig{host: "localhost", port: 6334}},
		{"ipv6", driver.ConnConfig{DSN: "qdrant://[::1]:6334"}, dsnConfig{host: "::1", port: 6334}},
		// A key to a remote host turns TLS on unless tls=false says otherwise.
		{"key in password", driver.ConnConfig{DSN: "qdrant://:s3cret@db.example"}, dsnConfig{host: "db.example", port: 6334, apiKey: "s3cret", tls: true, tlsDefaulted: true}},
		{"key with user", driver.ConnConfig{DSN: "qdrant://any:s3cret@db.example"}, dsnConfig{host: "db.example", port: 6334, apiKey: "s3cret", tls: true, tlsDefaulted: true}},
		{"password field wins", driver.ConnConfig{DSN: "qdrant://:fromdsn@db.example", Password: driver.NewSecret("override")},
			dsnConfig{host: "db.example", port: 6334, apiKey: "override", tls: true, tlsDefaulted: true}},
		{"key to localhost", driver.ConnConfig{DSN: "qdrant://:k@localhost"}, dsnConfig{host: "localhost", port: 6334, apiKey: "k"}},
		{"key to 127.0.0.1", driver.ConnConfig{DSN: "qdrant://:k@127.0.0.1:7"}, dsnConfig{host: "127.0.0.1", port: 7, apiKey: "k"}},
		{"key to ::1", driver.ConnConfig{DSN: "qdrant://:k@[::1]"}, dsnConfig{host: "::1", port: 6334, apiKey: "k"}},
		{"key to remote, explicit plaintext", driver.ConnConfig{DSN: "qdrant://:k@db.example?tls=false&allow_plaintext=1"},
			dsnConfig{host: "db.example", port: 6334, apiKey: "k", plaintextKey: true}},
		{"key to remote, plaintext option", driver.ConnConfig{DSN: "qdrant://:k@db.example", Options: map[string]string{"tls": "false", "allow_plaintext": "true"}},
			dsnConfig{host: "db.example", port: 6334, apiKey: "k", plaintextKey: true}},
		{"key to remote, tls=true", driver.ConnConfig{DSN: "qdrant://:k@db.example?tls=true"}, dsnConfig{host: "db.example", port: 6334, apiKey: "k", tls: true}},
		{"tls", driver.ConnConfig{DSN: "qdrant://db.example?tls=true"}, dsnConfig{host: "db.example", port: 6334, tls: true}},
		{"tls off", driver.ConnConfig{DSN: "qdrant://db.example?tls=false&allow_plaintext=1"}, dsnConfig{host: "db.example", port: 6334}},
		{"allow_plaintext alone keeps TLS", driver.ConnConfig{DSN: "qdrant://db.example?allow_plaintext=1"}, dsnConfig{host: "db.example", port: 6334, tls: true, tlsDefaulted: true}},
		{"tls off on loopback", driver.ConnConfig{DSN: "qdrant://localhost?tls=false"}, dsnConfig{host: "localhost", port: 6334}},
		{"tls option", driver.ConnConfig{DSN: "qdrant://db.example", Options: map[string]string{"tls": "1"}}, dsnConfig{host: "db.example", port: 6334, tls: true}},
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

func TestParseDSNRefusals(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  driver.ConnConfig
		kind error
	}{
		{"wrong scheme", driver.ConnConfig{DSN: "postgres://x"}, driver.ErrSyntax},
		{"bad port", driver.ConnConfig{DSN: "qdrant://h:notaport"}, driver.ErrSyntax},
		{"port out of range", driver.ConnConfig{DSN: "qdrant://h:70000"}, driver.ErrSyntax},
		{"api_key param", driver.ConnConfig{DSN: "qdrant://h?api_key=topsecretvalue"}, driver.ErrSyntax},
		{"unknown param", driver.ConnConfig{DSN: "qdrant://h?pool=3"}, driver.ErrSyntax},
		{"bad tls", driver.ConnConfig{DSN: "qdrant://h?tls=maybe"}, driver.ErrSyntax},
		{"remote plaintext without opt-in", driver.ConnConfig{DSN: "qdrant://h?tls=false"}, driver.ErrSyntax},
		{"remote plaintext, allow_plaintext=0", driver.ConnConfig{DSN: "qdrant://:k@h?tls=false&allow_plaintext=0"}, driver.ErrSyntax},
		{"bad allow_plaintext", driver.ConnConfig{DSN: "qdrant://h?allow_plaintext=maybe"}, driver.ErrSyntax},
		{"qdrant cloud in clear", driver.ConnConfig{DSN: "qdrant://xyz.eu-central-1-0.aws.cloud.qdrant.io?tls=false&allow_plaintext=1"}, driver.ErrSyntax},
		{"qdrant cloud in clear, odd case", driver.ConnConfig{DSN: "qdrant://XYZ.us-east4-0.GCP.Cloud.Qdrant.IO.?tls=false&allow_plaintext=1"}, driver.ErrSyntax},
		{"unknown option", driver.ConnConfig{DSN: "qdrant://h", Options: map[string]string{"sslmode": "x"}}, driver.ErrSyntax},
		{"path", driver.ConnConfig{DSN: "qdrant://h/coll"}, driver.ErrSyntax},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseDSN(tt.cfg)
			if !errors.Is(err, tt.kind) {
				t.Fatalf("err = %v, want %v", err, tt.kind)
			}
			if strings.Contains(err.Error(), "topsecretvalue") {
				t.Errorf("error leaks the key: %v", err)
			}
		})
	}
}

// TestOpenRedactsKey opens against a closed port: the error must be a
// connection error that never shows the API key, from the DSN or the
// password field.
func TestOpenRedactsKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "qdrant://:dsn-secret-key@127.0.0.1:1"},
		{DSN: "qdrant://127.0.0.1:1", Password: driver.NewSecret("field-secret-key")},
		{DSN: "qdrant://:dsn-secret-key@127.0.0.1:1/bad"},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%s) succeeded against a closed port", driver.RedactDSN(cfg.DSN))
		}
		if msg := err.Error(); strings.Contains(msg, "dsn-secret-key") || strings.Contains(msg, "field-secret-key") {
			t.Errorf("Open error leaks the API key: %v", msg)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("Open error is not a qdrant *driver.Error: %v", err)
		}
	}
}

func TestOpenUnreachableIsConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "qdrant://127.0.0.1:1"})
	if !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("err = %v, want ErrConnection", err)
	}
}

// The plaintext refusals say what to do.
func TestPlaintextRuleMessages(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"qdrant://h?tls=false", "allow_plaintext=1"},
		{"qdrant://h?allow_plaintext=maybe", "allow_plaintext must be"},
		{"qdrant://xyz.eu-central-1-0.aws.cloud.qdrant.io?tls=false&allow_plaintext=1", "Qdrant Cloud"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDSN(%q) = %v, want an error containing %q", tc.dsn, err, tc.want)
		}
	}
}
