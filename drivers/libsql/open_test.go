package libsql

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "libsql" || info.Kind != driver.KindSQL || info.Dialect != "sqlite" {
		t.Fatalf("info = %+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "libsql" {
		t.Errorf("schemes = %v, want only libsql (http/https/ws/wss need an explicit driver)", info.Schemes)
	}
	for _, c := range []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer,
		driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly} {
		if !driver.HasCapability(info, c) {
			t.Errorf("missing capability %s", c)
		}
	}
}

func TestParseDSN(t *testing.T) {
	tok := driver.NewSecret("tok-from-ref")
	tests := []struct {
		name      string
		cfg       driver.ConnConfig
		url       string
		token     string
		plaintext bool
		kind      error
	}{
		{name: "libsql defaults to https", cfg: driver.ConnConfig{DSN: "libsql://db-org.turso.io"}, url: "https://db-org.turso.io"},
		{name: "libsql tls=0", cfg: driver.ConnConfig{DSN: "libsql://localhost:8080?tls=0"}, url: "http://localhost:8080"},
		{name: "libsql tls=false", cfg: driver.ConnConfig{DSN: "libsql://localhost:8080?tls=false"}, url: "http://localhost:8080"},
		{name: "libsql tls=1", cfg: driver.ConnConfig{DSN: "libsql://h?tls=1"}, url: "https://h"},
		{name: "https", cfg: driver.ConnConfig{DSN: "https://h:8443"}, url: "https://h:8443"},
		{name: "http", cfg: driver.ConnConfig{DSN: "http://127.0.0.1:8080/"}, url: "http://127.0.0.1:8080"},
		{name: "path prefix kept", cfg: driver.ConnConfig{DSN: "https://proxy.example/sqld/"}, url: "https://proxy.example/sqld"},
		{name: "wss maps to https", cfg: driver.ConnConfig{DSN: "wss://h"}, url: "https://h"},
		{name: "ws maps to http", cfg: driver.ConnConfig{DSN: "ws://localhost:8080"}, url: "http://localhost:8080"},
		{name: "token from ref", cfg: driver.ConnConfig{DSN: "libsql://h", Password: tok}, url: "https://h", token: "tok-from-ref"},
		{name: "token in password position", cfg: driver.ConnConfig{DSN: "libsql://:dsn-tok@h"}, url: "https://h", token: "dsn-tok"},
		{name: "ref overrides DSN token", cfg: driver.ConnConfig{DSN: "libsql://:dsn-tok@h", Password: tok}, url: "https://h", token: "tok-from-ref"},
		{name: "token over libsql tls=0 to remote host", cfg: driver.ConnConfig{DSN: "libsql://db.example:8080?tls=0&allow_plaintext=1", Password: tok}, url: "http://db.example:8080", token: "tok-from-ref", plaintext: true},
		{name: "no token over plain http to remote host", cfg: driver.ConnConfig{DSN: "http://db.example:8080?allow_plaintext=1"}, url: "http://db.example:8080"},
		{name: "token over http to remote host, opted in", cfg: driver.ConnConfig{DSN: "http://db.example:8080?allow_plaintext=true", Password: tok}, url: "http://db.example:8080", token: "tok-from-ref", plaintext: true},
		{name: "allow_plaintext alone keeps https", cfg: driver.ConnConfig{DSN: "libsql://db.example?allow_plaintext=1"}, url: "https://db.example"},
		{name: "token over plain http to loopback", cfg: driver.ConnConfig{DSN: "http://localhost:8080", Password: tok}, url: "http://localhost:8080", token: "tok-from-ref"},

		{name: "file scheme", cfg: driver.ConnConfig{DSN: "file:/tmp/a.db"}, kind: driver.ErrSyntax},
		{name: "sqlite scheme", cfg: driver.ConnConfig{DSN: "sqlite:/tmp/a.db"}, kind: driver.ErrSyntax},
		{name: "bare path", cfg: driver.ConnConfig{DSN: "/tmp/a.db"}, kind: driver.ErrSyntax},
		{name: "no host", cfg: driver.ConnConfig{DSN: "libsql://"}, kind: driver.ErrSyntax},
		{name: "authToken param", cfg: driver.ConnConfig{DSN: "libsql://h?authToken=abc"}, kind: driver.ErrSyntax},
		{name: "auth_token param", cfg: driver.ConnConfig{DSN: "libsql://h?auth_token=abc"}, kind: driver.ErrSyntax},
		{name: "jwt param", cfg: driver.ConnConfig{DSN: "libsql://h?jwt=abc"}, kind: driver.ErrSyntax},
		{name: "unknown param", cfg: driver.ConnConfig{DSN: "libsql://h?foo=1"}, kind: driver.ErrSyntax},
		{name: "repeated tls", cfg: driver.ConnConfig{DSN: "libsql://h?tls=0&tls=1"}, kind: driver.ErrSyntax},
		{name: "bad tls", cfg: driver.ConnConfig{DSN: "libsql://h?tls=maybe"}, kind: driver.ErrSyntax},
		{name: "tls on https", cfg: driver.ConnConfig{DSN: "https://h?tls=0"}, kind: driver.ErrSyntax},
		{name: "fragment", cfg: driver.ConnConfig{DSN: "libsql://h#x"}, kind: driver.ErrSyntax},
		{name: "token over plain http to remote host", cfg: driver.ConnConfig{DSN: "http://db.example:8080", Password: tok}, kind: driver.ErrSyntax},
		{name: "token over ws to remote host", cfg: driver.ConnConfig{DSN: "ws://db.example:8080", Password: tok}, kind: driver.ErrSyntax},
		{name: "DSN token over plain http to remote host", cfg: driver.ConnConfig{DSN: "http://:dsn-tok@db.example:8080"}, kind: driver.ErrSyntax},
		{name: "plain http to remote host without opt-in", cfg: driver.ConnConfig{DSN: "http://db.example:8080"}, kind: driver.ErrSyntax},
		{name: "libsql tls=0 to remote host without opt-in", cfg: driver.ConnConfig{DSN: "libsql://db.example:8080?tls=0", Password: tok}, kind: driver.ErrSyntax},
		{name: "allow_plaintext=0", cfg: driver.ConnConfig{DSN: "ws://db.example:8080?allow_plaintext=0"}, kind: driver.ErrSyntax},
		{name: "bad allow_plaintext", cfg: driver.ConnConfig{DSN: "libsql://h?allow_plaintext=maybe"}, kind: driver.ErrSyntax},
		{name: "turso over plain http", cfg: driver.ConnConfig{DSN: "libsql://db-org.turso.io?tls=0&allow_plaintext=1"}, kind: driver.ErrSyntax},
		{name: "turso over http://", cfg: driver.ConnConfig{DSN: "http://DB-Org.AWS-US-East-1.Turso.IO.?allow_plaintext=1"}, kind: driver.ErrSyntax},
		{name: "unknown option", cfg: driver.ConnConfig{DSN: "libsql://h", Options: map[string]string{"busy_timeout": "1"}}, kind: driver.ErrSyntax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := parseDSN(tt.cfg)
			if tt.kind != nil {
				if !errors.Is(err, tt.kind) {
					t.Fatalf("got %+v, %v; want %v", d, err, tt.kind)
				}
				for _, secret := range []string{"abc", "dsn-tok", "tok-from-ref"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error %q leaks %q", err, secret)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.url != tt.url || d.token != tt.token || d.plaintextToken != tt.plaintext {
				t.Errorf("got url=%q token=%q plaintext=%v; want %q %q %v", d.url, d.token, d.plaintextToken, tt.url, tt.token, tt.plaintext)
			}
		})
	}
}

