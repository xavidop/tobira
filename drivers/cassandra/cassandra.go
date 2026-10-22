package cassandra

import (
	"context"
	"errors"
	"sync/atomic"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "cassandra"

func init() { driver.Register(New()) }

type cassandraDriver struct{}

// New returns the cassandra driver.
func New() driver.Driver { return cassandraDriver{} }

func (cassandraDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindWideColumn,
		Schemes: []string{"cassandra", "scylla"},
		Dialect: "cql",
		Version: "0.1.0-dev",
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapCompleter, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open gocql session (a pool per host). It is safe for
// concurrent use.
type conn struct {
	session  *gocql.Session
	set      settings
	proto    int // the negotiated protocol version
	readOnly bool
	closed   atomic.Bool
	catalog  *sqlcomplete.Cache // completion's view of the schema
	schemas  *schemaCache       // Describe answers
	udfs     udfCache           // the read-only guard's view of user functions
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.Browser            = (*conn)(nil)
	_ driver.Mutator            = (*conn)(nil)
	_ driver.Completer          = (*conn)(nil)
	_ driver.CatalogInvalidator = (*conn)(nil)
)

func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// keyspace returns ns, or the DSN's keyspace when ns is empty.
func (c *conn) keyspace(ns string) string {
	if ns == "" {
		return c.set.keyspace
	}
	return ns
}

// Ping reads the node's version from system.local.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	var v string
	err := c.session.Query("SELECT release_version FROM system.local").Idempotent(true).ScanContext(ctx, &v)
	return mapError(ctx, "ping", err)
}

// Close closes the session and its connections. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.session.Close()
	return nil
}
