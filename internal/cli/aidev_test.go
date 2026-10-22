package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/assistant/assistanttest"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/tui"
)

// `tobira ai dev` builds the assistant in Genkit dev mode, says how to
// attach the Dev UI, and blocks until the context ends.
func TestAIDevBuildsDevModeAssistantAndBlocks(t *testing.T) {
	h := newHarness(t)
	withAISection(t, h, "ai:\n  provider: ollama\n")
	h.env = map[string]string{"XDG_CONFIG_HOME": "/xdg"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gotDeps cli.AssistantDeps
	h.newAssistant = func(_ context.Context, _ config.AI, d cli.AssistantDeps) (assistant.Assistant, error) {
		gotDeps = d
		cancel() // the command must now return
		return &assistanttest.Fake{ModelName: "ollama/llama3.3"}, nil
	}
	out, errOut, code := h.exec(t, ctx, "ai", "dev")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !gotDeps.DevMode || gotDeps.Manager == nil {
		t.Fatalf("deps = %+v, want DevMode and a Manager", gotDeps)
	}
	if gotDeps.PromptDir != "/xdg/tobira/prompts" {
		t.Fatalf("PromptDir = %q", gotDeps.PromptDir)
	}
	if !strings.Contains(out, "Genkit dev mode: run `genkit start -- tobira ai dev`") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestAIDevNeedsAnAISection(t *testing.T) {
	h := newHarness(t)
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
		t.Error("factory called without an ai: section")
		return nil, nil
	}
	_, errOut, code := h.exec(t, context.Background(), "ai", "dev")
	if code == 0 || !strings.Contains(errOut, "ai is not configured") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestAIDevFactoryErrorIsRedacted(t *testing.T) {
	h := newHarness(t)
	withAISection(t, h, "ai:\n  provider: googleai\n")
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
		return nil, assistantErr("auth failed for postgres://u:hunter2pw@h/db")
	}
	_, errOut, code := h.exec(t, context.Background(), "ai", "dev")
	if code == 0 || strings.Contains(errOut, "hunter2pw") || !strings.Contains(errOut, "auth failed") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

type assistantErr string

func (e assistantErr) Error() string { return string(e) }

// The command is hidden: it is a developer tool, not part of the CLI.
func TestAIDevIsHidden(t *testing.T) {
	h := newHarness(t)
	out, _, _ := h.exec(t, context.Background(), "--help")
	if strings.Contains(out, "\n  ai ") {
		t.Fatalf("help lists the ai command:\n%s", out)
	}
}

// The TUI's assistant gets the prompt override directory too.
func TestTUIAssistantGetsPromptDir(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	withAISection(t, h, "ai:\n  provider: ollama\n")
	h.env = map[string]string{"XDG_CONFIG_HOME": "/xdg"}
	var gotDeps cli.AssistantDeps
	h.newAssistant = func(_ context.Context, _ config.AI, d cli.AssistantDeps) (assistant.Assistant, error) {
		gotDeps = d
		return &assistanttest.Fake{}, nil
	}
	h.runTUI = func(context.Context, tui.Options) error { return nil }
	if _, errOut, code := h.exec(t, context.Background()); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if gotDeps.PromptDir != "/xdg/tobira/prompts" || gotDeps.DevMode {
		t.Fatalf("deps = %+v", gotDeps)
	}
}

// The help says what dev mode exposes: every flow to local processes, and
// traces Genkit may keep on disk.
func TestAIDevHelpWarnsAboutExposure(t *testing.T) {
	h := newHarness(t)
	out, _, code := h.exec(t, context.Background(), "ai", "dev", "--help")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"127.0.0.1", "any local process", ".genkit/", "test connections"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help lacks %q:\n%s", want, out)
		}
	}
}
