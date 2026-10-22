package sqlite

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Name is the registry name of the driver.
const Name = "sqlite"

func init() { driver.Register(New()) }

type sqliteDriver struct{}

// New returns the sqlite driver.
func New() driver.Driver { return sqliteDriver{} }

func (sqliteDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"sqlite", "sqlite3", "file"},
		Dialect:      "sqlite",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly},
	}
}

// conn is an open SQLite database. ro is always set; rw is nil when the
// connection was opened read-only.
type conn struct {
	path   string
	rw, ro *sql.DB
	closed atomic.Bool

	txMu sync.Mutex
	txs  map[*tx]struct{} // open transactions, rolled back by Close

	catalog *sqlcomplete.Cache // completion's view of the schema
}

// handle picks the handle for q: read-only queries and read-only connections
// always use the read-only handle.
func (c *conn) handle(readOnly bool) *sql.DB {
	if c.rw == nil || readOnly {
		return c.ro
	}
	return c.rw
}

func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return mapError(op, err)
	}
	return nil
}

func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return mapError("ping", c.ro.PingContext(ctx))
}

// Close releases both handles. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.abortTxs()
	var err error
	if c.rw != nil {
		err = c.rw.Close()
	}
	if e := c.ro.Close(); err == nil {
		err = e
	}
	return mapError("close", err)
}
