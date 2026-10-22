package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// AssistantDeps are what the assistant factory gets besides the ai: config.
type AssistantDeps struct {
	Manager *session.Manager
	// History records tool-run queries; nil records nothing.
	History *history.Store
	Secrets secrets.Resolver
	Logger  *slog.Logger
	// Getenv reads the provider key variables (cli.Deps.Getenv).
	Getenv func(string) string
	// PromptDir holds prompt overrides (<name>.prompt files);
	// $XDG_CONFIG_HOME/tobira/prompts. "" means none.
	PromptDir string
	// DevMode runs Genkit in dev mode for the Dev UI; only `tobira ai dev`
	// sets it.
	DevMode bool
}

// assistantError is a factory error with its message redacted. It unwraps
// to the assistant sentinels only, never to the original error.
type assistantError struct {
	msg  string
	kind []error
}

func (e *assistantError) Error() string   { return e.msg }
func (e *assistantError) Unwrap() []error { return e.kind }

// buildAssistant builds the AI assistant when the config has an ai: section
// and the binary wires a factory. It returns (nil, nil) when AI is not
// configured. A factory error is returned redacted, for the TUI and the CLI
// to show; it never stops the caller.
func (a *app) buildAssistant(ctx context.Context, cfg *config.Config, mgr *session.Manager, hist *history.Store, log *slog.Logger) (assistant.Assistant, error) {
	return a.buildAssistantMode(ctx, cfg, mgr, hist, log, false)
}

// buildAssistantMode is buildAssistant, in Genkit dev mode when dev is set.
func (a *app) buildAssistantMode(ctx context.Context, cfg *config.Config, mgr *session.Manager, hist *history.Store, log *slog.Logger, dev bool) (assistant.Assistant, error) {
	if cfg.AI == nil || a.deps.NewAssistant == nil {
		return nil, nil
	}
	asst, err := a.deps.NewAssistant(ctx, *cfg.AI, AssistantDeps{
		Manager: mgr, History: hist, Secrets: a.deps.Resolver, Logger: log, Getenv: a.deps.Getenv,
		PromptDir: config.PromptDir(a.deps.Getenv, a.deps.Home), DevMode: dev,
	})
	if err != nil {
		e := &assistantError{msg: driver.RedactMessage(err.Error(), driver.ConnConfig{})}
		for _, s := range []error{assistant.ErrNotConfigured, assistant.ErrAuth, assistant.ErrRateLimit, assistant.ErrUnavailable, assistant.ErrProvider} {
			if errors.Is(err, s) {
				e.kind = append(e.kind, s)
			}
		}
		log.Debug("ai unavailable", "error", e.msg)
		return nil, e
	}
	return asst, nil
}
