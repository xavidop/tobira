package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// MCP holds the mcp: section. A nil *MCP (no section) means the defaults,
// which expose nothing: see Config.MCPSettings.
type MCP struct {
	// Expose names the connections an MCP client may use. Empty exposes
	// nothing, and the server refuses to start.
	Expose []string
	// AllowWrites names the exposed connections a client may write to. It is
	// always a subset of Expose.
	AllowWrites []string
	// MaxRows caps the rows of one tool result, between 1 and MCPMaxRowsCap.
	MaxRows int
	// SendRows is always or never: whether tool results carry row data.
	SendRows string
	// Token is a secret ref for the HTTP bearer token; "" means a token is
	// generated at start.
	Token string

	// fileExpose and fileWrites are the lists as the file wrote them when
	// they had duplicates (nil otherwise), so Validate can name file
	// positions. SetExpose and SetAllowWrites clear them.
	fileExpose, fileWrites []string
}

// SetExpose replaces Expose (with a flag's list, say); Validate then names
// positions in names.
func (m *MCP) SetExpose(names []string) { m.Expose, m.fileExpose = names, nil }

// SetAllowWrites replaces AllowWrites; Validate then names positions in
// names.
func (m *MCP) SetAllowWrites(names []string) { m.AllowWrites, m.fileWrites = names, nil }

// asWritten is the list Validate reports positions in: the file's list when
// cur came from the file, else cur.
func asWritten(cur, file []string) []string {
	if file != nil {
		return file
	}
	return cur
}

// Limits and defaults of the mcp: settings.
const (
	MCPMaxRowsCap     = 1000
	mcpDefaultMaxRows = 200
)

// MCPSendRowsModes lists the valid mcp.send_rows values; the first is the
// default.
func MCPSendRowsModes() []string { return []string{"always", "never"} }

func defaultMCP() MCP {
	return MCP{MaxRows: mcpDefaultMaxRows, SendRows: MCPSendRowsModes()[0]}
}

// MCPSettings returns the mcp: section, or the defaults when the file has
// none.
func (c *Config) MCPSettings() MCP {
	if c == nil || c.MCP == nil {
		return defaultMCP()
	}
	return *c.MCP
}

var mcpKeyList = []string{"expose", "allow_writes", "max_rows", "send_rows", "token"}

