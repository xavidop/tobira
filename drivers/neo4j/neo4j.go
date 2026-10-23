package neo4j

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "neo4j"

func init() { driver.Register(New()) }

type neo4jDriver struct{}

// New returns the neo4j driver.
func New() driver.Driver { return neo4jDriver{} }

func (neo4jDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindGraph,
		Schemes: []string{"neo4j", "neo4j+s", "neo4j+ssc", "bolt", "bolt+s", "bolt+ssc"},
		Dialect: "cypher",
		Version: "0.1.0-dev",
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapExplainer, driver.CapGraphReader, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open Neo4j driver (a connection pool). It is safe for
// concurrent use.
type conn struct {
	drv      neo4j.DriverWithContext
	set      settings
	readOnly bool
	closed   atomic.Bool
	schemas  *schemaCache // Describe answers, briefly
}

var (
	_ driver.Conn        = (*conn)(nil)
	_ driver.Browser     = (*conn)(nil)
	_ driver.Explainer   = (*conn)(nil)
	_ driver.GraphReader = (*conn)(nil)
)

// check refuses a call on a closed connection or a done context.
func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, op, err)
	}
	return nil
}

// session opens a session on database db ("" for the connection's), in
// read mode when readOnly: the server then refuses writes.
func (c *conn) session(ctx context.Context, db string, readOnly bool) neo4j.SessionWithContext {
	mode := neo4j.AccessModeWrite
	if readOnly || c.readOnly {
		mode = neo4j.AccessModeRead
	}
	if db == "" {
		db = c.set.database
	}
	return c.drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: mode, DatabaseName: db})
}

// Ping checks the server answers: the driver opens a connection and
// authenticates on it.
func (c *conn) Ping(ctx context.Context) error {
	const op = "ping"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	return mapError(ctx, op, c.drv.VerifyConnectivity(ctx))
}

// Close closes the pool. It is idempotent.
func (c *conn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return mapError(context.Background(), "close", c.drv.Close(context.Background()))
}
