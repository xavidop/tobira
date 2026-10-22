package snowflake

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "snowflake"

// version is the driver version.
const version = "0.1.0-dev"

func init() {
	silenceSDKLogging()
	disableSDKProbes()
	driver.Register(New())
}

type sfDriver struct{}

// New returns the snowflake driver.
func New() driver.Driver { return sfDriver{} }

func (sfDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindSQL,
		Schemes: []string{"snowflake"},
		Dialect: "snowflake",
		Version: version,
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor,
			driver.CapCompleter, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// maxNamespacePools bounds the extra pools Query opens for Query.Namespace.
const maxNamespacePools = 8

// conn is an open Snowflake connection: a database/sql pool of gosnowflake
// sessions on the DSN's database and schema, plus one pool per other
// namespace a query named in Query.Namespace (a session's schema is part
// of its login).
type conn struct {
	db   *sql.DB
	base sf.Config // the DSN's config; namespace pools start from it
	// transport is shared by every pool (base.Transporter wraps it); Close
	// drops its idle connections.
	transport *http.Transport
	cfg       driver.ConnConfig
	readOnly  bool
	// defaultDB and defaultSchema are CURRENT_DATABASE() and
	// CURRENT_SCHEMA() at Open; either may be "".
	defaultDB, defaultSchema string
	closed                   atomic.Bool

	nsMu  sync.Mutex
	nsDBs map[string]*sql.DB

	txMu sync.Mutex
	txs  map[*sfTx]struct{}

	catalog *sqlcomplete.Cache
}

// querier is what runs a statement: a pool or a transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
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

// defaultNamespace is the connection's namespace name, "" when it has none.
func (c *conn) defaultNamespace() string {
	if c.defaultDB == "" || c.defaultSchema == "" {
		return ""
	}
	return namespaceName(c.defaultDB, c.defaultSchema)
}

func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return c.scrub(mapError(ctx, "ping", c.db.PingContext(ctx)))
}

// scrub masks the connection's secrets in err.
func (c *conn) scrub(err error) error { return scrubError(err, c.cfg) }

// Close rolls back open transactions and closes every pool, logging the
// sessions out. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.abortTxs()
	c.nsMu.Lock()
	pools := c.nsDBs
	c.nsDBs = nil
	c.nsMu.Unlock()
	err := c.db.Close()
	for _, p := range pools {
		err = errors.Join(err, p.Close())
	}
	c.transport.CloseIdleConnections()
	return c.scrub(mapError(context.Background(), "close", err))
}

// pool returns the pool whose sessions use namespace ns: the main pool for
// "" and the default namespace, otherwise a pool opened on first use and
// kept until Close. At most maxNamespacePools such pools exist.
func (c *conn) pool(ctx context.Context, op, ns string) (*sql.DB, error) {
	if ns == "" || ns == c.defaultNamespace() {
		return c.db, nil
	}
	db, schema, err := splitNamespace(ns)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	if schema == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("a namespace is database.schema"))
	}
	c.nsMu.Lock()
	defer c.nsMu.Unlock()
	if c.closed.Load() {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if p, ok := c.nsDBs[ns]; ok {
		return p, nil
	}
	if len(c.nsDBs) >= maxNamespacePools {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("too many namespaces in use on one connection; qualify table names instead"))
	}
	cfg := c.base
	cfg.Database, cfg.Schema = db, schema
	p := openPool(cfg)
	if err := p.PingContext(ctx); err != nil {
		_ = p.Close()
		return nil, c.scrub(mapError(ctx, op, err))
	}
	if c.nsDBs == nil {
		c.nsDBs = map[string]*sql.DB{}
	}
	c.nsDBs[ns] = p
	return p, nil
}
