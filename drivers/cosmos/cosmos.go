package cosmos

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "cosmos"

func init() { driver.Register(New()) }

type cosmosDriver struct{}

// New returns the cosmos driver.
func New() driver.Driver { return cosmosDriver{} }

func (cosmosDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindDocument,
		Schemes: []string{"cosmos"},
		Dialect: "cosmos-sql",
		Version: "0.1.0-dev",
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapVectorSearcher,
			driver.CapCompleter, driver.CapWatcher, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open Cosmos DB account client. It is safe for concurrent use.
type conn struct {
	client    *azcosmos.Client
	transport *http.Transport
	defaultDB string // the DSN's database; "" when it names none
	readOnly  bool
	embedder  driver.Embedder
	closed    atomic.Bool
	catalog   *catalogCache
	props     *propsCache
}

var (
	_ driver.Conn           = (*conn)(nil)
	_ driver.Browser        = (*conn)(nil)
	_ driver.Mutator        = (*conn)(nil)
	_ driver.VectorSearcher = (*conn)(nil)
	_ driver.Watcher        = (*conn)(nil)
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

// database returns the namespace's name, or the default one; "" with an
// ErrSyntax when neither names a database.
func (c *conn) database(op, ns string) (string, error) {
	if ns == "" {
		ns = c.defaultDB
	}
	if ns == "" {
		return "", driver.Wrap(driver.ErrSyntax, Name, op,
			errors.New("no database: add ?database= to the DSN or pick a namespace"))
	}
	return ns, nil
}

// container returns a client for db's container.
func (c *conn) container(op, db, name string) (*azcosmos.ContainerClient, error) {
	cc, err := c.client.NewContainer(db, name)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	return cc, nil
}

// Ping reads the default database, or lists one database when the DSN
// names none.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return ping(ctx, c.client, c.defaultDB)
}

func ping(ctx context.Context, client *azcosmos.Client, db string) error {
	if db != "" {
		d, err := client.NewDatabase(db)
		if err != nil {
			return driver.Wrap(driver.ErrSyntax, Name, "ping", err)
		}
		_, err = d.Read(ctx, nil)
		return mapError(ctx, "ping", err)
	}
	pager := client.NewQueryDatabasesPager("SELECT TOP 1 * FROM root", &azcosmos.QueryDatabasesOptions{})
	_, err := pager.NextPage(ctx)
	return mapError(ctx, "ping", err)
}

// Close releases the client and its idle connections. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.client.Close()
	c.transport.CloseIdleConnections()
	return nil
}
