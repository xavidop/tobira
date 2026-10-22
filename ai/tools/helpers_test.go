package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// dsnPassword is the password in every test DSN. No tool output may contain it.
const dsnPassword = "s3cretpw"

// fakeHistory records history entries in memory.
type fakeHistory struct {
	mu      sync.Mutex
	entries []history.Entry
}

func (h *fakeHistory) Record(_ context.Context, e history.Entry, _ driver.ConnConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, e)
	return nil
}

func (h *fakeHistory) all() []history.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]history.Entry(nil), h.entries...)
}

// env is a Toolset over a Fake with People, two connections to it ("dev"
// and "dev2"), a read-only one ("ro") and a broken entry.
type env struct {
	t    *testing.T
	fake *drivertest.Fake
	mgr  *session.Manager
	hist *fakeHistory
	ts   *Toolset

	mu     sync.Mutex
	events assistant.Events
	calls  []assistant.ToolCall
}

type envOption func(*ToolDeps, *[]drivertest.FakeOption)

func withPolicy(p RowPolicy) envOption {
	return func(d *ToolDeps, _ *[]drivertest.FakeOption) { d.Policy = p }
}

func withMaxRows(n int) envOption {
	return func(d *ToolDeps, _ *[]drivertest.FakeOption) { d.MaxRows = n }
}

func withFakeOptions(opts ...drivertest.FakeOption) envOption {
	return func(_ *ToolDeps, o *[]drivertest.FakeOption) { *o = append(*o, opts...) }
}

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	deps := ToolDeps{Policy: RowsAlways}
	fopts := []drivertest.FakeOption{drivertest.WithPeople()}
	for _, o := range opts {
		o(&deps, &fopts)
	}
	f := drivertest.NewFake(fopts...)
	addOrders(f)
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Connections: []config.Connection{
			{Name: "dev", Driver: "fake", DSN: "fake://admin:" + dsnPassword + "@db.local/app"},
			{Name: "dev2", Driver: "fake", DSN: "fake://admin:" + dsnPassword + "@db.local/app2"},
			{Name: "ro", Driver: "fake", DSN: "fake://reader:" + dsnPassword + "@db.local/app", ReadOnly: true},
		},
		Broken: []config.BrokenConnection{{Name: "bad", Line: 12, Err: "line 12: dsn is not a URL"}},
	}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	e := &env{t: t, fake: f, mgr: mgr, hist: &fakeHistory{}}
	deps.Manager = mgr
	deps.History = e.hist
	e.ts = NewToolset(deps)
	e.ts.fallbackEvents = e.currentEvents
	return e
}

// addOrders adds an orders collection with a foreign key to People, a
// comment, a default and DDL that holds a secret.
func addOrders(f *drivertest.Fake) {
	f.AddCollection("main", "orders",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "person_id", Type: driver.TypeInt}, {Name: "status", Type: driver.TypeString, NativeType: "text"}},
		[]driver.Row{{driver.Int(1), driver.Int(3), driver.String("open")}, {driver.Int(2), driver.Int(4), driver.String("paid")}},
		"id")
	f.OnDescribe("main", "orders", func(s *driver.CollectionSchema) {
		s.Comment = "customer orders"
		s.DDL = "CREATE TABLE orders (... DEFAULT 'token=" + dsnPassword + "')"
		s.ForeignKeys = []driver.ForeignKey{{Name: "orders_person", Fields: []string{"person_id"}, References: driver.CollectionRef{Namespace: "main", Name: "tobira_people"}, RefFields: []string{"id"}}}
		s.Indexes = append(s.Indexes, driver.Index{Name: "orders_status", Fields: []string{"status"}})
		for i := range s.Fields {
			if s.Fields[i].Name == "status" {
				s.Fields[i].Default = "'open'"
				s.Fields[i].Comment = "order state"
			}
		}
	})
}

func (e *env) currentEvents() assistant.Events {
	e.mu.Lock()
	ev := e.events
	e.mu.Unlock()
	ev.ToolCall = func(tc assistant.ToolCall) {
		e.mu.Lock()
		e.calls = append(e.calls, tc)
		e.mu.Unlock()
	}
	return ev
}

func (e *env) setEvents(ev assistant.Events) {
	e.mu.Lock()
	e.events = ev
	e.mu.Unlock()
}

func (e *env) toolCalls() []assistant.ToolCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]assistant.ToolCall(nil), e.calls...)
}

// fakeOps lists the ops the Fake saw, in order.
func (e *env) fakeOps() []string {
	var out []string
	for _, c := range e.fake.Calls() {
		out = append(out, c.Op)
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// golden compares got with testdata/<name>.golden, rewriting it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func noSecret(t *testing.T, what string, b []byte) {
	t.Helper()
	if bytes.Contains(b, []byte(dsnPassword)) {
		t.Errorf("%s contains the DSN password:\n%s", what, b)
	}
}
