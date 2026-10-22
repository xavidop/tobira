package influxdb

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
	grpcErr := func(c codes.Code, msg string) error {
		return fmt.Errorf("flight reader: %w", status.Error(c, msg))
	}
	for _, tc := range []struct {
		err  error
		kind error
		code string
	}{
		{grpcErr(codes.Unauthenticated, "bad token"), driver.ErrAuth, "Unauthenticated"},
		{grpcErr(codes.PermissionDenied, "no"), driver.ErrPermission, "PermissionDenied"},
		{grpcErr(codes.Unavailable, "connection refused"), driver.ErrConnection, "Unavailable"},
		{grpcErr(codes.InvalidArgument, "Error during planning: table 'public.iox.nope' not found"), driver.ErrNotFound, "InvalidArgument"},
		{grpcErr(codes.InvalidArgument, "Schema error: No field named x."), driver.ErrNotFound, "InvalidArgument"},
		{grpcErr(codes.Internal, "Cannot retrieve database: database not found: x"), driver.ErrNotFound, "Internal"},
		{grpcErr(codes.InvalidArgument, `SQL error: ParserError("Expected: an SQL statement")`), driver.ErrSyntax, "InvalidArgument"},
		{grpcErr(codes.InvalidArgument, "This feature is not implemented: SHOW DATABASES"), driver.ErrUnsupported, "InvalidArgument"},
		{grpcErr(codes.InvalidArgument, "Error during planning: DML not supported: Insert Into"), driver.ErrReadOnly, "InvalidArgument"},
		{grpcErr(codes.Unimplemented, "x"), driver.ErrUnsupported, "Unimplemented"},
		{grpcErr(codes.InvalidArgument, "Error while planning query: Query DELETE FROM cpu not implemented yet"), driver.ErrReadOnly, "InvalidArgument"},
		{grpcErr(codes.Unimplemented, "Query UPDATE cpu SET a = 1 not implemented yet"), driver.ErrReadOnly, "Unimplemented"},
		{grpcErr(codes.InvalidArgument, "This feature is not implemented: Unsupported ast node Insert"), driver.ErrReadOnly, "InvalidArgument"},
		{grpcErr(codes.Internal, "boom"), nil, "Internal"},
	} {
		got := mapError(context.Background(), "query", tc.err)
		var de *driver.Error
		if !errors.As(got, &de) {
			t.Fatalf("mapError(%v) = %T, want *driver.Error", tc.err, got)
		}
		if de.Kind != tc.kind || de.Code != tc.code || de.Driver != Name {
			t.Errorf("mapError(%v) = kind %v code %q driver %q, want %v %q %q", tc.err, de.Kind, de.Code, de.Driver, tc.kind, tc.code, Name)
		}
		if st, ok := status.FromError(got); !ok || st.Code().String() != tc.code {
			t.Errorf("mapError(%v): the gRPC status is no longer reachable", tc.err)
		}
	}
}

func TestMapErrorContext(t *testing.T) {
	if got := mapError(context.Background(), "query", context.Canceled); !errors.Is(got, context.Canceled) {
		t.Errorf("context.Canceled lost its identity: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := mapError(ctx, "query", status.Error(codes.Canceled, "context canceled"))
	if !errors.Is(got, context.Canceled) {
		t.Errorf("a gRPC Canceled under a cancelled ctx = %v, want context.Canceled", got)
	}
	if driver.KindOf(got) != nil {
		t.Errorf("a cancellation got kind %v", driver.KindOf(got))
	}
	if mapError(context.Background(), "x", nil) != nil {
		t.Error("mapError(nil) != nil")
	}
}
