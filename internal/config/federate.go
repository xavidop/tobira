package config

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Federate holds the federate: section, the settings of cross-source
// queries. A nil *Federate (no section) means the defaults.
type Federate struct {
	// MaxSourceRows caps the rows staged from each source of a cross-source
	// query, from 1 to FederateMaxSourceRowsCap; 0 means the engine's
	// default (10,000).
	MaxSourceRows int
}

// FederateMaxSourceRowsCap bounds federate.max_source_rows: staging more
// rows per source than this would not fit a laptop's disk and patience.
const FederateMaxSourceRowsCap = 1_000_000

// FederateMaxSourceRows is the federate.max_source_rows value, or 0 when the
// file sets none (or c is nil), which means the engine's default.
func (c *Config) FederateMaxSourceRows() int {
	if c == nil || c.Federate == nil {
		return 0
	}
	return c.Federate.MaxSourceRows
}

var federateKeyList = []string{"max_source_rows"}

// loadFederate decodes and validates the federate: section, returning the
// line of the offending node on error. Errors never quote a value or an
// unknown key.
func loadFederate(n *yaml.Node) (*Federate, int, error) {
	if n.Kind != yaml.MappingNode {
		return nil, n.Line, errors.New("federate must be a mapping")
	}
	f := &Federate{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		switch k.Value {
		case "max_source_rows":
			if v.Tag == "!!null" {
				continue
			}
			if err := aiInt(v, &f.MaxSourceRows, 1, FederateMaxSourceRowsCap); err != nil {
				return nil, v.Line, fmt.Errorf("federate.max_source_rows: %w", err)
			}
		default:
			if g := guessKeyIn(k.Value, federateKeyList); g != "" {
				return nil, k.Line, fmt.Errorf("unknown federate key (did you mean %q?)", g)
			}
			return nil, k.Line, errors.New("unknown federate key")
		}
	}
	return f, 0, nil
}
