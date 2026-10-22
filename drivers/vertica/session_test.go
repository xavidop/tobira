package vertica

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// A cancelled statement returns to the caller without waiting for the
// CLOSE_SESSION login, here against a server that accepts and never
// answers (the old synchronous interrupt waited for its 10 s timeout).
func TestCancelDoesNotWaitForInterruptLogin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	d, _ := parseDSN("vertica://u@" + ln.Addr().String() + "/db?tlsmode=none")
	s := &session{c: &conn{url: buildURL(d, "x"), cfg: driver.ConnConfig{}}, id: "node-1:0x1"}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = s.do(ctx, func(context.Context) (*sql.Rows, error) {
		time.Sleep(time.Second) // the statement ends on its own a second later
		return nil, errors.New("ended")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("do(cancelled) = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("do(cancelled) took %v, want it not to wait for the interrupt's login", d)
	}
	_ = ln.Close()
	for c := range accepted { // the interrupt's login sees EOF and ends
		_ = c.Close()
	}
	time.Sleep(200 * time.Millisecond)
}
