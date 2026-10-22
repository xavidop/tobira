package mssql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"io"
	"testing"

	mssqldb "github.com/microsoft/go-mssqldb"

	"github.com/xavidop/tobira/driver"
)

func TestMapErrorServerNumbers(t *testing.T) {
	for _, tc := range []struct {
		number int32
		kind   error
	}{
		{208, driver.ErrNotFound},
		{2812, driver.ErrNotFound},
		{911, driver.ErrNotFound},
		{18456, driver.ErrAuth},
		{102, driver.ErrSyntax},
		{156, driver.ErrSyntax},
		{105, driver.ErrSyntax},
		{2627, driver.ErrConstraint},
		{2601, driver.ErrConstraint},
		{547, driver.ErrConstraint},
		{515, driver.ErrConstraint},
		{229, driver.ErrPermission},
		{262, driver.ErrPermission},
		{1205, driver.ErrConflict},
		{1222, driver.ErrConflict},
		{3960, driver.ErrConflict},
		{3906, driver.ErrReadOnly},
	} {
		err := mapError(context.Background(), "query", mssqldb.Error{Number: tc.number, Message: "m"})
		if !errors.Is(err, tc.kind) {
			t.Errorf("number %d: got %v, want %v", tc.number, err, tc.kind)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Code == "" {
			t.Errorf("number %d: want a *driver.Error with driver %q and a code, got %#v", tc.number, Name, err)
		}
	}
	err := mapError(context.Background(), "query", mssqldb.Error{Number: 50000, Message: "custom"})
	var de *driver.Error
	if !errors.As(err, &de) || de.Kind != nil || de.Code != "50000" {
		t.Errorf("unknown number: want no kind and code 50000, got %#v", err)
	}
}

func TestMapErrorConnectionAndContext(t *testing.T) {
	for _, e := range []error{io.EOF, io.ErrUnexpectedEOF, sqldriver.ErrBadConn, sql.ErrConnDone} {
		if err := mapError(context.Background(), "query", e); !errors.Is(err, driver.ErrConnection) {
			t.Errorf("%v: got %v, want ErrConnection", e, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mapError(ctx, "query", io.EOF)
	if !errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrConnection) {
		t.Errorf("EOF under a cancelled ctx: got %v, want context.Canceled and no ErrConnection", err)
	}
	if err := mapError(context.Background(), "query", context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: got %v", err)
	}
	if mapError(context.Background(), "query", nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
	kept := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
	if mapError(context.Background(), "query", kept) != kept {
		t.Error("a *driver.Error must pass through unchanged")
	}
}
