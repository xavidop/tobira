package ai

import (
	"context"
	"fmt"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
	"github.com/firebase/genkit/go/plugins/compat_oai/openai"
	"github.com/firebase/genkit/go/plugins/googlegenai"
	"github.com/firebase/genkit/go/plugins/ollama"
	"github.com/openai/openai-go/option"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
)

// Defaults for the settings a provider needs but the config leaves empty.
const (
	defaultOllamaServer   = "http://localhost:11434"
	defaultVertexLocation = "us-central1"
	// openAIPlaceholderKey is sent to an OpenAI-compatible base_url when no
	// key is configured; such servers commonly ignore the key.
	openAIPlaceholderKey = "unused"
)

// provider describes one ai.provider value.
type provider struct {
	defaultModel string
	// keyEnv lists the environment variables holding the API key, in
	// precedence order. Empty means the provider takes no key.
	keyEnv []string
	// deferInit postpones genkit.Init to the first request, for a plugin
	// whose init may touch the network.
	deferInit bool
	// plugin builds the Genkit plugin. key is the resolved API key; getenv
	// reads the environment (Deps.Getenv).
	plugin func(cfg config.AI, key driver.Secret, getenv func(string) string) api.Plugin
	// afterInit registers what the plugin does not resolve on its own.
	afterInit func(g *genkit.Genkit, p api.Plugin, cfg config.AI)
}

// providers is keyed by config.AIProviders.
var providers = map[string]provider{
	"googleai": {
		defaultModel: "gemini-3.8-flash",
		keyEnv:       []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		plugin: func(_ config.AI, key driver.Secret, _ func(string) string) api.Plugin {
			return &googlegenai.GoogleAI{APIKey: key.Reveal()}
		},
	},
	"vertexai": {
		defaultModel: "gemini-3.8-flash",
		// Application Default Credentials; detection may probe the GCE
		// metadata server when no credentials file exists.
		deferInit: true,
		plugin: func(cfg config.AI, _ driver.Secret, _ func(string) string) api.Plugin {
			loc := cfg.Location
			if loc == "" {
				loc = defaultVertexLocation
			}
			return &googlegenai.VertexAI{ProjectID: cfg.Project, Location: loc}
		},
	},
	"anthropic": {
		defaultModel: "claude-sonnet-5-5",
		keyEnv:       []string{"ANTHROPIC_API_KEY"},
		plugin: func(_ config.AI, key driver.Secret, _ func(string) string) api.Plugin {
			return &anthropic.Anthropic{APIKey: key.Reveal()}
		},
	},
	"openai": {
		defaultModel: "gpt-5",
		keyEnv:       []string{"OPENAI_API_KEY"},
		plugin: func(cfg config.AI, key driver.Secret, _ func(string) string) api.Plugin {
			k := key.Reveal()
			if k == "" {
				// An OpenAI-compatible server (base_url) may need no key, but
				// the plugin refuses an empty one: send a placeholder.
				k = openAIPlaceholderKey
			}
			p := &openai.OpenAI{APIKey: k}
			if cfg.BaseURL != "" {
				p.Opts = []option.RequestOption{option.WithBaseURL(cfg.BaseURL)}
			}
			return p
		},
	},
	"ollama": {
		defaultModel: "llama3.3",
		plugin: func(cfg config.AI, _ driver.Secret, _ func(string) string) api.Plugin {
			addr := cfg.BaseURL
			if addr == "" {
				addr = defaultOllamaServer
			}
			return &ollama.Ollama{ServerAddress: addr}
		},
		// Ollama models must be defined explicitly. The capabilities are
		// declared rather than detected, since detection asks the server.
		afterInit: func(g *genkit.Genkit, p api.Plugin, cfg config.AI) {
			p.(*ollama.Ollama).DefineModel(g, ollama.ModelDefinition{Name: cfg.Model, Type: "chat"}, &gai.ModelOptions{
				Label:    cfg.Model,
				Supports: &gai.ModelSupports{Multiturn: true, SystemRole: true, Tools: true},
			})
		},
	},
}

// lookupProvider returns the provider named name: one of providers, or the
// scripted fake in a binary built with -tags aifake.
func lookupProvider(name string) (provider, bool) {
	if p, ok := providers[name]; ok {
		return p, true
	}
	return fakeProvider(name)
}

// DefaultModel returns the model used when ai.model is empty, or "" for an
// unknown provider.
func DefaultModel(providerName string) string {
	p, _ := lookupProvider(providerName)
	return p.defaultModel
}

// resolveKey returns the provider's API key: the ai.api_key ref when set,
// else the first non-empty provider variable. vertexai and ollama take no
// key and get a zero Secret. A missing key wraps assistant.ErrAuth.
func resolveKey(ctx context.Context, cfg config.AI, deps Deps) (driver.Secret, error) {
	p, ok := lookupProvider(cfg.Provider)
	if !ok {
		return driver.Secret{}, fmt.Errorf("%w: unknown provider", assistant.ErrProvider)
	}
	if len(p.keyEnv) == 0 {
		return driver.Secret{}, nil
	}
	if cfg.APIKey != "" {
		key, err := deps.Secrets.Secret(ctx, "ai.api_key", cfg.APIKey)
		if err != nil {
			// The resolver's message is safe, but its cause may hold the ref's
			// path: keep the message and drop the chain.
			return driver.Secret{}, &providerError{msg: assistant.ErrAuth.Error() + ": " + err.Error(), kind: []error{assistant.ErrAuth}}
		}
		return key, nil
	}
	getenv := deps.Getenv
	for _, name := range p.keyEnv {
		if v := getenv(name); v != "" {
			return driver.NewSecret(v), nil
		}
	}
	if cfg.Provider == "openai" && cfg.BaseURL != "" {
		return driver.Secret{}, nil // an OpenAI-compatible server; see openAIPlaceholderKey
	}
	return driver.Secret{}, fmt.Errorf("%w: set ai.api_key (a secret ref) or %s", assistant.ErrAuth, p.keyEnv[0])
}
