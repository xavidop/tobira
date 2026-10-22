package postgres

import (
	"context"
	"net"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sockets remembers the sockets a pool has dialled, cancel-request sockets
// included, so Close can drop them when a hung server would otherwise hold
// pgx's cleanup for its fixed 15 s deadline.
//
// A dial that is already blocked on a network partition when closeAll runs is
// not interrupted by it; it ends with its own dial timeout, and the socket it
// returns is closed on arrival.
type sockets struct {
	mu   sync.Mutex
	open map[*trackedConn]struct{}
	shut bool
}

// trackSockets wraps the dialler of cfg so every socket it opens is tracked.
func trackSockets(cfg *pgxpool.Config) *sockets {
	s := &sockets{open: map[*trackedConn]struct{}{}}
	dial := cfg.ConnConfig.DialFunc
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		s.mu.Lock()
		shut := s.shut
		s.mu.Unlock()
		if shut { // closeAll ran: dial nothing new
			return nil, net.ErrClosed
		}
		nc, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		tc := &trackedConn{Conn: nc, owner: s}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.shut { // closeAll ran while dialling
			_ = nc.Close()
			return nil, net.ErrClosed
		}
		s.open[tc] = struct{}{}
		return tc, nil
	}
	return s
}

// closeAll closes every tracked socket and any dialled afterwards.
func (s *sockets) closeAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.shut = true
	conns := make([]*trackedConn, 0, len(s.open))
	for c := range s.open {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Conn.Close()
	}
}

type trackedConn struct {
	net.Conn
	owner *sockets
}

func (c *trackedConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.open, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}
