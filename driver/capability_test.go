package driver_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/xavidop/tobira/driver"
)

type browseConn struct {
	baseConn
	unsupported map[driver.Capability]bool
}

func (c *browseConn) Browse(context.Context, driver.CollectionRef, driver.BrowseOptions) (driver.ResultSet, error) {
	return nil, nil
}

// Supports makes browseConn a Supporter.
func (c *browseConn) Supports(cap driver.Capability) bool { return !c.unsupported[cap] }

type plainBrowser struct{ baseConn }

func (plainBrowser) Browse(context.Context, driver.CollectionRef, driver.BrowseOptions) (driver.ResultSet, error) {
	return nil, nil
}

type fakeTx struct{ baseConn }

func (*fakeTx) Commit(context.Context) error   { return nil }
func (*fakeTx) Rollback(context.Context) error { return nil }
func (*fakeTx) Insert(context.Context, driver.CollectionRef, driver.Object) (driver.Object, error) {
	return nil, nil
}
func (*fakeTx) Update(context.Context, driver.CollectionRef, driver.Object, driver.Object) error {
	return nil
}
func (*fakeTx) Delete(context.Context, driver.CollectionRef, driver.Object) error { return nil }

func TestAs(t *testing.T) {
	t.Run("implements", func(t *testing.T) {
		if _, ok := driver.As[driver.Browser](&plainBrowser{}); !ok {
			t.Error("plain Browser should be ok")
		}
		if _, ok := driver.As[driver.Browser](&browseConn{}); !ok {
			t.Error("supporting Browser should be ok")
		}
	})
	t.Run("supporter says no", func(t *testing.T) {
		c := &browseConn{unsupported: map[driver.Capability]bool{driver.CapBrowser: true}}
		if b, ok := driver.As[driver.Browser](c); ok || b != nil {
			t.Error("want not ok and zero value")
		}
	})
	t.Run("missing interface", func(t *testing.T) {
		if _, ok := driver.As[driver.Mutator](&baseConn{}); ok {
			t.Error("want not ok")
		}
	})
	t.Run("tx mutator", func(t *testing.T) {
		var tx driver.Tx = &fakeTx{}
		if _, ok := driver.As[driver.Mutator](tx); !ok {
			t.Error("Tx should be a Mutator")
		}
	})
	t.Run("nil", func(t *testing.T) {
		if _, ok := driver.As[driver.Browser](nil); ok {
			t.Error("nil should not be ok")
		}
	})
	t.Run("non-capability T falls back to type assertion", func(t *testing.T) {
		c := &browseConn{unsupported: map[driver.Capability]bool{driver.CapBrowser: true}}
		if _, ok := driver.As[*browseConn](c); !ok {
			t.Error("concrete type assertion should work")
		}
		if _, ok := driver.As[driver.Supporter](c); !ok {
			t.Error("Supporter assertion should work")
		}
		if _, ok := driver.As[driver.Conn](c); !ok {
			t.Error("Conn assertion should work")
		}
	})
}

func TestCapabilitiesOf(t *testing.T) {
	tests := []struct {
		name string
		c    any
		want []driver.Capability
	}{
		{"none", &baseConn{}, nil},
		{"browser", &plainBrowser{}, []driver.Capability{driver.CapBrowser}},
		{"supporter filters", &browseConn{unsupported: map[driver.Capability]bool{driver.CapBrowser: true}}, nil},
		{"tx", &fakeTx{}, []driver.Capability{driver.CapMutator}},
		{"nil", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := driver.CapabilitiesOf(tc.c)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestCapabilitiesOfOrder(t *testing.T) {
	c := &everything{}
	want := []driver.Capability{
		driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor,
		driver.CapVectorSearcher, driver.CapTimeSeriesReader, driver.CapGraphReader,
		driver.CapCompleter, driver.CapExporter, driver.CapWatcher,
	}
	if got := driver.CapabilitiesOf(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, ok := driver.As[driver.Watcher](c); !ok {
		t.Error("Watcher")
	}
}

type invalidatingConn struct{ n int }

func (c *invalidatingConn) Invalidate() { c.n++ }

// CatalogInvalidator is optional and not a listed capability: As finds it
// with a plain assertion, and CapabilitiesOf does not report it.
func TestCatalogInvalidator(t *testing.T) {
	c := &invalidatingConn{}
	inv, ok := driver.As[driver.CatalogInvalidator](c)
	if !ok {
		t.Fatal("As did not find the CatalogInvalidator")
	}
	inv.Invalidate()
	if c.n != 1 {
		t.Fatalf("Invalidate calls = %d", c.n)
	}
	if _, ok := driver.As[driver.CatalogInvalidator](struct{}{}); ok {
		t.Fatal("As found a CatalogInvalidator on a value without Invalidate")
	}
	if caps := driver.CapabilitiesOf(c); len(caps) != 0 {
		t.Fatalf("CapabilitiesOf = %v, want none", caps)
	}
}

// CapReadOnly is a declared guarantee, not an interface: HasCapability reads
// it from Info, and CapabilitiesOf never reports it, whatever the value.
func TestCapReadOnlyIsDeclared(t *testing.T) {
	info := driver.Info{Capabilities: []driver.Capability{driver.CapBrowser, driver.CapReadOnly}}
	if !driver.HasCapability(info, driver.CapReadOnly) {
		t.Fatal("HasCapability(CapReadOnly) = false for an Info that declares it")
	}
	if driver.HasCapability(driver.Info{Capabilities: []driver.Capability{driver.CapBrowser}}, driver.CapReadOnly) {
		t.Fatal("HasCapability(CapReadOnly) = true for an Info that does not declare it")
	}
	if driver.HasCapability(driver.Info{}, driver.CapBrowser) {
		t.Fatal("HasCapability on an empty Info = true")
	}
	if string(driver.CapReadOnly) != "readonly" {
		t.Fatalf("CapReadOnly = %q", driver.CapReadOnly)
	}
	for _, c := range driver.CapabilitiesOf(&everything{}) {
		if c == driver.CapReadOnly {
			t.Fatal("CapabilitiesOf reports CapReadOnly")
		}
	}
	if _, ok := driver.As[driver.Browser](&browseConn{unsupported: map[driver.Capability]bool{driver.CapReadOnly: true}}); !ok {
		t.Fatal("an unsupported CapReadOnly must not hide Browser")
	}
}
