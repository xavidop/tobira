//go:build !aifake

package ai

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
)

// The scripted fake provider cannot be reached in a normal build.
func TestFakeProviderIsNotReachable(t *testing.T) {
	t.Setenv("TOBIRA_AI_FAKE_SCRIPT", "/dev/null")
	_, err := New(context.Background(), config.AI{Provider: "fake"}, Deps{Getenv: env(map[string]string{"TOBIRA_AI_FAKE_SCRIPT": "/dev/null"})})
	if !errors.Is(err, assistant.ErrProvider) {
		t.Fatalf("err = %v, want unknown provider", err)
	}
	if _, ok := lookupProvider("fake"); ok || DefaultModel("fake") != "" {
		t.Fatal("the fake provider is registered in a normal build")
	}
}
