package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// The end-to-end tests run the real binary and never modify it, so each
// variant (default, aifake) is built once per test process and shared.
// Linking the binary with every driver is what made the aifake suite run
// past go test's default timeout when every test built its own.
var (
	sharedMu   sync.Mutex
	sharedBins = map[string]string{}
	sharedDir  string
)

// TestMain removes the shared build directory after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDir != "" {
		_ = os.RemoveAll(sharedDir)
	}
	os.Exit(code)
}

// sharedBuild returns the path of the binary built from this package with
// the given build tags (empty for none), building it on first use.
func sharedBuild(t *testing.T, tags string) string {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if bin, ok := sharedBins[tags]; ok {
		return bin
	}
	if sharedDir == "" {
		dir, err := os.MkdirTemp("", "tobira-e2e-")
		if err != nil {
			t.Fatal(err)
		}
		sharedDir = dir
	}
	bin := filepath.Join(sharedDir, "tobira-"+tags)
	args := []string{"build"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "-o", bin, ".")
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	sharedBins[tags] = bin
	return bin
}
