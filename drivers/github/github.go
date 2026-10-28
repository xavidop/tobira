package github

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "github"

// version is the driver version Info reports and the User-Agent carries.
const version = "0.1.0-dev"

func init() { driver.Register(New()) }

type githubDriver struct{}

// New returns the github driver.
func New() driver.Driver { return githubDriver{} }

func (githubDriver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindOther,
		Schemes: []string{"github"},
		Dialect: "github",
		Version: version,
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapCompleter, driver.CapReadOnly,
		},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open connection: one HTTP client, the parsed DSN and a few
// caches. It is safe for concurrent use.
type conn struct {
	dsn      dsnConfig
	cfg      driver.ConnConfig
	client   *client
	readOnly bool

	repos   *repoCache // repository metadata, for Collections and Describe
	catalog *nameCache // repository names, for completion
	closed  atomic.Bool
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.Browser            = (*conn)(nil)
	_ driver.Mutator            = (*conn)(nil)
	_ driver.Explainer          = (*conn)(nil)
	_ driver.Completer          = (*conn)(nil)
	_ driver.CatalogInvalidator = (*conn)(nil)
)

// check fails a call on a closed connection or a done context.
func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, op, err)
	}
	return nil
}

// Ping proves the endpoint and the token: it reads the DSN's repository
// when one is named, else the rate limit, which costs no request (an
// endpoint without rate limiting, as on some GitHub Enterprise Servers,
// answers 404, and the root document is read instead).
func (c *conn) Ping(ctx context.Context) error {
	const op = "ping"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.dsn.repo != "" {
		_, err := c.repos.get(ctx, op, c.dsn.owner, c.dsn.repo)
		return err
	}
	_, err := c.client.getJSON(ctx, "/rate_limit", nil)
	if err != nil && driver.KindOf(err) == driver.ErrNotFound {
		_, err = c.client.getJSON(ctx, "/", nil)
	}
	return c.fail(ctx, op, err)
}

// Close cancels requests in flight and drops the idle connections. It is
// idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.client.shutdown()
	return nil
}

// now is the clock, replaced in tests of the rate-limit messages.
var now = time.Now
