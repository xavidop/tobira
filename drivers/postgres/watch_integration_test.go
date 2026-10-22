//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

var watchSeq atomic.Int64

// openWatchPG opens a connection whose backends carry a unique
// application_name, so a test can count them in pg_stat_activity.
func openWatchPG(t *testing.T, readOnly bool) (*conn, string) {
	t.Helper()
	app := fmt.Sprintf("tobira-watch-%d", watchSeq.Add(1))
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN, ReadOnly: readOnly, Options: map[string]string{"application_name": app}})
	if err != nil {
		t.Fatal(err)
	}
	pc := c.(*conn)
	t.Cleanup(func() { _ = c.Close() })
	return pc, app
}

// backendsLike counts the backends of app whose last statement matches like.
func backendsLike(t *testing.T, app, like string) int {
	t.Helper()
	admin, err := pgx.Connect(context.Background(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var n int
	if err := admin.QueryRow(context.Background(), "select count(*) from pg_stat_activity where application_name = $1 and query like $2", app, like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitBackends(t *testing.T, app string, want int) { t.Helper(); waitLike(t, app, "%", want) }

func waitLike(t *testing.T, app, like string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for backendsLike(t, app, like) != want {
		if time.Now().After(deadline) {
			t.Fatalf("backends(%s, %q) = %d, want %d", app, like, backendsLike(t, app, like), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func recv(t *testing.T, ch <-chan driver.Event) (driver.Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return driver.Event{}, false
	}
}

func notify(t *testing.T, c *conn, channel, payload string) {
	t.Helper()
	mustExec(t, c, fmt.Sprintf("select pg_notify(%s, %s)", quoteLit(channel), quoteLit(payload)))
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func TestWatchDeliversNotifications(t *testing.T) {
	for _, ro := range []bool{false, true} {
		t.Run(fmt.Sprintf("readOnly=%v", ro), func(t *testing.T) {
			c := openPG(t, ro)
			sender := openPG(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := c.Watch(ctx, driver.WatchRequest{Channel: "tobira_evt"})
			if err != nil {
				t.Fatal(err)
			}
			// LISTEN is established when Watch returns: no notification is missed.
			notify(t, sender, "tobira_evt", "hello")
			notify(t, sender, "tobira_evt", "")
			notify(t, sender, "tobira_other", "ignored")
			notify(t, sender, "tobira_evt", "wörld")
			for _, want := range []string{"hello", "", "wörld"} {
				ev, ok := recv(t, ch)
				if !ok {
					t.Fatal("channel closed early")
				}
				got, _ := ev.Payload.AsText()
				if ev.Op != "notify" || ev.Channel != "tobira_evt" || got != want || ev.Err != nil || ev.Time.IsZero() {
					t.Errorf("event = %+v, want notify tobira_evt %q", ev, want)
				}
			}
		})
	}
}

func TestWatchQuotesChannel(t *testing.T) {
	c := openPG(t, false)
	for _, name := range []string{`MixedCase`, `with space`, `a"quote`, `semi;colon -- x`, `drop table tobira_people`, `ünï`} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := c.Watch(ctx, driver.WatchRequest{Channel: name})
			if err != nil {
				t.Fatal(err)
			}
			notify(t, c, name, "ok")
			ev, ok := recv(t, ch)
			if !ok || ev.Channel != name {
				t.Fatalf("event = %+v ok=%v, want channel %q", ev, ok, name)
			}
		})
	}
	if n := count(t, c, "tobira_people"); n == 0 {
		t.Error("tobira_people vanished")
	}
}

func TestWatchHijacksConnectionAndReleasesIt(t *testing.T) {
	c, app := openWatchPG(t, false)
	collect(t, c, driver.Query{Text: "select 1"}) // the pool has its connection
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Watch(ctx, driver.WatchRequest{Channel: "tobira_hijack"})
	if err != nil {
		t.Fatal(err)
	}
	if got := backendsLike(t, app, "LISTEN%"); got != 1 {
		t.Errorf("listening backends = %d, want 1", got)
	}
	if n := c.pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("pool still holds %d acquired conns: the listener must not be pooled", n)
	}
	// The pool keeps working, many times over, and never hands out the listener.
	for i := 0; i < 5; i++ {
		if rows, _, _ := collect(t, c, driver.Query{Text: "select 42"}); len(rows) != 1 {
			t.Fatal("pool unusable while watching")
		}
	}
	if got := backendsLike(t, app, "LISTEN%"); got != 1 {
		t.Errorf("listening backends after pool use = %d, want 1 (DISCARD ALL must not reach it)", got)
	}
	pid := listenerPID(t, app)
	cancel()
	for range ch { // closes
	}
	// The listener's backend is gone, not just idle.
	deadline := time.Now().Add(5 * time.Second)
	for backendAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("listener backend %d still alive after stop", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// listenerPID returns the pid of app's backend that is LISTENing.
func listenerPID(t *testing.T, app string) int {
	t.Helper()
	admin, err := pgx.Connect(context.Background(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var pid int
	if err := admin.QueryRow(context.Background(), "select pid from pg_stat_activity where application_name = $1 and query like 'LISTEN%'", app).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func backendAlive(t *testing.T, pid int) bool {
	t.Helper()
	admin, err := pgx.Connect(context.Background(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var n int
	if err := admin.QueryRow(context.Background(), "select count(*) from pg_stat_activity where pid = $1", pid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestWatchConnCloseEndsStream(t *testing.T) {
	c, app := openWatchPG(t, false)
	ch, err := c.Watch(context.Background(), driver.WatchRequest{Channel: "tobira_close"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = c.Close()
	for ev := range ch {
		if ev.Err != nil {
			t.Errorf("Close surfaced an error event: %v", ev.Err)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Close + stream end took %v", d)
	}
	waitBackends(t, app, 0)
	if _, err := c.Watch(context.Background(), driver.WatchRequest{Channel: "x"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Watch on a closed conn = %v, want ErrConnection", err)
	}
}

func TestWatchTerminalError(t *testing.T) {
	c, app := openWatchPG(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Watch(ctx, driver.WatchRequest{Channel: "tobira_term"})
	if err != nil {
		t.Fatal(err)
	}
	// Kill the listener's backend: it is the one that is LISTENing and idle.
	mustExec(t, c, fmt.Sprintf(`select pg_terminate_backend(pid) from pg_stat_activity
		where application_name = %s and pid <> pg_backend_pid() and query like 'LISTEN%%'`, quoteLit(app)))
	ev, ok := recv(t, ch)
	if !ok || ev.Err == nil {
		t.Fatalf("event = %+v ok=%v, want a terminal error event", ev, ok)
	}
	if _, ok := recv(t, ch); ok {
		t.Error("channel still open after the terminal error")
	}
}

func TestWatchRefusals(t *testing.T) {
	c := openPG(t, false)
	ref := driver.CollectionRef{Name: "tobira_people"}
	if _, err := c.Watch(context.Background(), driver.WatchRequest{Collection: &ref}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("collection watch = %v, want ErrUnsupported", err)
	}
	if _, err := c.Watch(context.Background(), driver.WatchRequest{Collection: &ref, Channel: "x"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("collection+channel watch = %v, want ErrUnsupported", err)
	}
	if _, err := c.Watch(context.Background(), driver.WatchRequest{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("empty request = %v, want ErrSyntax", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Watch(ctx, driver.WatchRequest{Channel: "x"}); err == nil {
		t.Error("Watch with a cancelled ctx succeeded")
	}
}

func TestWatchChannelTooLong(t *testing.T) {
	c := openPG(t, false)
	if _, err := c.Watch(context.Background(), driver.WatchRequest{Channel: strings.Repeat("a", 64)}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("64-byte channel = %v, want ErrSyntax", err)
	}
	if _, err := c.Watch(context.Background(), driver.WatchRequest{Channel: strings.Repeat("é", 32)}); !errors.Is(err, driver.ErrSyntax) { // 64 bytes
		t.Errorf("64-byte multibyte channel = %v, want ErrSyntax", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Watch(ctx, driver.WatchRequest{Channel: strings.Repeat("a", 63)}); err != nil {
		t.Errorf("63-byte channel: %v", err)
	}
}

func TestWatchSlowConsumerDoesNotBlockCancel(t *testing.T) {
	c := openPG(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Watch(ctx, driver.WatchRequest{Channel: "tobira_slow"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ { // far more than any buffer; nobody reads
		notify(t, c, "tobira_slow", "x")
	}
	cancel()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not close after cancel with an unread backlog")
		}
	}
}
