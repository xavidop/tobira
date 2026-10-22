//go:build integration

package postgres

import (
	"context"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// freezeProxy forwards TCP to target until freeze is called, after which it
// accepts connections and reads nothing more: a server that has hung.
type freezeProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	frozen chan struct{}
	done   chan struct{}
	conns  []net.Conn
}

func newFreezeProxy(t *testing.T, target string) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{ln: ln, target: target, frozen: make(chan struct{}), done: make(chan struct{})}
	go p.serve()
	t.Cleanup(func() {
		close(p.done)
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			_ = c.Close()
		}
	})
	return p
}

func (p *freezeProxy) freeze() { close(p.frozen) }

func (p *freezeProxy) serve() {
	for {
		in, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns = append(p.conns, in)
		p.mu.Unlock()
		select {
		case <-p.frozen: // hung: accepted, never answered
			continue
		default:
		}
		out, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = in.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, out)
		p.mu.Unlock()
		go p.pipe(out, in)
		go p.pipe(in, out)
	}
}

// pipe copies src to dst in chunks and stops forwarding once frozen.
func (p *freezeProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			select {
			case <-p.frozen:
				<-p.done // swallow: the peer never hears back
				return
			default:
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				_ = dst.Close()
			}
			return
		}
	}
}

// closeBound is the documented bound on Close against a hung server.
const closeBound = 6 * time.Second

// TestCloseBoundedOnHungServer: a server that stops answering must not hold
// Close for longer than the reset timeout plus the cancel deadline delay.
func TestCloseBoundedOnHungServer(t *testing.T) {
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFreezeProxy(t, u.Host)
	u.Host = proxy.ln.Addr().String()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: u.String()})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := c.Query(ctxT(t), driver.Query{Text: "select n from tobira_series"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rs.Next(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	proxy.freeze()
	// Releasing the connection runs DISCARD ALL against the hung server.
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > closeBound {
		t.Errorf("Close took %v on a hung server, want <= %v", d, closeBound)
	} else {
		t.Logf("Close took %v", d)
	}
}
