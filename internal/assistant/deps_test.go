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
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
		if strings.Contains(string(out), "firebase/genkit") {
			t.Fatalf("go %v lists Genkit in the root module", args)
		}
	}
}
