package main

import (
	"context"

	"github.com/xavidop/tobira/ai"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

// newEmbedder builds the Genkit embedder for ai.embedder, for vector
// search. It returns a nil interface when there is none or on error.
func newEmbedder(ctx context.Context, cfg config.AI, d cli.EmbedderDeps) (driver.Embedder, error) {
	e, err := ai.NewEmbedder(ctx, cfg, ai.Deps{Secrets: d.Secrets, Logger: d.Logger, Getenv: d.Getenv, DevMode: d.DevMode})
	if err != nil || e == nil {
		return nil, err
	}
	return e, nil
}
