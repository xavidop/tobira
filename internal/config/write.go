package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/driver"
	"gopkg.in/yaml.v3"
)

// Errors returned by the write functions; test with errors.Is.
var (
	// ErrDuplicate is returned when a connection name is already taken.
	ErrDuplicate = errors.New("connection name already exists")
	// ErrNotFound is returned when the named connection is not in the file.
	ErrNotFound = errors.New("connection not found")
	// ErrAmbiguous is returned when a name matches more than one entry; the
	// caller must target one by line.
	ErrAmbiguous = errors.New("connection name matches more than one entry; use its line")
	// ErrConflict is returned when the file changed since it was loaded.
	ErrConflict = errors.New("config file changed since it was loaded; reload and try again")
)

// Stat identifies a version of the config file: it is the conflict token.
// Take one with LoadWithStat (or StatOf) when the file is read and hand it
// to the write functions, which refuse to write when the file no longer
// matches. Each write returns the Stat of what it wrote, so a caller can
// chain writes without reloading.
type Stat struct {
	Exists  bool
	ModTime time.Time
	Size    int64
}

// StatOf returns the current Stat of path. A missing file is a valid Stat
// with Exists false.
func StatOf(path string) (Stat, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Stat{}, nil
	}
	if err != nil {
		return Stat{}, fmt.Errorf("config %s: %w", path, err)
	}
	return Stat{Exists: true, ModTime: fi.ModTime(), Size: fi.Size()}, nil
}

// Equal reports whether s and o identify the same version of the file.
func (s Stat) Equal(o Stat) bool {
	return s.Exists == o.Exists && s.Size == o.Size && s.ModTime.Equal(o.ModTime)
}

// LoadWithStat is Load plus the Stat of the file as it was read. The Stat is
// taken first, so a change racing the read makes the later write conflict
// instead of silently overwriting it.
func LoadWithStat(path string) (*Config, Stat, error) {
	st, err := StatOf(path)
	if err != nil {
		return nil, Stat{}, err
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, Stat{}, err
	}
	return cfg, st, nil
}

// ColorNames lists the colour names a connection's color: setting may use;
// the sidebar maps them onto the theme palette.
func ColorNames() []string {
	return []string{"red", "green", "yellow", "orange", "cyan", "blue", "magenta"}
}

// hasControl reports whether s holds a control character: C0 (including
// newline, tab and NUL), DEL or C1.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

var secretOptionKey = regexp.MustCompile(`(?i)pass|pwd|secret|token`)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateConnection checks c as the connection form and the write functions
// do. drivers is the set of registered drivers; nil skips the checks that
// need it (driver known, scheme belongs to the driver). The DSN must carry
// no password: passwords live in a secret ref only.
func ValidateConnection(c Connection, drivers []driver.Info) error {
	if err := ValidateName(c.Name); err != nil {
		return err
	}
	if err := ValidateDSN(c.DSN, c.Driver, drivers); err != nil {
		return err
	}
	if err := ValidatePasswordRef(c.Password); err != nil {
		return err
	}
	if c.Color != "" && !slices.Contains(ColorNames(), strings.ToLower(c.Color)) {
		return fmt.Errorf("color %q is not one of %s", c.Color, strings.Join(ColorNames(), ", "))
	}
	for _, t := range c.Tags {
		if strings.TrimSpace(t) == "" {
			return errors.New("tags must not be empty strings")
		}
		if hasControl(t) {
			return errors.New("tags must not contain control characters")
		}
	}
	for k, v := range c.Options {
		if secretOptionKey.MatchString(k) {
			return errors.New("options must not carry secrets; put the password in a password ref")
		}
		if hasControl(k) || hasControl(v) {
			return errors.New("options must not contain control characters")
		}
	}
	return nil
}

// ValidateName checks a connection name: letters, digits, '.', '_' and '-'.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) { // also excludes control characters
		return errors.New("name is required and may only contain letters, digits, '.', '_' and '-'")
	}
	return nil
}

