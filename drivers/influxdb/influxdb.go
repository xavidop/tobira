package influxdb

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "influxdb"

func init() { driver.Register(New()) }

type influxDriver struct{}

// New returns the influxdb driver.
func New() driver.Driver { return influxDriver{} }

func (influxDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindTimeSeries,
		Schemes: []string{"influxdb3", "influxdb"},
		Dialect: "influxdb3-sql",
		Version: "0.1.0-dev",
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapTimeSeriesReader, driver.CapReadOnly,
		},
	}
}

// language is the query language of a connection's Query calls.
type language uint8

const (
	langSQL      language = iota // InfluxDB 3 SQL (the default)
	langInfluxQL                 // InfluxQL
)

func (l language) String() string {
	if l == langInfluxQL {
		return "influxql"
	}
	return "sql"
}

var errClosed = errors.New("connection is closed")

// conn is an open InfluxDB 3 connection: a Flight client for queries and
// an HTTP client for the few management reads (ping, database list).
type conn struct {
	client   *influxdb3.Client
	http     *http.Client
	set      settings
	redact   driver.ConnConfig // DSN and resolved token, for scrubbing messages
	readOnly bool              // informational: every call is read-only
	rangeCap int               // most rows a Range returns (maxRangePoints)
	closed   atomic.Bool
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

func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	_, err := c.get(ctx, "ping", "/ping")
	return err
}

// Close releases the Flight and HTTP clients. It is idempotent. A result
// set still open fails on its next read.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return mapError(context.Background(), "close", c.client.Close())
}
