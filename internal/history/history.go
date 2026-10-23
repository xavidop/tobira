// Package history keeps a local log of the queries tobira ran.
//
// It stores entries in a SQLite file, but reaches SQLite only through the
// driver registry, so the root module stays free of database SDKs.
package history

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
)

// Source says which front end ran a query.
type Source string

// The sources of a history entry.
const (
	SourceCLI Source = "cli"
	SourceTUI Source = "tui"
	SourceMCP Source = "mcp"
	// SourceAI marks a query run by an assistant tool.
	SourceAI Source = "ai"
)

const (
	// DefaultMaxEntries is how many entries Record keeps unless Open is
	// given MaxEntries.
	DefaultMaxEntries = 10000
	// defaultLimit is the List limit when Filter.Limit is not positive.
	defaultLimit = 100
	// recordTimeout bounds one Record call (insert plus trim).
	recordTimeout = 2 * time.Second
	// openTimeout bounds Open, whatever deadline the caller has.
	openTimeout = 2 * time.Second
)

const schema = `CREATE TABLE IF NOT EXISTS history (
  id INTEGER PRIMARY KEY,
  at TEXT NOT NULL,
  conn TEXT NOT NULL,
  source TEXT NOT NULL,
  query TEXT NOT NULL,
  ok INTEGER NOT NULL,
  rows INTEGER NOT NULL,
  ms INTEGER NOT NULL,
  client TEXT
)`

// addClientColumn upgrades a history file made before the client column.
const addClientColumn = "ALTER TABLE history ADD COLUMN client TEXT"

// maxClientRunes caps a stored client name.
const maxClientRunes = 64

// Entry is one recorded query.
type Entry struct {
	ID     int64
	At     time.Time // UTC
	Conn   string    // connection name, or redacted DSN for ad-hoc
	Source Source
	Query  string // already redacted when stored
	OK     bool
	Rows   int64
	Took   time.Duration // stored as ms
	// Client names the program behind the call, for MCP calls; "" for none.
	// Record escapes it and keeps its first 64 runes.
	Client string
}

// Filter selects entries for List.
type Filter struct {
	Conn   string // exact match; "" = all
	Source Source // exact match; "" = all
	// ExcludeSources leaves out entries from these front ends.
	ExcludeSources []Source
	Search         string // case-insensitive substring of Query; "" = none
	Limit          int    // <=0 → 100; capped at 10000
}

// Store is the history log. A nil *Store is valid and does nothing. It is
// safe for concurrent use.
type Store struct {
	conn driver.Conn
	log  *slog.Logger
	// hasClient says the client column exists (it could not be added to a
	// read-only pre-M5 file).
	hasClient bool
	// schema is the schema version SchemaVersion reports.
	schema int
	// maxEntries is how many entries Record keeps and List can return.
	maxEntries int
}

// MaxEntries is how many entries the store keeps: the MaxEntries option
// given to Open, else DefaultMaxEntries. It is 0 for a nil Store.
func (s *Store) MaxEntries() int {
	if s == nil {
		return 0
	}
	return s.maxEntries
}

// SetLogger sets where List reports rows it had to skip (at debug level).
// Call it before sharing the store; nil means no logging.
func (s *Store) SetLogger(l *slog.Logger) {
	if s != nil {
		s.log = l
	}
}

