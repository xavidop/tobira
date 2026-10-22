package redis

import (
	"context"
	"errors"
	"io"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// serverError is a Redis error reply as go-redis returns it.
type serverError string

func (e serverError) Error() string { return string(e) }
func (serverError) RedisError()     {}

func TestMapError(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		err  error
		kind error
		code string
	}{
		{serverError("WRONGPASS invalid username-password pair or user is disabled."), driver.ErrAuth, "WRONGPASS"},
		{serverError("NOAUTH Authentication required."), driver.ErrAuth, "NOAUTH"},
		{serverError("NOPERM User x has no permissions to run the 'get' command"), driver.ErrPermission, "NOPERM"},
		{serverError("WRONGTYPE Operation against a key holding the wrong kind of value"), driver.ErrConflict, "WRONGTYPE"},
		{serverError("ERR unknown command 'FOO', with args beginning with: "), driver.ErrSyntax, "ERR"},
		{serverError("ERR syntax error"), driver.ErrSyntax, "ERR"},
		{serverError("ERR wrong number of arguments for 'get' command"), driver.ErrSyntax, "ERR"},
		{serverError("ERR no such key"), driver.ErrNotFound, "ERR"},
		{serverError("SEARCH_INDEX_NOT_FOUND Index not found: nope"), driver.ErrNotFound, "SEARCH_INDEX_NOT_FOUND"},
		{serverError("Unknown index name"), driver.ErrNotFound, ""},
		{serverError("nope: no such index"), driver.ErrNotFound, ""},
		{serverError("READONLY You can't write against a read only replica."), driver.ErrReadOnly, "READONLY"},
		{serverError("LOADING Redis is loading the dataset in memory"), driver.ErrConnection, "LOADING"},
		{serverError("MOVED 3999 127.0.0.1:6381"), driver.ErrUnsupported, "MOVED"},
		{serverError("ERR value is not an integer or out of range"), nil, "ERR"},
		{io.EOF, driver.ErrConnection, ""},
		{goredis.ErrClosed, driver.ErrConnection, ""},
	} {
		got := mapError(ctx, "query", tt.err)
		var de *driver.Error
		if !errors.As(got, &de) {
			t.Errorf("mapError(%v) = %#v, want a *driver.Error", tt.err, got)
			continue
		}
		if de.Kind != tt.kind || de.Code != tt.code || de.Driver != Name || de.Op != "query" {
			t.Errorf("mapError(%v) = kind %v code %q, want kind %v code %q", tt.err, de.Kind, de.Code, tt.kind, tt.code)
		}
		if !errors.Is(got, tt.err) {
			t.Errorf("mapError(%v) lost the original error", tt.err)
		}
	}
	if mapError(ctx, "x", nil) != nil {
		t.Error("mapError(nil) != nil")
	}
	cerr := mapError(ctx, "query", context.Canceled)
	if !errors.Is(cerr, context.Canceled) || driver.KindOf(cerr) != nil {
		t.Errorf("mapError(Canceled) = %v, want a kindless error matching context.Canceled", cerr)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if got := mapError(done, "query", io.EOF); !errors.Is(got, context.Canceled) {
		t.Errorf("a connection error after our own cancel = %v, want it to match context.Canceled", got)
	}
}
