package memdriver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	name      = "memdriver"
	namespace = "main"
)

// Register this driver in driver.Default so `memdriver:` DSNs resolve to it.
// A real driver package does exactly this in init, and importing the package
// (usually with a blank import) is what makes the driver available.
func init() { driver.Register(New()) }

// New returns the driver.
func New() driver.Driver { return memDriver{} }

type memDriver struct{}

// Info is static: it must not depend on a connection. No capabilities are
// listed because memConn implements only driver.Conn.
func (memDriver) Info() driver.Info {
	return driver.Info{
		Name:    name,
		Kind:    driver.KindOther,
		Schemes: []string{"memdriver"},
		Dialect: "memdriver",
		Version: "example",
	}
}

// Open builds a private copy of the data, so connections never share state.
func (memDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, driver.Wrap(nil, name, "open", err)
	}
	return &memConn{
		people:   drivertest.PeopleDataset(),
		readOnly: cfg.ReadOnly,
	}, nil
}

type memConn struct {
	people   drivertest.Dataset
	readOnly bool
	closed   atomic.Bool
}

func (c *memConn) ref() driver.CollectionRef {
	return driver.CollectionRef{Namespace: namespace, Name: drivertest.PeopleCollection()}
}

// check is the guard every call starts with: honour ctx, refuse a closed Conn.
func (c *memConn) check(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, name, op, err) // errors.Is(err, context.Canceled) still holds
	}
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, name, op, errors.New("connection is closed"))
	}
	return nil
}

func (c *memConn) Ping(ctx context.Context) error { return c.check(ctx, "ping") }

// Close is idempotent.
func (c *memConn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *memConn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: namespace, Label: "namespace", Default: true}}, nil
}

func (c *memConn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	if ns.Name != namespace {
		return nil, driver.Wrap(driver.ErrNotFound, name, "collections", fmt.Errorf("namespace %q", ns.Name))
	}
	return []driver.Collection{{
		Ref:         c.ref(),
		Type:        driver.CollectionTable,
		RowEstimate: int64(len(c.people.Rows)),
	}}, nil
}

func (c *memConn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	if ref != c.ref() {
		return nil, driver.Wrap(driver.ErrNotFound, name, "describe", fmt.Errorf("collection %q", ref.Name))
	}
	fields := make([]driver.Field, len(c.people.Columns))
	for i, col := range c.people.Columns {
		fields[i] = driver.Field{Name: col.Name, NativeType: col.Type.String(), Type: col.Type, Nullable: !col.Key}
	}
	return &driver.CollectionSchema{
		Ref:         ref,
		Type:        driver.CollectionTable,
		Fields:      fields,
		PrimaryKey:  c.people.Key,
		RowEstimate: int64(len(c.people.Rows)),
		SizeBytes:   -1,
	}, nil
}

// Query understands one statement: select * from <collection>.
func (c *memConn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(q.Text), ";")))
	if len(words) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, name, "query", errors.New("empty query"))
	}
	if words[0] != "select" {
		// Anything that is not a read is a write. The read-only guard is a hard
		// refusal and the query text cannot switch it off.
		if c.readOnly || q.ReadOnly {
			return nil, driver.Wrap(driver.ErrReadOnly, name, "query", errors.New("writes are refused"))
		}
		return nil, driver.Wrap(driver.ErrUnsupported, name, "query", errors.New("only select is supported"))
	}
	if len(words) != 4 || words[1] != "*" || words[2] != "from" {
		return nil, driver.Wrap(driver.ErrSyntax, name, "query", errors.New("want: select * from <collection>"))
	}
	if words[3] != c.ref().Name {
		return nil, driver.Wrap(driver.ErrNotFound, name, "query", fmt.Errorf("collection %q", words[3]))
	}

	rows := c.people.Rows
	truncated := false
	if q.Limit > 0 && len(rows) > q.Limit {
		rows, truncated = rows[:q.Limit], true
	}
	cols := make([]driver.Column, len(c.people.Columns))
	for i, col := range c.people.Columns {
		ref := c.ref()
		cols[i] = driver.Column{Name: col.Name, NativeType: col.Type.String(), Type: col.Type, Source: &ref, Key: col.Key}
	}
	return driver.NewSliceResultSet(cols, rows, driver.Stats{RowsAffected: -1, Truncated: truncated}), nil
}
