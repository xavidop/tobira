package snowflake

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A 307 is never followed: Go replays a POST body (the login request holds
// the password) on a 307 or 308 to any host, and keeps Authorization on a
// redirect to the same host name, so the second server must never see a
// request.
func TestRedirectIsNotFollowed(t *testing.T) {
	const pw = "s3cr3t-passw0rd"
	var hits, creds atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), pw) || r.Header.Get("Authorization") != "" {
			creds.Add(1)
		}
		writeJSON(w, map[string]any{"success": true, "data": map[string]any{"token": "t", "masterToken": "m"}})
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	u, _ := url.Parse(redirector.URL)
	dsn := "snowflake://ada@acct/DB/PUBLIC?protocol=http&host=" + u.Hostname() + "&port=" + u.Port()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(pw)})
	if err == nil {
		_ = c.Close()
		t.Error("Open through a redirect succeeded")
	}
	if err != nil && strings.Contains(err.Error(), pw) {
		t.Errorf("the error carries the password: %v", err)
	}
	if n := creds.Load(); n != 0 {
		t.Errorf("the redirect target got credentials %d times", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

// Plain http to a host that is not loopback needs allow_plaintext=1, and
// Snowflake's own endpoints are never reached in clear.
func TestPlaintextRule(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"snowflake://u@a?host=sf.internal&protocol=http", "allow_plaintext=1"},
		{"snowflake://u@a?host=sf.internal&protocol=http&allow_plaintext=0", "allow_plaintext=1"},
		{"snowflake://u@a?allow_plaintext=maybe", "allow_plaintext must be"},
		{"snowflake://u@a?protocol=http&allow_plaintext=1", "Snowflake"},
		{"snowflake://u@a?host=xy.privatelink.snowflakecomputing.com&protocol=http&allow_plaintext=1", "Snowflake"},
		{"snowflake://u@a?host=XY.SnowflakeComputing.COM.&protocol=http&allow_plaintext=1", "Snowflake"},
		{"snowflake://u@a?host=xy.cn-northwest-1.aws.snowflakecomputing.cn&protocol=http&allow_plaintext=1", "Snowflake"},
	} {
		_, err := parseDSN(tc.dsn)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDSN(%q) = %v, want an error containing %q", tc.dsn, err, tc.want)
		}
	}
	for _, dsn := range []string{
		"snowflake://u@a?host=sf.internal&protocol=http&allow_plaintext=1",
		"snowflake://u@a?host=localhost&protocol=http",
		"snowflake://u@a?allow_plaintext=1", // alone it keeps https
	} {
		if _, err := parseDSN(dsn); err != nil {
			t.Errorf("parseDSN(%q): %v", dsn, err)
		}
	}
	_, _, err := buildConfig(driver.ConnConfig{DSN: "snowflake://u@a?host=sf.internal&protocol=http", Password: driver.NewSecret("x")})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("buildConfig(remote http without the opt-in) = %v, want ErrSyntax", err)
	}
}
