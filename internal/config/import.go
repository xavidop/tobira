package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ValidateForImport checks a connection about to be added by an import. It
// is ValidateConnection, except that a password ref and a colour are held
// to the rules Load applies: a URL-form ref of a provider that is not
// compiled in, or a colour outside the list, came from a config file and
// survives an export and import unchanged. The error never quotes a value.
func ValidateForImport(c Connection) error {
	probe := c
	if c.Password != "" && ValidateLoadedPasswordRef(c.Password) == nil {
		probe.Password = ""
	}
	if c.Color != "" && !hasControl(c.Color) {
		probe.Color = ""
	}
	return ValidateConnection(probe, nil)
}

// AddConnections appends conns, in order, to the connections in the config
// file at path, in one atomic write. It refuses the whole batch, writing
// nothing, when any entry fails ValidateForImport, a name is taken in the
// file (ErrDuplicate) or repeated in the batch, or the file changed since
// expect was taken (ErrConflict). Like AddConnection it creates a missing
// file and keeps every byte outside the new entries.
func AddConnections(path string, conns []Connection, expect Stat) (Stat, error) {
	seen := map[string]bool{}
	for _, c := range conns {
		if err := ValidateForImport(c); err != nil {
			return Stat{}, fmt.Errorf("connection %q: %w", c.Name, err)
		}
		if seen[c.Name] {
			return Stat{}, fmt.Errorf("%w: %q appears twice in the batch", ErrDuplicate, c.Name)
		}
		seen[c.Name] = true
	}
	return newWriter().edit(path, expect, func(d *doc) error {
		for _, c := range conns {
			if d.find(c.Name) >= 0 {
				return fmt.Errorf("%w: %q", ErrDuplicate, c.Name)
			}
		}
		for _, c := range conns {
			if err := d.insert(c); err != nil {
				return err
			}
			// insert splices after the last item; record the new one so the
			// next entry lands after it.
			if err := d.rescan(path); err != nil {
				return err
			}
		}
		return nil
	})
}

// rescan rebuilds d's item list from its current text.
func (d *doc) rescan(path string) error {
	nd, err := parseDoc(path, []byte(d.text()))
	if err != nil {
		return err
	}
	nd.crlf = d.crlf
	*d = *nd
	return nil
}

// MarshalConnections renders conns as a config document holding only a
// connections: list, in the layout the writer uses. It is what `tobira
// connections export` prints; ParseConnectionList reads it back.
func MarshalConnections(conns []Connection) ([]byte, error) {
	if len(conns) == 0 {
		return []byte("connections: []\n"), nil
	}
	var b bytes.Buffer
	b.WriteString("connections:\n")
	for _, c := range conns {
		lines, err := render(c, 2)
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.Bytes(), nil
}

var yamlLineRE = regexp.MustCompile(`line \d+`)

// ListEntry is one entry of a parsed connections list: a connection that
// passed Load's checks, or (Broken non-nil) one that did not.
type ListEntry struct {
	Conn   Connection
	Broken *BrokenConnection
}

// ParseConnectionList reads a connections list in the config's shape: a
// document with a connections: key (YAML or JSON), or a bare list. Entries
// are validated as Load validates them and returned in file order. Errors
// never quote the input.
func ParseConnectionList(data []byte) ([]ListEntry, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// yaml's messages can quote the offending text; keep only the line.
		if l := yamlLineRE.FindString(err.Error()); l != "" {
			return nil, fmt.Errorf("not valid YAML or JSON (%s)", l)
		}
		return nil, errors.New("not valid YAML or JSON")
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, errors.New("empty document")
	}
	top := root.Content[0]
	var list *yaml.Node
	switch top.Kind {
	case yaml.SequenceNode:
		list = top
	case yaml.MappingNode:
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == "connections" {
				list = top.Content[i+1]
			}
		}
		if list == nil {
			return nil, errors.New("no connections: list")
		}
		if list.Kind == yaml.ScalarNode && list.Tag == "!!null" {
			return nil, nil
		}
	default:
		return nil, errors.New("want a connections: list")
	}
	if list.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: connections must be a list", list.Line)
	}
	var out []ListEntry
	seen := map[string]bool{}
	for _, n := range list.Content {
		c, err := loadConnection(n, seen)
		if err != nil {
			b := brokenEntry(n, c, err)
			out = append(out, ListEntry{Broken: &b})
			continue
		}
		seen[c.Name] = true
		out = append(out, ListEntry{Conn: c})
	}
	return out, nil
}

// IsSecretOptionKey reports whether an options: key looks like it holds a
// secret (pass, pwd, secret or token in its name); the writer refuses such
// options.
func IsSecretOptionKey(k string) bool { return secretOptionKey.MatchString(k) }
