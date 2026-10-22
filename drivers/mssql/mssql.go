package mssql

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
const Name = "mssql"

func init() { driver.Register(New()) }

type mssqlDriver struct{}

// New returns the mssql driver.
func New() driver.Driver { return mssqlDriver{} }

func (mssqlDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"sqlserver", "mssql"},
		Dialect:      "tsql",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open pool of SQL Server connections. A connection that goes
// back to the pool is reset (sp_reset_connection) before its next use, so
// no session state (USE, SET options, temporary tables, an open
// transaction) reaches a later call.
type conn struct {
	db       *sql.DB
	readOnly bool
	closed   atomic.Bool

	// loginDB and defaultSchema are DB_NAME() and SCHEMA_NAME() of a fresh
	// session: the database whose schemas are listed by bare name.
	loginDB       string
	defaultSchema string

	txMu sync.Mutex
	txs  map[*msTx]struct{} // open transactions, rolled back by Close

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
	return mapError(ctx, "ping", c.db.PingContext(ctx))
}

// Close releases the pool. It is idempotent. Open transactions are rolled
// back first. Idle connections close at once; a connection still held by an
// open ResultSet closes when that result set is closed.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.abortTxs()
	return mapError(context.Background(), "close", c.db.Close())
}
