package d1

import (
	"context"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "d1"

func init() { driver.Register(New()) }

type d1Driver struct{}

// New returns the Cloudflare D1 driver.
func New() driver.Driver { return d1Driver{} }

// Info claims the d1 scheme. There is no Transactor: D1's REST API runs
// each request on its own, with no session to hold a transaction open.
func (d1Driver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"d1"},
		Dialect:      "sqlite",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapCompleter, driver.CapReadOnly},
	}
}

// conn is one D1 database reached over the REST API. It holds no server-side
// session: every statement is one HTTPS request.
type conn struct {
	client   *client
	readOnly bool
	closed   atomic.Bool
	catalog  *sqlcomplete.Cache
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

// Ping runs SELECT 1.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	_, err := c.client.exec(ctx, "SELECT 1", nil)
	return mapError("ping", err)
}

// Close cancels every request in flight and drops the HTTP connections. It
// is idempotent.
func (c *conn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.client.shutdown()
	}
	return nil
}
