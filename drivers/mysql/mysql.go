package mysql

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
const Name = "mysql"

func init() {
	driver.Register(New())
	driver.Register(newFlavored(flavorMariaDB))
	driver.Register(newFlavored(flavorSingleStore))
}

// mysqlDriver opens go-sql-driver pools. The mysql driver detects the flavor
// unless the DSN forces one; the mariadb and singlestore drivers (flavored)
// are the same code with their own name, scheme and capabilities, and force
// their flavor.
type mysqlDriver struct {
	flavor   flavor
	flavored bool
}

// New returns the mysql driver.
func New() driver.Driver { return mysqlDriver{} }

// newFlavored returns the driver registered for one flavor.
func newFlavored(f flavor) mysqlDriver { return mysqlDriver{flavor: f, flavored: true} }

func (d mysqlDriver) Info() driver.Info {
	info := driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"mysql"},
		Dialect:      "mysql",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly},
	}
	if d.flavored {
		info.Name, info.Schemes = d.flavor.String(), []string{d.flavor.String()}
		if d.flavor == flavorSingleStore { // no read-only transactions, no provable Tx
			info.Capabilities = []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapCompleter}
		}
	}
	return info
}

var errClosed = errors.New("connection is closed")

// conn is an open pool of MySQL connections.
type conn struct {
	db       *sql.DB
	readOnly bool
	version  string // SELECT VERSION() at Open; with the flavors it gates the read-only path (readOnlyRefusal)
	detected flavor // the server's flavor, as Open detected it
	flavor   flavor // the flavor in use: forced by the DSN, else detected
	name     string // the opening driver's Info().Name, carried by errors (named)
	closed   atomic.Bool

	// sessionCheckSQL overrides the post-check's session query; tests use it
	// to make that check fail. "" means sessionCheckSQL.
	sessionCheckSQL string

	txMu sync.Mutex
	txs  map[*myTx]struct{} // open transactions, rolled back by Close

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

func (c *conn) Ping(ctx context.Context) (err error) {
	defer func() { err = c.named(err) }()
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return mapError(ctx, "ping", c.db.PingContext(ctx))
}

// Close releases the pool. It is idempotent. Open transactions are rolled
// back first. Idle connections close at once; a connection still held by an
// open ResultSet closes when that result set is closed.
func (c *conn) Close() (err error) {
	defer func() { err = c.named(err) }()
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.abortTxs()
	return mapError(context.Background(), "close", c.db.Close())
}
