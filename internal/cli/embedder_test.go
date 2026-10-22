package cli_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/assistant/assistanttest"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/tui"
)

const embedderSection = "ai:\n  provider: ollama\n  embedder: ollama/nomic-embed-text\n"

// embedHarness is a harness whose config has an ai: section with an
// embedder, and whose embedder factory records its calls.
func embedHarness(t *testing.T, section string, e driver.Embedder, err error) (*harness, *[]config.AI) {
	t.Helper()
	h := newHarness(t)
	if werr := os.WriteFile(h.cfg, []byte(testConfig+section), 0o600); werr != nil {
		t.Fatal(werr)
	}
	h.tty = true // the TUI runs only on a terminal
	var calls []config.AI
	h.newEmbedder = func(_ context.Context, cfg config.AI, d cli.EmbedderDeps) (driver.Embedder, error) {
		if d.Getenv == nil || d.Logger == nil {
			t.Errorf("embedder deps missing collaborators: %+v", d)
		}
		calls = append(calls, cfg)
		return e, err
	}
	return h, &calls
}

// openVia opens local-pg through m and returns the ConnConfig the driver got.
func openVia(t *testing.T, h *harness, open func(context.Context, string) (driver.Conn, error)) driver.ConnConfig {
	t.Helper()
	if _, err := open(context.Background(), "local-pg"); err != nil {
		t.Fatal(err)
	}
	return h.opened[len(h.opened)-1]
}

// The TUI's session hands the ai.embedder embedder to every Open.
func TestTUISessionGetsEmbedder(t *testing.T) {
	e := drivertest.HashEmbedder{Dims: 3}
	h, calls := embedHarness(t, embedderSection, e, nil)
	var got driver.ConnConfig
	h.runTUI = func(_ context.Context, o tui.Options) error {
		got = openVia(t, h, o.Manager.Open)
		return nil
	}
	if _, errOut, code := h.exec(t, context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(*calls) != 1 || (*calls)[0].Embedder != "ollama/nomic-embed-text" {
		t.Fatalf("factory calls = %+v", *calls)
	}
	if got.Embedder != e {
		t.Fatalf("Open got Embedder %v, want the factory's", got.Embedder)
	}
}

// `tobira mcp` serves a session with the embedder.
func TestMCPSessionGetsEmbedder(t *testing.T) {
	e := drivertest.HashEmbedder{Dims: 3}
	h, _ := embedHarness(t, embedderSection+"mcp:\n  expose: [local-pg]\n", e, nil)
	var got driver.ConnConfig
	h.serveMCP = func(_ context.Context, o cli.MCPOptions) error {
		got = openVia(t, h, o.Manager.Open)
		return nil
	}
	if _, errOut, code := h.exec(t, context.Background(), "mcp"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.Embedder != e {
		t.Fatalf("Open got Embedder %v, want the factory's", got.Embedder)
	}
}

// No ai.embedder: the factory is not called and connections get none. A
// factory error leaves the session without an embedder; it does not stop
// the command, since only text similarity needs one.
func TestEmbedderAbsentOrFailing(t *testing.T) {
	for _, tc := range []struct {
		name, section string
		err           error
		wantCalls     int
	}{
		{"no-ai-section", "", nil, 0},
		{"no-embedder", "ai:\n  provider: ollama\n", nil, 0},
		{"factory-error", embedderSection, errors.New("boom"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := embedHarness(t, tc.section, drivertest.HashEmbedder{}, tc.err)
			var got driver.ConnConfig
			h.runTUI = func(_ context.Context, o tui.Options) error {
				got = openVia(t, h, o.Manager.Open)
				return nil
			}
			if _, errOut, code := h.exec(t, context.Background()); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if len(*calls) != tc.wantCalls {
				t.Fatalf("factory called %d times, want %d", len(*calls), tc.wantCalls)
			}
			if got.Embedder != nil {
				t.Fatalf("Embedder = %v, want nil", got.Embedder)
			}
		})
	}
}

// `tobira ai dev` builds its embedder in dev mode, like its assistant; the
// other commands do not.
func TestAIDevEmbedderInDevMode(t *testing.T) {
	h, _ := embedHarness(t, embedderSection, drivertest.HashEmbedder{}, nil)
	var dev []bool
	h.newEmbedder = func(_ context.Context, _ config.AI, d cli.EmbedderDeps) (driver.Embedder, error) {
		dev = append(dev, d.DevMode)
		return drivertest.HashEmbedder{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
		cancel()
		return &assistanttest.Fake{ModelName: "ollama/llama3.3"}, nil
	}
	if _, errOut, code := h.exec(t, ctx, "ai", "dev"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	h.runTUI = func(context.Context, tui.Options) error { return nil }
	if _, errOut, code := h.exec(t, context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(dev) != 2 || !dev[0] || dev[1] {
		t.Fatalf("DevMode per build = %v, want [true false]", dev)
	}
}

// The TUI is told why the embedder could not be built, redacted, so the
// "text similarity needs ai.embedder" refusal can say why; with an
// embedder, or with none configured, it is told nothing.
func TestTUIGetsEmbedderError(t *testing.T) {
	for _, tc := range []struct {
		name, section string
		err           error
		want          string
	}{
		{"factory-error", embedderSection, errors.New("ollama: bad endpoint postgres://bob:hunter2@db.local/x"), "ollama: bad endpoint"},
		{"embedder", embedderSection, nil, ""},
		{"no-embedder", "ai:\n  provider: ollama\n", errors.New("never called"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := embedHarness(t, tc.section, drivertest.HashEmbedder{Dims: 3}, tc.err)
			var got error
			h.runTUI = func(_ context.Context, o tui.Options) error {
				got = o.EmbedderError
				return nil
			}
			if _, errOut, code := h.exec(t, context.Background()); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("EmbedderError = %v, want nil", got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Error(), tc.want) {
				t.Fatalf("EmbedderError = %v, want it to contain %q", got, tc.want)
			}
			if strings.Contains(got.Error(), "hunter2") {
				t.Fatalf("EmbedderError leaks the password: %v", got)
			}
		})
	}
}
