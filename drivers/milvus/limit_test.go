package milvus

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// The gRPC receive cap is the 64 MiB response budget of the other drivers,
// and a reply over it is ErrLimit.
func TestRecvCapIs64MiBAndErrLimit(t *testing.T) {
	if maxRecvMsg != 64<<20 {
		t.Errorf("maxRecvMsg = %d MiB, want 64", maxRecvMsg>>20)
	}
	big := status.Error(codes.ResourceExhausted, "grpc: received message larger than max (70000000 vs. 67108864)")
	if err := mapError(context.Background(), "query", big); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("mapError(%v) = %v, want ErrLimit", big, err)
	}
}
