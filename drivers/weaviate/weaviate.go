package weaviate

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/weaviate/weaviate-go-client/v5/weaviate/connection"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/data"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/db"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/schema"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "weaviate"

// defaultNamespace is the only namespace: Weaviate has no grouping above
// collections (multi-tenancy is per collection).
const defaultNamespace = "default"

func init() { driver.Register(New()) }

type weaviateDriver struct{}

// New returns the weaviate driver.
func New() driver.Driver { return weaviateDriver{} }

func (weaviateDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindVector,
		Schemes:      []string{"weaviate"},
		Dialect:      "weaviate",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapVectorSearcher, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open client. It is safe for concurrent use: the client keeps
// no per-call state and its http.Client is shared.
type conn struct {
	con       *connection.Connection
	transport *http.Transport
	schema    *schema.API
	data      *data.API
	readOnly  bool
	embedder  driver.Embedder
	// version is the server version, read at Open; the client's path
	// builders ask for it.
	version atomic.Value // string
	// versions is the client's version provider; Open refreshes it once,
	// before the connection is shared, so it never writes afterwards.
	versions *db.VersionProvider
	closed   atomic.Bool
	// pageSize is the number of objects a streamed query reads per
	// request; tests lower it.
	pageSize int
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

// Ping reads the server's meta document, which needs a valid API key when
// authentication is on.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	_, err := c.meta(ctx, "ping")
	return err
}

// Close drops the idle connections of the connection's own transport. It
// is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.transport.CloseIdleConnections()
	return nil
}
