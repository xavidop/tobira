package cassandra

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
)

// reqErr is a server error as gocql reports it.
type reqErr struct {
	code int
	msg  string
}

func (e reqErr) Code() int       { return e.code }
func (e reqErr) Message() string { return e.msg }
func (e reqErr) Error() string   { return e.msg }

func TestMapErrorServerCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind error
		code string
	}{
		{reqErr{0x2200, "table nope does not exist"}, driver.ErrNotFound, "0x2200"},
		{reqErr{0x2200, "keyspace nope does not exist"}, driver.ErrNotFound, "0x2200"},
		{reqErr{0x2200, "unconfigured table nope"}, driver.ErrNotFound, "0x2200"},
		{reqErr{0x2200, "Undefined column name x"}, driver.ErrSyntax, "0x2200"},
		{reqErr{0x2000, "line 1:0 no viable alternative"}, driver.ErrSyntax, "0x2000"},
		{reqErr{0x0100, "Provided username alice and/or password are incorrect"}, driver.ErrAuth, "0x0100"},
		{reqErr{0x2100, "User alice has no SELECT permission"}, driver.ErrPermission, "0x2100"},
		{reqErr{0x2400, "Table already exists"}, driver.ErrConflict, "0x2400"},
		{reqErr{0x1700, "CAS operation result is unknown"}, driver.ErrConflict, "0x1700"},
		{reqErr{0x1000, "Cannot achieve consistency level QUORUM"}, driver.ErrConnection, "0x1000"},
		{reqErr{0x1200, "Operation timed out"}, driver.ErrConnection, "0x1200"},
		{reqErr{0x1100, "Operation timed out"}, driver.ErrConnection, "0x1100"},
		{reqErr{0x1001, "overloaded"}, driver.ErrConnection, "0x1001"},
		{reqErr{0x1400, "function failure"}, nil, "0x1400"},
	} {
		err := mapError(context.Background(), "query", tc.err)
		if driver.KindOf(err) != tc.kind {
			t.Errorf("mapError(%v) kind = %v, want %v", tc.err, driver.KindOf(err), tc.kind)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != tc.code || de.Driver != Name || de.Op != "query" {
			t.Errorf("mapError(%v) = %#v, want code %s", tc.err, err, tc.code)
		}
	}
}

func TestMapErrorClientErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind error
	}{
		{gocql.ErrNoConnections, driver.ErrConnection},
		{gocql.ErrSessionClosed, driver.ErrConnection},
		{gocql.ErrConnectionClosed, driver.ErrConnection},
		{gocql.ErrTimeoutNoResponse, driver.ErrConnection},
		{gocql.ErrUseStmt, driver.ErrUnsupported},
		{gocql.ErrKeyspaceDoesNotExist, driver.ErrNotFound},
		{gocql.ErrNotFound, driver.ErrNotFound},
		{fmt.Errorf("wrapped: %w", gocql.ErrNoConnections), driver.ErrConnection},
		{errors.New("gocql: expected 2 values send got 1"), driver.ErrSyntax},
		{errors.New("something else"), nil},
	} {
		if got := driver.KindOf(mapError(context.Background(), "query", tc.err)); got != tc.kind {
			t.Errorf("mapError(%v) kind = %v, want %v", tc.err, got, tc.kind)
		}
	}
}

func TestMapErrorContext(t *testing.T) {
	err := mapError(context.Background(), "query", context.Canceled)
	if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
		t.Errorf("Canceled: got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = mapError(ctx, "query", errors.New("gocql: connection closed"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("an error after cancel must match context.Canceled: %v", err)
	}
	de := &driver.Error{Kind: driver.ErrReadOnly, Driver: Name, Op: "query", Err: errors.New("x")}
	if got := mapError(context.Background(), "query", de); got != error(de) {
		t.Errorf("a *driver.Error must pass through, got %v", got)
	}
	if mapError(context.Background(), "query", nil) != nil {
		t.Error("nil must stay nil")
	}
}

func TestOpenErrorKind(t *testing.T) {
	for msg, kind := range map[string]error{
		"gocql: unable to create session: unable to discover protocol version: Provided username cassandra and/or password are incorrect": driver.ErrAuth,
		"gocql: unable to create session: control: unable to connect to initial hosts: authentication failed: Bad credentials":            driver.ErrAuth,
		"gocql: unable to create session: unable to discover protocol version: dial tcp 127.0.0.1:1: connect: connection refused":         driver.ErrConnection,
		"gocql: unable to create session: keyspace does not exist":                                                                        driver.ErrNotFound,
	} {
		if got := driver.KindOf(openError(context.Background(), errors.New(msg))); got != kind {
			t.Errorf("openError(%q) kind = %v, want %v", msg, got, kind)
		}
	}
}
