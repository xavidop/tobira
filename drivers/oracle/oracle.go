package oracle

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
const Name = "oracle"

func init() { driver.Register(New()) }

type oracleDriver struct{}

// New returns the oracle driver.
func New() driver.Driver { return oracleDriver{} }

func (oracleDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"oracle"},
		Dialect:      "oracle",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open pool of Oracle sessions.
type conn struct {
	db       *sql.DB
	readOnly bool
	version  string // PRODUCT_COMPONENT_VERSION at Open; gates the read-only path (guardVerified)
	user     string // the login schema (USER), which CURRENT_SCHEMA is reset to
	closed   atomic.Bool

	txMu sync.Mutex
	txs  map[*oraTx]struct{} // open transactions, rolled back by Close

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
// back first.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.abortTxs()
	return mapError(context.Background(), "close", c.db.Close())
}
