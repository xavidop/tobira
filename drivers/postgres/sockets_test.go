package postgres

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSocketsRefuseDialAfterCloseAll(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cfg, err := pgxpool.ParseConfig("postgres://u@" + ln.Addr().String() + "/db")
	if err != nil {
		t.Fatal(err)
	}
	s := trackSockets(cfg)
	dial := cfg.ConnConfig.DialFunc
	nc, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s.closeAll()
	if _, err := nc.Write([]byte("x")); err == nil {
		t.Error("tracked socket still open after closeAll")
	}
	if _, err := dial(context.Background(), "tcp", ln.Addr().String()); !errors.Is(err, net.ErrClosed) {
		t.Errorf("dial after closeAll = %v, want net.ErrClosed", err)
	}
}
