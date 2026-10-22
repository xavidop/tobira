package libsql

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "libsql"

func init() { driver.Register(New()) }

type libsqlDriver struct{}

// New returns the libsql driver.
func New() driver.Driver { return libsqlDriver{} }

// Info claims only the libsql scheme: http(s):// and ws(s):// DSNs work when
// the connection names the driver explicitly.
func (libsqlDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"libsql"},
		Dialect:      "sqlite",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly},
	}
}

// conn is an open libSQL database. It holds no server-side session: each
// statement outside a transaction runs on a fresh stream.
type conn struct {
	client   *client
	readOnly bool
	closed   atomic.Bool

	txMu sync.Mutex
	txs  map[*tx]struct{} // open transactions, rolled back by Close

	catalog *sqlcomplete.Cache
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

// Ping runs SELECT 1 on a fresh stream.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	_, err := c.readOnlyExec(ctx, hranaStmt{SQL: "SELECT 1", WantRows: true})
	return mapError("ping", err)
}

// Close rolls back open transactions (concurrently, bounded by
// endTimeout), then cancels every request still in flight and drops the
// HTTP connections. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.abortTxs()
	c.client.shutdown()
	return nil
}
