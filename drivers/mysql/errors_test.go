package mysql

import (
	"context"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

func TestErrorMapping(t *testing.T) {
	my := func(n uint16) error { return &gomysql.MySQLError{Number: n, Message: "boom"} }
	tests := []struct {
		name string
		err  error
		kind error
		code string
	}{
		{"access denied", my(1045), driver.ErrAuth, "1045"},
		{"no such table", my(1146), driver.ErrNotFound, "1146"},
		{"unknown database", my(1049), driver.ErrNotFound, "1049"},
		{"syntax", my(1064), driver.ErrSyntax, "1064"},
		{"duplicate key", my(1062), driver.ErrConstraint, "1062"},
		{"fk parent row", my(1451), driver.ErrConstraint, "1451"},
		{"fk child row", my(1452), driver.ErrConstraint, "1452"},
		{"table access denied", my(1142), driver.ErrPermission, "1142"},
		{"db access denied", my(1044), driver.ErrPermission, "1044"},
		{"deadlock", my(1213), driver.ErrConflict, "1213"},
		{"lock wait timeout", my(1205), driver.ErrConflict, "1205"},
		{"read-only transaction", my(1792), driver.ErrReadOnly, "1792"},
		{"cannot connect socket", my(2002), driver.ErrConnection, "2002"},
		{"cannot connect tcp", my(2003), driver.ErrConnection, "2003"},
		{"lost connection", my(2013), driver.ErrConnection, "2013"},
		{"wrapped", fmt.Errorf("x: %w", my(1146)), driver.ErrNotFound, "1146"},
		{"unknown code keeps message", my(1366), nil, "1366"},
		{"invalid conn", gomysql.ErrInvalidConn, driver.ErrConnection, ""},
		{"bad conn", sqldriver.ErrBadConn, driver.ErrConnection, ""},
		{"eof", io.EOF, driver.ErrConnection, ""},
		{"unexpected eof", io.ErrUnexpectedEOF, driver.ErrConnection, ""},
		{"net error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, driver.ErrConnection, ""},
		{"plain error", errors.New("odd"), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapError(context.Background(), "query", tt.err)
			var de *driver.Error
			if !errors.As(err, &de) {
				t.Fatalf("not a *driver.Error: %v", err)
			}
			if de.Driver != Name || de.Op != "query" || de.Code != tt.code {
				t.Errorf("got driver=%q op=%q code=%q", de.Driver, de.Op, de.Code)
			}
			if tt.kind == nil && driver.KindOf(err) != nil {
				t.Errorf("want no kind, got %v", driver.KindOf(err))
			}
			if tt.kind != nil && !errors.Is(err, tt.kind) {
				t.Errorf("want kind %v, got %v", tt.kind, err)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("underlying error lost: %v", err)
			}
		})
	}
}

func TestErrorMappingContext(t *testing.T) {
	err := mapError(context.Background(), "query", context.Canceled)
	if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
		t.Errorf("canceled: %v (kind %v)", err, driver.KindOf(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = mapError(ctx, "query", gomysql.ErrInvalidConn)
	if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
		t.Errorf("connection lost to our own cancel: %v (kind %v)", err, driver.KindOf(err))
	}
	if mapError(context.Background(), "query", nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
	already := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
	if got := mapError(context.Background(), "query", already); got != already {
		t.Errorf("a *driver.Error was re-wrapped: %v", got)
	}
}