// ValidatePasswordRef checks a password secret ref; empty is valid. The
// error never quotes the ref.
func ValidatePasswordRef(ref string) error { return checkPasswordRef(ref, false) }

var urlFormRef = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://`)

// ValidateLoadedPasswordRef is the check for a password read from a config
// file. Besides registered schemes it accepts URL-form refs (op://, aws-sm://,
// vault://, ...) of providers that are not compiled in; those fail later, when
// the password is resolved. A literal such as "hunter:2" is rejected. The
// error never quotes the value.
func ValidateLoadedPasswordRef(ref string) error { return checkPasswordRef(ref, true) }

func checkPasswordRef(ref string, allowURLForm bool) error {
	if ref == "" {
		return nil
	}
	errNotRef := errors.New("not a secret reference: use env:NAME, file:///path, dotenv://path#KEY or keyring:NAME")
	if hasControl(ref) {
		return errNotRef
	}
	// The mamori errors quote the ref, so they are deliberately dropped.
	refs, err := mamori.ParseRefs(ref)
	if err != nil {
		return errNotRef
	}
	known := mamori.RegisteredSchemes()
	for _, r := range refs {
		if emptyRef(r.Raw) {
			return errNotRef
		}
		// mamori splits a chain only before another scheme, so a literal
		// after a comma ("env:A,SECRET") stays inside the previous path. An
		// env var name never holds a comma.
		if r.Scheme == "env" && strings.ContainsAny(r.Path, ", \t") {
			return errNotRef
		}
		// ParseRefs accepts any "x:y", so a literal password such as
		// "hunter:2" parses; only a registered provider scheme is a ref.
		if allowURLForm && urlFormRef.MatchString(r.Raw) {
			continue
		}
		if !slices.Contains(known, r.Scheme) {
			return errNotRef
		}
	}
	return nil
}

// emptyRef reports whether raw is a scheme with nothing after it (env:,
// file://, op://).
func emptyRef(raw string) bool {
	_, rest, ok := strings.Cut(raw, ":")
	return ok && strings.TrimPrefix(rest, "//") == ""
}

// ValidateDSN checks dsn for the named driver ("" means pick by scheme):
// non-empty, no embedded password, and (with drivers non-nil) a known driver
// whose schemes include the DSN's.
func ValidateDSN(dsn, driverName string, drivers []driver.Info) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("DSN is required")
	}
	if hasControl(dsn) {
		return errors.New("DSN must not contain control characters")
	}
	if err := validateDriver(dsn, driverName, drivers); err != nil {
		return err
	}
	if driver.HasPassword(dsn) {
		return errors.New("DSN contains a password; put it in a password ref (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME) instead")
	}
	return nil
}

func validateDriver(dsn, driverName string, drivers []driver.Info) error {
	// Only URL-style DSNs have a scheme; "user:pw@tcp(h)/db" style DSNs must
	// not be mistaken for one.
	scheme := ""
	if strings.Contains(dsn, "://") {
		scheme = driver.Scheme(dsn)
	}
	if driverName == "" {
		if scheme == "" {
			return errors.New("choose a driver, or use a DSN with a scheme (scheme://...)")
		}
		if drivers != nil && !slices.ContainsFunc(drivers, func(i driver.Info) bool { return slices.Contains(i.Schemes, scheme) }) {
			return fmt.Errorf("no driver registered for DSN scheme %q", scheme)
		}
		return nil
	}
	if drivers == nil {
		return nil
	}
	i := slices.IndexFunc(drivers, func(i driver.Info) bool { return i.Name == driverName })
	if i < 0 {
		return fmt.Errorf("unknown driver %q", driverName)
	}
	if schemes := drivers[i].Schemes; scheme != "" && len(schemes) > 0 && !slices.Contains(schemes, scheme) {
		return fmt.Errorf("DSN scheme %q does not match driver %q (want %s)", scheme, driverName, strings.Join(schemes, ", "))
	}
	return nil
}

// writer performs config edits. rename is injectable so tests can fail the
// final step of the atomic write.
type writer struct {
	rename func(oldpath, newpath string) error
}

