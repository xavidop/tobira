package main

import (
	"context"

	"github.com/xavidop/tobira/ai"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

// checkAI is the online provider check of `tobira doctor --online`.
func checkAI(ctx context.Context, cfg config.AI, d cli.AICheckDeps) error {
	return ai.Check(ctx, cfg, ai.Deps{Secrets: d.Secrets, Getenv: d.Getenv})
}
