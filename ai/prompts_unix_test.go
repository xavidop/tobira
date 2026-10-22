//go:build unix

package ai

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xavidop/tobira/internal/config"
)

// An override that is not a regular file (here a FIFO, which would
// block a read forever) is an error naming the file, not a hang.
func TestPromptOverrideMustBeARegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ask.prompt")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := New(context.Background(), config.AI{Provider: "ollama"}, Deps{Getenv: env(nil), PromptDir: dir})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want one naming %s", err, path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("New blocked on a FIFO override")
	}
}