func newWriter() writer { return writer{rename: os.Rename} }

// AddConnection appends c to the connections in the config file at path,
// creating the file (mode 0600) and its directory (0700) when missing. expect
// is the Stat taken when the file was loaded; a file that changed since
// returns ErrConflict. A taken name returns ErrDuplicate. The returned Stat
// describes the written file.
//
// The edit is a text splice located through the parsed yaml.Node tree, so
// every byte outside the new entry (comments, blank lines, key order,
// other sections) is preserved exactly. The result is validated with the
// same rules as Load before anything is written, and written atomically.
func AddConnection(path string, c Connection, expect Stat) (Stat, error) {
	return newWriter().add(path, c, expect)
}

// UpdateConnection replaces the connection named oldName with c (which may
// rename it) in place. Comments inside the replaced entry are not kept;
// everything outside it is. Errors as for AddConnection, plus ErrNotFound,
// and ErrAmbiguous when the name matches several entries.
func UpdateConnection(path, oldName string, c Connection, expect Stat) (Stat, error) {
	return newWriter().update(path, byName(oldName), c, expect)
}

// UpdateConnectionAt is UpdateConnection for the entry that starts on line
// (as reported by Config.Broken or Config.Occurrences).
func UpdateConnectionAt(path string, line int, c Connection, expect Stat) (Stat, error) {
	return newWriter().update(path, byLine(line), c, expect)
}

// RemoveConnection deletes the connection named name. Errors as for
// UpdateConnection.
func RemoveConnection(path, name string, expect Stat) (Stat, error) {
	return newWriter().remove(path, byName(name), expect)
}

// RemoveConnectionAt deletes the entry that starts on line.
func RemoveConnectionAt(path string, line int, expect Stat) (Stat, error) {
	return newWriter().remove(path, byLine(line), expect)
}

// locator finds the index of the target entry in d.
type locator func(d *doc) (int, error)

func byName(name string) locator {
	return func(d *doc) (int, error) {
		var at []string
		idx := -1
		for i, it := range d.items {
			if it.name == name {
				if idx < 0 {
					idx = i
				}
				at = append(at, strconv.Itoa(it.line))
			}
		}
		switch {
		case idx < 0:
			return -1, fmt.Errorf("%w: %q", ErrNotFound, name)
		case len(at) > 1:
			return -1, fmt.Errorf("%w: %q appears %d times (lines %s)", ErrAmbiguous, name, len(at), strings.Join(at, ", "))
		}
		return idx, nil
	}
}

func byLine(line int) locator {
	return func(d *doc) (int, error) {
		for i, it := range d.items {
			if it.line == line {
				return i, nil
			}
		}
		return -1, fmt.Errorf("%w: no entry at line %d", ErrNotFound, line)
	}
}

func (w writer) add(path string, c Connection, expect Stat) (Stat, error) {
	if err := ValidateConnection(c, nil); err != nil {
		return Stat{}, err
	}
	return w.edit(path, expect, func(d *doc) error {
		if d.find(c.Name) >= 0 {
			return fmt.Errorf("%w: %q", ErrDuplicate, c.Name)
		}
		return d.insert(c)
	})
}

func (w writer) update(path string, loc locator, c Connection, expect Stat) (Stat, error) {
	return w.edit(path, expect, func(d *doc) error {
		i, err := loc(d)
		if err != nil {
			return err
		}
		// A hand-written value the user did not touch (a URL-form password
		// ref of a provider that is not compiled in, a colour outside the
		// list) is kept as it is; a changed value goes through the strict
		// check.
		probe := c
		if cfg, err := parse(path, []byte(d.text())); err == nil {
			line := d.items[i].line
			old, ok := cfg.FindAt(line)
			if b, isBroken := cfg.FindBrokenAt(line); isBroken {
				old, ok = b.Partial, true // the values that passed their checks
			}
			if ok {
				if old.Password == c.Password {
					probe.Password = ""
				}
				if old.Color == c.Color {
					probe.Color = ""
				}
			}
		}
		if err := ValidateConnection(probe, nil); err != nil {
			return err
		}
		for j, it := range d.items {
			if j != i && it.name == c.Name {
				return fmt.Errorf("%w: %q", ErrDuplicate, c.Name)
			}
		}
		return d.replace(i, c)
	})
}

