package firestore

import (
	"context"
	"errors"
	"sync/atomic"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "firestore"

// version is the driver version, also sent in the user agent.
const version = "0.1.0-dev"

func init() { driver.Register(New()) }

type firestoreDriver struct{}

// New returns the firestore driver.
func New() driver.Driver { return firestoreDriver{} }

func (firestoreDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindDocument,
		Schemes: []string{"firestore"},
		Dialect: "firestore-json",
		Version: version,
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapCompleter, driver.CapWatcher, driver.CapReadOnly,
		},
	}
}

func (firestoreDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	return openWith(ctx, cfg)
}

var errClosed = errors.New("connection is closed")

// conn is an open Firestore client on one database. It is safe for
// concurrent use.
type conn struct {
	client   *fs.Client
	dsn      dsnConfig
	cfg      driver.ConnConfig
	secrets  []string // key fragments masked out of every error
	readOnly bool
	closed   atomic.Bool
	catalog  *catalogCache
}

// check refuses a call on a closed connection or with a done context.
func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// fail maps err and masks credentials in its message.
func (c *conn) fail(ctx context.Context, op string, err error) error {
	return scrubError(mapError(ctx, op, err), c.cfg, c.secrets)
}

// Ping lists one root collection id: Firestore has no ping call, and this
// is the cheapest read that needs only list access.
func (c *conn) Ping(ctx context.Context) error {
	const op = "ping"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	it := c.client.Collections(ctx)
	it.PageInfo().MaxSize = 1
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		return c.fail(ctx, op, err)
	}
	return nil
}

// Close closes the client. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	// The client also closes the emulator's gRPC connection.
	if err := c.client.Close(); err != nil {
		return c.fail(context.Background(), "close", err)
	}
	return nil
}

var _ driver.Conn = (*conn)(nil)
