package firestore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

func TestMapErrorCodes(t *testing.T) {
	tests := []struct {
		op   string
		code codes.Code
		want error
	}{
		{"query", codes.NotFound, driver.ErrNotFound},
		{"insert", codes.AlreadyExists, driver.ErrConflict},
		{"query", codes.PermissionDenied, driver.ErrPermission},
		{"open", codes.Unauthenticated, driver.ErrAuth},
		{"query", codes.InvalidArgument, driver.ErrSyntax},
		{"query", codes.FailedPrecondition, driver.ErrUnsupported}, // a missing composite index
		{"browse", codes.FailedPrecondition, driver.ErrUnsupported},
		{"update", codes.FailedPrecondition, driver.ErrConflict}, // the document changed since it was read
		{"delete", codes.FailedPrecondition, driver.ErrConflict},
		{"update", codes.Aborted, driver.ErrConflict},
		{"ping", codes.Unavailable, driver.ErrConnection},
		{"query", codes.Unimplemented, driver.ErrUnsupported},
		{"query", codes.ResourceExhausted, nil},
		{"query", codes.Internal, nil},
	}
	for _, tt := range tests {
		err := mapError(context.Background(), tt.op, status.Error(tt.code, "boom"))
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Op != tt.op {
			t.Errorf("%s %v: want a *driver.Error from %s/%s, got %#v", tt.op, tt.code, Name, tt.op, err)
			continue
		}
		if driver.KindOf(err) != tt.want {
			t.Errorf("%s %v: kind %v, want %v", tt.op, tt.code, driver.KindOf(err), tt.want)
		}
		if de.Code != tt.code.String() {
			t.Errorf("%s %v: Code %q, want %q", tt.op, tt.code, de.Code, tt.code.String())
		}
	}
}

// Only a FailedPrecondition about an index gets the index hint.
func TestMapErrorIndexHint(t *testing.T) {
	err := mapError(context.Background(), "query", status.Error(codes.FailedPrecondition, "The query requires an index. You can create it here: https://console.firebase.google.com/x"))
	if !strings.Contains(err.Error(), "composite index") {
		t.Errorf("no hint: %v", err)
	}
	err = mapError(context.Background(), "query", status.Error(codes.FailedPrecondition, "Firestore does not support descending key scans"))
	if strings.Contains(err.Error(), "composite index") || !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("hint on another precondition: %v", err)
	}
}

func TestMapErrorContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, in := range []error{status.Error(codes.Canceled, "context canceled"), status.Error(codes.Unavailable, "x"), context.Canceled} {
		err := mapError(ctx, "query", in)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("mapError(%v) with a cancelled ctx: want context.Canceled, got %v", in, err)
		}
		if driver.KindOf(err) != nil {
			t.Errorf("mapError(%v) with a cancelled ctx: kind %v, want none", in, driver.KindOf(err))
		}
	}
	if err := mapError(context.Background(), "query", context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded, got %v", err)
	}
	if err := mapError(context.Background(), "query", io.ErrUnexpectedEOF); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("want ErrConnection, got %v", err)
	}
	if mapError(context.Background(), "query", nil) != nil {
		t.Error("mapError(nil) != nil")
	}
	pre := driver.Wrap(driver.ErrSyntax, Name, "query", errors.New("x"))
	if mapError(context.Background(), "browse", pre) != pre {
		t.Error("mapError rewrapped a *driver.Error")
	}
}
