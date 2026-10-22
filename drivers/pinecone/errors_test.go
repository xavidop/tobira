package pinecone

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

func TestMapError(t *testing.T) {
	bg := context.Background()
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"unauthenticated", status.Error(codes.Unauthenticated, "bad key"), driver.ErrAuth},
		{"permission", status.Error(codes.PermissionDenied, "no"), driver.ErrPermission},
		{"not found", status.Error(codes.NotFound, "x"), driver.ErrNotFound},
		{"invalid", status.Error(codes.InvalidArgument, "Vector dimension 3 does not match"), driver.ErrSyntax},
		{"exists", status.Error(codes.AlreadyExists, "x"), driver.ErrConflict},
		{"unavailable", status.Error(codes.Unavailable, "x"), driver.ErrConnection},
		{"unimplemented", status.Error(codes.Unimplemented, ""), driver.ErrUnsupported},
		{"rest 401", &pinecone.PineconeError{Code: 401, Msg: errors.New("unauthorized")}, driver.ErrAuth},
		{"rest 403", &pinecone.PineconeError{Code: 403, Msg: errors.New("forbidden")}, driver.ErrPermission},
		{"rest 404", &pinecone.PineconeError{Code: 404, Msg: errors.New("NOT_FOUND")}, driver.ErrNotFound},
		{"rest 400", &pinecone.PineconeError{Code: 400, Msg: errors.New("bad")}, driver.ErrSyntax},
		{"rest 409", &pinecone.PineconeError{Code: 409, Msg: errors.New("exists")}, driver.ErrConflict},
		{"rest 503", &pinecone.PineconeError{Code: 503, Msg: errors.New("down")}, driver.ErrConnection},
		{"dial", &url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, driver.ErrConnection},
	} {
		got := mapError(bg, "query", tc.err)
		if !errors.Is(got, tc.want) {
			t.Errorf("%s: mapError = %v, want kind %v", tc.name, got, tc.want)
		}
		var de *driver.Error
		if !errors.As(got, &de) || de.Driver != Name || de.Op != "query" {
			t.Errorf("%s: not a pinecone driver.Error: %#v", tc.name, got)
		}
	}
	if mapError(bg, "x", nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
	// A plain error has no kind.
	if k := driver.KindOf(mapError(bg, "x", errors.New("odd"))); k != nil {
		t.Errorf("plain error kind = %v", k)
	}
	// A driver.Error passes through.
	in := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("ro"))
	if mapError(bg, "other", in) != in {
		t.Error("driver.Error was rewrapped")
	}
}

// When our ctx is done, the error carries the ctx error, whatever the
// transport said.
func TestMapErrorContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mapError(ctx, "query", status.Error(codes.Canceled, "context canceled"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	err = mapError(context.Background(), "query", fmt.Errorf("wrapped: %w", context.DeadlineExceeded))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: %v", err)
	}
}

// Errors from a conn are redacted with its config: the key never shows.
func TestConnErrorRedacted(t *testing.T) {
	c := &conn{cfg: driver.ConnConfig{DSN: "pinecone://?api_key=k3y-s3cr3t"}, apiKey: "k3y-s3cr3t"}
	err := c.fail(context.Background(), "query", status.Error(codes.Unauthenticated, "bad key k3y-s3cr3t"))
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("kind lost: %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "k3y-s3cr3t") {
		t.Errorf("error leaks the key: %s", msg)
	}
}

// A key with reserved characters is masked in its URL-escaped form too.
func TestConnErrorRedactsEscapedKey(t *testing.T) {
	const key = "pc/k3y+s3cr3t=="
	c := &conn{cfg: driver.ConnConfig{Password: driver.NewSecret(key)}, apiKey: key}
	for _, msg := range []string{"bad key " + url.QueryEscape(key), "bad key " + url.PathEscape(key), "bad key " + key} {
		err := c.fail(context.Background(), "query", status.Error(codes.Unauthenticated, msg))
		if strings.Contains(err.Error(), "k3y") {
			t.Errorf("error leaks the key: %s", err)
		}
		if !errors.Is(err, driver.ErrAuth) {
			t.Errorf("kind lost: %v", err)
		}
	}
}
