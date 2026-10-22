package connio

import (
	"fmt"

	"github.com/xavidop/tobira/internal/config"
)

// ParseFile reads the YAML or JSON `tobira connections export` prints (or
// any connections: list in the config's shape). Password refs are kept. An
// entry the config loader would reject, such as one with a literal
// password, is skipped with the loader's reason, which never quotes a value.
func ParseFile(data []byte) ([]Candidate, error) {
	entries, err := config.ParseConnectionList(data)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		if b := e.Broken; b != nil {
			label := b.Name
			if label == "" {
				label = fmt.Sprintf("entry at line %d", b.Line)
			}
			out = append(out, Candidate{Label: label, Skip: b.Err})
			continue
		}
		out = append(out, Candidate{Label: e.Conn.Name, Conn: e.Conn})
	}
	return out, nil
}
