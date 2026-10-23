package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/textutil"
	"gopkg.in/yaml.v3"
)

// Config is the parsed tobira configuration.
type Config struct {
	Connections []Connection
	// Broken lists the connection entries that failed validation, in file
	// order. They are not in Connections; a broken entry never makes the
	// rest of the file unloadable.
	Broken []BrokenConnection
	TUI    TUI
	// AI is the ai: section; nil when the file has none, which means the
	// assistant is not configured.
	AI *AI
	// MCP is the mcp: section; nil when the file has none, which means the
	// defaults (see MCPSettings).
	MCP *MCP
	// Plugins is the plugins: section; nil when the file has none, which
	// means the defaults (see PluginSettings).
	Plugins *Plugins
	// History is the history: section; nil when the file has none, which
	// means the default location (see HistoryPath).
	History *History
	// Warnings are notes about the file that do not stop it loading. They
	// never contain a secret.
	Warnings []string
	// lines maps each loaded connection's name to the line it starts on.
	lines map[string]int
}

// BrokenConnection is a connection entry that failed validation.
type BrokenConnection struct {
	// Name is the entry's name, or "" when it has none or the name itself is
	// invalid (a control character in it, say).
	Name string
	// Line is the 1-based line where the entry starts.
	Line int
	// Err says what is wrong. It never contains the offending value.
	Err string
	// Partial holds the fields that parsed and passed their checks, for
	// pre-filling an edit form. A value that failed a secret check (a
	// literal password, a DSN with a password) is never in it.
	Partial Connection
}

// TUI holds the tui: section. Theme is auto, tobira-dark or tobira-light;
// Keymap is default or vim.
type TUI struct {
	Theme  string `yaml:"theme"`
	Keymap string `yaml:"keymap"`
}

func defaultTUI() TUI { return TUI{Theme: "auto", Keymap: "default"} }

var tuiKeys = map[string]bool{"theme": true, "keymap": true}

// Connection is one named database connection.
type Connection struct {
	Name string `yaml:"name"`
	// Driver is the driver name; it may be empty when DSN carries a scheme.
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
	// Password is the raw mamori secret ref (env:, file://, op://...), never
	// a resolved value.
	Password string            `yaml:"password"`
	Color    string            `yaml:"color"`
	ReadOnly bool              `yaml:"read_only"`
	Tags     []string          `yaml:"tags"`
	Options  map[string]string `yaml:"options"`
	// SSH is the ssh: block, nil when the connection is direct.
	SSH *SSH `yaml:"ssh"`
}

var connectionKeyList = []string{"name", "driver", "dsn", "password", "color", "read_only", "tags", "options", "ssh"}

var connectionKeys = map[string]bool{
	"name": true, "driver": true, "dsn": true, "password": true,
	"color": true, "read_only": true, "tags": true, "options": true, "ssh": true,
}

// Load reads and validates the config file at path. A missing file yields an
// empty Config and no error, as on a fresh install. Errors name the path and,
// where known, the line, and never include a password value.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{TUI: defaultTUI()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return parse(path, data)
}

