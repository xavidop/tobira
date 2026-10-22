package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/xavidop/tobira/internal/textutil"
	"gopkg.in/yaml.v3"
)

// AI holds the ai: section. A nil *AI (no section) means the assistant is
// not configured and tobira never contacts a model.
type AI struct {
	// Provider is googleai, vertexai, anthropic, openai or ollama.
	Provider string
	// Model is the provider's model name; "" means the provider default.
	Model string
	// APIKey is a secret ref (env:, file://, keyring:, ...), never a key.
	APIKey string
	// BaseURL is an OpenAI-compatible endpoint or the ollama server.
	BaseURL string
	// Project and Location select the Vertex AI project and region.
	Project  string
	Location string
	// SendRows says whether row data may be sent: ask, always or never.
	SendRows string
	// MaxRows caps the rows per tool result sent to the model.
	MaxRows int
	// MaxToolCalls caps the tool calls of one answer turn.
	MaxToolCalls int
	// ContextTokens is the approximate schema-context budget.
	ContextTokens int
	// Embedder names the embedding model for text similarity search:
	// "provider/model", or a model of Provider. "" means none.
	Embedder string
}

// Limits of the numeric ai: settings.
const (
	AIMaxRowsCap         = 200
	aiMaxToolCallsCap    = 64
	aiMinContextTokens   = 1000
	aiMaxContextTokens   = 1_000_000
	aiDefaultMaxRows     = 50
	aiDefaultToolCalls   = 8
	aiDefaultContextToks = 8000
)

// AIProviders lists the valid ai.provider values; the first is the default.
func AIProviders() []string {
	return []string{"googleai", "vertexai", "anthropic", "openai", "ollama"}
}

// AIKeyEnv lists the environment variables that hold provider's API key
// when ai.api_key is not set, in precedence order. It is nil for a
// provider that takes no key (vertexai, ollama) or is unknown. The ai
// module reads the same variables; a test there keeps the two in step.
func AIKeyEnv(provider string) []string {
	switch provider {
	case "googleai":
		return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
	case "anthropic":
		return []string{"ANTHROPIC_API_KEY"}
	case "openai":
		return []string{"OPENAI_API_KEY"}
	}
	return nil
}

// knownAIProvider reports whether p is a valid ai.provider value: one of
// AIProviders, or the scripted fake in an aifake build.
func knownAIProvider(p string) bool {
	return slices.Contains(AIProviders(), p) || (p != "" && p == AIFakeProvider())
}

// SendRowsModes lists the valid ai.send_rows values; the first is the default.
func SendRowsModes() []string { return []string{"ask", "always", "never"} }

func defaultAI() AI {
	return AI{
		Provider:      AIProviders()[0],
		SendRows:      SendRowsModes()[0],
		MaxRows:       aiDefaultMaxRows,
		MaxToolCalls:  aiDefaultToolCalls,
		ContextTokens: aiDefaultContextToks,
	}
}

var aiKeyList = []string{
	"provider", "model", "api_key", "base_url", "project", "location",
	"send_rows", "send_sample_rows", "max_rows", "max_tool_calls", "context_tokens", "embedder",
}

// loadAI decodes and validates the ai: section, returning the line of the
// offending node on error. Errors never quote a value: api_key may hold a
// literal key, and an unknown key may be a secret typed as a key.
func loadAI(n *yaml.Node) (*AI, int, error) {
	a := defaultAI()
	if n.Kind != yaml.MappingNode {
		return nil, n.Line, errors.New("ai must be a mapping")
	}
	var sendRowsLine, aliasLine int
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var err error
		switch k.Value {
		case "provider":
			err = aiString(v, &a.Provider)
			if err == nil && !knownAIProvider(a.Provider) {
				err = fmt.Errorf("unknown provider (want %s)", wantList(AIProviders()))
			}
		case "model":
			err = aiString(v, &a.Model)
		case "api_key":
			err = aiString(v, &a.APIKey)
			if err == nil && ValidateLoadedPasswordRef(a.APIKey) != nil {
				err = errors.New("must be a secret reference (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME), never a literal key")
			}
		case "base_url":
			err = aiString(v, &a.BaseURL)
			if err == nil && a.BaseURL != "" {
				err = checkBaseURL(a.BaseURL)
			}
		case "project":
			err = aiString(v, &a.Project)
		case "location":
			err = aiString(v, &a.Location)
		case "embedder":
			err = aiString(v, &a.Embedder)
		case "send_rows":
			sendRowsLine = k.Line
			err = aiString(v, &a.SendRows)
			if err == nil && !slices.Contains(SendRowsModes(), a.SendRows) {
				err = fmt.Errorf("want %s", wantList(SendRowsModes()))
			}
		case "send_sample_rows":
			aliasLine = k.Line
			var b bool
			if v.Kind != yaml.ScalarNode || v.Decode(&b) != nil {
				err = errors.New("must be true or false")
			} else if b {
				a.SendRows = "always"
			} else {
				a.SendRows = "never"
			}
		case "max_rows":
			err = aiInt(v, &a.MaxRows, 1, AIMaxRowsCap)
		case "max_tool_calls":
			err = aiInt(v, &a.MaxToolCalls, 1, aiMaxToolCallsCap)
		case "context_tokens":
			err = aiInt(v, &a.ContextTokens, aiMinContextTokens, aiMaxContextTokens)
		default:
			if g := guessAIKey(k.Value); g != "" {
				return nil, k.Line, fmt.Errorf("unknown ai key (did you mean %q?)", g)
			}
			return nil, k.Line, errors.New("unknown ai key")
		}
		if err != nil {
			return nil, v.Line, fmt.Errorf("ai.%s: %w", k.Value, err)
		}
	}
	if sendRowsLine != 0 && aliasLine != 0 {
		return nil, max(sendRowsLine, aliasLine), errors.New("set ai.send_rows or its alias ai.send_sample_rows, not both")
	}
	return &a, 0, nil
}

// aiString decodes a scalar string with no control characters into dst. A
// null value leaves dst unchanged (the default).
func aiString(v *yaml.Node, dst *string) error {
	if v.Tag == "!!null" {
		return nil
	}
	var s string
	if v.Kind != yaml.ScalarNode || v.Decode(&s) != nil {
		return errors.New("must be a string")
	}
	if hasControl(s) {
		return errors.New("must not contain control characters")
	}
	if s != "" {
		*dst = s
	}
	return nil
}

// aiInt decodes an integer in [lo, hi] into dst.
func aiInt(v *yaml.Node, dst *int, lo, hi int) error {
	var n int
	if v.Kind != yaml.ScalarNode || v.Tag != "!!int" || v.Decode(&n) != nil {
		return errors.New("must be an integer")
	}
	if n < lo || n > hi {
		return fmt.Errorf("must be between %d and %d", lo, hi)
	}
	*dst = n
	return nil
}

// checkBaseURL accepts an http(s) URL with a host and no credentials.
func checkBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http:// or https:// URL")
	}
	if u.User != nil {
		return errors.New("must not contain credentials; put the key in ai.api_key (a secret ref)")
	}
	return nil
}

// guessAIKey returns the ai key closest (distance at most 2) to key, or "".
func guessAIKey(key string) string {
	prefix, _, _ := strings.Cut(key, ":")
	best, bestDist := "", 3
	for _, k := range aiKeyList {
		if d := textutil.Levenshtein(prefix, k); d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}
