package weaviate

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A response over the cap is a size limit (ErrLimit), and the cap is the
// 64 MiB the other HTTP drivers use.
func TestResponseOverCapIsErrLimit(t *testing.T) {
	if maxResponseBytes != 64<<20 {
		t.Errorf("maxResponseBytes = %d MiB, want 64", maxResponseBytes>>20)
	}
	b := &cappedBody{rc: io.NopCloser(strings.NewReader(strings.Repeat("a", 64))), left: 16}
	_, err := io.ReadAll(b)
	if got := mapError(context.Background(), "query", err); !errors.Is(got, driver.ErrLimit) {
		t.Errorf("mapError(%v) = %v, want ErrLimit", err, got)
	}
}
