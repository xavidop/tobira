package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/auth/credentials"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
)

// checkEndpoints are where Check asks each provider. Tests point them at a
// local server.
type checkEndpoints struct {
	googleAI, anthropic, openAI string
	// vertexToken fetches an access token with Application Default
	// Credentials.
	vertexToken func(ctx context.Context) error
}

func defaultCheckEndpoints() checkEndpoints {
	return checkEndpoints{
		googleAI:    "https://generativelanguage.googleapis.com/v1beta",
		anthropic:   "https://api.anthropic.com/v1",
		openAI:      "https://api.openai.com/v1",
		vertexToken: vertexToken,
	}
}

// vertexToken detects Application Default Credentials and fetches a token.
func vertexToken(ctx context.Context) error {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		return err
	}
	_, err = creds.Token(ctx)
	return err
}

// Check asks the provider of cfg, over the network, whether it answers
// with the configured key, never following a redirect: it lists the models (googleai, anthropic,
// openai or an OpenAI-compatible base_url, ollama's /api/tags), or for
// vertexai fetches a token with Application Default Credentials. It
// generates nothing and spends no tokens. The key is resolved as for the
// assistant and sent only in a request header. Errors wrap the assistant
// sentinels (ErrAuth for a missing or refused key, ErrRateLimit,
// ErrUnavailable, ErrProvider) and never carry the key or a response
// body. `tobira doctor --online` calls it.
func Check(ctx context.Context, cfg config.AI, deps Deps) error {
	return check(ctx, cfg, deps, defaultCheckEndpoints(), checkClient())
}

// checkClient is the HTTP client of Check: the default transport, never
// following a redirect. Go would forward the x-api-key and x-goog-api-key
// headers to another host on a redirect (it strips only Authorization and
// cookies), so a 3xx is answered as the provider's error instead.
func checkClient() *http.Client { return noRedirects(&http.Client{}) }

// noRedirects makes c return a redirect response as is instead of
// following it.
func noRedirects(c *http.Client) *http.Client {
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func check(ctx context.Context, cfg config.AI, deps Deps, ep checkEndpoints, client *http.Client) error {
	if _, ok := lookupProvider(cfg.Provider); !ok {
		return fmt.Errorf("%w: unknown provider", assistant.ErrProvider)
	}
	if deps.Getenv == nil {
		deps.Getenv = func(string) string { return "" }
	}
	key, err := resolveKey(ctx, cfg, deps)
	if err != nil {
		return err
	}
	header := http.Header{}
	var url string
	switch cfg.Provider {
	case "googleai":
		url = ep.googleAI + "/models?pageSize=1"
		header.Set("x-goog-api-key", key.Reveal())
	case "anthropic":
		url = ep.anthropic + "/models?limit=1"
		header.Set("x-api-key", key.Reveal())
		header.Set("anthropic-version", "2023-06-01")
	case "openai":
		base := ep.openAI
		if cfg.BaseURL != "" {
			base = cfg.BaseURL
		}
		url = strings.TrimRight(base, "/") + "/models"
		if k := key.Reveal(); k != "" {
			header.Set("Authorization", "Bearer "+k)
		}
	case "ollama":
		base := cfg.BaseURL
		if base == "" {
			base = defaultOllamaServer
		}
		url = strings.TrimRight(base, "/") + "/api/tags"
	case "vertexai":
		if err := ep.vertexToken(ctx); err != nil {
			return fmt.Errorf("%w: no usable Application Default Credentials: %s", assistant.ErrAuth, firstLineOf(err.Error()))
		}
		return nil
	default:
		return nil // the scripted test provider: nothing to reach
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %s", assistant.ErrProvider, redactKey(err.Error(), key))
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %s", assistant.ErrUnavailable, ctxErr)
		}
		return fmt.Errorf("%w: cannot reach %s: %s", assistant.ErrUnavailable, req.URL.Host, redactKey(rootCause(err), key))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return statusError(resp.StatusCode)
}

// statusError classifies an HTTP status; nil for 2xx.
func statusError(code int) error {
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: the provider refused the key (HTTP %d)", assistant.ErrAuth, code)
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%w: HTTP %d", assistant.ErrRateLimit, code)
	case code >= 500:
		return fmt.Errorf("%w: HTTP %d", assistant.ErrUnavailable, code)
	}
	return fmt.Errorf("%w: unexpected HTTP %d", assistant.ErrProvider, code)
}

// rootCause is the innermost error's message, without the URL a
// *url.Error repeats.
func rootCause(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err.Error()
		}
		err = next
	}
}

func redactKey(msg string, key driver.Secret) string {
	return driver.RedactMessage(msg, driver.ConnConfig{Password: key})
}

// firstLineOf is s up to its first line break, trimmed.
func firstLineOf(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