// PathFor is the history file path given an environment lookup and the home
// directory: $XDG_STATE_HOME/tobira/history.db, else
// ~/.local/state/tobira/history.db on every OS.
func PathFor(getenv func(string) string, home string) (string, error) {
	dir := getenv("XDG_STATE_HOME")
	if dir == "" {
		if home == "" {
			return "", errors.New("history: cannot locate the state directory (set XDG_STATE_HOME)")
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "tobira", "history.db"), nil
}

// Resolve is the history file path: configured (the history.path config
// key) when it is not empty, else PathFor. In a configured path, $VAR and
// ${VAR} are replaced with getenv's value, and then a leading "~" with home.
// Every variable it names must be set and not empty, and the result must be
// an absolute path to a file, so a typo never lands the history somewhere
// unexpected.
func Resolve(configured string, getenv func(string) string, home string) (string, error) {
	if configured == "" {
		return PathFor(getenv, home)
	}
	var unset string
	p := os.Expand(configured, func(name string) string {
		v := getenv(name)
		if v == "" && unset == "" {
			unset = name
		}
		return v
	})
	if unset != "" {
		return "", fmt.Errorf("history.path: $%s is not set", unset)
	}
	if p == "~" {
		return "", errors.New("history.path: must name a file, not the home directory")
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		if home == "" {
			return "", errors.New("history.path: cannot expand ~ without a home directory")
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("history.path: must be an absolute path once expanded")
	}
	return filepath.Clean(p), nil
}

// OpenOption changes how Open prepares the file.
type OpenOption func(*openOptions)

type openOptions struct {
	keepDirMode bool
	maxEntries  int
}

// MaxEntries sets how many entries Record keeps (the history.max_entries
// config key); n < 1 means DefaultMaxEntries.
func MaxEntries(n int) OpenOption { return func(o *openOptions) { o.maxEntries = n } }

// KeepDirMode leaves the mode of an existing parent directory alone, for a
// configured history.path that may sit in a directory the user shares
// (their home, say). Directories Open creates are still 0700. Without it
// Open also tightens an existing parent to 0700, as it does for the
// default location, whose parent is tobira's own state directory.
func KeepDirMode() OpenOption { return func(o *openOptions) { o.keepDirMode = true } }

// DefaultPath is PathFor over the process environment.
func DefaultPath() (string, error) {
	home, _ := os.UserHomeDir()
	return PathFor(os.Getenv, home)
}

// Open opens (creating it if needed) the history file at path through the
// registry's "sqlite" driver. Missing directories are created with mode 0700
// and the file with 0600 (a looser file is tightened). It returns a wrapped driver.ErrUnsupported when reg has no
// sqlite driver.
func Open(ctx context.Context, reg *driver.Registry, path string, opts ...OpenOption) (*Store, error) {
	var o openOptions
	for _, opt := range opts {
		opt(&o)
	}
	if reg == nil {
		reg = driver.Default
	}
	if _, ok := reg.Lookup("sqlite"); !ok {
		return nil, fmt.Errorf("history: the sqlite driver is not available: %w", driver.ErrUnsupported)
	}
	// Independent of the caller's deadline (for example --timeout): history
	// must neither stall a command nor inherit its budget.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openTimeout)
	defer cancel()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("history: create directory: %w", err)
	}
	// MkdirAll applies the mode only to directories it creates; tighten our own.
	if fi, err := os.Stat(dir); err == nil && !o.keepDirMode && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("history: restrict directory: %w", err)
		}
	}
	if err := privateFile(path); err != nil {
		return nil, fmt.Errorf("history: prepare file: %w", err)
	}
	conn, err := reg.Open(ctx, driver.ConnConfig{Driver: "sqlite", DSN: dsnFor(path)})
	if err != nil {
		return nil, fmt.Errorf("history: open: %w", err)
	}
	s := &Store{conn: conn, maxEntries: DefaultMaxEntries}
	if o.maxEntries > 0 {
		s.maxEntries = o.maxEntries
	}
	if err := s.exec(ctx, schema); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("history: create schema: %w", err)
	}
	// A file that cannot be migrated (read-only, say) still opens: only
	// Record needs the column, and it leaves the client out without it.
	s.hasClient = s.migrate(ctx) == nil
	s.schema = SchemaOriginal
	if s.hasClient {
		s.schema = SchemaClient
	}
	return s, nil
}

