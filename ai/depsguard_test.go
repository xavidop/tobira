package ai

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// genkitMCPPlugin is Genkit's MCP plugin. tobira serves MCP through mcp-go
// directly (see ai/mcpserver), at a newer mcp-go than Genkit v1.13.1 was
// built against, so the plugin must never be linked: it would compile
// against an mcp-go it was not written for.
const genkitMCPPlugin = "github.com/firebase/genkit/go/plugins/mcp"

func TestGenkitMCPPluginIsNotLinked(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	for _, dir := range []string{".", "../cmd/tobira"} {
		cmd := exec.Command("go", "list", "-deps", "-test", "./...")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list in %s: %v", dir, err)
		}
		for _, pkg := range strings.Fields(string(out)) {
			if pkg == genkitMCPPlugin || strings.HasPrefix(pkg, genkitMCPPlugin+"/") {
				t.Errorf("%s links %s", dir, pkg)
			}
		}
	}
}
