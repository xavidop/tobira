package sqlcomplete

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Invalidator is driver.CatalogInvalidator, kept under its old name: a
// connection that caches catalog lookups, invalidated by the host after
// anything that may change the catalog.
type Invalidator = driver.CatalogInvalidator

// DefaultTTL is how long a Cache keeps a catalog answer.
const DefaultTTL = 60 * time.Second

// Cache wraps a Catalog and remembers its answers for a TTL. Concurrent
// misses for one key share a single load. It is safe for concurrent use.
// Errors are never cached, and an answer loaded before an Invalidate is not
// stored. Returned slices are shared: callers must not modify them.
type Cache struct {
	inner Catalog
	ttl   time.Duration
	now   func() time.Time

	mu       sync.Mutex
	gen      uint64 // bumped by Invalidate
	entries  map[string]cacheEntry
	inflight map[string]*flight
}

type cacheEntry struct {
	at    time.Time
	names []string
}

// flight is one load in progress; done closes when names and err are set.
type flight struct {
	done  chan struct{}
	names []string
	err   error
}

var (
	_ Catalog     = (*Cache)(nil)
	_ Invalidator = (*Cache)(nil)
)

// NewCache returns a caching Catalog over inner; a non-positive ttl means
// DefaultTTL.
func NewCache(inner Catalog, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Cache{
		inner: inner, ttl: ttl, now: time.Now,
		entries: map[string]cacheEntry{}, inflight: map[string]*flight{},
	}
}

// Invalidate drops every cached answer and detaches loads in progress, so
// neither they nor later callers' loads can repopulate the cache with data
// read before the invalidation.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.gen++
	clear(c.entries)
	clear(c.inflight)
	c.mu.Unlock()
}

func (c *Cache) get(ctx context.Context, key string, load func() ([]string, error)) ([]string, error) {
	for {
		c.mu.Lock()
		if e, ok := c.entries[key]; ok && c.now().Sub(e.at) < c.ttl {
			c.mu.Unlock()
			return e.names, nil
		}
		if f, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// The leader's cancellation is not ours: load again.
			if f.err != nil && ctx.Err() == nil && (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) {
				continue
			}
			return f.names, f.err
		}
		f := &flight{done: make(chan struct{})}
		c.inflight[key] = f
		gen := c.gen
		c.mu.Unlock()

		c.lead(key, f, gen, load)
		return f.names, f.err
	}
}

// lead runs the load for flight f and always settles it: the deferred
// cleanup also runs when load panics or ends the goroutine with
// runtime.Goexit, so a dead flight never blocks waiters or later callers.
func (c *Cache) lead(key string, f *flight, gen uint64, load func() ([]string, error)) {
	finished := false
	defer func() {
		if !finished {
			f.names, f.err = nil, errAborted
		}
		c.mu.Lock()
		if c.inflight[key] == f {
			delete(c.inflight, key)
		}
		if f.err == nil && c.gen == gen {
			c.entries[key] = cacheEntry{at: c.now(), names: f.names}
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.names, f.err = c.run(load)
	finished = true
}

var errAborted = errors.New("sqlcomplete: catalog load aborted")

// maxStack bounds the stack text attached to a recovered catalog panic.
const maxStack = 4 << 10

// run calls load, turning a panic into an error (with a truncated stack, so
// bugs stay diagnosable) rather than crashing the caller or leaving waiters
// blocked on a flight that will not finish.
func (c *Cache) run(load func() ([]string, error)) (names []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			if len(stack) > maxStack {
				stack = append(stack[:maxStack:maxStack], "...(truncated)"...)
			}
			names, err = nil, fmt.Errorf("sqlcomplete: catalog panic: %v\n%s", r, stack)
		}
	}()
	return load()
}

// Namespaces implements Catalog.
func (c *Cache) Namespaces(ctx context.Context) ([]string, error) {
	return c.get(ctx, "n", func() ([]string, error) { return c.inner.Namespaces(ctx) })
}

// Collections implements Catalog.
func (c *Cache) Collections(ctx context.Context, ns string) ([]string, error) {
	return c.get(ctx, "c\x00"+ns, func() ([]string, error) { return c.inner.Collections(ctx, ns) })
}

// Fields implements Catalog.
func (c *Cache) Fields(ctx context.Context, ns, coll string) ([]string, error) {
	return c.get(ctx, "f\x00"+ns+"\x00"+coll, func() ([]string, error) { return c.inner.Fields(ctx, ns, coll) })
}
