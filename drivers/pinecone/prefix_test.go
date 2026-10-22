package pinecone

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// An Open error names the driver and the operation once, and its kind
// once: redact rebuilds the message from the inner error, not from the
// rendered one.
func TestOpenErrorHasOnePrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"bad key"}}`))
	}))
	defer srv.Close()
	dsn := "pinecone://" + strings.TrimPrefix(srv.URL, "http://") + "?tls=false"
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("wrong-key-123")})
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("Open = %v, want ErrAuth", err)
	}
	msg := err.Error()
	if strings.Count(msg, "pinecone:") != 1 || strings.Count(msg, "ping:") != 1 || strings.Count(msg, "authentication") > 1 {
		t.Errorf("Open error repeats its prefix or kind: %q", msg)
	}
}
