package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// A turn resolves its connection's driver (Options.ResolveDriver) in the
// turn's goroutine before the assistant runs; a failure ends the turn with
// the reason and the assistant never runs.
func TestAITurnResolvesTheDriverFirst(t *testing.T) {
	f := askFake()
	h := newAIHarness(t, f, nil)
	var mu sync.Mutex
	var resolved []string
	fail := false
	h.m.resolveDriver = func(_ context.Context, conn string) error {
		mu.Lock()
		defer mu.Unlock()
		resolved = append(resolved, conn)
		if fail {
			return errors.New(`connection "local" is unavailable: driver plugin "ext" did not answer: boom`)
		}
		return nil
	}
	h.openAI()
	h.askAI("who is here?")
	mu.Lock()
	got := strings.Join(resolved, ",")
	mu.Unlock()
	if got != "local" || len(f.Calls()) != 1 {
		t.Fatalf("resolved %q, assistant calls %d; want local resolved, then one call", got, len(f.Calls()))
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	h.askAI("and now?")
	if n := len(f.Calls()); n != 1 {
		t.Errorf("the assistant ran on an unavailable connection (%d calls)", n)
	}
	if txt := h.text(); !strings.Contains(txt, "is unavailable") {
		t.Errorf("the panel does not say why:\n%s", txt)
	}
}
