package bigquery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/api/option"

	"github.com/xavidop/tobira/driver"
)

// countingServer counts the requests it gets, and those that carry a
// credential (an Authorization header, or a JWT assertion in the body).
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits, creds atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = r.ParseForm()
		if r.Header.Get("Authorization") != "" || r.PostForm.Get("assertion") != "" {
			creds.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stolen","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &creds
}

// redirectTo answers every request with a 307 to target, keeping the path.
func redirectTo(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A 307 from the API endpoint is never followed: the auth transport adds
// the bearer token to every request it sends, the redirected one included,
// so the second server must never see a request.
func TestAPIRedirectIsNotFollowed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.s3cr3t-access-token","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenSrv.Close)
	target, hits, creds := countingServer(t)
	api := redirectTo(t, target.URL)
	pw := serviceAccountJSON(t, testServiceAccountKey(t), tokenSrv.URL)
	c, err := openWith(context.Background(), driver.ConnConfig{DSN: "bigquery://p/ds", Password: driver.NewSecret(pw)}, option.WithEndpoint(api.URL))
	if err == nil {
		_ = c.Close()
		t.Error("Open through a redirect succeeded")
	}
	if err != nil && strings.Contains(err.Error(), "s3cr3t-access-token") {
		t.Errorf("the error carries the access token: %v", err)
	}
	if n := creds.Load(); n != 0 {
		t.Errorf("the redirect target got a credential %d times", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

// A 307 from the token endpoint is never followed either: the signed JWT
// assertion must not be replayed to another server.
func TestTokenRedirectIsNotFollowed(t *testing.T) {
	_, apiSrv := newFakeAPI(t)
	target, hits, creds := countingServer(t)
	tokenSrv := redirectTo(t, target.URL)
	pw := serviceAccountJSON(t, testServiceAccountKey(t), tokenSrv.URL+"/token")
	c, err := openWith(context.Background(), driver.ConnConfig{DSN: "bigquery://p/ds", Password: driver.NewSecret(pw)}, option.WithEndpoint(apiSrv.URL))
	if err == nil {
		_ = c.Close()
		t.Error("Open through a redirecting token endpoint succeeded")
	}
	if n := creds.Load(); n != 0 {
		t.Errorf("the redirect target got the assertion %d times", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}
