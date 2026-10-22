package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "redis"

func init() {
	// go-redis logs through a process-wide logger that writes to stderr
	// (every failed pool dial, for one), which would draw over the TUI. The
	// driver owns its SDK's global, so it silences it here; the errors that
	// matter come back from the calls themselves.
	goredis.SetLogger(discardLogger{})
	driver.Register(New())
}

// discardLogger is go-redis's process-wide logger: it writes nothing.
type discardLogger struct{}

func (discardLogger) Printf(context.Context, string, ...any) {}

type redisDriver struct{}

// New returns the redis driver.
func New() driver.Driver { return redisDriver{} }

func (redisDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindKeyValue,
		Schemes:      []string{"redis", "rediss"},
		Dialect:      "redis",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapWatcher, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// maxPools bounds the go-redis pools a connection keeps, one per logical
// database; the least recently used one is closed beyond it (never the
// DSN's database, and a pool still in use closes when its last user
// releases it).
const maxPools = 16

// pool is one database's go-redis client with its users.
type pool struct {
	cl      *goredis.Client
	refs    int
	used    uint64 // tick of the last client() call, for LRU
	evicted bool   // removed from the map; close when refs drops to 0
}

// conn is an open Redis connection: one go-redis pool per logical database,
// created on first use. The pool for the DSN's database exists from Open.
type conn struct {
	opts      *goredis.Options // template; DB is replaced per pool
	defaultDB int
	readOnly  bool
	closed    atomic.Bool

	mu      sync.Mutex
	pools   map[int]*pool
	tick    uint64
	dbCount int // CONFIG GET databases at Open; 0 = unknown (cap at nsCap)

	flagsMu sync.Mutex
	flags   map[string]bool // COMMAND INFO name -> refused under read-only (see verifyCommand)
}

func newConn(opts *goredis.Options, readOnly bool) *conn {
	c := &conn{opts: opts, defaultDB: opts.DB, readOnly: readOnly, pools: map[int]*pool{}, flags: map[string]bool{}}
	c.pools[opts.DB] = &pool{cl: goredis.NewClient(opts)}
	return c
}

func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// dbLimit is the number of databases a namespace may name.
func (c *conn) dbLimit() int {
	if c.dbCount > 0 {
		return min(c.dbCount, nsCap)
	}
	return nsCap
}

// client returns the pool for a namespace ("" is the DSN's database) and a
// release function the caller must call when done with it. A namespace
// that is not a database number below the server's database count is
// ErrNotFound.
func (c *conn) client(op, ns string) (*goredis.Client, int, func(), error) {
	db := c.defaultDB
	if ns != "" {
		n, err := strconv.Atoi(ns)
		if err != nil || n < 0 || strconv.Itoa(n) != ns {
			return nil, 0, nil, driver.Wrap(driver.ErrNotFound, Name, op, errors.New("namespace "+strconv.Quote(ns)+" is not a database number"))
		}
		db = n
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return nil, 0, nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if db != c.defaultDB && db >= c.dbLimit() {
		return nil, 0, nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("database %d does not exist: the server has %d", db, c.dbLimit()))
	}
	c.tick++
	p, ok := c.pools[db]
	if !ok {
		o := *c.opts
		o.DB = db
		p = &pool{cl: goredis.NewClient(&o)}
		c.pools[db] = p
	}
	p.used = c.tick // before evicting, so the new pool is the most recent
	p.refs++
	if !ok {
		c.evictLocked()
	}
	var once sync.Once
	release := func() { once.Do(func() { c.release(p) }) }
	return p.cl, db, release, nil
}

// evictLocked drops least recently used pools beyond maxPools.
func (c *conn) evictLocked() {
	for len(c.pools) > maxPools {
		victim, oldest := -1, uint64(0)
		for db, p := range c.pools {
			if db == c.defaultDB {
				continue
			}
			if victim < 0 || p.used < oldest {
				victim, oldest = db, p.used
			}
		}
		if victim < 0 {
			return
		}
		p := c.pools[victim]
		delete(c.pools, victim)
		p.evicted = true
		if p.refs == 0 {
			_ = p.cl.Close()
		}
	}
}

func (c *conn) release(p *pool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.refs--
	if p.refs == 0 && p.evicted {
		_ = p.cl.Close()
	}
}

// poolCount is the number of open pools (for tests).
func (c *conn) poolCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pools)
}

func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	cl, _, release, err := c.client("ping", "")
	if err != nil {
		return err
	}
	defer release()
	return mapError(ctx, "ping", cl.Ping(ctx).Err())
}

// Close releases every pool. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, p := range c.pools {
		errs = append(errs, p.cl.Close())
	}
	c.pools = nil
	return mapError(context.Background(), "close", errors.Join(errs...))
}
