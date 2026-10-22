package ai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultModels(t *testing.T) {
	want := map[string]string{
		"googleai": "gemini-3.8-flash", "vertexai": "gemini-3.8-flash",
		"anthropic": "claude-sonnet-5-5", "openai": "gpt-5", "ollama": "llama3.3",
	}
	for _, p := range config.AIProviders() {
		if got := DefaultModel(p); got != want[p] {
			t.Errorf("DefaultModel(%q) = %q, want %q", p, got, want[p])
		}
	}
}

func refResolver(t *testing.T, wantRef, value string) secrets.Resolver {
	return secrets.Resolver{Resolve: func(_ context.Context, ref string) ([]byte, error) {
		if ref != wantRef {
			t.Errorf("resolved ref %q, want %q", ref, wantRef)
		}
		return []byte(value + "\n"), nil
	}}
}

func TestKeyPrecedence(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		cfg      config.AI
		deps     Deps
		want     string
		wantErr  error
		errHints []string
	}{
		{"ref beats env", config.AI{Provider: "googleai", APIKey: "env:MY_KEY"},
			Deps{Secrets: refResolver(t, "env:MY_KEY", "from-ref"), Getenv: env(map[string]string{"GEMINI_API_KEY": "from-env"})},
			"from-ref", nil, nil},
		{"GEMINI_API_KEY", config.AI{Provider: "googleai"},
			Deps{Getenv: env(map[string]string{"GEMINI_API_KEY": "gem", "GOOGLE_API_KEY": "goo"})}, "gem", nil, nil},
		{"GOOGLE_API_KEY fallback", config.AI{Provider: "googleai"},
			Deps{Getenv: env(map[string]string{"GOOGLE_API_KEY": "goo"})}, "goo", nil, nil},
		{"googleai missing", config.AI{Provider: "googleai"}, Deps{Getenv: env(nil)}, "", assistant.ErrAuth,
			[]string{"set ai.api_key (a secret ref) or GEMINI_API_KEY"}},
		{"anthropic env", config.AI{Provider: "anthropic"},
			Deps{Getenv: env(map[string]string{"ANTHROPIC_API_KEY": "ant"})}, "ant", nil, nil},
		{"anthropic missing", config.AI{Provider: "anthropic"}, Deps{Getenv: env(nil)}, "", assistant.ErrAuth,
			[]string{"set ai.api_key (a secret ref) or ANTHROPIC_API_KEY"}},
		{"openai env", config.AI{Provider: "openai"},
			Deps{Getenv: env(map[string]string{"OPENAI_API_KEY": "oai"})}, "oai", nil, nil},
		{"openai missing", config.AI{Provider: "openai"}, Deps{Getenv: env(nil)}, "", assistant.ErrAuth,
			[]string{"OPENAI_API_KEY"}},
		{"vertexai needs none", config.AI{Provider: "vertexai"}, Deps{Getenv: env(nil)}, "", nil, nil},
		{"ollama needs none", config.AI{Provider: "ollama"}, Deps{Getenv: env(nil)}, "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := resolveKey(ctx, tt.cfg, tt.deps)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				for _, h := range tt.errHints {
					if !strings.Contains(err.Error(), h) {
						t.Fatalf("err %q lacks %q", err, h)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if key.Reveal() != tt.want {
				t.Fatalf("key = %q, want %q", key.Reveal(), tt.want)
			}
		})
	}
}

// A ref that fails to resolve is an auth problem, and the message never holds
// the ref's path.
func TestKeyRefFailureIsRedactedAuthError(t *testing.T) {
	deps := Deps{Getenv: env(nil), Secrets: secrets.Resolver{Resolve: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("open /secret/place/gemini.key: denied")
	}}}
	_, err := resolveKey(context.Background(), config.AI{Provider: "googleai", APIKey: "file:///secret/place/gemini.key"}, deps)
	if !errors.Is(err, assistant.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "secret/place") {
		t.Fatalf("err leaks the ref: %q", err)
	}
}

