package ai

import (
	"context"
	"net"
	"os"
	"testing"

	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
)

// NewToolHost initialises Genkit with no model and no prompts, pinned to
// prod however the shell is set: the MCP server's instance.
func TestNewToolHostIsBareAndPinnedToProd(t *testing.T) {
	port := freePort(t)
	t.Setenv("GENKIT_ENV", "dev")
	t.Setenv("GENKIT_REFLECTION_PORT", port)
	t.Setenv("GENKIT_TELEMETRY_SERVER", "http://telemetry.tobira.test:4033")
	t.Setenv("GENKIT_REFLECTION_V2_SERVER", "ws://reflection.tobira.test:4100")
	// No prompt file of the working directory may be loaded.
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/prompts", 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/prompts/stray.prompt", "/stray.prompt"} {
		if err := os.WriteFile(dir+p, []byte("---\nmodel: x/y\n---\nhi"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	g, err := NewToolHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := api.CurrentEnvironment(); got != api.EnvironmentProd {
		t.Fatalf("Genkit environment = %q, want prod", got)
	}
	for _, k := range []string{"GENKIT_TELEMETRY_SERVER", "GENKIT_REFLECTION_V2_SERVER"} {
		if v := os.Getenv(k); v != "" {
			t.Fatalf("%s = %q, want it cleared", k, v)
		}
	}
	if tools := genkit.ListTools(g); len(tools) != 0 {
		t.Errorf("tools = %d, want none", len(tools))
	}
	if p := genkit.LookupPrompt(g, "stray"); p != nil {
		t.Error("./prompts/stray.prompt was loaded")
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("reflection port %s is bound: %v", port, err)
	}
	_ = l.Close()
}