func (w writer) remove(path string, loc locator, expect Stat) (Stat, error) {
	return w.edit(path, expect, func(d *doc) error {
		i, err := loc(d)
		if err != nil {
			return err
		}
		d.delete(i)
		return nil
	})
}

// edit is the shared read, splice, validate, atomic-write sequence.
//
// Broken entries elsewhere in the file do not block an edit: a file with
// several bad entries is repaired one entry at a time.
func (w writer) edit(path string, expect Stat, mutate func(*doc) error) (Stat, error) {
	path = resolveLink(path)
	cur, err := StatOf(path)
	if err != nil {
		return Stat{}, err
	}
	if !cur.Equal(expect) {
		return Stat{}, ErrConflict
	}
	var data []byte
	if cur.Exists {
		if data, err = os.ReadFile(path); err != nil {
			return Stat{}, fmt.Errorf("config %s: %w", path, err)
		}
		if after, err := StatOf(path); err != nil || !after.Equal(cur) {
			return Stat{}, ErrConflict
		}
	}
	d, err := parseDoc(path, data)
	if err != nil {
		return Stat{}, err
	}
	if err := mutate(d); err != nil {
		return Stat{}, err
	}
	if _, err := parse(path, []byte(d.text())); err != nil {
		return Stat{}, err
	}
	if err := w.writeAtomic(path, d.bytes()); err != nil {
		return Stat{}, err
	}
	return StatOf(path)
}

// resolveLink follows path through symlinks to the file a write must
// replace, so the link itself survives. A dangling link resolves to its
// (missing) target, which the write then creates.
func resolveLink(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	for range 40 {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			break
		}
		t, err := os.Readlink(path)
		if err != nil {
			break
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(path), t)
		}
		path = t
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(dir, filepath.Base(path))
	}
	return path
}

func (w writer) writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	f, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("config %s: %w", path, err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("config %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if err = os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if err = w.rename(tmp, path); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	// Make the rename durable. Best effort: some platforms cannot open or
	// sync a directory, and the file itself is already in place.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ---- text splicing -------------------------------------------------------

// doc is the config text, split into lines, with the location of the
// connections list taken from the yaml.Node tree.
type doc struct {
	lines []string
	// keyLine is the 0-based line of the "connections:" key, -1 if absent.
	keyLine int
	// items are the list entries, in order.
	items []item
	// dash is the column of the "- " of the entries (or the column new ones
	// get when there are none).
	dash int
	// crlf is whether the file used CRLF line ends; text is handled with LF
	// and converted back on output.
	crlf bool
	// emptyFlow marks "connections: []".
	emptyFlow bool
}

// item is one list entry: its name and the 0-based inclusive line range.
type item struct {
	name       string
	line       int // 1-based line of the entry's node, as Config.Broken reports it
	first, end int
}

var emptyFlowRE = regexp.MustCompile(`^(\s*connections:)\s*\[\s*\](.*)$`)

func parseDoc(path string, data []byte) (*doc, error) {
	crlf := bytes.Contains(data, []byte("\r\n"))
	if crlf {
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}
	d := &doc{lines: strings.Split(string(data), "\n"), keyLine: -1, dash: 2, crlf: crlf}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return d, nil
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config %s: line %d: top level must be a mapping", path, top.Line)
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		k, v := top.Content[i], top.Content[i+1]
		if k.Value != "connections" {
			continue
		}
		d.keyLine = k.Line - 1
		d.dash = k.Column - 1 + 2
		switch {
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null" && v.Value == "":
		case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle != 0 && len(v.Content) == 0:
			if !emptyFlowRE.MatchString(d.lines[d.keyLine]) {
				return nil, fmt.Errorf("config %s: line %d: unsupported layout of connections; edit the file by hand", path, v.Line)
			}
			d.emptyFlow = true
		case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0:
			if err := d.scan(path, v); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("config %s: line %d: connections is not a block list; edit the file by hand", path, v.Line)
		}
	}
	return d, nil
}

// scan locates each entry of the block list v.
func (d *doc) scan(path string, v *yaml.Node) error {
	for i, n := range v.Content {
		dashLine, col := -1, 0
		for l := n.Line - 1; l >= 0; l-- {
			t := strings.TrimLeft(d.lines[l], " ")
			if t == "-" || strings.HasPrefix(t, "- ") {
				dashLine, col = l, len(d.lines[l])-len(t)
				break
			}
		}
		if dashLine < 0 {
			return fmt.Errorf("config %s: line %d: unsupported layout of connections; edit the file by hand", path, n.Line)
		}
		if i == 0 {
			d.dash = col
		}
		it := item{first: dashLine, end: dashLine, line: n.Line}
		for l := dashLine + 1; l < len(d.lines); l++ {
			t := strings.TrimLeft(d.lines[l], " ")
			if t == "" {
				continue
			}
			if len(d.lines[l])-len(t) <= col {
				break
			}
			it.end = l
		}
		// A comment block at the end of the range, however deeply indented,
		// sits above the next entry and belongs to it.
		// Lines inside a block scalar are content, whatever they start with.
		for it.end > it.first && it.end > d.lastLine(n)-1 && strings.HasPrefix(strings.TrimLeft(d.lines[it.end], " "), "#") {
			it.end--
		}
		if n.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(n.Content); j += 2 {
				if n.Content[j].Value == "name" {
					it.name = n.Content[j+1].Value
				}
			}
		}
		d.items = append(d.items, it)
	}
	return nil
}

