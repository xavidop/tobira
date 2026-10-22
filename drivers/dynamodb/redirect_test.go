package dynamodb

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

// No redirect is ever followed. Go turns a POST into a GET on a 301, 302 or
// 303 and follows it, keeping Authorization (the SigV4 signature) on the
// same host name and forwarding X-Amz-Security-Token to any host; a 307 or
// 308 is followed whenever the body can be replayed. The second server must
// never see a request.
func TestRedirectIsNotFollowed(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits, creds atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Amz-Security-Token") != "" {
					creds.Add(1)
				}
				w.Header().Set("Content-Type", "application/x-amz-json-1.0")
				_, _ = w.Write([]byte(`{"TableNames":[]}`))
			}))
			t.Cleanup(target.Close)
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, status)
			}))
			t.Cleanup(redirector.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := New().Open(ctx, driver.ConnConfig{
				DSN:      "dynamodb://AKIDEXAMPLE@us-east-1?endpoint=" + redirector.URL,
				Password: driver.NewSecret("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"),
			})
			if err == nil {
				_ = c.Close()
				t.Error("Open through a redirect succeeded")
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

// Plain http to a host that is not loopback needs allow_plaintext=1, and
// AWS's own endpoints are never reached in clear.
func TestPlaintextRule(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"dynamodb://us-east-1?endpoint=http://dynamo.internal:8000", "allow_plaintext=1"},
		{"dynamodb://us-east-1?endpoint=http://dynamo.internal:8000&allow_plaintext=0", "allow_plaintext=1"},
		{"dynamodb://us-east-1?allow_plaintext=maybe", "allow_plaintext must be"},
		{"dynamodb://us-east-1?endpoint=http://dynamodb.us-east-1.amazonaws.com&allow_plaintext=1", "AWS"},
		{"dynamodb://us-east-1?endpoint=http://DynamoDB.US-East-1.AmazonAWS.com.&allow_plaintext=1", "AWS"},
		{"dynamodb://cn-north-1?endpoint=http://dynamodb.cn-north-1.amazonaws.com.cn&allow_plaintext=1", "AWS"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn})
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDSN(%q) = %v, want ErrSyntax containing %q", tc.dsn, err, tc.want)
		}
	}
	for _, dsn := range []string{
		"dynamodb://us-east-1?endpoint=http://dynamo.internal:8000&allow_plaintext=1",
		"dynamodb://us-east-1?allow_plaintext=1&endpoint=http://localhost:8000",
		"dynamodb://us-east-1?allow_plaintext=1",
		"dynamodb://us-east-1?endpoint=http://LOCALHOST:8000",
		"dynamodb://us-east-1?endpoint=http://[::1]:8000",
	} {
		if _, err := parseDSN(driver.ConnConfig{DSN: dsn}); err != nil {
			t.Errorf("parseDSN(%q): %v", dsn, err)
		}
	}
}
