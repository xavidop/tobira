package redis

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

var errNoDial = errors.New("no dial")

func TestClientPoolBounds(t *testing.T) {
	noDial := func(context.Context, string, string) (net.Conn, error) { return nil, errNoDial }
	c := newConn(&goredis.Options{Addr: "x:1", Protocol: 2, DB: 0, Dialer: noDial, MaxRetries: -1, DialerRetries: 1}, false)
	t.Cleanup(func() { _ = c.Close() })
	c.dbCount = 40
	if _, _, _, err := c.client("x", "40"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("db 40 of 40 = %v, want ErrNotFound", err)
	}
	held, _, releaseHeld, err := c.client("x", "1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 40; i++ {
		_, _, release, err := c.client("x", itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if n := c.poolCount(); n > maxPools {
		t.Errorf("%d pools open, want at most %d", n, maxPools)
	}
	// db 1 was evicted while held: it must stay usable until released.
	if err := held.Ping(context.Background()).Err(); !errors.Is(err, errNoDial) {
		t.Errorf("a held pool was closed by eviction: %v", err)
	}
	releaseHeld()
	if err := held.Ping(context.Background()).Err(); err == nil || !strings.Contains(err.Error(), "client is closed") {
		t.Errorf("an evicted pool was not closed on release: %v", err)
	}
	// The default pool is never evicted.
	if _, _, release, err := c.client("x", ""); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	c.dbCount = 0 // CONFIG GET refused: cap at nsCap
	if _, _, _, err := c.client("x", itoa(nsCap)); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("db %d with no count = %v, want ErrNotFound", nsCap, err)
	}
}