// loadMCP decodes and validates the mcp: section, returning the line of the
// offending node on error. Errors never quote a value: a list item or an
// unknown key may be a secret typed in the wrong place. Which names exist,
// and whether allow_writes is a subset of expose, are not checked here: see
// MCP.Validate and the load warnings.
func loadMCP(n *yaml.Node) (m *MCP, errLine int, err error) {
	d := defaultMCP()
	if n.Kind != yaml.MappingNode {
		return nil, n.Line, errors.New("mcp must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var err error
		switch k.Value {
		case "expose":
			d.Expose, err = mcpNames(v)
		case "allow_writes":
			d.AllowWrites, err = mcpNames(v)
		case "max_rows":
			err = aiInt(v, &d.MaxRows, 1, MCPMaxRowsCap)
		case "send_rows":
			err = aiString(v, &d.SendRows)
			if err == nil && !slices.Contains(MCPSendRowsModes(), d.SendRows) {
				err = fmt.Errorf("want %s", wantList(MCPSendRowsModes()))
			}
		case "token":
			err = aiString(v, &d.Token)
			if err == nil && ValidateLoadedPasswordRef(d.Token) != nil {
				err = errors.New("must be a secret reference (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME), never a literal token")
			}
		default:
			if g := guessMCPKey(k.Value); g != "" {
				return nil, k.Line, fmt.Errorf("unknown mcp key (did you mean %q?)", g)
			}
			return nil, k.Line, errors.New("unknown mcp key")
		}
		if err != nil {
			return nil, v.Line, fmt.Errorf("mcp.%s: %w", k.Value, err)
		}
	}
	return &d, 0, nil
}

// mcpNames decodes a list of strings without control characters.
func mcpNames(v *yaml.Node) ([]string, error) {
	if v.Tag == "!!null" {
		return nil, nil
	}
	if v.Kind != yaml.SequenceNode {
		return nil, errors.New("must be a list of connection names")
	}
	out := make([]string, 0, len(v.Content))
	for _, it := range v.Content {
		var s string
		if it.Kind != yaml.ScalarNode || it.Decode(&s) != nil {
			return nil, errors.New("every entry must be a string")
		}
		if hasControl(s) {
			return nil, errors.New("entries must not contain control characters")
		}
		out = append(out, s)
	}
	return out, nil
}

// dedupe drops repeated names, keeping the first, and returns the indexes of
// the repeats.
func dedupe(names []string) (out []string, dups []int) {
	for i, n := range names {
		if slices.Contains(out, n) {
			dups = append(dups, i)
			continue
		}
		out = append(out, n)
	}
	return out, dups
}

// Validate checks the section against the connections that loaded: every
// exposed name must be one of them, and allow_writes must be a subset of
// expose. It is the start check of `tobira mcp`; the config loader only
// warns, so a stale name never breaks other commands. Messages give list
// positions, never the entries (a DSN could be typed there).
func (m *MCP) Validate(conns []Connection) error {
	var problems []string
	expose := asWritten(m.Expose, m.fileExpose)
	for i, name := range expose {
		if slices.Contains(expose[:i], name) {
			continue // a repeat: warned about and ignored
		}
		if !slices.ContainsFunc(conns, func(c Connection) bool { return c.Name == name }) {
			problems = append(problems, fmt.Sprintf("mcp.expose[%d]: no connection with that name", i))
		}
	}
	writes := asWritten(m.AllowWrites, m.fileWrites)
	for i, name := range writes {
		if slices.Contains(writes[:i], name) {
			continue
		}
		if !slices.Contains(expose, name) {
			problems = append(problems, fmt.Sprintf("mcp.allow_writes[%d]: not in mcp.expose (allow_writes must be a subset)", i))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// mcpWarnings lists what is wrong with the section's names, for
// Config.Warnings, then removes repeated entries from m. Positions are those
// in the file's lists.
func mcpWarnings(m *MCP, c *Config) []string {
	var w []string
	_, dups := dedupe(m.Expose)
	for _, i := range dups {
		w = append(w, fmt.Sprintf("mcp.expose[%d]: duplicate entry, ignored", i))
	}
	for i, name := range m.Expose {
		if slices.Contains(m.Expose[:i], name) {
			continue
		}
		switch {
		case slices.ContainsFunc(c.Connections, func(x Connection) bool { return x.Name == name }):
		case slices.ContainsFunc(c.Broken, func(b BrokenConnection) bool { return b.Name == name }):
			w = append(w, fmt.Sprintf("mcp.expose[%d]: that connection is invalid and will not be served", i))
		default:
			w = append(w, fmt.Sprintf("mcp.expose[%d]: no connection with that name", i))
		}
	}
	_, dups = dedupe(m.AllowWrites)
	for _, i := range dups {
		w = append(w, fmt.Sprintf("mcp.allow_writes[%d]: duplicate entry, ignored", i))
	}
	for i, name := range m.AllowWrites {
		if !slices.Contains(m.AllowWrites[:i], name) && !slices.Contains(m.Expose, name) {
			w = append(w, fmt.Sprintf("mcp.allow_writes[%d]: not in mcp.expose, so it is not writable", i))
		}
	}
	file := MCP{Expose: slices.Clone(m.Expose), AllowWrites: slices.Clone(m.AllowWrites)}
	if m.Expose, dups = dedupe(m.Expose); len(dups) > 0 {
		m.fileExpose = file.Expose
	}
	if m.AllowWrites, dups = dedupe(m.AllowWrites); len(dups) > 0 {
		m.fileWrites = file.AllowWrites
	}
	return w
}

// guessMCPKey returns the mcp key closest (distance at most 2) to key, or "".
func guessMCPKey(key string) string { return guessKeyIn(key, mcpKeyList) }
