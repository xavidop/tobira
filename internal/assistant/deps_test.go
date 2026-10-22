package assistant_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The root module never depends on Genkit, not even transitively in the
// module graph: Genkit's go.mod requires database SDKs. It lives in ai/.
func TestRootModuleHasNoGenkit(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	for _, args := range [][]string{{"list", "-m", "all"}, {"list", "-deps", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = "../.." // the root module
		// The proxy stays on: go list -m all reads the go.mod of every module
		// in the graph, which a cold module cache (CI) has not downloaded.
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
		if strings.Contains(string(out), "firebase/genkit") {
			t.Fatalf("go %v lists Genkit in the root module", args)
		}
	}
}
