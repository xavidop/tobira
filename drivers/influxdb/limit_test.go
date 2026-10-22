package influxdb

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// A gRPC reply over the client's receive cap is a size limit; a quota
// ResourceExhausted is not.
func TestRecvCapIsErrLimit(t *testing.T) {
	big := status.Error(codes.ResourceExhausted, "grpc: received message larger than max (70000000 vs. 67108864)")
	if err := mapError(context.Background(), "query", big); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("mapError(%v) = %v, want ErrLimit", big, err)
	}
	quota := status.Error(codes.ResourceExhausted, "rate limit exceeded")
	if err := mapError(context.Background(), "query", quota); errors.Is(err, driver.ErrLimit) {
		t.Errorf("mapError(%v) = %v, want no ErrLimit", quota, err)
	}
}
