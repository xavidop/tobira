package main

import (
	"context"

	"github.com/xavidop/tobira/ai"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

// newAssistant builds the Genkit assistant for the ai: config section.
func newAssistant(ctx context.Context, cfg config.AI, d cli.AssistantDeps) (assistant.Assistant, error) {
	return ai.New(ctx, cfg, ai.Deps{
		Manager: d.Manager, History: d.History, Secrets: d.Secrets, Logger: d.Logger, Getenv: d.Getenv,
		PromptDir: d.PromptDir, DevMode: d.DevMode,
	})
}
