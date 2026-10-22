//go:build integration

package mysql

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// stallProxy forwards TCP to the test server. While stalled it holds what the
// server sends, so a client round trip hangs as on a dead network.
type stallProxy struct {
	ln      net.Listener
	mu      sync.Mutex
	stalled bool
	resume  chan struct{}
	conns   []net.Conn
}

func newStallProxy(t *testing.T) *stallProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallProxy{ln: ln, resume: make(chan struct{})}
	go func() {
		for {
			cc, err := ln.Accept()
			if err != nil {
				return
			}
			sc, err := net.Dial("tcp", testHostPort)
			if err != nil {
				_ = cc.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, cc, sc)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(sc, cc); _ = sc.Close() }()
			go func() { _, _ = io.Copy(p.gate(cc), sc); _ = cc.Close() }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		p.unstall()
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
	})
	return p
}

func (p *stallProxy) dsn() string { return "root:tobira@tcp(" + p.ln.Addr().String() + ")/tobira" }

func (p *stallProxy) stall() {
	p.mu.Lock()
	p.stalled = true
	p.resume = make(chan struct{})
	p.mu.Unlock()
}

func (p *stallProxy) unstall() {
	p.mu.Lock()
	if p.stalled {
		p.stalled = false
		close(p.resume)
	}
	p.mu.Unlock()
}

// gate wraps the client side: a write waits while the proxy is stalled.
func (p *stallProxy) gate(w io.Writer) io.Writer {
	return writerFunc(func(b []byte) (int, error) {
		p.mu.Lock()
		stalled, resume := p.stalled, p.resume
		p.mu.Unlock()
		if stalled {
			<-resume
		}
		return w.Write(b)
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// A Tx statement whose text holds a backslash reads the session's
// sql_mode before it runs. Rollback must interrupt that round trip, as it
// interrupts the statement itself, instead of waiting for it.
func TestRollbackInterruptsTxPreRunCheck(t *testing.T) {
	p := newStallProxy(t)
	c := openDSN(t, p.dsn(), false)
	tx := begin(t, c, driver.TxOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p.stall()
	done := make(chan error, 1)
	go func() {
		rs, err := tx.Query(ctx, driver.Query{Text: `SELECT 'a\\b'`})
		if err == nil {
			_ = rs.Close()
		}
		done <- err
	}()
	// Wait until the check is in flight: the Tx connection is claimed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		tx.mu.Lock()
		busy := tx.open != nil
		tx.mu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the statement never claimed the Tx connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let the sql_mode round trip reach the stalled proxy

	within(t, "Rollback during the pre-run check", 3*time.Second, func() { _ = tx.Rollback(context.Background()) })
	p.unstall()
	select {
	case err := <-done:
		if err == nil {
			t.Error("the interrupted statement reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the statement did not return after Rollback")
	}
}
