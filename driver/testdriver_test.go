package driver_test

import (
	"context"

	"github.com/xavidop/tobira/driver"
)

// fakeDriver is a tiny in-test driver.
type fakeDriver struct {
	info driver.Info
	got  *driver.ConnConfig
}

func newFake(name string, schemes ...string) *fakeDriver {
	return &fakeDriver{info: driver.Info{Name: name, Kind: driver.KindSQL, Schemes: schemes}}
}

func (d *fakeDriver) Info() driver.Info { return d.info }

func (d *fakeDriver) Open(_ context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	if d.got != nil {
		*d.got = cfg
	}
	return &baseConn{name: d.info.Name}, nil
}

// baseConn implements only the core Conn.
type baseConn struct{ name string }

func (c *baseConn) Query(context.Context, driver.Query) (driver.ResultSet, error) {
	return driver.NewSliceResultSet(
		[]driver.Column{{Name: "n", Type: driver.TypeInt}},
		[]driver.Row{{driver.Int(1)}}, driver.Stats{}), nil
}
func (c *baseConn) Ping(context.Context) error { return nil }
func (c *baseConn) Close() error               { return nil }
func (c *baseConn) Namespaces(context.Context) ([]driver.Namespace, error) {
	return nil, nil
}
func (c *baseConn) Collections(context.Context, driver.NamespaceRef) ([]driver.Collection, error) {
	return nil, nil
}
func (c *baseConn) Describe(context.Context, driver.CollectionRef) (*driver.CollectionSchema, error) {
	return nil, nil
}
