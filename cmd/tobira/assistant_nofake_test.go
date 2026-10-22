//go:build !aifake

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

// In a normal build ai.provider: fake is an unknown provider, even with a
// script set.
func TestFakeProviderIsRefused(t *testing.T) {
	scrubGenkitEnv(t)
	getenv := func(k string) string {
		if k == "TOBIRA_AI_FAKE_SCRIPT" {
			return "/dev/null"
		}
		return ""
	}
	_, err := newAssistant(context.Background(), config.AI{Provider: "fake"}, cli.AssistantDeps{Getenv: getenv})
	if !errors.Is(err, assistant.ErrProvider) || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("provider fake: err = %v, want unknown provider", err)
	}
}