// lastLine is the 1-based last line known to hold content of n: the
// furthest line of any descendant, counting the body of multi-line scalars.
func (d *doc) lastLine(n *yaml.Node) int {
	last := d.scalarEnd(n)
	for _, c := range n.Content {
		last = max(last, d.lastLine(c))
	}
	return last
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// scalarEnd is the 1-based last line of scalar n, read from the source text:
// a block scalar runs while lines are as indented as its first, a quoted one
// to its closing quote. Other nodes end on their own line.
func (d *doc) scalarEnd(n *yaml.Node) int {
	if n.Kind != yaml.ScalarNode || n.Line < 1 || n.Line > len(d.lines) {
		return n.Line
	}
	switch {
	case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		base := indentOf(d.lines[n.Line-1])
		if strings.HasPrefix(strings.TrimLeft(d.lines[n.Line-1], " "), "- ") {
			base += 2
		}
		ci, end := -1, n.Line
		for j := n.Line; j < len(d.lines); j++ {
			if strings.TrimSpace(d.lines[j]) == "" {
				continue
			}
			ind := indentOf(d.lines[j])
			if ci < 0 {
				if ind <= base {
					break
				}
				ci = ind
			}
			if ind < ci {
				break
			}
			end = j + 1
		}
		return end
	case n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0:
		double := n.Style&yaml.DoubleQuotedStyle != 0
		for j := n.Line - 1; j < len(d.lines); j++ {
			rs := []rune(d.lines[j])
			i := 0
			if j == n.Line-1 {
				i = n.Column // just past the opening quote (Column is 1-based)
			}
			for ; i < len(rs); i++ {
				switch {
				case double && rs[i] == '\\':
					i++
				case !double && rs[i] == '\'' && i+1 < len(rs) && rs[i+1] == '\'':
					i++
				case (double && rs[i] == '"') || (!double && rs[i] == '\''):
					return j + 1
				}
			}
		}
	}
	return n.Line
}

func (d *doc) find(name string) int {
	return slices.IndexFunc(d.items, func(it item) bool { return it.name == name })
}

// render returns c as list-entry lines starting at column dash.
func render(c Connection, dash int) ([]string, error) {
	m := &yaml.Node{Kind: yaml.MappingNode}
	add := func(k string, v *yaml.Node) { m.Content = append(m.Content, scalar(k), v) }
	add("name", scalar(c.Name))
	if c.Driver != "" {
		add("driver", scalar(c.Driver))
	}
	add("dsn", scalar(c.DSN))
	if c.Password != "" {
		add("password", scalar(c.Password))
	}
	if c.Color != "" {
		add("color", scalar(c.Color))
	}
	if c.ReadOnly {
		add("read_only", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	if len(c.Tags) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, t := range c.Tags {
			seq.Content = append(seq.Content, scalar(t))
		}
		add("tags", seq)
	}
	if len(c.Options) > 0 {
		opts := &yaml.Node{Kind: yaml.MappingNode}
		keys := make([]string, 0, len(c.Options))
		for k := range c.Options {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			opts.Content = append(opts.Content, scalar(k), scalar(c.Options[k]))
		}
		add("options", opts)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	body := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	out := make([]string, len(body))
	for i, l := range body {
		if i == 0 {
			out[i] = strings.Repeat(" ", dash) + "- " + l
		} else {
			out[i] = strings.Repeat(" ", dash+2) + l
		}
	}
	return out, nil
}

// scalar is a string node; the encoder quotes it when plain would be wrong.
func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func (d *doc) insert(c Connection) error {
	entry, err := render(c, d.dash)
	if err != nil {
		return err
	}
	switch {
	case len(d.items) > 0:
		d.splice(d.items[len(d.items)-1].end+1, 0, entry)
	case d.keyLine >= 0:
		if d.emptyFlow {
			m := emptyFlowRE.FindStringSubmatch(d.lines[d.keyLine])
			d.lines[d.keyLine] = m[1] + m[2]
		}
		d.splice(d.keyLine+1, 0, entry)
	default:
		d.trimTrailingBlank()
		d.lines = append(d.lines, "connections:")
		d.lines = append(d.lines, entry...)
		d.lines = append(d.lines, "")
	}
	return nil
}

// trimTrailingBlank makes the text end at its last line's end, without the
// final empty element a trailing newline leaves.
func (d *doc) trimTrailingBlank() {
	if n := len(d.lines); n > 0 && d.lines[n-1] == "" {
		d.lines = d.lines[:n-1]
	}
	if len(d.lines) == 1 && d.lines[0] == "" {
		d.lines = nil
	}
}

func (d *doc) replace(i int, c Connection) error {
	it := d.items[i]
	entry, err := render(c, d.dash)
	if err != nil {
		return err
	}
	d.splice(it.first, it.end-it.first+1, entry)
	return nil
}

func (d *doc) delete(i int) {
	it := d.items[i]
	d.splice(it.first, it.end-it.first+1, nil)
	// Do not leave two blank lines where the entry sat between blanks.
	if at := it.first; at > 0 && at < len(d.lines) && strings.TrimSpace(d.lines[at-1]) == "" && strings.TrimSpace(d.lines[at]) == "" {
		d.splice(at-1, 1, nil)
	}
}

func (d *doc) splice(at, del int, ins []string) {
	d.lines = slices.Concat(d.lines[:at], ins, d.lines[at+del:])
}

// text returns the edited text with LF ends, always newline-terminated.
func (d *doc) text() string {
	s := strings.Join(d.lines, "\n")
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// bytes returns text in the file's own line-end style.
func (d *doc) bytes() []byte {
	s := d.text()
	if d.crlf {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	return []byte(s)
}

// SetPasswordRefAt sets the password: of the entry that starts on line to
// ref, touching nothing else: the entry's other fields and comments, loaded
// or not, stay byte for byte. An existing password: line is replaced; without
// one a line is added at the entry's key indentation. Only ref is validated,
// so a broken entry can be given a reference. Errors as for UpdateConnectionAt.
func SetPasswordRefAt(path string, line int, ref string, expect Stat) (Stat, error) {
	if ref == "" {
		return Stat{}, errors.New("password reference is required")
	}
	if err := ValidatePasswordRef(ref); err != nil {
		return Stat{}, err
	}
	return newWriter().edit(path, expect, func(d *doc) error {
		i, err := byLine(line)(d)
		if err != nil {
			// A bare "-" entry (its mapping starts on the next line) may be
			// addressed by the dash line too.
			if j := slices.IndexFunc(d.items, func(it item) bool { return it.first+1 == line }); j >= 0 {
				i = j
			} else {
				return err
			}
		}
		if err := d.setPassword(i, ref); err != nil {
			return err
		}
		if n := passwordKeys(d.text(), d.items[i].line); n != 1 {
			return errors.New("this entry's layout is not supported; set password: by hand")
		}
		return nil
	})
}

// passwordKeys counts the password keys of the mapping that starts on line.
func passwordKeys(text string, line int) int {
	var root yaml.Node
	if yaml.Unmarshal([]byte(text), &root) != nil {
		return -1
	}
	n := 0
	var walk func(*yaml.Node)
	walk = func(m *yaml.Node) {
		if m.Kind == yaml.MappingNode && m.Line == line {
			for i := 0; i+1 < len(m.Content); i += 2 {
				if m.Content[i].Value == "password" {
					n++
				}
			}
			return
		}
		for _, c := range m.Content {
			walk(c)
		}
	}
	walk(&root)
	return n
}

// passwordKeyRE matches a password key, bare or quoted, with what follows.
var passwordKeyRE = regexp.MustCompile(`^(\s*(?:-\s+)?)(?:"password"|'password'|password)\s*:(.*)$`)

// valueComment returns the trailing comment of a one-line scalar value
// (with the whitespace before it), or "".
func valueComment(v string) string {
	v = strings.TrimLeft(v, " \t")
	switch {
	case v == "":
		return ""
	case v[0] == '"' || v[0] == '\'':
		q, end := v[0], -1
		for i := 1; i < len(v); i++ {
			if q == '"' && v[i] == '\\' {
				i++
			} else if v[i] == q {
				if q == '\'' && i+1 < len(v) && v[i+1] == '\'' {
					i++
					continue
				}
				end = i + 1
				break
			}
		}
		if end < 0 {
			return ""
		}
		rest := v[end:]
		if t := strings.TrimLeft(rest, " \t"); strings.HasPrefix(t, "#") && len(t) < len(rest) {
			return rest
		}
		return ""
	case v[0] == '#':
		return ""
	}
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		// Unquoted: a comment starts at a "#" that follows whitespace.
		for j := i; j < len(v); j++ {
			if v[j] == '#' && (v[j-1] == ' ' || v[j-1] == '\t') {
				k := j
				for k > 0 && (v[k-1] == ' ' || v[k-1] == '\t') {
					k--
				}
				return v[k:]
			}
		}
	}
	return ""
}

// setPassword replaces or adds the password: line of item i.
func (d *doc) setPassword(i int, ref string) error {
	it := d.items[i]
	first := d.lines[it.first]
	rest := strings.TrimLeft(strings.TrimLeft(first, " ")[1:], " ") // after the "-"
	if rest == "" || strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "#") {
		return errors.New("this entry's layout is not supported; set password: by hand")
	}
	keyCol := len(first) - len(rest)
	val, err := yaml.Marshal(ref)
	if err != nil {
		return err
	}
	value := strings.TrimSuffix(string(val), "\n")
	if strings.Contains(value, "\n") {
		return errors.New("password reference cannot be written on one line")
	}
	for l := it.first; l <= it.end; l++ {
		m := passwordKeyRE.FindStringSubmatch(d.lines[l])
		if m == nil || len(m[1]) != keyCol {
			continue
		}
		// Drop any continuation lines of a multi-line old value.
		del := 1
		for l+del <= it.end {
			t := strings.TrimLeft(d.lines[l+del], " ")
			if t == "" || strings.HasPrefix(t, "#") || indentOf(d.lines[l+del]) <= keyCol {
				break
			}
			del++
		}
		d.splice(l, del, []string{m[1] + "password: " + value + valueComment(m[2])})
		return nil
	}
	d.splice(it.end+1, 0, []string{strings.Repeat(" ", keyCol) + "password: " + value})
	return nil
}
