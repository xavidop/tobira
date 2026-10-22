package elasticsearch

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

	es "github.com/elastic/go-elasticsearch/v8"

	"github.com/xavidop/tobira/driver"
)

// Registry names of the two drivers this package provides.
const (
	Name           = "elasticsearch"
	OpenSearchName = "opensearch"
)

// defaultNamespace is the only namespace: a cluster has no grouping above
// indexes that tobira shows.
const defaultNamespace = "default"

// flavor tells Elasticsearch and OpenSearch apart where their APIs differ:
// the product header, point-in-time endpoints and vector search.
type flavor int

const (
	flavorElastic flavor = iota
	flavorOpenSearch
)

func (f flavor) name() string {
	if f == flavorOpenSearch {
		return OpenSearchName
	}
	return Name
}

func init() {
	driver.Register(New())
	driver.Register(NewOpenSearch())
}

type esDriver struct{ fl flavor }

// New returns the elasticsearch driver.
func New() driver.Driver { return esDriver{fl: flavorElastic} }

// NewOpenSearch returns the opensearch driver: the same code, with
// OpenSearch's point-in-time API and without vector search (OpenSearch's
// k-NN plugin has another query shape).
func NewOpenSearch() driver.Driver { return esDriver{fl: flavorOpenSearch} }

func (d esDriver) Info() driver.Info {
	caps := []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer}
	if d.fl == flavorElastic {
		caps = append(caps, driver.CapVectorSearcher)
	}
	return driver.Info{
		Name:         d.fl.name(),
		Kind:         driver.KindSearch,
		Schemes:      []string{d.fl.name()},
		Dialect:      d.fl.name(),
		Version:      "0.1.0-dev",
		Capabilities: append(caps, driver.CapReadOnly),
	}
}

var errClosed = errors.New("connection is closed")

// defaultPageSize is the number of hits one search request fetches while a
// result streams.
const defaultPageSize = 1000

// conn is an open client. It is safe for concurrent use: the client is, and
// conn keeps no per-call state.
type conn struct {
	fl        flavor
	client    *es.BaseClient
	transport *http.Transport
	readOnly  bool
	// allowSystem lets read-only paths reach system, dot-prefixed and
	// hidden indexes (allow_system_indices=1).
	allowSystem bool
	embedder    driver.Embedder
	pageSize    int // hits per page; tests lower it
	pitMu       sync.Mutex
	pits        map[string]struct{} // open points in time, released on Close
	closed      atomic.Bool
}

var (
	_ driver.Conn           = (*conn)(nil)
	_ driver.Browser        = (*conn)(nil)
	_ driver.Mutator        = (*conn)(nil)
	_ driver.Explainer      = (*conn)(nil)
	_ driver.VectorSearcher = (*conn)(nil)
	_ driver.Supporter      = (*conn)(nil)
)

// Supports hides VectorSearcher on an OpenSearch connection.
func (c *conn) Supports(cp driver.Capability) bool {
	return cp != driver.CapVectorSearcher || c.fl == flavorElastic
}

func (c *conn) name() string { return c.fl.name() }

// check fails a call on a closed connection or a done context.
func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, c.name(), op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, c.name(), op, err)
	}
	return nil
}

// Ping asks for the cluster root. A 403 still proves the credentials: a
// key limited to some indexes may lack the monitor privilege it needs.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	_, err := c.do(ctx, "ping", http.MethodGet, pathOf(), nil, nil)
	if errors.Is(err, driver.ErrPermission) {
		return nil
	}
	return err
}

// Close releases the points in time its results and browse cursors still
// hold (best effort, within five seconds), then closes the client and its
// idle connections. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.releaseAllPITs()
	err := c.client.Close(context.Background())
	c.transport.CloseIdleConnections()
	if err != nil && !errors.Is(err, es.ErrAlreadyClosed) {
		return driver.Wrap(nil, c.name(), "close", err)
	}
	return nil
}
