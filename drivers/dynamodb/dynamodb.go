package dynamodb

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "dynamodb"

// namespaceName is the single namespace: a DynamoDB connection sees the
// tables of one account in one region.
const namespaceName = "default"

func init() { driver.Register(New()) }

type ddbDriver struct{}

// New returns the dynamodb driver.
func New() driver.Driver { return ddbDriver{} }

func (ddbDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindKeyValue,
		Schemes: []string{"dynamodb"},
		Dialect: "partiql",
		Version: "0.1.0-dev",
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapCompleter, driver.CapWatcher, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open DynamoDB client. It is safe for concurrent use.
type conn struct {
	db        *dynamodb.Client
	streams   *dynamodbstreams.Client
	transport *http.Transport // closed on Close, so no idle connection outlives the conn
	region    string
	readOnly  bool
	secrets   []string // credential values masked in every error, longest first
	closed    atomic.Bool
	done      chan struct{} // closed by Close, so watchers end with the connection
	catalog   *catalogCache
	limits    limits
}

// limits bounds what the driver holds in memory. They are per connection
// so tests can lower them.
type limits struct {
	prefetch    int // items a query result reads ahead to infer its columns
	resultBytes int // bytes of items held at once by a prefetch, a page or a sample
	sortItems   int // items a sorted Browse may read and sort in memory
	sample      int // items Describe samples
}

var defaultLimits = limits{prefetch: 1000, resultBytes: 32 << 20, sortItems: 10000, sample: 100}

func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// Ping lists at most one table: the cheapest signed call.
func (c *conn) Ping(ctx context.Context) error {
	const op = "ping"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	_, err := c.db.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(1)})
	return c.fail(ctx, op, err)
}

// Close ends every watcher of the connection and releases idle HTTP
// connections. It is idempotent; DynamoDB keeps no session to end.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.done)
	c.transport.CloseIdleConnections()
	return nil
}

func (c *conn) writable(op string) error {
	if c.readOnly {
		return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
	}
	return nil
}

var _ driver.Conn = (*conn)(nil)
