package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestAIAbsentIsNil(t *testing.T) {
	for name, body := range map[string]string{
		"no section":   "connections: []\n",
		"null section": "ai:\n",
		"empty file":   "",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(writeTemp(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if c.AI != nil {
				t.Fatalf("AI = %+v, want nil (not configured)", c.AI)
			}
		})
	}
}

func TestAIDefaults(t *testing.T) {
	c, err := Load(writeTemp(t, "ai: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := &AI{Provider: "googleai", SendRows: "ask", MaxRows: 50, MaxToolCalls: 8, ContextTokens: 8000}
	if !reflect.DeepEqual(c.AI, want) {
		t.Fatalf("AI = %+v, want %+v", c.AI, want)
	}
}

func TestAIFullSection(t *testing.T) {
	body := `ai:
  provider: openai
  model: gpt-5
  api_key: env:OPENAI_API_KEY
  base_url: https://llm.internal.example/v1
  project: proj
  location: europe-west4
  send_rows: always
  max_rows: 200
  max_tool_calls: 3
  context_tokens: 12000
  embedder: text-embedding-3-small
`
	c, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := &AI{
		Provider: "openai", Model: "gpt-5", APIKey: "env:OPENAI_API_KEY",
		BaseURL: "https://llm.internal.example/v1", Project: "proj", Location: "europe-west4",
		SendRows: "always", MaxRows: 200, MaxToolCalls: 3, ContextTokens: 12000,
		Embedder: "text-embedding-3-small",
	}
	if !reflect.DeepEqual(c.AI, want) {
		t.Fatalf("AI = %+v, want %+v", c.AI, want)
	}
}

func TestAIEveryProvider(t *testing.T) {
	for _, p := range AIProviders() {
		c, err := Load(writeTemp(t, "ai:\n  provider: "+p+"\n"))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if c.AI.Provider != p {
			t.Fatalf("provider = %q, want %q", c.AI.Provider, p)
		}
	}
	want := []string{"googleai", "vertexai", "anthropic", "openai", "ollama"}
	if !reflect.DeepEqual(AIProviders(), want) {
		t.Fatalf("AIProviders = %v, want %v", AIProviders(), want)
	}
}

func TestAISendSampleRowsAlias(t *testing.T) {
	for in, want := range map[string]string{"true": "always", "false": "never"} {
		c, err := Load(writeTemp(t, "ai:\n  send_sample_rows: "+in+"\n"))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if c.AI.SendRows != want {
			t.Fatalf("send_sample_rows: %s gave send_rows %q, want %q", in, c.AI.SendRows, want)
		}
	}
}

func TestAIValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr []string // substrings the error must contain
		secret  string   // a value that must never appear in the error
	}{
		{"unknown provider", "ai:\n  provider: skynet\n", []string{"line 2", "ai.provider", "googleai"}, "skynet"},
		{"literal api key", "ai:\n  api_key: sk-live-abcdef123456\n", []string{"line 2", "ai.api_key", "secret reference"}, "sk-live-abcdef123456"},
		{"literal key that looks like a ref", "ai:\n  api_key: \"hunter:22secret\"\n", []string{"ai.api_key"}, "22secret"},
		{"both row switches", "ai:\n  send_rows: never\n  send_sample_rows: true\n", []string{"send_rows", "send_sample_rows"}, ""},
		{"bad send_rows", "ai:\n  send_rows: sometimes\n", []string{"line 2", "ai.send_rows", "ask, always or never"}, "sometimes"},
		{"send_sample_rows not bool", "ai:\n  send_sample_rows: maybe\n", []string{"ai.send_sample_rows"}, "maybe"},
		{"max_rows over cap", "ai:\n  max_rows: 201\n", []string{"line 2", "ai.max_rows", "200"}, "201"},
		{"max_rows zero", "ai:\n  max_rows: 0\n", []string{"ai.max_rows"}, ""},
		{"max_rows not int", "ai:\n  max_rows: lots\n", []string{"ai.max_rows", "integer"}, "lots"},
		{"max_tool_calls zero", "ai:\n  max_tool_calls: 0\n", []string{"ai.max_tool_calls"}, ""},
		{"max_tool_calls huge", "ai:\n  max_tool_calls: 1000\n", []string{"ai.max_tool_calls"}, "1000"},
		{"context_tokens tiny", "ai:\n  context_tokens: 10\n", []string{"ai.context_tokens"}, ""},
		{"base_url with password", "ai:\n  base_url: https://u:pa55word@llm.example/v1\n", []string{"ai.base_url"}, "pa55word"},
		{"base_url not http", "ai:\n  base_url: ftp://llm.example\n", []string{"ai.base_url"}, "llm.example"},
		{"control in model", "ai:\n  model: \"gem\\u0085ini\"\n", []string{"ai.model", "control"}, ""},
		{"unknown key", "ai:\n  provder: googleai\n", []string{"line 2", "did you mean \"provider\""}, ""},
		{"unknown key never quoted", "ai:\n  sk-live-abcdef123456: x\n", []string{"unknown ai key"}, "sk-live-abcdef123456"},
		{"not a mapping", "ai: googleai\n", []string{"line 1", "ai must be a mapping"}, ""},
		{"provider wrong type", "ai:\n  provider: [a, b]\n", []string{"ai.provider"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not contain %q", err, w)
				}
			}
			// The temp path is random and may contain the digits.
			if tt.secret != "" && strings.Contains(strings.ReplaceAll(err.Error(), path, ""), tt.secret) {
				t.Fatalf("error %q echoes the value %q", err, tt.secret)
			}
		})
	}
}

func TestAIValidRefs(t *testing.T) {
	for _, ref := range []string{"env:GEMINI_API_KEY", "file:///run/secrets/key", "dotenv://.env#GEMINI_API_KEY", "op://dev/gemini/key"} {
		c, err := Load(writeTemp(t, "ai:\n  api_key: \""+ref+"\"\n"))
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if c.AI.APIKey != ref {
			t.Fatalf("api_key = %q, want %q", c.AI.APIKey, ref)
		}
	}
}

// An ai: section problem does not touch connection loading rules: it is a
// whole-file error, like a bad tui: section.
func TestAIErrorIsWholeFile(t *testing.T) {
	_, err := Load(writeTemp(t, "connections:\n  - {name: a, driver: sqlite}\nai:\n  provider: nope\n"))
	if err == nil || !strings.Contains(err.Error(), "line 4") {
		t.Fatalf("err = %v, want a whole-file error at line 4", err)
	}
}

func TestAIKeyEnv(t *testing.T) {
	for p, want := range map[string][]string{
		"googleai":  {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		"anthropic": {"ANTHROPIC_API_KEY"},
		"openai":    {"OPENAI_API_KEY"},
		"vertexai":  nil,
		"ollama":    nil,
		"nope":      nil,
	} {
		if got := AIKeyEnv(p); !reflect.DeepEqual(got, want) {
			t.Errorf("AIKeyEnv(%q) = %v, want %v", p, got, want)
		}
	}
}
