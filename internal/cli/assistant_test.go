package cli_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/assistant/assistanttest"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/tui"
)

func withAISection(t *testing.T, h *harness, section string) {
	t.Helper()
	if err := os.WriteFile(h.cfg, []byte(testConfig+section), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With an ai: section the TUI gets the assistant cmd/tobira builds, over the
// same Manager the TUI uses.
func TestTUIGetsTheAssistant(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	withAISection(t, h, "ai:\n  provider: anthropic\n  api_key: env:MY_KEY\n")
	h.env = map[string]string{"ANTHROPIC_API_KEY": "from-harness"}
	fake := &assistanttest.Fake{ModelName: "anthropic/claude-sonnet-5-5"}
	var gotCfg config.AI
	var gotDeps cli.AssistantDeps
	h.newAssistant = func(_ context.Context, c config.AI, d cli.AssistantDeps) (assistant.Assistant, error) {
		gotCfg, gotDeps = c, d
		return fake, nil
	}
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	if _, errOut, code := h.exec(t, context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.Assistant != fake || got.AIError != nil {
		t.Fatalf("Assistant=%v AIError=%v, want the fake and no error", got.Assistant, got.AIError)
	}
	if gotCfg.Provider != "anthropic" || gotCfg.APIKey != "env:MY_KEY" {
		t.Errorf("factory got config %+v", gotCfg)
	}
	if gotDeps.Manager == nil || gotDeps.Manager != got.Manager || gotDeps.Logger == nil {
		t.Errorf("factory deps = %+v, want the TUI's Manager and a logger", gotDeps)
	}
	// The env-key fallback reads the injected environment, not os.Getenv.
	if gotDeps.Getenv == nil || gotDeps.Getenv("ANTHROPIC_API_KEY") != "from-harness" {
		t.Errorf("factory Getenv does not read the injected environment")
	}
}

// No ai: section: the factory is never called, nothing is contacted, and
// the TUI gets a nil assistant (it explains how to configure AI).
func TestTUIWithoutAISectionHasNoAssistant(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
		t.Error("factory called without an ai: section")
		return nil, nil
	}
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	if _, errOut, code := h.exec(t, context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.Assistant != nil || got.AIError != nil {
		t.Fatalf("Assistant=%v AIError=%v, want nil, nil", got.Assistant, got.AIError)
	}
}

// A failing factory does not stop the TUI: it opens with no assistant and
// the error (redacted) to show in the panel.
func TestTUIKeepsTheAssistantError(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	withAISection(t, h, "ai:\n  provider: googleai\n")
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
		return nil, errors.Join(assistant.ErrAuth, errors.New("set ai.api_key (a secret ref) or GEMINI_API_KEY; postgres://u:hunter2pw@h/db"))
	}
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	if _, errOut, code := h.exec(t, context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got.Assistant != nil {
		t.Fatalf("Assistant = %v, want nil", got.Assistant)
	}
	if !errors.Is(got.AIError, assistant.ErrAuth) || !strings.Contains(got.AIError.Error(), "GEMINI_API_KEY") {
		t.Fatalf("AIError = %v, want the ErrAuth error", got.AIError)
	}
	if strings.Contains(got.AIError.Error(), "hunter2pw") {
		t.Fatalf("AIError is not redacted: %q", got.AIError)
	}
}
