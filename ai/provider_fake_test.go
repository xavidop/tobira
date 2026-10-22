//go:build aifake

package ai

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
)

// With -tags aifake, ai.provider: fake answers from $TOBIRA_AI_FAKE_SCRIPT.
func TestFakeProviderAnswersFromTheScript(t *testing.T) {
	h := newHarness(t, nil) // for its Manager
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(`[{"json":{"query":"select 42","explanation":"canned"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), config.AI{Provider: "fake"}, Deps{Manager: h.mgr, Getenv: env(map[string]string{"TOBIRA_AI_FAKE_SCRIPT": path})})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model() != "fake/scripted" {
		t.Fatalf("Model = %q", a.Model())
	}
	res, err := a.Ask(context.Background(), assistant.AskRequest{Scope: scopeColl, Question: "q"}, assistant.Events{})
	if err != nil || res.Query != "select 42" {
		t.Fatalf("Ask = %+v, %v", res, err)
	}
	if _, err := New(context.Background(), config.AI{Provider: "fake"}, Deps{Manager: h.mgr, Getenv: env(nil)}); err == nil {
		t.Fatal("the fake provider without a script must fail")
	}
}
