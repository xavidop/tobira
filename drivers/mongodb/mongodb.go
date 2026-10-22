package mongodb

import (
	"context"
	"errors"
	"sync/atomic"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "mongodb"

func init() { driver.Register(New()) }

type mongoDriver struct{}

// New returns the mongodb driver.
func New() driver.Driver { return mongoDriver{} }

func (mongoDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindDocument,
		Schemes: []string{"mongodb", "mongodb+srv"},
		Dialect: "mongodb",
		Version: "0.1.0-dev",
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapExplainer,
			driver.CapCompleter, driver.CapWatcher, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open MongoDB client (a pool). It is safe for concurrent use.
type conn struct {
	client    *mongo.Client
	defaultDB string // the DSN's database, or "test"
	namedDB   bool   // the DSN named its database
	readOnly  bool
	closed    atomic.Bool
	catalog   *catalogCache // completion's view of the catalog
}

func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// database returns the namespace's database, or the default one.
func (c *conn) database(ns string) *mongo.Database {
	if ns == "" {
		ns = c.defaultDB
	}
	return c.client.Database(ns)
}

func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return mapError(ctx, "ping", c.client.Ping(ctx, nil))
}

// Close disconnects the client. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return mapError(context.Background(), "close", c.client.Disconnect(context.Background()))
}

var _ driver.Conn = (*conn)(nil)
