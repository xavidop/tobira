package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Name is the registry name of the driver.
const Name = "postgres"

func init() {
	driver.Register(New())
	for _, f := range []flavor{flavorRedshift, flavorCockroach, flavorGreenplum} {
		driver.Register(newFlavored(f))
	}
}

// postgresDriver opens pgx pools. connect, probeVector and detect are fields,
// not package variables, so tests can inspect the pool config without a
// server; a nil probeVector skips the pgvector probe and a nil detect makes
// an unforced connection PostgreSQL.
//
// flavor is the flavor the driver forces ("" for the postgres driver, which
// asks the server unless the DSN names one).
type postgresDriver struct {
	connect     func(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error)
	probeVector func(ctx context.Context, pool *pgxpool.Pool) string
	detect      func(ctx context.Context, c *conn) (flavor, error)
	flavor      flavor
}

// New returns the postgres driver. It detects Redshift, CockroachDB and
// Greenplum from the server unless the DSN forces a flavor (flavor=…).
func New() driver.Driver {
	return &postgresDriver{connect: connectPool, probeVector: probeVectorSchema, detect: detectServerFlavor}
}

// newFlavored returns the driver registered for one flavor: same code, its
// own name and DSN schemes, and only the capabilities the flavor has.
func newFlavored(f flavor) driver.Driver {
	return &postgresDriver{connect: connectPool, probeVector: probeVectorSchema, detect: detectServerFlavor, flavor: f}
}

func (d *postgresDriver) Info() driver.Info {
	info := driver.Info{
		Name:         Name,
		Kind:         driver.KindSQL,
		Schemes:      []string{"postgres", "postgresql"},
		Dialect:      "postgresql",
		Version:      "0.1.0-dev",
		Capabilities: flavorPostgres.capabilities(),
	}
	if d.flavor != "" {
		info.Name, info.Capabilities, info.Schemes = string(d.flavor), d.flavor.capabilities(), nil
		for _, sc := range []string{"redshift", "cockroachdb", "cockroach", "greenplum"} {
			if flavorSchemes[sc] == d.flavor {
				info.Schemes = append(info.Schemes, sc)
			}
		}
	}
	return info
}

var errClosed = errors.New("connection is closed")

// conn is an open pool of PostgreSQL connections.
type conn struct {
	pool     *pgxpool.Pool
	socks    *sockets
	readOnly bool
	flavor   flavor // never "" once Open returns
	name     string // the opening driver's Info().Name, carried by errors
	closed   atomic.Bool
	// typeNames caches pg_type names of non-builtin OIDs (uint32 -> string).
	typeNames sync.Map

	txMu sync.Mutex
	txs  map[*pgTx]struct{} // open transactions, rolled back by Close

	watch watchers // running Watch listeners, ended by Close

	catalog *sqlcomplete.Cache // completion's view of the schema

	// vectorSchema is the schema of the pgvector extension, found at Open;
	// "" when the extension is not installed in the database.
	vectorSchema string
	embedder     driver.Embedder // ConnConfig.Embedder, for text similarity
	vec          vectorCache     // Similar's schemas and index metrics
}

// Supports reports the capabilities this connection offers at runtime: only
// those of its flavor, and VectorSearcher only when the database has the
// pgvector extension.
func (c *conn) Supports(cp driver.Capability) bool {
	if !c.flavor.has(cp) {
		return false
	}
	if cp == driver.CapVectorSearcher {
		return c.vectorSchema != ""
	}
	return true
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

func (c *conn) Ping(ctx context.Context) (err error) {
	defer func() { err = c.named(err) }()
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	return mapError(ctx, "ping", c.pool.Ping(ctx))
}

// Close releases the pool. It is idempotent. Close waits for every acquired
// connection to come back, so a ResultSet the caller never closed holds Close
// until the caller closes it. Against a hung server it returns within about
// closeGrace (4 s): once that passes, all remaining sockets are dropped, which
// also fails the queries of callers still in flight, so pgx's own 15 s cleanup
// deadlines are not waited out.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	done := make(chan struct{})
	go func() {
		c.stopWatchers()
		c.abortTxs() // pool.Close waits for every acquired connection
		c.pool.Close()
		close(done)
	}()
	timer := time.NewTimer(closeGrace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		c.socks.closeAll()
		<-done
	}
	return nil
}
