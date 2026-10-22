package chroma

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "chroma"

func init() { driver.Register(New()) }

type chromaDriver struct{}

// New returns the chroma driver.
func New() driver.Driver { return chromaDriver{} }

func (chromaDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindVector,
		Schemes:      []string{"chroma"},
		Dialect:      "chroma",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapVectorSearcher, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open HTTP client for one tenant. Namespaces are the tenant's
// databases; database is the default one. It is safe for concurrent use:
// http.Client is, and conn keeps no per-call state.
type conn struct {
	api      *apiClient
	tenant   string
	database string
	token    string // for masking in errors; never logged
	readOnly bool
	embedder driver.Embedder
	closed   atomic.Bool
}

var (
	_ driver.Conn           = (*conn)(nil)
	_ driver.Browser        = (*conn)(nil)
	_ driver.Mutator        = (*conn)(nil)
	_ driver.VectorSearcher = (*conn)(nil)
)

// check fails a call on a closed connection or a done context.
func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, op, err)
	}
	return nil
}

// Ping reads the default database: unlike the heartbeat it needs a valid
// token where the server checks one.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	_, err := c.api.getDatabase(ctx, c.tenant, c.database)
	return c.fail(ctx, "ping", err)
}

// Close drops the idle HTTP connections. It is idempotent.
func (c *conn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		if t, ok := c.api.http.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
	return nil
}
