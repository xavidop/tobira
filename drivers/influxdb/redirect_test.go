package influxdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// A redirect is never followed: Go keeps Authorization on a redirect to the
// same host name (another port, or https to http), so the token must never
// reach the second server.
func TestRedirectIsNotFollowed(t *testing.T) {
	var hits, authed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			authed.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: "influxdb3://" + strings.TrimPrefix(redirector.URL, "http://") + "/db", Password: driver.NewSecret("s3cr3t-t0ken")})
	if err == nil {
		_ = c.Close()
		t.Error("Open through a redirect succeeded")
	}
	if err != nil && strings.Contains(err.Error(), "s3cr3t-t0ken") {
		t.Errorf("the error carries the token: %v", err)
	}
	if n := authed.Load(); n != 0 {
		t.Errorf("the redirect target got the token %d times", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}
