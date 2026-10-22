package memdemo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	// Name is the driver's registry name. The plugin binary is named
	// tobira-driver-<Name>, which is the name tobira registers it under.
	Name = "memdemo"
	// Namespace is the only namespace.
	Namespace = "main"
)

// Driver is the memdemo driver. Every Conn it opens shares its store.
type Driver struct{ s *store }

// New returns a driver whose store holds the seed data.
func New() *Driver { return &Driver{s: &store{data: seed()}} }

// Info is static: it does not depend on a connection.
func (*Driver) Info() driver.Info {
	return driver.Info{
		Name:    Name,
		Kind:    driver.KindOther,
		Schemes: []string{Name},
		Dialect: Name,
		Version: "example",
		Capabilities: []driver.Capability{
			driver.CapBrowser, driver.CapMutator, driver.CapExplainer,
			driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly,
		},
	}
}

// Open returns a Conn on the driver's store. The DSN is just "memdemo:"
// (or "memdemo://"); there is nothing to dial.
func (d *Driver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, driver.Wrap(nil, Name, "open", err)
	}
	return &Conn{s: d.s, readOnly: cfg.ReadOnly}, nil
}

// table is one collection: its columns (one of them the key) and its rows
// in insertion order. Rows are never changed in place, only replaced, so a
// copy of the rows slice is a snapshot.
type table struct {
	cols []driver.Column
	key  string
	rows []driver.Row
}

func (t *table) clone() *table {
	return &table{cols: t.cols, key: t.key, rows: slices.Clone(t.rows)}
}

// col returns the index of the named column.
func (t *table) col(name string) (int, bool) {
	i := slices.IndexFunc(t.cols, func(c driver.Column) bool { return c.Name == name })
	return i, i >= 0
}

// keyIndex returns the index of the key column.
func (t *table) keyIndex() int {
	i, _ := t.col(t.key)
	return i
}

// find returns the index of the row whose key equals key.
func (t *table) find(key driver.Value) (int, bool) {
	k := t.keyIndex()
	i := slices.IndexFunc(t.rows, func(r driver.Row) bool { return equal(r[k], key) })
	return i, i >= 0
}

// data is a set of collections by name.
type data map[string]*table

func (d data) clone() data {
	out := make(data, len(d))
	for name, t := range d {
		out[name] = t.clone()
	}
	return out
}

// names returns the collection names, sorted.
func (d data) names() []string { return slices.Sorted(maps.Keys(d)) }

// table returns the collection ref names, or ErrNotFound.
func (d data) table(op string, ref driver.CollectionRef) (*table, error) {
	if ref.Namespace != "" && ref.Namespace != Namespace {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("namespace %q", ref.Namespace))
	}
	t, ok := d[ref.Name]
	if !ok {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %q", ref.Name))
	}
	return t, nil
}

// seed returns fresh seed data: the People collection.
func seed() data {
	people := drivertest.PeopleDataset()
	cols := make([]driver.Column, len(people.Columns))
	for i, c := range people.Columns {
		cols[i] = driver.Column{Name: c.Name, NativeType: c.Type.String(), Type: c.Type, Key: c.Key}
	}
	return data{drivertest.PeopleCollection(): {cols: cols, key: people.Key[0], rows: people.Rows}}
}

// store is the shared database. version counts committed writes, so a
// transaction can tell whether the store changed since it began.
type store struct {
	mu      sync.RWMutex
	data    data
	version uint64
}

func (s *store) read(fn func(data) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(s.data)
}

// write runs fn on a copy of the data and keeps the copy only when fn
// succeeds, so a failed write changes nothing.
func (s *store) write(fn func(data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.data.clone()
	if err := fn(d); err != nil {
		return err
	}
	s.data = d
	s.version++
	return nil
}

// snapshot returns a private copy of the data and the version it is at.
func (s *store) snapshot() (data, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.clone(), s.version
}

// commit replaces the data with d, unless the store changed since version.
func (s *store) commit(d data, version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != version {
		return driver.Wrap(driver.ErrConflict, Name, "commit", errors.New("the data changed since the transaction began"))
	}
	s.data = d
	s.version++
	return nil
}
