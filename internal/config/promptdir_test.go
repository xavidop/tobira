package config

import "testing"

// Prompt overrides live next to the default config file: under
// $XDG_CONFIG_HOME/tobira/prompts, else <home>/.config/tobira/prompts.
func TestPromptDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := PromptDir(env(map[string]string{"XDG_CONFIG_HOME": "/x"}), "/h"); got != "/x/tobira/prompts" {
		t.Fatalf("xdg: %q", got)
	}
	if got := PromptDir(env(nil), "/h"); got != "/h/.config/tobira/prompts" {
		t.Fatalf("home: %q", got)
	}
	if got := PromptDir(env(nil), ""); got != "" {
		t.Fatalf("no home and no xdg: %q, want none", got)
	}
}
