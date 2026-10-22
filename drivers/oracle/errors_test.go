package oracle

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"

	"github.com/sijms/go-ora/v2/network"

	"github.com/xavidop/tobira/driver"
)

func oraErr(code int, msg string) error {
	return &network.OracleError{ErrCode: code, ErrMsg: msg}
}

func TestMapErrorKinds(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		code int
		want error
	}{
		{942, driver.ErrNotFound},
		{4043, driver.ErrNotFound},
		{904, driver.ErrNotFound},
		{1918, driver.ErrNotFound},
		{1017, driver.ErrAuth},
		{28000, driver.ErrAuth},
		{1045, driver.ErrPermission},
		{1031, driver.ErrPermission},
		{1456, driver.ErrReadOnly},
		{16000, driver.ErrReadOnly},
		{1, driver.ErrConstraint},
		{1400, driver.ErrConstraint},
		{2290, driver.ErrConstraint},
		{2291, driver.ErrConstraint},
		{2292, driver.ErrConstraint},
		{900, driver.ErrSyntax},
		{933, driver.ErrSyntax},
		{936, driver.ErrSyntax},
		{60, driver.ErrConflict},
		{8177, driver.ErrConflict},
		{54, driver.ErrConflict},
		{1466, driver.ErrConflict},
		{3113, driver.ErrConnection},
		{12541, driver.ErrConnection},
		{12514, driver.ErrConnection},
	} {
		err := mapError(ctx, "query", oraErr(tc.code, "ORA-x: boom"))
		if !errors.Is(err, tc.want) {
			t.Errorf("ORA-%05d: want %v, got %v", tc.code, tc.want, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Op != "query" {
			t.Errorf("ORA-%05d: want a *driver.Error from %s/query, got %#v", tc.code, Name, err)
			continue
		}
		if want := oraCode(tc.code); de.Code != want {
			t.Errorf("ORA-%05d: Code = %q, want %q", tc.code, de.Code, want)
		}
	}
}

func TestMapErrorUnknownCodeKeepsMessage(t *testing.T) {
	err := mapError(context.Background(), "query", oraErr(6502, "ORA-06502: numeric or value error"))
	if driver.KindOf(err) != nil {
		t.Errorf("unknown code: want no kind, got %v", driver.KindOf(err))
	}
	if err.Error() == "" {
		t.Error("empty message")
	}
}

func TestMapErrorCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, in := range []error{oraErr(1013, "ORA-01013: user requested cancel"), io.EOF, context.Canceled} {
		err := mapError(ctx, "query", in)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%v under a cancelled ctx: want context.Canceled, got %v", in, err)
		}
		if errors.Is(err, driver.ErrConnection) {
			t.Errorf("%v under a cancelled ctx: must not be ErrConnection", in)
		}
	}
}

func TestMapErrorConnection(t *testing.T) {
	ctx := context.Background()
	for _, in := range []error{io.EOF, io.ErrUnexpectedEOF, sql.ErrConnDone} {
		if err := mapError(ctx, "query", in); !errors.Is(err, driver.ErrConnection) {
			t.Errorf("%v: want ErrConnection, got %v", in, err)
		}
	}
}

func TestMapErrorPassesDriverErrors(t *testing.T) {
	in := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
	if got := mapError(context.Background(), "browse", in); got != in {
		t.Errorf("a *driver.Error must pass through unchanged, got %v", got)
	}
	if mapError(context.Background(), "query", nil) != nil {
		t.Error("nil must stay nil")
	}
}
