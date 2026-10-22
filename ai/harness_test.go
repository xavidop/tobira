package ai

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/ai/aitest"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// Sentinels: none of them may ever reach a model request.
const (
	sentinelKey = "sk-SENTINEL-apikey-7f3a9c"                        // the provider API key
	keyRef      = "op://vault/tobira-ai-key-ref-SENTINEL/credential" // ai.api_key
	pwRef       = "op://vault/tobira-db-pw-ref-SENTINEL/password"    // the connection's password ref
	dbPassword  = "db-SENTINEL-pw-41c2"                              // what pwRef resolves to
	fakeModel   = "fake/model"
)

// harness is an assistant over the fake model and a Fake database with
// People and an orders collection, through a connection "dev" whose
// password comes from pwRef.
type harness struct {
	a    *genkitAssistant
	m    *aitest.Model
	fake *drivertest.Fake
	mgr  *session.Manager
}

type harnessOption func(*config.AI, *Deps)

func withSendRows(mode string) harnessOption {
	return func(c *config.AI, _ *Deps) { c.SendRows = mode }
}

func withMaxToolCalls(n int) harnessOption {
	return func(c *config.AI, _ *Deps) { c.MaxToolCalls = n }
}

func withPromptDir(dir string) harnessOption {
	return func(_ *config.AI, d *Deps) { d.PromptDir = dir }
}

func newHarness(t *testing.T, script []aitest.Response, opts ...harnessOption) *harness {
	t.Helper()
	return newHarnessWith(t, script, nil, opts...)
}

// newHarnessWith is newHarness with more connections to the same Fake
// after "dev".
func newHarnessWith(t *testing.T, script []aitest.Response, more []config.Connection, opts ...harnessOption) *harness {
	t.Helper()
	return newHarnessFake(t, script, nil, more, opts...)
}

// newHarnessFake is newHarnessWith, with more options for the Fake.
func newHarnessFake(t *testing.T, script []aitest.Response, fakeOpts []drivertest.FakeOption, more []config.Connection, opts ...harnessOption) *harness {
	t.Helper()
	f := drivertest.NewFake(append([]drivertest.FakeOption{drivertest.WithPeople()}, fakeOpts...)...)
	f.AddCollection("main", "orders",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "note", Type: driver.TypeString, NativeType: "text"}},
		[]driver.Row{{driver.Int(1), driver.String("vip " + dbPassword)}, {driver.Int(2), driver.String("regular")}},
		"id")
	f.OnDescribe("main", "orders", func(s *driver.CollectionSchema) {
		s.Comment = "orders; legacy password " + dbPassword + "; ai key " + sentinelKey
	})
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: append([]config.Connection{
		{Name: "dev", Driver: "fake", DSN: "fake://admin@db.local/app", Password: pwRef},
	}, more...)}
	res := secrets.Resolver{Resolve: func(_ context.Context, ref string) ([]byte, error) {
		if ref != pwRef {
			t.Errorf("unexpected ref %q", ref)
		}
		return []byte(dbPassword), nil
	}}
	mgr := session.New(cfg, reg, res, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })

	ai := config.AI{Provider: "fake", APIKey: keyRef, SendRows: "always", MaxRows: 50, MaxToolCalls: 8, ContextTokens: 8000}
	deps := Deps{Manager: mgr, Logger: nil}
	for _, o := range opts {
		o(&ai, &deps)
	}
	if deps.Logger == nil {
		deps.Logger = discardLogger()
	}
	prompts, err := loadPromptSources(deps.PromptDir, deps.Logger)
	if err != nil {
		t.Fatal(err)
	}
	m := aitest.NewModel(script...)
	a := &genkitAssistant{cfg: ai, deps: deps, model: fakeModel, key: driver.NewSecret(sentinelKey), prompts: prompts}
	a.initFn = func(ctx context.Context) (*genkit.Genkit, error) {
		g := genkit.Init(ctx, genkit.WithPromptFS(noPromptFS), genkit.WithPromptDir("."), genkit.WithDefaultModel(fakeModel))
		m.Define(g, fakeModel)
		return g, nil
	}
	return &harness{a: a, m: m, fake: f, mgr: mgr}
}

// events records what a turn reports.
type events struct {
	mu     sync.Mutex
	calls  []assistant.ToolCall
	usages []assistant.Usage
}

func (e *events) Events() assistant.Events {
	return assistant.Events{
		ToolCall: func(tc assistant.ToolCall) { e.mu.Lock(); e.calls = append(e.calls, tc); e.mu.Unlock() },
		Usage:    func(u assistant.Usage) { e.mu.Lock(); e.usages = append(e.usages, u); e.mu.Unlock() },
	}
}

func (e *events) toolCalls() []assistant.ToolCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]assistant.ToolCall(nil), e.calls...)
}

// noSentinels fails when any captured request holds a secret or a ref.
func (h *harness) noSentinels(t *testing.T) {
	t.Helper()
	reqs := h.m.RequestJSON()
	if len(reqs) == 0 {
		t.Fatal("no request captured")
	}
	for i, r := range reqs {
		for _, s := range []string{sentinelKey, keyRef, pwRef, dbPassword, "SENTINEL"} {
			if bytes.Contains(r, []byte(s)) {
				t.Errorf("request %d contains %q:\n%s", i, s, r)
			}
		}
	}
}

// lastRequest is the last captured request as JSON text.
func (h *harness) lastRequest(t *testing.T) string {
	t.Helper()
	reqs := h.m.RequestJSON()
	if len(reqs) == 0 {
		t.Fatal("no request captured")
	}
	return string(reqs[len(reqs)-1])
}

// toolNames lists the tools offered in request i.
func (h *harness) toolNames(i int) []string {
	var out []string
	for _, d := range h.m.Requests()[i].Tools {
		out = append(out, d.Name)
	}
	return out
}

// golden compares got with testdata/<name>.golden, rewriting it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func hasAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
