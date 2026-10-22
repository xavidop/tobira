package vertica

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "vertica"

// version is the driver version.
const version = "0.1.0-dev"

func init() {
	silenceSDKLogging()
	driver.Register(New())
}

type vDriver struct{}

// New returns the vertica driver.
func New() driver.Driver { return vDriver{} }

func (vDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindSQL,
		Schemes: []string{"vertica"},
		Dialect: "vertica",
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

// conn is an open Vertica connection: a database/sql pool of
// vertica-sql-go sessions on the DSN's database, plus one pool per other
// schema a query named in Query.Namespace (each of its sessions starts
// with that schema first on its search path).
type conn struct {
	db       *sql.DB
	url      string // vertica-sql-go's connection string; holds the secret
	cfg      driver.ConnConfig
	readOnly bool
	// defaultSchema is CURRENT_SCHEMA() at Open.
	defaultSchema string
	closed        atomic.Bool

	nsMu  sync.Mutex
	nsDBs map[string]*sql.DB

	txMu sync.Mutex
	txs  map[*vTx]struct{}

	catalog *sqlcomplete.Cache
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
	return c.scrub(mapError(ctx, "ping", c.db.PingContext(ctx)))
}

// scrub masks the connection's secrets in err.
func (c *conn) scrub(err error) error { return scrubError(err, c.cfg) }

// Close rolls back open transactions and closes every pool. It is
// idempotent.
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
	return c.scrub(mapError(context.Background(), "close", err))
}

// pool returns the pool whose sessions resolve unqualified names in
// namespace ns: the main pool for "" and the default schema, otherwise a
// pool opened on first use and kept until Close. At most
// maxNamespacePools such pools exist.
func (c *conn) pool(ctx context.Context, op, ns string) (*sql.DB, error) {
	if ns == "" || ns == c.defaultSchema {
		return c.db, nil
	}
	if err := checkSchemaName(op, ns); err != nil {
		return nil, err
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
	p := openPool(c.url, c.sessionSetup(ns))
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

// sessionSetup is what every new session of a pool runs first: READ ONLY
// session characteristics on a read-only connection, and, for a namespace
// pool, a search path that starts with that schema.
func (c *conn) sessionSetup(ns string) []string {
	var setup []string
	if c.readOnly {
		setup = append(setup, setReadOnlySession)
	}
	if ns != "" {
		setup = append(setup, "SET SEARCH_PATH TO "+quoteIdent(ns)+", "+defaultSearchPath)
	}
	return setup
}

// defaultSearchPath is Vertica's default search path, which a namespace
// pool's sessions keep after their own schema, so system tables still
// resolve unqualified.
const defaultSearchPath = `"$user", public, v_catalog, v_monitor, v_internal`

// setReadOnlySession makes every later transaction of the session READ
// ONLY, the server-side layer under the read-only guard.
const setReadOnlySession = "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY"
