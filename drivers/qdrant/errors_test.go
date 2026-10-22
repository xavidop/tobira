package qdrant

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

func TestMapError(t *testing.T) {
	bg := context.Background()
	for _, tt := range []struct {
		code codes.Code
		kind error
	}{
		{codes.Unauthenticated, driver.ErrAuth},
		{codes.PermissionDenied, driver.ErrPermission},
		{codes.NotFound, driver.ErrNotFound},
		{codes.InvalidArgument, driver.ErrSyntax},
		{codes.AlreadyExists, driver.ErrConflict},
		{codes.Unavailable, driver.ErrConnection},
		{codes.Unimplemented, driver.ErrUnsupported},
		{codes.FailedPrecondition, driver.ErrSyntax},
		{codes.Internal, nil},
	} {
		// The go-client wraps status errors in its own type.
		err := mapError(bg, "query", fmt.Errorf("Scroll() failed: %w", status.Error(tt.code, "msg")))
		var de *driver.Error
		if !errors.As(err, &de) {
			t.Fatalf("%v: not a *driver.Error: %v", tt.code, err)
		}
		if de.Kind != tt.kind || de.Driver != Name || de.Op != "query" || de.Code != tt.code.String() {
			t.Errorf("%v: got kind %v driver %q op %q code %q", tt.code, de.Kind, de.Driver, de.Op, de.Code)
		}
	}
	if mapError(bg, "x", nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
}

func TestMapErrorContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mapError(ctx, "query", status.Error(codes.Canceled, "context canceled"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call: err = %v, want context.Canceled", err)
	}
	if driver.KindOf(err) != nil {
		t.Errorf("cancelled call has kind %v", driver.KindOf(err))
	}
	// Unavailable caused by our own cancellation is not a connection error.
	err = mapError(ctx, "query", status.Error(codes.Unavailable, "closing"))
	if errors.Is(err, driver.ErrConnection) || !errors.Is(err, context.Canceled) {
		t.Errorf("unavailable after cancel: err = %v", err)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 0)
	defer dcancel()
	<-dctx.Done()
	err = mapError(dctx, "query", status.Error(codes.DeadlineExceeded, "deadline"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: err = %v", err)
	}
	// A driver.Error passes through untouched.
	in := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
	if got := mapError(context.Background(), "other", in); got != in {
		t.Errorf("driver.Error rewrapped: %v", got)
	}
}
