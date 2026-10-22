package sdk_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
)

// recordingDriver wraps a driver whose Conns are Transactors, counting
// Conn.Close and Tx.Rollback calls. Its Conns offer only Conn and
// Transactor.
type recordingDriver struct {
	driver.Driver
	onOpen     func(driver.ConnConfig)
	closed     atomic.Int32
	rolledBack atomic.Int32
}

func (d *recordingDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	if d.onOpen != nil {
		d.onOpen(cfg)
	}
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &recordingConn{Conn: c, d: d}, nil
}

type recordingConn struct {
	driver.Conn
	d *recordingDriver
}

func (c *recordingConn) Close() error {
	c.d.closed.Add(1)
	return c.Conn.Close()
}

func (c *recordingConn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.Transactor).Begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &recordingTx{Tx: tx, d: c.d}, nil
}

type recordingTx struct {
	driver.Tx
	d *recordingDriver
}

func (t *recordingTx) Rollback(ctx context.Context) error {
	t.d.rolledBack.Add(1)
	return t.Tx.Rollback(ctx)
}

// bareDriver's Conns implement driver.Conn and nothing else.
type bareDriver struct{ driver.Driver }

type bareConn struct{ driver.Conn }

func (d bareDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return bareConn{c}, nil
}

// invalidatingDriver's Conns are CatalogInvalidators that count calls.
type invalidatingDriver struct {
	driver.Driver
	calls atomic.Int32
}

type invalidatingConn struct {
	driver.Conn
	d *invalidatingDriver
}

func (c invalidatingConn) Invalidate() { c.d.calls.Add(1) }

func (d *invalidatingDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return invalidatingConn{Conn: c, d: d}, nil
}

func errContaining(msg string) error { return errors.New(msg) }

func assertNoSecret(t *testing.T, err error, secret string) {
	t.Helper()
	st := status.Convert(err)
	for _, s := range []string{st.Message(), convert.ErrorInfoOf(st).GetMessage(), convert.FromStatus(err).Error()} {
		if strings.Contains(s, secret) {
			t.Fatalf("secret leaked: %q", s)
		}
	}
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if got := status.Code(err); got != code {
		t.Fatalf("code %v (%v), want %v", got, err, code)
	}
}
