package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Watcher = (*conn)(nil)

// eventBuffer is how many notifications may wait for a slow consumer. Past
// it the listener stops reading, so the server queues them, until the
// consumer catches up or ctx ends.
const eventBuffer = 64

// maxChannelLen is PostgreSQL's identifier limit (NAMEDATALEN-1); the server
// would silently truncate a longer channel name and listen on the wrong one.
const maxChannelLen = 63

// listenCleanupTimeout bounds the close of a finished listener.
const listenCleanupTimeout = time.Second

// watchers tracks the running listeners so Close can end them.
type watchers struct {
	mu     sync.Mutex
	cancel map[*struct{}]context.CancelFunc
	wg     sync.WaitGroup
}

// add registers a listener. It reports false when the connection is closed.
func (c *conn) addWatcher(cancel context.CancelFunc) (id *struct{}, ok bool) {
	c.watch.mu.Lock()
	defer c.watch.mu.Unlock()
	if c.closed.Load() {
		return nil, false
	}
	if c.watch.cancel == nil {
		c.watch.cancel = map[*struct{}]context.CancelFunc{}
	}
	id = new(struct{})
	c.watch.cancel[id] = cancel
	c.watch.wg.Add(1)
	return id, true
}

func (c *conn) removeWatcher(id *struct{}) {
	c.watch.mu.Lock()
	delete(c.watch.cancel, id)
	c.watch.mu.Unlock()
	c.watch.wg.Done()
}

// stopWatchers ends every listener and waits for them to finish.
func (c *conn) stopWatchers() {
	c.watch.mu.Lock()
	for _, cancel := range c.watch.cancel {
		cancel()
	}
	c.watch.mu.Unlock()
	c.watch.wg.Wait()
}

// Watch LISTENs on req.Channel and streams its notifications as Events with
// Op "notify". The listener owns a connection taken out of the pool for good
// (hijacked), because LISTEN state lives in the session and a pooled
// connection is reset with DISCARD ALL; the socket is tracked like the pool's,
// so Close ends the stream. LISTEN is established before Watch returns, so a
// notification sent afterwards is not missed. The channel closes when ctx
// ends or the connection closes; a lost server connection arrives as a final
// Event with Err set. Change streams (req.Collection) are not available.
//
// LISTEN needs no transaction, so it works on read-only connections.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (_ <-chan driver.Event, err error) {
	defer func() { err = c.named(err) }()
	const op = "watch"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if !c.flavor.has(driver.CapWatcher) {
		return nil, c.flavor.unsupported(op, driver.CapWatcher)
	}
	if req.Collection != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("postgres has no change streams; watch a LISTEN channel"))
	}
	if req.Channel == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("a channel name is required"))
	}
	if len(req.Channel) > maxChannelLen {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("channel name is %d bytes; PostgreSQL allows %d", len(req.Channel), maxChannelLen))
	}
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	id, ok := c.addWatcher(cancel)
	if !ok {
		cancel()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	// Setup ends when the caller gives up or Close runs, whichever is first.
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	defer context.AfterFunc(wctx, scancel)()
	pc, err := c.pool.Acquire(sctx)
	if err != nil {
		cancel()
		c.removeWatcher(id)
		return nil, mapError(ctx, op, err)
	}
	pg := pc.Hijack() // never returned to the pool
	if _, err := pg.Exec(sctx, "LISTEN "+quoteIdent(req.Channel)); err != nil {
		cancel()
		c.removeWatcher(id)
		closeListener(pg)
		return nil, mapError(ctx, op, err)
	}
	out := make(chan driver.Event, eventBuffer)
	go func() {
		defer c.removeWatcher(id)
		defer cancel()
		c.listen(wctx, ctx, pg, out)
	}()
	return out, nil
}

// listen forwards notifications until the caller's ctx or the connection
// ends, closes out, then tears the listener down. wctx is cancelled by Close;
// caller by the Watch caller.
func (c *conn) listen(wctx, caller context.Context, pg *pgx.Conn, out chan<- driver.Event) {
	lctx, stop := context.WithCancel(wctx)
	defer stop()
	go func() { // the caller's ctx ends the stream too
		select {
		case <-caller.Done():
			stop()
		case <-lctx.Done():
		}
	}()
	// The wait itself is not tied to lctx: a cancelled pgx wait on an idle
	// connection lingers for cancelDeadlineDelay. Dropping the socket ends the
	// wait at once, and with it the session and its LISTEN.
	go func() {
		<-lctx.Done()
		_ = pg.PgConn().Conn().Close()
	}()
	var final *driver.Event
	for lctx.Err() == nil {
		n, err := pg.WaitForNotification(context.Background())
		if err != nil {
			if lctx.Err() == nil { // not asked to stop: the connection broke
				final = &driver.Event{Time: time.Now(), Err: c.named(mapError(wctx, "watch", err))}
			}
			break
		}
		select {
		case out <- driver.Event{Time: time.Now(), Op: "notify", Channel: n.Channel, Payload: driver.String(n.Payload)}:
		case <-lctx.Done():
		}
	}
	if final != nil {
		select { // the consumer may still be draining a full buffer
		case out <- *final:
		case <-lctx.Done():
		}
	}
	close(out)
	closeListener(pg)
}

// closeListener closes the listener's connection, which ends its session and
// with it every LISTEN.
func closeListener(pg *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), listenCleanupTimeout)
	defer cancel()
	_ = pg.Close(ctx)
}
