package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/xavidop/tobira/driver"
)

func TestErrorMapping(t *testing.T) {
	ex := func(code int32) error { return &ch.Exception{Code: code, Message: "boom"} }
	tests := []struct {
		name string
		err  error
		kind error
		code string
	}{
		{"readonly", ex(164), driver.ErrReadOnly, "164"},
		{"auth failed", ex(516), driver.ErrAuth, "516"},
		{"unknown user", ex(192), driver.ErrAuth, "192"},
		{"wrong password", ex(193), driver.ErrAuth, "193"},
		{"required password", ex(194), driver.ErrAuth, "194"},
		{"unknown table", ex(60), driver.ErrNotFound, "60"},
		{"unknown database", ex(81), driver.ErrNotFound, "81"},
		{"syntax", ex(62), driver.ErrSyntax, "62"},
		{"access denied", ex(497), driver.ErrPermission, "497"},
		{"database access denied", ex(291), driver.ErrPermission, "291"},
		{"constraint", ex(469), driver.ErrConstraint, "469"},
		{"not implemented", ex(48), driver.ErrUnsupported, "48"},
		{"network error", ex(210), driver.ErrConnection, "210"},
		{"wrapped", fmt.Errorf("x: %w", ex(60)), driver.ErrNotFound, "60"},
		{"http wrapped", &ch.HTTPError{StatusCode: 500, Err: ex(62)}, driver.ErrSyntax, "62"},
		{"unknown code keeps message", ex(999), nil, "999"},
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
			if de.Kind != tt.kind || de.Code != tt.code || de.Driver != Name || de.Op != "query" {
				t.Errorf("got kind=%v code=%q driver=%q op=%q", de.Kind, de.Code, de.Driver, de.Op)
			}
			if !errors.Is(err, tt.err) {
				t.Error("the cause is lost")
			}
		})
	}
}

func TestErrorMappingContext(t *testing.T) {
	if mapError(context.Background(), "q", nil) != nil {
		t.Fatal("nil must stay nil")
	}
	err := mapError(context.Background(), "q", context.Canceled)
	if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
		t.Errorf("context error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, cause := range []error{io.EOF, &ch.Exception{Code: 394, Message: "cancelled"}} {
		err = mapError(ctx, "q", cause)
		if !errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrConnection) {
			t.Errorf("%v under a cancelled ctx = %v, want context.Canceled and no connection kind", cause, err)
		}
	}
	already := driver.Wrap(driver.ErrReadOnly, Name, "q", errors.New("x"))
	if mapError(context.Background(), "other", already) != already {
		t.Error("a *driver.Error must pass through")
	}
}

func TestDetach(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d, done := detach(parent)
	defer done()
	if _, ok := d.Deadline(); ok {
		t.Fatal("the detached context still has a deadline (clickhouse-go would send max_execution_time)")
	}
	<-d.Done()
	err := mapError(parent, "query", d.Err())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a call ended by the parent's deadline = %v, want DeadlineExceeded", err)
	}
}
