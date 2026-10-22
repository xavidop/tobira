package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// History holds the history: section.
type History struct {
	// Path is the history file, as written: it may start with "~" or name
	// environment variables ($VAR, ${VAR}), which history.Resolve expands.
	// Empty means the default location.
	Path string
}

// HistoryPath is the raw history.path value, or "" when the file sets none
// (or c is nil).
func (c *Config) HistoryPath() string {
	if c == nil || c.History == nil {
		return ""
	}
	return c.History.Path
}

var historyKeyList = []string{"path"}

// loadHistory decodes and validates the history: section, returning the
// line of the offending node on error. Errors never quote a value or an
// unknown key.
func loadHistory(n *yaml.Node) (*History, int, error) {
	if n.Kind != yaml.MappingNode {
		return nil, n.Line, errors.New("history must be a mapping")
	}
	h := &History{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value != "path" {
			if g := guessKeyIn(k.Value, historyKeyList); g != "" {
				return nil, k.Line, fmt.Errorf("unknown history key (did you mean %q?)", g)
			}
			return nil, k.Line, errors.New("unknown history key")
		}
		if v.Tag == "!!null" {
			continue
		}
		p, err := historyPathValue(v)
		if err != nil {
			return nil, v.Line, fmt.Errorf("history.path: %w", err)
		}
		h.Path = p
	}
	return h, 0, nil
}

// historyPathValue checks a history.path value: a non-empty string without
// control characters that is absolute, or starts with "~" or "$" (expanded
// when the path is resolved). A relative path is refused, since it would
// depend on the directory tobira runs from.
func historyPathValue(v *yaml.Node) (string, error) {
	var s string
	if v.Kind != yaml.ScalarNode || v.Tag != "!!str" || v.Decode(&s) != nil {
		return "", errors.New("must be a string")
	}
	switch {
	case strings.TrimSpace(s) == "":
		return "", errors.New("must not be empty")
	case hasControl(s):
		return "", errors.New("must not contain control characters")
	case !filepath.IsAbs(s) && !strings.HasPrefix(s, "~") && !strings.HasPrefix(s, "$"):
		return "", errors.New("must be absolute, or start with ~ or an environment variable")
	}
	return s, nil
}

// topLevelKeys are the sections a config file may have.
var topLevelKeys = []string{"connections", "tui", "ai", "mcp", "plugins", "history"}

// topLevelWarnings warns about each top-level key tobira does not know,
// which would otherwise be ignored without a word. The key is not quoted.
func topLevelWarnings(data []byte) []string {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	m := root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if slices.Contains(topLevelKeys, k.Value) {
			continue
		}
		if g := guessKeyIn(k.Value, topLevelKeys); g != "" {
			out = append(out, fmt.Sprintf("line %d: unknown top-level key (did you mean %q?)", k.Line, g))
		} else {
			out = append(out, fmt.Sprintf("line %d: unknown top-level key; it is ignored", k.Line))
		}
	}
	return out
}
