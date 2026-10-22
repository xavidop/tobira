package mongodb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xavidop/tobira/driver"
)

func TestMapError(t *testing.T) {
	cmdErr := func(code int32) error {
		return mongo.CommandError{Code: code, Message: fmt.Sprintf("server says %d", code)}
	}
	for _, tc := range []struct {
		code int32
		kind error
	}{
		{18, driver.ErrAuth},
		{13, driver.ErrPermission},
		{26, driver.ErrNotFound},
		{2, driver.ErrSyntax},
		{9, driver.ErrSyntax},
		{14, driver.ErrSyntax},
		{40324, driver.ErrSyntax},
		{40415, driver.ErrSyntax},
		{59, driver.ErrSyntax},
		{11000, driver.ErrConstraint},
		{121, driver.ErrConstraint},
		{66, driver.ErrConstraint},
		{112, driver.ErrConflict},
		{166, driver.ErrUnsupported},
		{40573, driver.ErrUnsupported},
		{10107, driver.ErrConnection},
		{91, driver.ErrConnection},
	} {
		err := mapError(context.Background(), "query", cmdErr(tc.code))
		if !errors.Is(err, tc.kind) {
			t.Errorf("code %d: want %v, got %v", tc.code, tc.kind, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != fmt.Sprint(tc.code) || de.Driver != Name || de.Op != "query" {
			t.Errorf("code %d: driver.Error = %#v", tc.code, de)
		}
	}

	t.Run("unknown code has no kind", func(t *testing.T) {
		err := mapError(context.Background(), "query", cmdErr(424242))
		if driver.KindOf(err) != nil {
			t.Errorf("want no kind, got %v", err)
		}
	})
	t.Run("write exception", func(t *testing.T) {
		err := mapError(context.Background(), "insert", mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000, Message: "dup"}}})
		if !errors.Is(err, driver.ErrConstraint) {
			t.Errorf("want ErrConstraint, got %v", err)
		}
	})
	t.Run("context errors keep their identity", func(t *testing.T) {
		for _, ce := range []error{context.Canceled, context.DeadlineExceeded} {
			err := mapError(context.Background(), "query", fmt.Errorf("wrapped: %w", ce))
			if !errors.Is(err, ce) || driver.KindOf(err) != nil {
				t.Errorf("want %v without kind, got %v", ce, err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := mapError(ctx, "query", errors.New("connection reset"))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("an error under a cancelled ctx: want context.Canceled, got %v", err)
		}
	})
	t.Run("client disconnected", func(t *testing.T) {
		if err := mapError(context.Background(), "ping", mongo.ErrClientDisconnected); !errors.Is(err, driver.ErrConnection) {
			t.Errorf("want ErrConnection, got %v", err)
		}
	})
	t.Run("driver errors pass through", func(t *testing.T) {
		in := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
		if got := mapError(context.Background(), "query", in); got != in {
			t.Errorf("got %v", got)
		}
	})
	if mapError(context.Background(), "x", nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
}
