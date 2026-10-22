package bigquery

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	bq "cloud.google.com/go/bigquery"
	bqapi "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "bigquery"

// version is the driver version.
const version = "0.1.0-dev"

func init() { driver.Register(New()) }

type bqDriver struct{}

// New returns the bigquery driver.
func New() driver.Driver { return bqDriver{} }

func (bqDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"bigquery"},
		Dialect:      "googlesql",
		Version:      version,
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapExplainer, driver.CapCompleter, driver.CapReadOnly},
	}
}

func (bqDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	return openWith(ctx, cfg)
}

var errClosed = errors.New("connection is closed")

// cancelTimeout bounds the background request that cancels the job of a
// cancelled query.
const cancelTimeout = 10 * time.Second

// conn is an open BigQuery client. BigQuery keeps no session between
// calls: every query is its own job.
type conn struct {
	client *bq.Client
	svc    *bqapi.Service // the REST API, for the table list (the client drops table types)
	// transport is the HTTP transport under both; Close drops its idle
	// connections.
	transport *http.Transport
	dsn       dsnConfig
	cfg       driver.ConnConfig
	// secrets are the parts of the service-account key that must never
	// reach an error message (see scrubMessage).
	secrets  []string
	readOnly bool
	closed   atomic.Bool

	// cancels tracks the background requests that cancel the jobs of
	// cancelled queries; Close waits for them. cancelMu orders adding to
	// it against Close.
	cancelMu sync.Mutex
	cancels  sync.WaitGroup

	catalog *sqlcomplete.Cache
}

func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, op, err)
	}
	return nil
}

// fail maps err and masks any credential in its message.
func (c *conn) fail(ctx context.Context, op string, err error) error {
	return scrubError(mapError(ctx, op, err), c.cfg, c.secrets)
}

// Ping reads the DSN's dataset, or lists one dataset when it names none.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return c.ping(ctx)
}

func (c *conn) ping(ctx context.Context) error {
	if c.dsn.dataset != "" {
		_, err := c.client.Dataset(c.dsn.dataset).Metadata(ctx)
		return c.fail(ctx, "ping", err)
	}
	it := c.client.Datasets(ctx)
	it.PageInfo().MaxSize = 1
	_, err := it.Next()
	if errors.Is(err, iterator.Done) {
		err = nil
	}
	return c.fail(ctx, "ping", err)
}

// Close releases the client. It is idempotent, and it waits (at most
// cancelTimeout) for the requests cancelling jobs of cancelled queries.
func (c *conn) Close() error {
	c.cancelMu.Lock()
	first := c.closed.CompareAndSwap(false, true)
	c.cancelMu.Unlock()
	if !first {
		return nil
	}
	c.cancels.Wait()
	err := c.client.Close()
	c.transport.CloseIdleConnections()
	return c.fail(context.Background(), "close", err)
}

// dataset resolves a namespace: "" is the DSN's dataset.
func (c *conn) dataset(ns string) string {
	if ns == "" {
		return c.dsn.dataset
	}
	return ns
}
