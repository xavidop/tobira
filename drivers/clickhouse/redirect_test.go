package clickhouse

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

// credentialServer counts requests and those carrying a ClickHouse
// credential (X-ClickHouse-Key, or basic auth).
func credentialServer(t *testing.T, tls bool) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits, creds atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if _, _, ok := r.BasicAuth(); ok || r.Header.Get("X-ClickHouse-Key") != "" {
			creds.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewUnstartedServer(h)
	if tls {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, &hits, &creds
}

// No redirect is ever followed over HTTP: Go forwards custom headers such as
// X-ClickHouse-Key (the password over TLS) to any host, from https to http
// included, so the second server must never see a request.
func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tls    bool
		params string
	}{
		{"https", true, "?protocol=http&secure=true&skip_verify=true"},
		{"http", false, "?protocol=http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, hits, creds := credentialServer(t, false) // a plain-http target: the downgrade case
			redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			})
			srv := httptest.NewUnstartedServer(redirect)
			if tc.tls {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			t.Cleanup(srv.Close)
			host := strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := New().Open(ctx, driver.ConnConfig{DSN: "clickhouse://default@" + host + "/db" + tc.params, Password: driver.NewSecret("s3cr3t-pw")})
			if err == nil {
				_ = c.Close()
				t.Error("Open through a redirect succeeded")
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t-pw") {
				t.Errorf("the error carries the password: %v", err)
			}
			if n := creds.Load(); n != 0 {
				t.Errorf("the redirect target got credentials %d times", n)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("the redirect target got %d requests", n)
			}
		})
	}
}

// Plain HTTP to a host that is not loopback needs allow_plaintext=1, and
// ClickHouse Cloud is never reached in clear. The parameter never becomes
// a server setting.
func TestHTTPPlaintextRule(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"clickhouse://u@ch.internal/db?protocol=http", "allow_plaintext=1"},
		{"clickhouse://u@localhost,ch.internal/db?protocol=http", "allow_plaintext=1"},
		{"clickhouse://u@ch.internal/db?protocol=http&allow_plaintext=0", "allow_plaintext=1"},
		{"clickhouse://u@ch.internal/db?protocol=http&allow_plaintext=maybe", "allow_plaintext must be"},
		{"clickhouse://u@abc.us-east-1.aws.clickhouse.cloud/db?protocol=http&allow_plaintext=1", "ClickHouse Cloud"},
		{"clickhouse://u@ABC.eu-west-1.AWS.ClickHouse.Cloud./db?protocol=http&allow_plaintext=1", "ClickHouse Cloud"},
	} {
		_, err := buildOptions(driver.ConnConfig{DSN: tc.dsn})
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("buildOptions(%q) = %v, want ErrSyntax containing %q", tc.dsn, err, tc.want)
		}
	}
	for _, dsn := range []string{
		"clickhouse://u@ch.internal/db?protocol=http&allow_plaintext=1",
		"clickhouse://u@ch.internal/db?protocol=http&secure=true",
		"clickhouse://u@localhost/db?protocol=http",
		"clickhouse://u@127.0.0.1,[::1]/db?protocol=http",
		"clickhouse://u@ch.internal/db?allow_plaintext=1", // native: the parameter is accepted and dropped
		"clickhouse://u@ch.internal/db",                   // native without a password sends no credential
	} {
		o, err := buildOptions(driver.ConnConfig{DSN: dsn})
		if err != nil {
			t.Errorf("buildOptions(%q): %v", dsn, err)
			continue
		}
		for k := range o.Settings {
			if strings.EqualFold(k, "allow_plaintext") {
				t.Errorf("buildOptions(%q): allow_plaintext leaked into the server settings", dsn)
			}
		}
	}
}

// A password over the plaintext native protocol to a host that is not
// loopback needs allow_plaintext=1 (or secure=true), the same rule as plain
// HTTP; ClickHouse Cloud is never reached in clear. A connection without a
// password sends no credential, so it is left alone.
func TestNativePlaintextRule(t *testing.T) {
	pw := driver.NewSecret("s3cr3t-pw")
	for _, tc := range []struct {
		cfg  driver.ConnConfig
		want string
	}{
		{driver.ConnConfig{DSN: "clickhouse://u:pw@ch.internal/db"}, "allow_plaintext=1"},
		{driver.ConnConfig{DSN: "clickhouse://u@ch.internal/db", Password: pw}, "allow_plaintext=1"},
		{driver.ConnConfig{DSN: "clickhouse://u:pw@ch.internal/db?protocol=native"}, "allow_plaintext=1"},
		{driver.ConnConfig{DSN: "clickhouse://u:pw@localhost,ch.internal/db"}, "allow_plaintext=1"},
		{driver.ConnConfig{DSN: "clickhouse://u:pw@ch.internal/db?allow_plaintext=0"}, "allow_plaintext=1"},
		{driver.ConnConfig{DSN: "clickhouse://u:pw@abc.us-east-1.aws.clickhouse.cloud/db?allow_plaintext=1"}, "ClickHouse Cloud"},
	} {
		_, err := buildOptions(tc.cfg)
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("buildOptions(%q) = %v, want ErrSyntax containing %q", tc.cfg.DSN, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "s3cr3t-pw") || strings.Contains(err.Error(), ":pw@")) {
			t.Errorf("buildOptions(%q): the error carries the password: %v", tc.cfg.DSN, err)
		}
	}
	for _, cfg := range []driver.ConnConfig{
		{DSN: "clickhouse://u:pw@ch.internal/db?allow_plaintext=1"},
		{DSN: "clickhouse://u@ch.internal/db?allow_plaintext=true", Password: pw},
		{DSN: "clickhouse://u:pw@ch.internal/db?secure=true"},
		{DSN: "clickhouse://u:pw@localhost/db"},
		{DSN: "clickhouse://u@127.0.0.1,[::1]/db", Password: pw},
		{DSN: "clickhouse://u@ch.internal/db"},
		{DSN: "clickhouse://ch.internal/db"},
	} {
		if _, err := buildOptions(cfg); err != nil {
			t.Errorf("buildOptions(%q): %v", cfg.DSN, err)
		}
	}
}