// Every provider initialises with a fake key and no network.
func TestNewSmokePerProvider(t *testing.T) {
	keys := map[string]string{"GEMINI_API_KEY": "fake-gemini", "ANTHROPIC_API_KEY": "fake-ant", "OPENAI_API_KEY": "fake-oai"}
	tests := []struct {
		cfg       config.AI
		wantModel string
	}{
		{config.AI{Provider: "googleai"}, "googleai/gemini-3.8-flash"},
		{config.AI{Provider: "anthropic"}, "anthropic/claude-sonnet-5-5"},
		{config.AI{Provider: "openai", BaseURL: "http://127.0.0.1:9/v1"}, "openai/gpt-5"},
		{config.AI{Provider: "openai", Model: "gpt-5.5"}, "openai/gpt-5.5"},
		{config.AI{Provider: "ollama"}, "ollama/llama3.3"},
		{config.AI{Provider: "ollama", Model: "qwen3", BaseURL: "http://127.0.0.1:9"}, "ollama/qwen3"},
		{config.AI{Provider: "googleai", Model: "gemini-3.8-flash-lite"}, "googleai/gemini-3.8-flash-lite"},
	}
	for _, tt := range tests {
		t.Run(tt.wantModel, func(t *testing.T) {
			a, err := New(context.Background(), tt.cfg, Deps{Getenv: env(keys)})
			if err != nil {
				t.Fatal(err)
			}
			if a.Model() != tt.wantModel {
				t.Fatalf("Model = %q, want %q", a.Model(), tt.wantModel)
			}
			ga := a.(*genkitAssistant)
			g, err := ga.genkit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if genkit.LookupModel(g, tt.wantModel) == nil {
				t.Fatalf("model %q is not resolvable", tt.wantModel)
			}
		})
	}
}

// Vertex AI's credential detection may probe the GCE metadata server, so its
// init is deferred to first use: New itself never touches the network.
func TestVertexInitIsDeferred(t *testing.T) {
	a, err := New(context.Background(), config.AI{Provider: "vertexai", Project: "p"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model() != "vertexai/gemini-3.8-flash" {
		t.Fatalf("Model = %q", a.Model())
	}
	ga := a.(*genkitAssistant)
	if ga.initialised() {
		t.Fatal("vertexai was initialised by New")
	}

	creds := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(creds, []byte(fakeServiceAccount), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", creds)
	g, err := ga.genkit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if genkit.LookupModel(g, "vertexai/gemini-3.8-flash") == nil {
		t.Fatal("vertex model not resolvable")
	}
}

// A failing plugin init (it panics in Genkit) becomes a redacted error.
func TestVertexInitFailureIsAnError(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	a, err := New(context.Background(), config.AI{Provider: "vertexai", Project: "p"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.(*genkitAssistant).genkit(context.Background())
	if !errors.Is(err, assistant.ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if !strings.Contains(err.Error(), "vertexai") {
		t.Fatalf("err %q should name the provider", err)
	}
}

func TestNewRefusesUnknownProvider(t *testing.T) {
	if _, err := New(context.Background(), config.AI{Provider: "skynet"}, Deps{Getenv: env(nil)}); err == nil {
		t.Fatal("want an error")
	}
}

func TestNewMissingKeyIsAuthError(t *testing.T) {
	_, err := New(context.Background(), config.AI{Provider: "googleai"}, Deps{Getenv: env(nil)})
	if !errors.Is(err, assistant.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

// fakeServiceAccount is a syntactically valid service-account file. Its key
// is never used: no token is fetched in tests.
const fakeServiceAccount = `{
  "type": "service_account",
  "project_id": "p",
  "private_key_id": "0",
  "private_key": "-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n",
  "client_email": "tobira-test@p.iam.gserviceaccount.com",
  "client_id": "0",
  "token_uri": "https://oauth2.googleapis.com/token"
}`
