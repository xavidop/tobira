package milvus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

func TestMapError(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"grpc unauthenticated", status.Error(codes.Unauthenticated, "auth check failure"), driver.ErrAuth},
		{"grpc permission", status.Error(codes.PermissionDenied, "no"), driver.ErrPermission},
		{"grpc unavailable", status.Error(codes.Unavailable, "down"), driver.ErrConnection},
		{"grpc unimplemented", status.Error(codes.Unimplemented, "old server"), driver.ErrUnsupported},
		{"grpc resource exhausted", status.Error(codes.ResourceExhausted, "received message larger than max"), driver.ErrLimit},
		{"parameter too large", statusErr(1102), driver.ErrLimit},
		{"collection not found", statusErr(100), driver.ErrNotFound},
		{"database not found", statusErr(800), driver.ErrNotFound},
		{"partition not found", statusErr(200), driver.ErrNotFound},
		{"bad parameter", statusErr(1100), driver.ErrSyntax},
		{"field not found", statusErr(1700), driver.ErrSyntax},
		{"not permitted", statusErr(1401), driver.ErrPermission},
		{"not authenticated", statusErr(1400), driver.ErrAuth},
		{"rate limit", statusErr(8), nil},
		{"service unavailable", statusErr(2), driver.ErrConnection},
		{"legacy collection missing", checkRPC(&commonpb.Status{ErrorCode: commonpb.ErrorCode_CollectionNotExists, Reason: "gone"}, nil), driver.ErrNotFound}, //nolint:staticcheck // the legacy code path
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mapError(ctx, "query", tc.err)
			var de *driver.Error
			if !errors.As(got, &de) || de.Driver != Name || de.Op != "query" {
				t.Fatalf("mapError = %#v, want a milvus driver.Error for op query", got)
			}
			if tc.want == nil {
				if driver.KindOf(got) != nil {
					t.Errorf("mapError kind = %v, want none", driver.KindOf(got))
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("mapError(%v) = %v, want kind %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMapErrorKeepsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := mapError(ctx, "query", status.Error(codes.Canceled, "context canceled"))
	if !errors.Is(got, context.Canceled) {
		t.Errorf("mapError on a cancelled ctx = %v, want context.Canceled", got)
	}
	if driver.KindOf(got) != nil {
		t.Errorf("a cancellation has no kind, got %v", driver.KindOf(got))
	}
}

func TestMapErrorPassesDriverErrors(t *testing.T) {
	in := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
	if got := mapError(context.Background(), "other", in); got != in {
		t.Errorf("mapError rewrapped a driver.Error: %v", got)
	}
}

func TestNotLoaded(t *testing.T) {
	if !notLoaded(statusErr(101)) {
		t.Error("collection not loaded must be recognised")
	}
	if !notLoaded(statusErr(201)) {
		t.Error("partition not loaded must be recognised")
	}
	if notLoaded(statusErr(100)) || notLoaded(nil) || notLoaded(errors.New("collection not loaded")) {
		t.Error("only the Milvus not-loaded codes count")
	}
}

// statusErr is the error checkRPC returns for a failed status with code.
func statusErr(code int32) error {
	return checkRPC(&commonpb.Status{Code: code, Reason: "boom"}, nil)
}

func TestCheckRPC(t *testing.T) {
	if err := checkRPC(&commonpb.Status{}, nil); err != nil {
		t.Errorf("success status = %v", err)
	}
	transport := errors.New("dial")
	if err := checkRPC(nil, transport); err != transport {
		t.Errorf("a transport error must pass through: %v", err)
	}
	if codeOf(statusErr(101)) != 101 || codeOf(errors.New("x")) != 0 {
		t.Error("codeOf")
	}
}

// A size limit says how to get past it.
func TestLimitHint(t *testing.T) {
	for _, in := range []error{status.Error(codes.ResourceExhausted, "too big"), statusErr(1102)} {
		err := mapError(context.Background(), "query", in)
		if !errors.Is(err, driver.ErrLimit) || !strings.Contains(err.Error(), "outputFields") {
			t.Errorf("mapError(%v) = %v, want ErrLimit with a hint", in, err)
		}
	}
}
