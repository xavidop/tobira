package pinecone

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "pinecone"

func init() { driver.Register(New()) }

type pineconeDriver struct{}

// New returns the pinecone driver.
func New() driver.Driver { return pineconeDriver{} }

func (pineconeDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindVector,
		Schemes:      []string{"pinecone"},
		Dialect:      "pinecone",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapVectorSearcher, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open client. It is safe for concurrent use: the SDK clients
// are, and the index handle cache is guarded by mu.
type conn struct {
	dsn      dsnConfig
	cfg      driver.ConnConfig
	apiKey   string
	client   *pinecone.Client
	http     *http.Client
	readOnly bool
	embedder driver.Embedder

	mu      sync.Mutex
	indexes map[string]*indexHandle // by index name
	closed  atomic.Bool
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

// Ping proves the credentials and the endpoint: it lists the indexes of a
// project, or reads the statistics of the one index host.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	if c.dsn.host != "" {
		h, err := c.index(ctx, "ping", c.dsn.index)
		if err != nil {
			return err
		}
		_, err = c.stats(ctx, "ping", h)
		return err
	}
	_, err := c.client.ListIndexes(ctx)
	return c.fail(ctx, "ping", err)
}

// Close closes every index connection and the idle HTTP connections. It
// is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.Lock()
	handles := c.indexes
	c.indexes = nil
	c.mu.Unlock()
	var errs []error
	for _, h := range handles {
		if err := h.conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	c.http.CloseIdleConnections()
	return c.fail(context.Background(), "close", errors.Join(errs...))
}
