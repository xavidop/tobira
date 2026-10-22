package libsql

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		err  error
		want error
		code string
	}{
		{&hranaError{Message: "SQLite error: no such table: nope", Code: "SQLITE_UNKNOWN"}, driver.ErrNotFound, "SQLITE_UNKNOWN"},
		{&hranaError{Message: "SQL input error: no such column: x (at offset 7)", Code: "SQL_INPUT_ERROR"}, driver.ErrNotFound, "SQL_INPUT_ERROR"},
		{&hranaError{Message: "SQLite error: UNIQUE constraint failed: t.id", Code: "SQLITE_CONSTRAINT"}, driver.ErrConstraint, "SQLITE_CONSTRAINT"},
		{&hranaError{Message: "SQLite error: NOT NULL constraint failed: u.a", Code: "SQLITE_CONSTRAINT"}, driver.ErrConstraint, "SQLITE_CONSTRAINT"},
		{&hranaError{Message: "SQL string could not be parsed: syntax error around L1:6: `selec`", Code: "SQL_PARSE_ERROR"}, driver.ErrSyntax, "SQL_PARSE_ERROR"},
		{&hranaError{Message: "SQL string could not be parsed: unsupported statement: vacuum", Code: "SQL_PARSE_ERROR"}, driver.ErrUnsupported, "SQL_PARSE_ERROR"},
		{&hranaError{Message: "SQL string contains more than one statement", Code: "SQL_MANY_STATEMENTS"}, driver.ErrSyntax, "SQL_MANY_STATEMENTS"},
		{&hranaError{Message: "Arguments do not match SQL parameters: value for parameter 1 not found", Code: "ARGS_INVALID"}, driver.ErrSyntax, "ARGS_INVALID"},
		{&hranaError{Message: "The stream has expired due to inactivity", Code: "STREAM_EXPIRED", Status: 400}, driver.ErrConnection, "STREAM_EXPIRED"},
		{&hranaError{Message: "SQLite error: attempt to write a readonly database", Code: "SQLITE_READONLY"}, driver.ErrReadOnly, "SQLITE_READONLY"},
		{&hranaError{Message: "SQLite error: database is locked", Code: "SQLITE_BUSY"}, driver.ErrConflict, "SQLITE_BUSY"},
		{&hranaError{Message: "HTTP 401: Unauthorized: `The JWT is invalid`", Status: 401}, driver.ErrAuth, "401"},
		{&hranaError{Message: "HTTP 403: Authorization forbidden", Status: 403}, driver.ErrPermission, "403"},
		{&hranaError{Message: "HTTP 404: not found", Status: 404}, driver.ErrNotFound, "404"},
		{&hranaError{Message: "HTTP 502: bad gateway", Status: 502}, driver.ErrConnection, "502"},
		{&url.Error{Op: "Post", URL: "http://h/v2/pipeline", Err: errors.New("dial tcp: connection refused")}, driver.ErrConnection, ""},
		{errStreamClosed, driver.ErrConnection, ""},
		{errResponseTooLarge, driver.ErrLimit, ""},
	}
	for _, tt := range tests {
		err := mapError("query", tt.err)
		if !errors.Is(err, tt.want) {
			t.Errorf("%v: kind = %v, want %v", tt.err, driver.KindOf(err), tt.want)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != "libsql" || de.Op != "query" || de.Code != tt.code {
			t.Errorf("%v: bad wrap %#v", tt.err, err)
		}
	}
	if mapError("query", nil) != nil {
		t.Error("nil must stay nil")
	}
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		wrapped := &url.Error{Op: "Post", URL: "http://h", Err: ctxErr}
		if err := mapError("query", wrapped); !errors.Is(err, ctxErr) || driver.KindOf(err) != nil {
			t.Errorf("ctx error lost or classified: %v", err)
		}
	}
	// An existing *driver.Error passes through.
	in := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
	if mapError("browse", in) != in {
		t.Error("driver.Error rewrapped")
	}
}