// parse validates config text. path is only used in error messages. Bad
// connection entries land in Config.Broken; only whole-file problems fail.
func parse(path string, data []byte) (*Config, error) {
	var doc struct {
		Connections []yaml.Node `yaml:"connections"`
		TUI         yaml.Node   `yaml:"tui"`
		AI          yaml.Node   `yaml:"ai"`
		MCP         yaml.Node   `yaml:"mcp"`
		Plugins     yaml.Node   `yaml:"plugins"`
		History     yaml.Node   `yaml:"history"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg := &Config{TUI: defaultTUI()}
	if doc.TUI.Kind != 0 && doc.TUI.Tag != "!!null" {
		t, line, err := loadTUI(&doc.TUI)
		if err != nil {
			return nil, fmt.Errorf("config %s: line %d: %w", path, line, err)
		}
		cfg.TUI = t
	}
	if doc.AI.Kind != 0 && doc.AI.Tag != "!!null" {
		a, line, err := loadAI(&doc.AI)
		if err != nil {
			return nil, fmt.Errorf("config %s: line %d: %w", path, line, err)
		}
		cfg.AI = a
	}
	if doc.MCP.Kind != 0 && doc.MCP.Tag != "!!null" {
		m, line, err := loadMCP(&doc.MCP)
		if err != nil {
			return nil, fmt.Errorf("config %s: line %d: %w", path, line, err)
		}
		cfg.MCP = m
	}
	if doc.Plugins.Kind != 0 && doc.Plugins.Tag != "!!null" {
		p, line, err := loadPlugins(&doc.Plugins)
		if err != nil {
			return nil, fmt.Errorf("config %s: line %d: %w", path, line, err)
		}
		cfg.Plugins = p
	}
	if doc.History.Kind != 0 && doc.History.Tag != "!!null" {
		h, line, err := loadHistory(&doc.History)
		if err != nil {
			return nil, fmt.Errorf("config %s: line %d: %w", path, line, err)
		}
		cfg.History = h
	}
	cfg.Warnings = append(cfg.Warnings, topLevelWarnings(data)...)
	seen := map[string]bool{}
	cfg.lines = map[string]int{}
	for i := range doc.Connections {
		n := &doc.Connections[i]
		c, err := loadConnection(n, seen)
		if err != nil {
			cfg.Broken = append(cfg.Broken, brokenEntry(n, c, err))
			continue
		}
		seen[c.Name] = true
		cfg.lines[c.Name] = n.Line
		cfg.Connections = append(cfg.Connections, c)
	}
	if cfg.MCP != nil {
		cfg.Warnings = append(cfg.Warnings, mcpWarnings(cfg.MCP, cfg)...)
	}
	return cfg, nil
}

// loadConnection decodes and validates one entry. On error the returned
// Connection holds whatever decoded (possibly nothing).
func loadConnection(n *yaml.Node, seen map[string]bool) (Connection, error) {
	keyErr := checkKeys(n)
	var c Connection
	if keyErr != nil && n.Kind != yaml.MappingNode {
		return c, keyErr
	}
	if err := n.Decode(&c); err != nil {
		// The yaml error quotes the offending value, so it is dropped.
		return decodeFields(n)
	}
	if keyErr != nil {
		return c, keyErr
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "ssh" && n.Content[i+1].Tag != "!!null" {
			if err := checkSSHKeys(n.Content[i+1]); err != nil {
				return c, err
			}
		}
	}
	return c, validate(c, seen)
}

// decodeFields decodes the mapping n one key at a time, for an entry whose
// whole-node decode failed. It returns what decoded and an error naming the
// first field of the wrong type (never its value).
func decodeFields(n *yaml.Node) (Connection, error) {
	var c Connection
	var bad string
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		var one Connection
		pair := &yaml.Node{Kind: yaml.MappingNode, Content: n.Content[i : i+2]}
		if err := pair.Decode(&one); err != nil {
			if bad == "" {
				bad = k
			}
			continue
		}
		switch k {
		case "name":
			c.Name = one.Name
		case "driver":
			c.Driver = one.Driver
		case "dsn":
			c.DSN = one.DSN
		case "password":
			c.Password = one.Password
		case "color":
			c.Color = one.Color
		case "read_only":
			c.ReadOnly = one.ReadOnly
		case "tags":
			c.Tags = one.Tags
		case "options":
			c.Options = one.Options
		case "ssh":
			c.SSH = one.SSH
		}
	}
	if bad == "" {
		return c, errors.New("a field has the wrong type")
	}
	return c, fmt.Errorf("field %q has the wrong type", bad)
}

// brokenEntry builds the Broken record for entry n, whose decode (possibly
// partial) result is c.
func brokenEntry(n *yaml.Node, c Connection, err error) BrokenConnection {
	b := BrokenConnection{Line: n.Line, Err: err.Error(), Partial: safeFields(c)}
	if c.Name != "" && !hasControl(c.Name) {
		b.Name = c.Name
	}
	return b
}

// safeFields keeps the fields of c that are safe to show and re-save: nothing
// with a control character and no secret that failed its check.
func safeFields(c Connection) Connection {
	var p Connection
	// A name that carries a credential (a DSN typed into the name field) is
	// dropped: the edit form must not show it.
	if !hasControl(c.Name) && driver.RedactMessage(c.Name, driver.ConnConfig{}) == c.Name {
		p.Name = c.Name
	}
	if !hasControl(c.Driver) {
		p.Driver = c.Driver
	}
	if !hasControl(c.DSN) && !driver.HasPassword(c.DSN) {
		p.DSN = c.DSN
	}
	if ValidateLoadedPasswordRef(c.Password) == nil {
		p.Password = c.Password
	}
	if !hasControl(c.Color) {
		p.Color = c.Color
	}
	p.ReadOnly = c.ReadOnly
	for _, t := range c.Tags {
		if !hasControl(t) {
			p.Tags = append(p.Tags, t)
		}
	}
	for k, v := range c.Options {
		if !secretOptionKey.MatchString(k) && !hasControl(k) && !hasControl(v) {
			if p.Options == nil {
				p.Options = map[string]string{}
			}
			p.Options[k] = v
		}
	}
	p.SSH = safeSSH(c.SSH)
	return p
}

// loadTUI decodes and validates the tui: section, returning the line of the
// offending node on error.
func loadTUI(n *yaml.Node) (TUI, int, error) {
	t := defaultTUI()
	if n.Kind != yaml.MappingNode {
		return t, n.Line, errors.New("tui must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; !tuiKeys[k.Value] {
			return t, k.Line, fmt.Errorf("unknown tui key %q", k.Value)
		}
	}
	if err := n.Decode(&t); err != nil {
		return t, n.Line, err
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		var err error
		switch k.Value {
		case "theme":
			if t.Theme == "" {
				t.Theme = "auto"
			}
			if !slices.Contains(ThemeNames(), t.Theme) {
				err = fmt.Errorf("unknown theme %q (want %s)", t.Theme, wantList(ThemeNames()))
			}
		case "keymap":
			if t.Keymap == "" {
				t.Keymap = "default"
			}
			if !slices.Contains(KeymapNames(), t.Keymap) {
				err = fmt.Errorf("unknown keymap %q (want %s)", t.Keymap, wantList(KeymapNames()))
			}
		}
		if err != nil {
			return t, v.Line, fmt.Errorf("tui.%s: %w", k.Value, err)
		}
	}
	return t, 0, nil
}

// ThemeNames lists the valid tui.theme values. internal/tui/theme keeps its
// own list (a test there asserts the two match) so that this package does
// not import the TUI.
func ThemeNames() []string { return []string{"auto", "tobira-dark", "tobira-light"} }

// KeymapNames lists the valid tui.keymap values; see ThemeNames.
func KeymapNames() []string { return []string{"default", "vim"} }

// wantList renders names as "a, b or c".
func wantList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// checkKeys rejects a non-mapping connection or an unknown key. An unknown
// key is never quoted: a secret written as a bare key (password:hunter2)
// would otherwise reach warnings, listings and the TUI.
func checkKeys(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("connection must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if !connectionKeys[k.Value] {
			if g := guessKey(k.Value); g != "" {
				return fmt.Errorf("unknown connection key (did you mean %q?)", g)
			}
			return errors.New("unknown connection key")
		}
	}
	return nil
}

// guessKey returns the known connection key closest (distance at most 2) to
// the part of key before any ':', or "".
func guessKey(key string) string {
	prefix, _, _ := strings.Cut(key, ":")
	best, bestDist := "", 3
	for _, k := range connectionKeyList {
		if d := textutil.Levenshtein(prefix, k); d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

// validate checks one decoded entry. Its errors never quote a value, and
// they do not name the connection: the caller attaches the name and line.
func validate(c Connection, seen map[string]bool) error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if seen[c.Name] {
		return errors.New("duplicate name")
	}
	// Control characters (C0, DEL, C1) in text the TUI and the CLI print
	// could drive the terminal; the writer rejects them too.
	if hasControl(c.Name) {
		return errors.New("name must not contain control characters")
	}
	if hasControl(c.DSN) {
		return errors.New("dsn must not contain control characters")
	}
	if hasControl(c.Driver) || hasControl(c.Color) {
		return errors.New("driver and color must not contain control characters")
	}
	for _, t := range c.Tags {
		if hasControl(t) {
			return errors.New("tags must not contain control characters")
		}
	}
	if c.Driver == "" && driver.Scheme(c.DSN) == "" {
		return errors.New("set driver or use a DSN with a scheme")
	}
	if c.Password != "" {
		// The error never quotes the value: a hand-written literal password
		// must not be echoed.
		if err := ValidateLoadedPasswordRef(c.Password); err != nil {
			return errors.New("password must be a secret reference (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME)")
		}
	}
	return validateSSH(c.SSH)
}

// FindAt returns the loaded connection whose entry starts on line.
func (c *Config) FindAt(line int) (Connection, bool) {
	for name, l := range c.lines {
		if l == line {
			return c.Find(name)
		}
	}
	return Connection{}, false
}

// FindBrokenAt returns the broken entry that starts on line.
func (c *Config) FindBrokenAt(line int) (BrokenConnection, bool) {
	for _, b := range c.Broken {
		if b.Line == line {
			return b, true
		}
	}
	return BrokenConnection{}, false
}

// Occurrences returns the start lines, in file order, of every entry named
// name, loaded or broken.
func (c *Config) Occurrences(name string) []int {
	var out []int
	if l, ok := c.lines[name]; ok {
		out = append(out, l)
	}
	for _, b := range c.Broken {
		if b.Name == name {
			out = append(out, b.Line)
		}
	}
	slices.Sort(out)
	return out
}

// FindBroken returns the first broken entry with the given name.
func (c *Config) FindBroken(name string) (BrokenConnection, bool) {
	for _, b := range c.Broken {
		if b.Name == name {
			return b, true
		}
	}
	return BrokenConnection{}, false
}

// Find returns the connection with the given name.
func (c *Config) Find(name string) (Connection, bool) {
	for _, conn := range c.Connections {
		if conn.Name == name {
			return conn, true
		}
	}
	return Connection{}, false
}

// Suggest returns the configured connection name closest to name by
// Levenshtein distance (at most 3; the first in config order wins a tie), or
// "" when none is close enough.
func (c *Config) Suggest(name string) string {
	best, bestDist := "", 4
	for _, conn := range c.Connections {
		if d := textutil.Levenshtein(name, conn.Name); d < bestDist {
			best, bestDist = conn.Name, d
		}
	}
	return best
}