// OpenReadOnly opens the existing history file at path read-only, for
// inspection (tobira doctor): it creates nothing, changes no mode, creates
// no table and runs no migration. It returns an error wrapping
// os.ErrNotExist when the file does not exist, and a wrapped
// driver.ErrUnsupported when reg has no sqlite driver. SchemaVersion then
// reports the schema found, SchemaNone for a file with no history table.
// Record on the store fails.
func OpenReadOnly(ctx context.Context, reg *driver.Registry, path string) (*Store, error) {
	if reg == nil {
		reg = driver.Default
	}
	if _, ok := reg.Lookup("sqlite"); !ok {
		return nil, fmt.Errorf("history: the sqlite driver is not available: %w", driver.ErrUnsupported)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openTimeout)
	defer cancel()
	conn, err := reg.Open(ctx, driver.ConnConfig{Driver: "sqlite", DSN: dsnFor(path), ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("history: open: %w", err)
	}
	s := &Store{conn: conn, maxEntries: DefaultMaxEntries}
	cols, err := s.columns(ctx, true)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("history: read schema: %w", err)
	}
	switch {
	case len(cols) == 0:
		s.schema = SchemaNone
	case slices.Contains(cols, "client"):
		s.schema, s.hasClient = SchemaClient, true
	default:
		s.schema = SchemaOriginal
	}
	return s, nil
}

