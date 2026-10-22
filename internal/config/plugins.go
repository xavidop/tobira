package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xavidop/tobira/internal/textutil"
)

// Plugins holds the plugins: section, which controls external driver
// plugins (tobira-driver-* binaries).
type Plugins struct {
	// Enabled turns plugin discovery on; the default is true.
	Enabled bool
	// Dirs replaces the default plugins directory (see PluginsDir) with
	// these, searched in order before PATH. A leading "~/" is expanded by
	// ExpandPluginDirs. Empty means the default directory.
	Dirs []string
}

func defaultPlugins() Plugins { return Plugins{Enabled: true} }

// PluginSettings returns the plugins: section, or the defaults when the
// file has none (or c is nil).
func (c *Config) PluginSettings() Plugins {
	if c == nil || c.Plugins == nil {
		return defaultPlugins()
	}
	return *c.Plugins
}

// PluginsDir returns the default plugins directory:
// $XDG_CONFIG_HOME/tobira/plugins, else <home>/.config/tobira/plugins, or
// "" when neither is known. `tobira driver install` writes there.
func PluginsDir(getenv func(string) string, home string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "tobira", "plugins")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "tobira", "plugins")
}

// ExpandPluginDirs expands a leading "~/" (or a bare "~") in each of dirs
// with home.
func ExpandPluginDirs(dirs []string, home string) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = expandHome(d, home)
	}
	return out
}

var pluginsKeyList = []string{"enabled", "dirs"}

// loadPlugins decodes and validates the plugins: section, returning the line
// of the offending node on error. Errors never quote a value or an unknown
// key.
func loadPlugins(n *yaml.Node) (*Plugins, int, error) {
	p := defaultPlugins()
	if n.Kind != yaml.MappingNode {
		return nil, n.Line, errors.New("plugins must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var err error
		switch k.Value {
		case "enabled":
			if v.Kind != yaml.ScalarNode || v.Tag != "!!bool" || v.Decode(&p.Enabled) != nil {
				err = errors.New("must be true or false")
			}
		case "dirs":
			p.Dirs, err = pluginDirs(v)
		default:
			if g := guessKeyIn(k.Value, pluginsKeyList); g != "" {
				return nil, k.Line, fmt.Errorf("unknown plugins key (did you mean %q?)", g)
			}
			return nil, k.Line, errors.New("unknown plugins key")
		}
		if err != nil {
			return nil, v.Line, fmt.Errorf("plugins.%s: %w", k.Value, err)
		}
	}
	return &p, 0, nil
}

func pluginDirs(v *yaml.Node) ([]string, error) {
	if v.Tag == "!!null" {
		return nil, nil
	}
	if v.Kind != yaml.SequenceNode {
		return nil, errors.New("must be a list of directories")
	}
	out := make([]string, 0, len(v.Content))
	for _, it := range v.Content {
		var s string
		if it.Kind != yaml.ScalarNode || it.Decode(&s) != nil {
			return nil, errors.New("every entry must be a string")
		}
		switch {
		case strings.TrimSpace(s) == "":
			return nil, errors.New("entries must not be empty")
		case hasControl(s):
			return nil, errors.New("entries must not contain control characters")
		}
		out = append(out, s)
	}
	return out, nil
}

// guessKeyIn returns the known key within edit distance 2 of key, if any.
func guessKeyIn(key string, known []string) string {
	prefix, _, _ := strings.Cut(key, ":")
	best, bestDist := "", 3
	for _, k := range known {
		if d := textutil.Levenshtein(prefix, k); d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}
