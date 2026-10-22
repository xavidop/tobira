package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/core/status"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
)

func TestProviderErrorRedactsKey(t *testing.T) {
	key := "sk-Te5t/k3y+with=odd&chars"
	forms := []string{key, url.QueryEscape(key), url.PathEscape(key)}
	raw := fmt.Errorf("POST https://api.example/v1?key=%s: 401 bad key %s (escaped %s)", forms[1], forms[0], forms[2])
	err := wrapProviderError(raw, driver.NewSecret(key))
	for _, f := range forms {
		if strings.Contains(err.Error(), f) {
			t.Fatalf("error %q contains the key form %q", err, f)
		}
	}
	if errors.Is(err, raw) {
		t.Fatal("the unredacted original must not be reachable through errors.Is/As")
	}
	var target interface{ Unwrap() error }
	if errors.As(err, &target) {
		if u := target.Unwrap(); u != nil && strings.Contains(u.Error(), "odd&chars") {
			t.Fatalf("Unwrap exposes the key: %q", u)
		}
	}
}

// A short key is still masked: the generic redactor skips secrets under four
// characters, but a resolved API key is always masked.
func TestProviderErrorRedactsShortKey(t *testing.T) {
	err := wrapProviderError(errors.New("bad key k3y"), driver.NewSecret("k3y"))
	if strings.Contains(err.Error(), "k3y") {
		t.Fatalf("error %q contains the short key", err)
	}
}

func TestProviderErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"unauthenticated", status.Errorf(status.ErrUnauthenticated, "nope"), assistant.ErrAuth},
		{"permission denied", status.Errorf(status.ErrPermissionDenied, "nope"), assistant.ErrAuth},
		{"rate limited", status.Errorf(status.ErrResourceExhausted, "slow down"), assistant.ErrRateLimit},
		{"unavailable", status.Errorf(status.ErrUnavailable, "down"), assistant.ErrUnavailable},
		{"dial error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, assistant.ErrUnavailable},
		{"url error", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("no such host")}, assistant.ErrUnavailable},
		{"bad model", status.Errorf(status.ErrNotFound, "model not found"), assistant.ErrProvider},
		{"plain", errors.New("boom"), assistant.ErrProvider},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := wrapProviderError(tt.err, driver.Secret{}); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestProviderErrorKeepsCancel(t *testing.T) {
	err := wrapProviderError(fmt.Errorf("generate: %w", context.Canceled), driver.Secret{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if wrapProviderError(nil, driver.Secret{}) != nil {
		t.Fatal("nil must stay nil")
	}
}