// columns lists the history table's columns; none when it does not exist.
func (s *Store) columns(ctx context.Context, readOnly bool) ([]string, error) {
	rs, err := s.conn.Query(ctx, driver.Query{Text: "PRAGMA table_info(history)", ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if len(r) > 1 {
			if name, ok := r[1].AsText(); ok {
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// migrate adds the client column to a history file that lacks it. It is
// idempotent, and safe when another process migrates at the same moment: the
// loser's duplicate column error means the work is done.
func (s *Store) migrate(ctx context.Context) error {
	cols, err := s.columns(ctx, false)
	if err != nil {
		return err
	}
	if slices.Contains(cols, "client") {
		return nil
	}
	if err := s.exec(ctx, addClientColumn); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	return nil
}

// Schema versions of a history file, as SchemaVersion reports them.
const (
	// SchemaNone is a file with no history table yet (OpenReadOnly only).
	SchemaNone = 0
	// SchemaOriginal is the first table, without the client column.
	SchemaOriginal = 1
	// SchemaClient adds the client column.
	SchemaClient = 2
)

// SchemaVersion is the schema of the open file: SchemaClient once it has
// the client column, else SchemaOriginal (a pre-M5 file that could not be
// migrated, read-only say), or SchemaNone for a file OpenReadOnly found
// without a history table. It is 0 for a nil Store.
func (s *Store) SchemaVersion() int {
	if s == nil {
		return 0
	}
	return s.schema
}

var dsnEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23", " ", "%20")

// dsnFor builds a file: DSN for path. The sqlite driver splits a DSN at "?"
// and percent-decodes the path of a file: DSN, so those characters are escaped.
func dsnFor(path string) string { return "file:" + dsnEscaper.Replace(path) }

// privateFile makes sure path exists with mode 0600, tightening a looser file.
func privateFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		return f.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(path, 0o600)
	}
	return nil
}

func (s *Store) exec(ctx context.Context, text string, params ...driver.Value) error {
	rs, err := s.conn.Query(ctx, driver.Query{Text: text, Params: params})
	if err != nil {
		return err
	}
	_, _, err = driver.Collect(ctx, rs, 0)
	return err
}

// Record stores e after masking its query text with driver.RedactMessage and
// cc, then trims the log to its last MaxEntries entries. It is bounded to
// two seconds. The caller decides what to do with the error; history failures
// must never fail the user's command.
func (s *Store) Record(ctx context.Context, e Entry, cc driver.ConnConfig) error {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	ok := int64(0)
	if e.OK {
		ok = 1
	}
	cols, marks := "at, conn, source, query, ok, rows, ms", "?, ?, ?, ?, ?, ?, ?"
	params := []driver.Value{
		driver.String(at.UTC().Format(time.RFC3339Nano)),
		driver.String(e.Conn),
		driver.String(string(e.Source)),
		driver.String(driver.RedactMessage(e.Query, cc)),
		driver.Int(ok),
		driver.Int(e.Rows),
		driver.Int(e.Took.Milliseconds()),
	}
	if s.hasClient {
		cols, marks = cols+", client", marks+", ?"
		params = append(params, clientValue(e.Client))
	}
	err := s.exec(ctx, "INSERT INTO history ("+cols+") VALUES ("+marks+")", params...)
	if err != nil {
		return fmt.Errorf("history: record: %w", err)
	}
	if err := s.exec(ctx, "DELETE FROM history WHERE id <= (SELECT MAX(id) - ? FROM history)", driver.Int(int64(s.maxEntries))); err != nil {
		return fmt.Errorf("history: trim: %w", err)
	}
	return nil
}

// clientValue is the client column value: NULL for none, else the name cut to
// 64 runes and then escaped (it comes from a remote program, so it must not
// carry terminal controls or invisible characters). Cutting first keeps an
// escape sequence whole.
func clientValue(client string) driver.Value {
	n := 0
	for i := range client {
		if n == maxClientRunes {
			client = client[:i]
			break
		}
		n++
	}
	client = escapeInvisible(query.EscapeControls(client))
	if client == "" {
		return driver.Null()
	}
	return driver.String(client)
}

// escapeInvisible escapes the invisible characters query.EscapeControls leaves
// alone (it is tuned for table cells): line and paragraph separators, the soft
// hyphen, the Arabic letter mark, word joiners and invisible operators, and
// tag characters.
func escapeInvisible(s string) string {
	if !strings.ContainsFunc(s, isInvisible) {
		return s
	}
	var sb strings.Builder
	for _, r := range s {
		if isInvisible(r) {
			fmt.Fprintf(&sb, "\\u{%04X}", r)
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func isInvisible(r rune) bool {
	return r == 0x2028 || r == 0x2029 || r == 0xAD || r == 0x61C || r >= 0x2060 && r <= 0x2064 || r >= 0xE0000 && r <= 0xE007F
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// List returns entries matching f, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Entry, error) {
	if s == nil {
		return nil, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, s.maxEntries)

	// LIKE folds ASCII case only. A non-ASCII term is matched in Go instead,
	// over every retained entry of the connection.
	goSearch := ""
	if !isASCII(f.Search) {
		goSearch = strings.ToLower(f.Search)
	}

	clientCol := "client"
	if !s.hasClient {
		clientCol = "NULL"
	}
	text := "SELECT id, at, conn, source, query, ok, rows, ms, " + clientCol + " FROM history"
	var where []string
	var params []driver.Value
	if f.Conn != "" {
		where = append(where, "conn = ?")
		params = append(params, driver.String(f.Conn))
	}
	if f.Source != "" {
		where = append(where, "source = ?")
		params = append(params, driver.String(string(f.Source)))
	}
	if len(f.ExcludeSources) > 0 {
		where = append(where, "source NOT IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(f.ExcludeSources)), ", ")+")")
		for _, src := range f.ExcludeSources {
			params = append(params, driver.String(string(src)))
		}
	}
	sqlLimit := limit
	switch {
	case goSearch != "":
		sqlLimit = s.maxEntries
	case f.Search != "":
		where = append(where, `query LIKE ? ESCAPE '\'`)
		params = append(params, driver.String("%"+likeEscaper.Replace(f.Search)+"%"))
	}
	if len(where) > 0 {
		text += " WHERE " + strings.Join(where, " AND ")
	}
	text += " ORDER BY id DESC LIMIT ?"
	params = append(params, driver.Int(int64(sqlLimit)))

	rs, err := s.conn.Query(ctx, driver.Query{Text: text, Params: params})
	if err != nil {
		return nil, fmt.Errorf("history: list: %w", err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, fmt.Errorf("history: list: %w", err)
	}
	out := make([]Entry, 0, min(len(rows), limit))
	for _, r := range rows {
		e, err := decode(r)
		if err != nil {
			if s.log != nil {
				s.log.Debug("history: skipping unreadable entry", "error", err.Error())
			}
			continue
		}
		if goSearch != "" && !strings.Contains(strings.ToLower(e.Query), goSearch) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// decode turns one SELECT row into an Entry, failing on any malformed column.
func decode(r driver.Row) (Entry, error) {
	if len(r) < 9 {
		return Entry{}, fmt.Errorf("row has %d columns, want 9", len(r))
	}
	var e Entry
	var ok bool
	if e.ID, ok = r[0].AsInt(); !ok {
		return Entry{}, errors.New("id is not an integer")
	}
	atText, ok := r[1].AsText()
	if !ok {
		return Entry{}, fmt.Errorf("entry %d: at is not text", e.ID)
	}
	at, err := time.Parse(time.RFC3339Nano, atText)
	if err != nil {
		return Entry{}, fmt.Errorf("entry %d: bad timestamp: %w", e.ID, err)
	}
	e.At = at.UTC()
	var src string
	var okInt, ms int64
	if e.Conn, ok = r[2].AsText(); !ok {
		return Entry{}, fmt.Errorf("entry %d: conn is not text", e.ID)
	}
	if src, ok = r[3].AsText(); !ok {
		return Entry{}, fmt.Errorf("entry %d: source is not text", e.ID)
	}
	e.Source = Source(src)
	if e.Query, ok = r[4].AsText(); !ok {
		return Entry{}, fmt.Errorf("entry %d: query is not text", e.ID)
	}
	if okInt, ok = r[5].AsInt(); !ok {
		return Entry{}, fmt.Errorf("entry %d: ok is not an integer", e.ID)
	}
	e.OK = okInt != 0
	if e.Rows, ok = r[6].AsInt(); !ok {
		return Entry{}, fmt.Errorf("entry %d: rows is not an integer", e.ID)
	}
	if ms, ok = r[7].AsInt(); !ok {
		return Entry{}, fmt.Errorf("entry %d: ms is not an integer", e.ID)
	}
	e.Took = time.Duration(ms) * time.Millisecond
	if !r[8].IsNull() {
		if e.Client, ok = r[8].AsText(); !ok {
			return Entry{}, fmt.Errorf("entry %d: client is not text", e.ID)
		}
	}
	return e, nil
}

// Clear deletes the entries matching f (its Limit is ignored) and returns
// how many it deleted. An empty Filter clears the whole log. The file is
// compacted afterwards, so a cleared secret is gone from the disk as well.
func (s *Store) Clear(ctx context.Context, f Filter) (int64, error) {
	if s == nil {
		return 0, nil
	}
	n, err := s.clear(ctx, f)
	if err != nil {
		return 0, fmt.Errorf("history: clear: %w", err)
	}
	if n > 0 {
		if err := s.exec(ctx, "VACUUM"); err != nil {
			return n, fmt.Errorf("history: compact: %w", err)
		}
	}
	return n, nil
}

func (s *Store) clear(ctx context.Context, f Filter) (int64, error) {
	if f.Conn == "" && f.Source == "" && len(f.ExcludeSources) == 0 && f.Search == "" {
		rs, err := s.conn.Query(ctx, driver.Query{Text: "SELECT COUNT(*) FROM history"})
		if err != nil {
			return 0, err
		}
		rows, _, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			return 0, err
		}
		var n int64
		if len(rows) > 0 && len(rows[0]) > 0 {
			n, _ = rows[0][0].AsInt()
		}
		if n == 0 {
			return 0, nil
		}
		return n, s.exec(ctx, "DELETE FROM history")
	}
	// A filtered clear removes exactly what the same filter lists, search
	// semantics included (non-ASCII text is matched in Go).
	f.Limit = s.maxEntries
	entries, err := s.List(ctx, f)
	if err != nil {
		return 0, err
	}
	const batch = 500
	for start := 0; start < len(entries); start += batch {
		end := min(start+batch, len(entries))
		marks := make([]string, 0, end-start)
		params := make([]driver.Value, 0, end-start)
		for _, e := range entries[start:end] {
			marks = append(marks, "?")
			params = append(params, driver.Int(e.ID))
		}
		if err := s.exec(ctx, "DELETE FROM history WHERE id IN ("+strings.Join(marks, ", ")+")", params...); err != nil {
			return 0, err
		}
	}
	return int64(len(entries)), nil
}

// Close releases the store.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.conn.Close()
}
