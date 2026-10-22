package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver, and the plugin binary is
// tobira-driver-<Name>.
const Name = "duckdb"

// Version is the driver version the plugin reports. The plugin's release
// build sets it with -ldflags -X (see .goreleaser.yaml).
var Version = "0.1.0-dev"

type duckDriver struct{}

// New returns the duckdb driver.
func New() driver.Driver { return duckDriver{} }

func (duckDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindSQL,
		Schemes: []string{"duckdb"},
		Dialect: "duckdb",
		Version: Version,
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapTransactor, driver.CapExplainer,
			driver.CapCompleter, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open DuckDB database: one database/sql pool over one DuckDB
// instance. Every instance starts locked, external access off (open.go),
// unless a writable DSN opted in with external_access=1 (external), in
// which case read-only queries are refused.
//
// readOnly conns run every statement through the read-only path. Such a
// conn normally has its own instance opened with access_mode=READ_ONLY.
// DuckDB allows one instance per file and configuration in a process, so
// when this process already holds the file with another configuration, the
// read-only conn shares that instance (shared is true) and relies on the
// lexical guard, the READ ONLY transaction, the statement-type check and
// the locked engine.
type conn struct {
	db       *sql.DB
	readOnly bool
	shared   bool
	external bool // the instance allows external access: read-only queries are refused
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

func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return mapError("ping", c.db.PingContext(ctx))
}

// Close rolls back open transactions and closes the pool. The DuckDB
// instance closes with its last pool. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.abortTxs()
	return mapError("close", c.db.Close())
}
