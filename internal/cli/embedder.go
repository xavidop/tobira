package cli

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// EmbedderDeps are the collaborators of the embedder factory.
type EmbedderDeps struct {
	// Secrets resolves the ai.api_key ref.
	Secrets secrets.Resolver
	Logger  *slog.Logger
	// Getenv reads the provider key variables (cli.Deps.Getenv).
	Getenv func(string) string
	// DevMode leaves Genkit's environment alone, as for the assistant; only
	// `tobira ai dev` sets it.
	DevMode bool
}

// newManager returns the session Manager for cfg. When the config sets
// ai.embedder and the binary wires an embedder factory, every connection
// the Manager opens gets that embedder, for text similarity search. Every
// connection also gets log (the --debug log) for driver warnings. The
// error is why the configured embedder could not be built, redacted (nil
// when it was built or none is configured); it does not stop the session,
// so callers that cannot show it ignore it.
func (a *app) newManager(ctx context.Context, cfg *config.Config, readOnly bool, log *slog.Logger) (*session.Manager, error) {
	return a.newManagerMode(ctx, cfg, readOnly, log, false)
}

// newManagerMode is newManager, with the embedder in Genkit dev mode when
// dev is set.
func (a *app) newManagerMode(ctx context.Context, cfg *config.Config, readOnly bool, log *slog.Logger, dev bool) (*session.Manager, error) {
	e, err := a.buildEmbedder(ctx, cfg, log, dev)
	return session.New(cfg, a.deps.Registry, a.deps.Resolver, readOnly, session.WithEmbedder(e), session.WithLogger(log)), err
}

// buildEmbedder builds the host embedder from ai.embedder, or returns nil
// when there is none. A factory error is logged and returned, redacted, and
// leaves the session without an embedder: only text similarity needs one,
// and it then says so. The factory reads no secret (the key is resolved on
// the first Embed), but its message is still redacted for DSN-shaped
// credentials before it is logged or shown.
func (a *app) buildEmbedder(ctx context.Context, cfg *config.Config, log *slog.Logger, dev bool) (driver.Embedder, error) {
	if cfg == nil || cfg.AI == nil || strings.TrimSpace(cfg.AI.Embedder) == "" || a.deps.NewEmbedder == nil {
		return nil, nil
	}
	e, err := a.deps.NewEmbedder(ctx, *cfg.AI, EmbedderDeps{Secrets: a.deps.Resolver, Logger: log, Getenv: a.deps.Getenv, DevMode: dev})
	if err != nil {
		msg := driver.RedactMessage(err.Error(), driver.ConnConfig{})
		log.Warn("ai.embedder unavailable", "error", msg)
		return nil, errors.New(msg)
	}
	return e, nil
}
