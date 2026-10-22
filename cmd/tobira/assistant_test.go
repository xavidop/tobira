package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

// The binary wires the Genkit assistant: ollama needs no key, so it builds
// with no environment and no network.
func TestNewAssistantBuildsGenkit(t *testing.T) {
	scrubGenkitEnv(t)
	a, err := newAssistant(context.Background(), config.AI{Provider: "ollama"}, cli.AssistantDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || a.Model() != "ollama/llama3.3" {
		t.Fatalf("assistant = %v", a)
	}
}

func TestNewAssistantErrorIsNilInterface(t *testing.T) {
	scrubGenkitEnv(t)
	a, err := newAssistant(context.Background(), config.AI{Provider: "googleai"}, cli.AssistantDeps{})
	if !errors.Is(err, assistant.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if a != nil {
		t.Fatalf("assistant = %#v, want a nil interface", a)
	}
}

// The injected Getenv reaches the env-key fallback.
func TestNewAssistantUsesInjectedGetenv(t *testing.T) {
	scrubGenkitEnv(t)
	getenv := func(k string) string {
		if k == "GEMINI_API_KEY" {
			return "fake-injected-key"
		}
		return ""
	}
	a, err := newAssistant(context.Background(), config.AI{Provider: "googleai"}, cli.AssistantDeps{Getenv: getenv})
	if err != nil || a == nil {
		t.Fatalf("newAssistant = %v, %v; want the key from the injected Getenv", a, err)
	}
}

// scrubGenkitEnv keeps the developer's shell out of a real genkit.Init: no
// provider key, no dev mode, no telemetry.
func scrubGenkitEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		"GENKIT_ENV", "GENKIT_TELEMETRY_SERVER", "GENKIT_REFLECTION_V2_SERVER", "GENKIT_REFLECTION_PORT",
	} {
		t.Setenv(k, "")
	}
}

// The prompt override directory reaches the assistant: an invalid override
// there fails New, naming the file.
func TestNewAssistantForwardsPromptDir(t *testing.T) {
	scrubGenkitEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "ask.prompt")
	if err := os.WriteFile(path, []byte("---\ninput: [broken\n---\nx"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newAssistant(context.Background(), config.AI{Provider: "ollama"}, cli.AssistantDeps{PromptDir: dir})
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
}

// The scripted fake provider's package is linked only with -tags aifake.
func TestFakeProviderIsNotLinked(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	for tags, want := range map[string]bool{"": false, "aifake": true} {
		cmd := exec.Command("go", "list", "-deps", "-tags", tags, ".")
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list -tags %q: %v\n%s", tags, err, out)
		}
		if got := strings.Contains(string(out), "github.com/xavidop/tobira/ai/aitest"); got != want {
			t.Fatalf("tags %q: aitest linked = %v, want %v", tags, got, want)
		}
	}
}

// DevMode reaches the assistant: Genkit's environment is left alone (a
// normal build pins it to prod).
func TestNewAssistantForwardsDevMode(t *testing.T) {
	scrubGenkitEnv(t)
	if _, err := newAssistant(context.Background(), config.AI{Provider: "ollama"}, cli.AssistantDeps{DevMode: true}); err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("GENKIT_ENV"); v != "" {
		t.Fatalf("dev mode: GENKIT_ENV = %q, want untouched", v)
	}
}
