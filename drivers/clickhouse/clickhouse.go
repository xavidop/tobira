package clickhouse

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "clickhouse"

func init() { driver.Register(New()) }

type chDriver struct{}

// New returns the clickhouse driver.
func New() driver.Driver { return chDriver{} }

func (chDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"clickhouse"},
		Dialect:      "clickhouse",
		Version:      version,
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapExplainer, driver.CapCompleter, driver.CapExporter, driver.CapReadOnly},
	}
}

// version is the driver version, also sent to the server as client info.
const version = "0.1.0-dev"

var errClosed = errors.New("connection is closed")

// maxNamespacePools bounds the extra pools Query opens for Query.Namespace.
const maxNamespacePools = 16

// conn is an open ClickHouse client: a pool on the DSN's database, plus one
// pool per other database a query named in Query.Namespace (the native
// protocol fixes the database when a connection is opened).
type conn struct {
	db        chdriver.Conn
	serverRO  bool        // the server runs db's user read-only already (see openPool)
	opts      *ch.Options // the DSN's options, before guardSettings; namespace pools start from them
	cfg       driver.ConnConfig
	readOnly  bool
	defaultNS string // currentDatabase() at Open
	closed    atomic.Bool

	nsMu  sync.Mutex
	nsDBs map[string]pool

	catalog *sqlcomplete.Cache // completion's view of the schema
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

func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	dctx, done := detach(ctx)
	defer done()
	return mapError(ctx, "ping", c.db.Ping(dctx))
}

// Close releases every pool. It is idempotent. A pool still serving an open
// ResultSet closes its connection when that result set is closed.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.nsMu.Lock()
	pools := c.nsDBs
	c.nsDBs = nil
	c.nsMu.Unlock()
	err := c.db.Close()
	for _, p := range pools {
		err = errors.Join(err, p.db.Close())
	}
	return mapError(context.Background(), "close", err)
}

// pool returns the pool whose connections use database ns: the main pool
// for "" and for the default database, otherwise a pool opened (and probed)
// on first use and kept until Close. At most maxNamespacePools such pools
// exist.
func (c *conn) pool(ctx context.Context, ns string) (pool, error) {
	if ns == "" || ns == c.defaultNS {
		return pool{db: c.db, serverRO: c.serverRO}, nil
	}
	c.nsMu.Lock()
	defer c.nsMu.Unlock()
	if c.closed.Load() {
		return pool{}, driver.Wrap(driver.ErrConnection, Name, "query", errClosed)
	}
	if p, ok := c.nsDBs[ns]; ok {
		return p, nil
	}
	if len(c.nsDBs) >= maxNamespacePools {
		return pool{}, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("too many databases in use on one connection; qualify table names instead"))
	}
	o := *c.opts
	o.Auth.Database = ns
	o.MaxIdleConns = 1
	p, _, err := openPool(ctx, &o, c.readOnly)
	if err != nil {
		return pool{}, scrubError(mapError(ctx, "query", err), c.cfg)
	}
	if c.nsDBs == nil {
		c.nsDBs = map[string]pool{}
	}
	c.nsDBs[ns] = p
	return p, nil
}

// roCtx makes the queries run under ctx on p carry readonly=1, the server
// layer of the read-only guard, unless p's server already runs the user
// read-only (and would refuse the setting).
func (p pool) roCtx(ctx context.Context) context.Context {
	if p.serverRO {
		return ctx
	}
	return ch.Context(ctx, ch.WithSettings(ch.Settings{readOnlyKey: 1}))
}

// roCtx is pool.roCtx for the main pool.
func (c *conn) roCtx(ctx context.Context) context.Context {
	return pool{db: c.db, serverRO: c.serverRO}.roCtx(ctx)
}

// detach returns a context that ends when ctx does but carries no deadline,
// for every call into clickhouse-go: it turns a deadline into the setting
// max_execution_time, which a user whose profile sets readonly=1 may not
// change. Cancellation still stops the query. done releases the context.
func detach(ctx context.Context) (detached context.Context, done func()) {
	if _, ok := ctx.Deadline(); !ok {
		return ctx, func() {}
	}
	d, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	return d, func() {
		stop()
		cancel()
	}
}
