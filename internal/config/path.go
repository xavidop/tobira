package config

import "path/filepath"

// Path returns the config file location. Precedence: the flag value, the
// TOBIRA_CONFIG variable, $XDG_CONFIG_HOME/tobira/config.yaml, then
// <home>/.config/tobira/config.yaml. A leading "~/" in the flag or variable is
// expanded with home.
func Path(flag string, getenv func(string) string, home string) string {
	if flag != "" {
		return expandHome(flag, home)
	}
	if v := getenv("TOBIRA_CONFIG"); v != "" {
		return expandHome(v, home)
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "tobira", "config.yaml")
	}
	return filepath.Join(home, ".config", "tobira", "config.yaml")
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if len(p) >= 2 && p[0] == '~' && (p[1] == '/' || p[1] == filepath.Separator) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// PromptDir returns the directory of prompt overrides:
// $XDG_CONFIG_HOME/tobira/prompts, else <home>/.config/tobira/prompts, or ""
// when neither is known.
func PromptDir(getenv func(string) string, home string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "tobira", "prompts")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "tobira", "prompts")
}