// A token sent without TLS to a remote host is logged as a warning through
// ConnConfig.Logger, never to stderr, and never with the token.
func TestPlaintextTokenWarning(t *testing.T) {
	var buf bytes.Buffer
	cfg := driver.ConnConfig{
		Name:     "edge",
		DSN:      "libsql://libsql.invalid:8080?tls=0&allow_plaintext=1",
		Password: driver.NewSecret("s3cr3t-token"),
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	out := drivertest.Stderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Error("Open on an unresolvable host succeeded")
			return
		}
		if strings.Contains(err.Error(), "s3cr3t-token") {
			t.Errorf("open error leaks the token: %v", err)
		}
		if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("open error kind: %v, want ErrConnection", err)
		}
	})
	if out != "" {
		t.Errorf("Open wrote to stderr:\n%s", out)
	}
	if !strings.Contains(buf.String(), "without TLS") {
		t.Errorf("logger got %q, want the plaintext warning", buf.String())
	}
	if strings.Contains(buf.String(), "s3cr3t-token") {
		t.Errorf("the warning carries the token: %q", buf.String())
	}
}

// The plaintext refusals say what to do.
func TestPlaintextRuleMessages(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"http://db.example:8080", "allow_plaintext=1"},
		{"libsql://db.example:8080?tls=0", "allow_plaintext=1"},
		{"libsql://db-org.turso.io?tls=0&allow_plaintext=1", "Turso"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDSN(%q) = %v, want an error containing %q", tc.dsn, err, tc.want)
		}
	}
}
