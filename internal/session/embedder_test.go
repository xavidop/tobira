package session

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

// WithEmbedder hands the host embedder to every Open (and Test), and a text
// similarity search on the opened connection uses it.
func TestWithEmbedderReachesOpen(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithVectors())
	rec := &recorder{Driver: f.Driver()}
	reg := driver.NewRegistry()
	if err := reg.Add(rec); err != nil {
		t.Fatal(err)
	}
	e := drivertest.HashEmbedder{Dims: 4}
	m := New(&config.Config{Connections: []config.Connection{fakeConn}}, reg, secrets.Resolver{}, false, WithEmbedder(e))
	defer func() { _ = m.CloseAll() }()
	c, err := m.Open(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Test(context.Background(), fakeConn); err != nil {
		t.Fatal(err)
	}
	if len(rec.cfgs) != 2 {
		t.Fatalf("%d opens", len(rec.cfgs))
	}
	for i, cfg := range rec.cfgs {
		if cfg.Embedder != e {
			t.Errorf("open %d: Embedder = %v, want the manager's", i, cfg.Embedder)
		}
	}
	vs, ok := driver.As[driver.VectorSearcher](c)
	if !ok {
		t.Fatal("no VectorSearcher")
	}
	rs, err := vs.Similar(context.Background(), driver.CollectionRef{Namespace: "main", Name: drivertest.VectorCollection}, driver.VectorQuery{Text: "near", K: 1})
	if err != nil {
		t.Fatalf("text similarity through the session: %v", err)
	}
	_ = rs.Close()
}

// Without WithEmbedder, Open gets no embedder (New stays source compatible).
func TestNoEmbedderByDefault(t *testing.T) {
	m, _, rec := newManager(t, false, fakeConn)
	defer func() { _ = m.CloseAll() }()
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if rec.cfgs[0].Embedder != nil {
		t.Fatalf("Embedder = %v, want nil", rec.cfgs[0].Embedder)
	}
}
