package main

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

func TestCheckAIIsWired(t *testing.T) {
	// A missing key fails before any request is made.
	err := checkAI(context.Background(), config.AI{Provider: "anthropic"}, cli.AICheckDeps{Getenv: func(string) string { return "" }})
	if !errors.Is(err, assistant.ErrAuth) {
		t.Errorf("err = %v, want ErrAuth for a missing key", err)
	}
}
