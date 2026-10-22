package tools

import (
	"strings"
	"testing"

	"github.com/xavidop/tobira/drivertest"
)

// A connection whose driver plugin could not report its Info is refused by
// every tool with the reason, listed as unavailable, and never opened.
func TestUnavailableConnection(t *testing.T) {
	const why = `driver plugin "ext" did not start: exit status 1`
	e := newEnv(t, withFakeOptions(drivertest.WithVectors()), func(d *ToolDeps, _ *[]drivertest.FakeOption) {
		d.Unavailable = map[string]string{"dev": why}
		d.Connections = []string{"dev"}
	})
	conns, err := e.ts.ListConnections(ctx)
	if err != nil || len(conns) != 1 {
		t.Fatalf("list_connections = %+v, %v", conns, err)
	}
	if conns[0].Status != "unavailable" || conns[0].Error != why {
		t.Errorf("list_connections entry = %+v, want status unavailable with the reason", conns[0])
	}
	if e.ts.VectorSearchable(ctx) {
		t.Error("an unavailable connection made similar available")
	}
	q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	s, _ := e.ts.Sample(ctx, SampleInput{Connection: "dev", Collection: "orders"})
	ns, _ := e.ts.ListNamespaces(ctx, ListNamespacesInput{Connection: "dev"})
	for name, msg := range map[string]string{"query": q.Error, "sample": s.Error, "list_namespaces": ns.Error} {
		if !strings.Contains(msg, `connection "dev" is unavailable: `+why) {
			t.Errorf("%s on an unavailable connection: error = %q", name, msg)
		}
	}
	if ops := e.fakeOps(); len(ops) != 0 {
		t.Errorf("an unavailable connection was opened: %v", ops)
	}
}
