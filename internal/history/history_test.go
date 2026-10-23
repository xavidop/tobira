package history_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/history"
)

// stubDriver stands in for the sqlite driver: it records every statement and
// serves canned rows for SELECTs. The real driver is exercised end to end in
// cmd/tobira.
type stubDriver struct {
	mu      sync.Mutex
	opened  []driver.ConnConfig
	queries []driver.Query
	rows    []driver.Row
	// pragma is what PRAGMA table_info(history) returns: the column names.
	pragma []string
	// alterErr is returned by an ALTER TABLE statement.
	alterErr error
}

func (d *stubDriver) Info() driver.Info {
	return driver.Info{Name: "sqlite", Schemes: []string{"sqlite"}}
}

func (d *stubDriver) Open(_ context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened = append(d.opened, cfg)
	return &stubConn{d: d}, nil
}

type stubConn struct{ d *stubDriver }

func (c *stubConn) Query(_ context.Context, q driver.Query) (driver.ResultSet, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.queries = append(c.d.queries, q)
	switch up := strings.TrimSpace(strings.ToUpper(q.Text)); {
	case strings.HasPrefix(up, "PRAGMA"):
		rows := make([]driver.Row, len(c.d.pragma))
		for i, name := range c.d.pragma {
			rows[i] = driver.Row{driver.Int(int64(i)), driver.String(name), driver.String("TEXT"), driver.Int(0), driver.Null(), driver.Int(0)}
		}
		return driver.NewSliceResultSet(nil, rows, driver.Stats{RowsAffected: -1}), nil
	case strings.HasPrefix(up, "ALTER") && c.d.alterErr != nil:
		return nil, c.d.alterErr
	case strings.HasPrefix(up, "SELECT"):
		return driver.NewSliceResultSet(nil, c.d.rows, driver.Stats{RowsAffected: -1}), nil
	}
	return driver.NewSliceResultSet(nil, nil, driver.Stats{}), nil
}
func (c *stubConn) Ping(context.Context) error { return nil }
func (c *stubConn) Close() error               { return nil }
func (c *stubConn) Namespaces(context.Context) ([]driver.Namespace, error) {
	return nil, driver.ErrUnsupported
}
func (c *stubConn) Collections(context.Context, driver.NamespaceRef) ([]driver.Collection, error) {
	return nil, driver.ErrUnsupported
}
func (c *stubConn) Describe(context.Context, driver.CollectionRef) (*driver.CollectionSchema, error) {
	return nil, driver.ErrUnsupported
}

func newStore(t *testing.T) (*history.Store, *stubDriver, string) {
	t.Helper()
	d := &stubDriver{}
	reg := driver.NewRegistry()
	if err := reg.Add(d); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state", "tobira", "history.db")
	s, err := history.Open(context.Background(), reg, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, d, path
}

func TestOpenCreatesPrivateFileAndSchema(t *testing.T) {
	_, d, path := newStore(t)
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
	if len(d.opened) != 1 || d.opened[0].DSN != "file:"+path {
		t.Errorf("opened = %+v, want one file:%s", d.opened, path)
	}
	if len(d.queries) == 0 || !strings.Contains(d.queries[0].Text, "CREATE TABLE IF NOT EXISTS history") {
		t.Errorf("first statement should create the schema, got %+v", d.queries)
	}
}

func TestOpenTightensLooseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	path := filepath.Join(t.TempDir(), "h.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := driver.NewRegistry()
	_ = reg.Add(&stubDriver{})
	s, err := history.Open(context.Background(), reg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestOpenWithoutSQLiteDriver(t *testing.T) {
	_, err := history.Open(context.Background(), driver.NewRegistry(), filepath.Join(t.TempDir(), "h.db"))
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestNilStoreIsNoop(t *testing.T) {
	var s *history.Store
	if err := s.Record(context.Background(), history.Entry{Query: "x"}, driver.ConnConfig{}); err != nil {
		t.Errorf("Record: %v", err)
	}
	got, err := s.List(context.Background(), history.Filter{})
	if err != nil || len(got) != 0 {
		t.Errorf("List = %v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestRecordRedactsQueryBeforeStoring(t *testing.T) {
	s, d, _ := newStore(t)
	cc := driver.ConnConfig{DSN: "postgres://u:s3cretpw@h/db", Password: driver.NewSecret("hunter2hunter2")}
	q := "select 'postgres://u:s3cretpw@h/db', 'hunter2hunter2'"
	err := s.Record(context.Background(), history.Entry{
		At: time.Unix(0, 0), Conn: "x", Source: history.SourceCLI, Query: q, OK: true, Rows: 1, Took: 1500 * time.Microsecond,
	}, cc)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range d.queries {
		for _, p := range rec.Params {
			if str := p.String(); strings.Contains(str, "s3cretpw") || strings.Contains(str, "hunter2hunter2") {
				t.Fatalf("secret reached the driver: %q", str)
			}
		}
	}
	var insert *driver.Query
	for i := range d.queries {
		if strings.HasPrefix(d.queries[i].Text, "INSERT") {
			insert = &d.queries[i]
		}
	}
	if insert == nil {
		t.Fatal("no INSERT issued")
	}
	if !strings.Contains(insert.Params[3].String(), "xxxxx") {
		t.Errorf("query param not masked: %q", insert.Params[3].String())
	}
	var deleted bool
	for _, rec := range d.queries {
		deleted = deleted || strings.HasPrefix(rec.Text, "DELETE FROM history")
	}
	if !deleted {
		t.Error("retention DELETE not issued")
	}
}

func TestListFilterAndDecode(t *testing.T) {
	s, d, _ := newStore(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d.rows = []driver.Row{{
		driver.Int(7), driver.String(at.Format(time.RFC3339Nano)), driver.String("c"), driver.String("tui"),
		driver.String("select 1"), driver.Int(1), driver.Int(3), driver.Int(42), driver.String("claude-code 2.1"),
	}}
	got, err := s.List(context.Background(), history.Filter{Conn: "c", Search: "50%_x", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	want := history.Entry{ID: 7, At: at, Conn: "c", Source: history.SourceTUI, Query: "select 1", OK: true, Rows: 3, Took: 42 * time.Millisecond, Client: "claude-code 2.1"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	last := d.queries[len(d.queries)-1]
	if !strings.Contains(last.Text, "ORDER BY id DESC") || !strings.Contains(last.Text, "LIKE") {
		t.Errorf("unexpected SQL: %s", last.Text)
	}
	joined := ""
	for _, p := range last.Params {
		joined += "|" + p.String()
	}
	if !strings.Contains(joined, `%50\%\_x%`) || !strings.HasSuffix(joined, "|5") {
		t.Errorf("params = %s", joined)
	}
}

func TestListLimitDefaultsAndCap(t *testing.T) {
	s, d, _ := newStore(t)
	for in, want := range map[int]string{0: "100", -3: "100", 50000: "10000"} {
		if _, err := s.List(context.Background(), history.Filter{Limit: in}); err != nil {
			t.Fatal(err)
		}
		last := d.queries[len(d.queries)-1]
		if got := last.Params[len(last.Params)-1].String(); got != want {
			t.Errorf("limit %d -> %s, want %s", in, got, want)
		}
	}
}

func TestPathFor(t *testing.T) {
	env := map[string]string{"XDG_STATE_HOME": "/s"}
	got, err := history.PathFor(func(k string) string { return env[k] }, "/home/u")
	if err != nil || got != filepath.Join("/s", "tobira", "history.db") {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = history.PathFor(func(string) string { return "" }, "/home/u")
	if err != nil || got != filepath.Join("/home/u", ".local", "state", "tobira", "history.db") {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := history.PathFor(func(string) string { return "" }, ""); err == nil {
		t.Error("want error without a home")
	}
}

// blockingDriver never finishes opening until its context ends.
type blockingDriver struct{ stubDriver }

func (d *blockingDriver) Open(ctx context.Context, _ driver.ConnConfig) (driver.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestOpenIsBoundedIndependentlyOfCaller(t *testing.T) {
	reg := driver.NewRegistry()
	if err := reg.Add(&blockingDriver{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := history.Open(context.Background(), reg, filepath.Join(t.TempDir(), "h.db"))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Open took %v, want about 2s", time.Since(start))
	}
}

func TestOpenTightensOwnDirOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	parent := filepath.Join(t.TempDir(), "state")
	own := filepath.Join(parent, "tobira")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(own, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := driver.NewRegistry()
	_ = reg.Add(&stubDriver{})
	s, err := history.Open(context.Background(), reg, filepath.Join(own, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if fi, _ := os.Stat(own); fi.Mode().Perm() != 0o700 {
		t.Errorf("own dir mode = %v, want 0700", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(parent); fi.Mode().Perm() != 0o755 {
		t.Errorf("parent mode = %v, want unchanged 0755", fi.Mode().Perm())
	}
}

func TestOpenEscapesPathInDSN(t *testing.T) {
	d := &stubDriver{}
	reg := driver.NewRegistry()
	_ = reg.Add(d)
	path := filepath.Join(t.TempDir(), "a b?c#d%e", "history.db")
	s, err := history.Open(context.Background(), reg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	dsn := d.opened[0].DSN
	for _, bad := range []string{"?", "#", " "} {
		if strings.Contains(dsn, bad) {
			t.Errorf("DSN %q contains raw %q", dsn, bad)
		}
	}
	if !strings.Contains(dsn, "%3F") || !strings.Contains(dsn, "%23") || !strings.Contains(dsn, "%25") {
		t.Errorf("DSN %q does not escape ? # %%", dsn)
	}
}

func TestListSearchUnicodeIsCaseInsensitive(t *testing.T) {
	s, d, _ := newStore(t)
	row := func(id int64, q string) driver.Row {
		return driver.Row{driver.Int(id), driver.String("2026-01-02T03:04:05Z"), driver.String("c"), driver.String("cli"),
			driver.String(q), driver.Int(1), driver.Int(0), driver.Int(0), driver.Null()}
	}
	d.rows = []driver.Row{row(3, "select 'ÉCOLE'"), row(2, "select 1"), row(1, "select 'école'")}
	got, err := s.List(context.Background(), history.Filter{Search: "École", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("got %+v, want newest match only (id 3)", got)
	}
	got, _ = s.List(context.Background(), history.Filter{Search: "école"})
	if len(got) != 2 {
		t.Errorf("got %d matches, want 2", len(got))
	}
}

func TestListSkipsCorruptRowsAndLogs(t *testing.T) {
	s, d, _ := newStore(t)
	var buf bytes.Buffer
	s.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	good := driver.Row{driver.Int(1), driver.String("2026-01-02T03:04:05Z"), driver.String("c"), driver.String("cli"),
		driver.String("select 1"), driver.Int(1), driver.Int(0), driver.Int(0), driver.Null()}
	bad := driver.Row{driver.Int(2), driver.String("not a time"), driver.String("c"), driver.String("cli"),
		driver.String("select 2"), driver.Int(1), driver.Int(0), driver.Int(0), driver.Null()}
	d.rows = []driver.Row{bad, good}
	got, err := s.List(context.Background(), history.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("got %+v, want only the good row", got)
	}
	if !strings.Contains(buf.String(), "skipping") {
		t.Errorf("no debug log for the bad row: %q", buf.String())
	}
}

// Queries run by the assistant's tools are stored with source "ai".
func TestRecordStoresSourceAI(t *testing.T) {
	s, d, _ := newStore(t)
	err := s.Record(context.Background(), history.Entry{
		At: time.Unix(0, 0), Conn: "x", Source: history.SourceAI, Query: "select 1", OK: true,
	}, driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range d.queries {
		if strings.HasPrefix(q.Text, "INSERT") {
			if got := q.Params[2].String(); got != "ai" {
				t.Fatalf("source param = %q, want %q", got, "ai")
			}
			return
		}
	}
	t.Fatal("no INSERT issued")
}

func lastInsert(t *testing.T, d *stubDriver) driver.Query {
	t.Helper()
	for i := len(d.queries) - 1; i >= 0; i-- {
		if strings.HasPrefix(d.queries[i].Text, "INSERT") {
			return d.queries[i]
		}
	}
	t.Fatal("no INSERT issued")
	return driver.Query{}
}

func countStatements(d *stubDriver, prefix string) int {
	n := 0
	for _, q := range d.queries {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q.Text)), prefix) {
			n++
		}
	}
	return n
}

func openStub(t *testing.T, d *stubDriver) error {
	t.Helper()
	reg := driver.NewRegistry()
	if err := reg.Add(d); err != nil {
		t.Fatal(err)
	}
	s, err := history.Open(context.Background(), reg, filepath.Join(t.TempDir(), "h.db"))
	if err == nil {
		_ = s.Close()
	}
	return err
}

func TestOpenAddsMissingClientColumn(t *testing.T) {
	d := &stubDriver{pragma: []string{"id", "at", "conn", "source", "query", "ok", "rows", "ms"}}
	if err := openStub(t, d); err != nil {
		t.Fatal(err)
	}
	if n := countStatements(d, "ALTER TABLE HISTORY ADD COLUMN CLIENT"); n != 1 {
		t.Fatalf("ALTER issued %d times, want 1: %+v", n, d.queries)
	}
}

func TestOpenLeavesCurrentSchemaAlone(t *testing.T) {
	d := &stubDriver{pragma: []string{"id", "at", "conn", "source", "query", "ok", "rows", "ms", "client"}}
	if err := openStub(t, d); err != nil {
		t.Fatal(err)
	}
	if n := countStatements(d, "ALTER"); n != 0 {
		t.Fatalf("ALTER issued %d times on a current schema, want 0", n)
	}
}

// Two processes opening an old database at once: the loser's ALTER fails
// with a duplicate column error, which is not a failure.
func TestOpenToleratesDuplicateColumn(t *testing.T) {
	d := &stubDriver{pragma: []string{"id"}, alterErr: errors.New("SQL logic error: duplicate column name: client (1)")}
	reg := driver.NewRegistry()
	if err := reg.Add(d); err != nil {
		t.Fatal(err)
	}
	s, err := history.Open(context.Background(), reg, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open failed on a duplicate column: %v", err)
	}
	defer func() { _ = s.Close() }()
	// The column exists (the other process added it), so the client is kept.
	err = s.Record(context.Background(), history.Entry{Conn: "x", Source: history.SourceMCP, Query: "q", OK: true, Client: "c"}, driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if in := lastInsert(t, d); !strings.Contains(in.Text, "client") {
		t.Errorf("duplicate column treated as a failed migration: %s", in.Text)
	}
}

// A database that cannot be migrated (read-only, say) still opens: history
// can be listed, and Record leaves the client out.
func TestOpenSurvivesFailedMigration(t *testing.T) {
	d := &stubDriver{pragma: []string{"id"}, alterErr: errors.New("attempt to write a readonly database")}
	reg := driver.NewRegistry()
	if err := reg.Add(d); err != nil {
		t.Fatal(err)
	}
	s, err := history.Open(context.Background(), reg, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open failed on a failed migration: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.List(context.Background(), history.Filter{}); err != nil {
		t.Fatal(err)
	}
	if q := d.queries[len(d.queries)-1]; strings.Contains(q.Text, "client") && !strings.Contains(q.Text, "NULL") {
		t.Errorf("List selects a column that does not exist: %s", q.Text)
	}
	err = s.Record(context.Background(), history.Entry{Conn: "x", Source: history.SourceMCP, Query: "select 1", OK: true, Client: "c"}, driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if in := lastInsert(t, d); strings.Contains(in.Text, "client") || len(in.Params) != 7 {
		t.Errorf("INSERT = %q with %d params, want no client column", in.Text, len(in.Params))
	}
}

func TestRecordStoresClientEscapedAndCapped(t *testing.T) {
	s, d, _ := newStore(t)
	rec := func(client string) driver.Value {
		t.Helper()
		err := s.Record(context.Background(), history.Entry{
			At: time.Unix(0, 0), Conn: "x", Source: history.SourceMCP, Query: "select 1", OK: true, Client: client,
		}, driver.ConnConfig{})
		if err != nil {
			t.Fatal(err)
		}
		in := lastInsert(t, d)
		if !strings.Contains(in.Text, "client") || len(in.Params) != 8 {
			t.Fatalf("INSERT = %q with %d params, want the client column", in.Text, len(in.Params))
		}
		return in.Params[7]
	}
	if got := rec("claude-code 2.1"); got.String() != "claude-code 2.1" {
		t.Errorf("client = %q", got.String())
	}
	if got := rec(""); !got.IsNull() {
		t.Errorf("empty client = %v, want NULL", got)
	}
	for in, want := range map[string]string{
		"a\x1b[2Jb":    `a\x1b[2Jb`,
		"a\nb":         "a b",
		"a\u202eb":     `a\u{202E}b`,
		"a\xffb":       `a\x{FF}b`,
		"a\u061cb":     `a\u{061C}b`,
		"a\u2060b":     `a\u{2060}b`,
		"a\u2028b":     `a\u{2028}b`,
		"a\u00adb":     `a\u{00AD}b`,
		"a\U000e0041b": `a\u{E0041}b`,
	} {
		if got := rec(in).String(); got != want {
			t.Errorf("client %q stored as %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 200)
	if got := rec(long).String(); utf8.RuneCountInString(got) != 64 {
		t.Errorf("client has %d runes, want 64", utf8.RuneCountInString(got))
	}
	// The cut is made before escaping, so an escape is never split.
	edge := strings.Repeat("a", 63) + "\u202e" + "tail"
	if got := rec(edge).String(); got != strings.Repeat("a", 63)+`\u{202E}` {
		t.Errorf("edge = %q", got)
	}
}

func TestListFiltersBySource(t *testing.T) {
	s, d, _ := newStore(t)
	if _, err := s.List(context.Background(), history.Filter{Source: history.SourceMCP, Conn: "c", Limit: 5}); err != nil {
		t.Fatal(err)
	}
	q := d.queries[len(d.queries)-1]
	if !strings.Contains(q.Text, "source = ?") || !strings.Contains(q.Text, "client") {
		t.Errorf("SQL = %s", q.Text)
	}
	var got []string
	for _, p := range q.Params {
		got = append(got, p.String())
	}
	if want := []string{"c", "mcp", "5"}; !slices.Equal(got, want) {
		t.Errorf("params = %v, want %v", got, want)
	}
	if _, err := s.List(context.Background(), history.Filter{}); err != nil {
		t.Fatal(err)
	}
	if q := d.queries[len(d.queries)-1]; strings.Contains(q.Text, "source =") {
		t.Errorf("empty Source filtered: %s", q.Text)
	}
}

// ExcludeSources leaves those front ends out (the editor's recall skips
// tool calls run by MCP clients and the AI).
func TestListExcludesSources(t *testing.T) {
	s, d, _ := newStore(t)
	if _, err := s.List(context.Background(), history.Filter{Conn: "c", ExcludeSources: []history.Source{history.SourceMCP, history.SourceAI}, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	q := d.queries[len(d.queries)-1]
	if !strings.Contains(q.Text, "source NOT IN (?, ?)") {
		t.Errorf("SQL = %s", q.Text)
	}
	var got []string
	for _, p := range q.Params {
		got = append(got, p.String())
	}
	if want := []string{"c", "mcp", "ai", "5"}; !slices.Equal(got, want) {
		t.Errorf("params = %v, want %v", got, want)
	}
}

func TestSchemaVersion(t *testing.T) {
	var nilStore *history.Store
	if v := nilStore.SchemaVersion(); v != 0 {
		t.Errorf("nil store version = %d, want 0", v)
	}
	s, _, _ := newStore(t)
	if v := s.SchemaVersion(); v != history.SchemaClient {
		t.Errorf("version = %d, want %d once the client column is added", v, history.SchemaClient)
	}
	reg := driver.NewRegistry()
	_ = reg.Add(&stubDriver{alterErr: errors.New("attempt to write a readonly database")})
	old, err := history.Open(context.Background(), reg, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if v := old.SchemaVersion(); v != history.SchemaOriginal {
		t.Errorf("unmigrated version = %d, want %d", v, history.SchemaOriginal)
	}
}

func TestResolve(t *testing.T) {
	env := map[string]string{"XDG_STATE_HOME": "/s", "DATA": "/data", "EMPTY": ""}
	getenv := func(k string) string { return env[k] }
	for _, tt := range []struct {
		configured, want, wantErr string
	}{
		{"", filepath.Join("/s", "tobira", "history.db"), ""},
		{"/var/h.db", "/var/h.db", ""},
		{"~/h/x.db", filepath.Join("/home/u", "h", "x.db"), ""},
		{"~", "", "a file"},
		{"$DATA/tobira/h.db", filepath.Join("/data", "tobira", "h.db"), ""},
		{"${DATA}/h.db", filepath.Join("/data", "h.db"), ""},
		{"$NOPE/h.db", "", "NOPE is not set"},
		{"$EMPTY/h.db", "", "EMPTY is not set"},
		{"$DATA", "/data", ""},
	} {
		got, err := history.Resolve(tt.configured, getenv, "/home/u")
		switch {
		case tt.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Resolve(%q) = %q, %v; want error containing %q", tt.configured, got, err, tt.wantErr)
			}
		case err != nil || got != tt.want:
			t.Errorf("Resolve(%q) = %q, %v; want %q", tt.configured, got, err, tt.want)
		}
	}
	if _, err := history.Resolve("~/h.db", getenv, ""); err == nil {
		t.Error("want an error expanding ~ without a home")
	}
	if _, err := history.Resolve("$REL", func(string) string { return "rel/h.db" }, "/h"); err == nil {
		t.Error("want an error for a path that expands to a relative one")
	}
}

// TestOpenKeepDirModeLeavesAnExistingDirAlone: a configured path in a
// directory the user owns (their home, say) must not chmod it, while a
// directory Open creates is 0700 and the file 0600.
func TestOpenKeepDirModeLeavesAnExistingDirAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := driver.NewRegistry()
	_ = reg.Add(&stubDriver{})
	path := filepath.Join(shared, "new", "deeper", "h.db")
	s, err := history.Open(context.Background(), reg, path, history.KeepDirMode())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o755 {
		t.Errorf("existing dir mode = %v, want unchanged 0755", fi.Mode().Perm())
	}
	for _, d := range []string{filepath.Join(shared, "new"), filepath.Dir(path)} {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("created dir %s mode = %v, want 0700", d, fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	s, err = history.Open(context.Background(), reg, filepath.Join(shared, "h.db"), history.KeepDirMode())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o755 {
		t.Errorf("parent of the file mode = %v, want unchanged 0755", fi.Mode().Perm())
	}
}

// TestOpenReadOnlyChangesNothing: OpenReadOnly opens an existing file
// read-only, issues no write (no CREATE, no ALTER), leaves a loose mode and
// the directory alone, and reports the schema it finds.
func TestOpenReadOnlyChangesNothing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		pragma []string
		want   int
	}{
		{"current", []string{"id", "at", "client"}, history.SchemaClient},
		{"pre-M5", []string{"id", "at"}, history.SchemaOriginal},
		{"no table yet", nil, history.SchemaNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "loose")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "h.db")
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(dir, 0o755)
			_ = os.Chmod(path, 0o644)
			d := &stubDriver{pragma: tt.pragma}
			reg := driver.NewRegistry()
			_ = reg.Add(d)
			s, err := history.OpenReadOnly(context.Background(), reg, path)
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			if v := s.SchemaVersion(); v != tt.want {
				t.Errorf("SchemaVersion = %d, want %d", v, tt.want)
			}
			if len(d.opened) != 1 || !d.opened[0].ReadOnly {
				t.Errorf("opened = %+v, want one read-only open", d.opened)
			}
			for _, q := range d.queries {
				if up := strings.ToUpper(strings.TrimSpace(q.Text)); !strings.HasPrefix(up, "PRAGMA") || !q.ReadOnly {
					t.Errorf("read-only open ran %q (ReadOnly=%v)", q.Text, q.ReadOnly)
				}
			}
			if runtime.GOOS != "windows" {
				if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
					t.Errorf("file mode = %v, want unchanged 0644", fi.Mode().Perm())
				}
				if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
					t.Errorf("dir mode = %v, want unchanged 0755", fi.Mode().Perm())
				}
			}
		})
	}
}

func TestOpenReadOnlyNeedsAnExistingFile(t *testing.T) {
	reg := driver.NewRegistry()
	_ = reg.Add(&stubDriver{})
	path := filepath.Join(t.TempDir(), "missing", "h.db")
	if _, err := history.OpenReadOnly(context.Background(), reg, path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Error("OpenReadOnly created the directory")
	}
	if _, err := history.OpenReadOnly(context.Background(), driver.NewRegistry(), path); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported without sqlite", err)
	}
}

func TestWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := history.Writable(path); err != nil {
		t.Errorf("Writable on a 0600 file in a 0700 dir: %v", err)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	_ = os.Chmod(path, 0o400)
	if err := history.Writable(path); err == nil {
		t.Error("Writable on a 0400 file: want an error")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o400 {
		t.Errorf("Writable changed the mode to %v", fi.Mode().Perm())
	}
	_ = os.Chmod(path, 0o600)
	_ = os.Chmod(dir, 0o500)
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if err := history.Writable(path); err == nil {
		t.Error("Writable in a read-only directory: want an error (SQLite writes a journal next to the file)")
	}
}

// History keeps a JSON query's secret-named fields (token, sig) and SQL
// text, but masks password values and key=value secrets.
func TestRecordKeepsSecretNamedJSONFields(t *testing.T) {
	cases := []struct{ query, keep, drop string }{
		{`{"collection":"sessions","filter":{"token":"t-123","sig":"abc"}}`, `"token":"t-123","sig":"abc"`, ""},
		{"update users set password='Pw9Zq9x' where id=1", "update users set password=", "Pw9Zq9x"},
		{`{"filter":{"password":"Pw8Zq9x"}}`, `"filter"`, "Pw8Zq9x"},
		{"select 1 -- https://h/x?sig=S1gZq9xx", "select 1", "S1gZq9xx"},
	}
	for _, tc := range cases {
		s, d, _ := newStore(t)
		err := s.Record(context.Background(), history.Entry{
			At: time.Unix(0, 0), Conn: "x", Source: history.SourceCLI, Query: tc.query, OK: true,
		}, driver.ConnConfig{})
		if err != nil {
			t.Fatal(err)
		}
		var stored string
		for _, q := range d.queries {
			if strings.HasPrefix(q.Text, "INSERT") {
				stored = q.Params[3].String()
			}
		}
		if !strings.Contains(stored, tc.keep) || (tc.drop != "" && strings.Contains(stored, tc.drop)) {
			t.Errorf("Record(%q) stored %q, want it to keep %q and drop %q", tc.query, stored, tc.keep, tc.drop)
		}
	}
}

// TestMaxEntriesOption: Record trims to the configured cap, List caps its
// limit there, and MaxEntries reports it (the default without the option).
func TestMaxEntriesOption(t *testing.T) {
	s, d, _ := newStore(t)
	if s.MaxEntries() != history.DefaultMaxEntries {
		t.Errorf("MaxEntries = %d, want the default %d", s.MaxEntries(), history.DefaultMaxEntries)
	}
	var nilStore *history.Store
	if nilStore.MaxEntries() != 0 {
		t.Error("a nil store has no cap")
	}

	reg := driver.NewRegistry()
	if err := reg.Add(d); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "history.db")
	capped, err := history.Open(context.Background(), reg, path, history.MaxEntries(250))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capped.Close() }()
	if capped.MaxEntries() != 250 {
		t.Fatalf("MaxEntries = %d, want 250", capped.MaxEntries())
	}
	if err := capped.Record(context.Background(), history.Entry{Conn: "c", Source: history.SourceCLI, Query: "select 1"}, driver.ConnConfig{}); err != nil {
		t.Fatal(err)
	}
	trim := d.queries[len(d.queries)-1]
	if !strings.HasPrefix(trim.Text, "DELETE FROM history WHERE id <= (SELECT MAX(id) - ?") || trim.Params[0].String() != "250" {
		t.Errorf("trim = %s %v, want the cap of 250", trim.Text, trim.Params)
	}
	if _, err := capped.List(context.Background(), history.Filter{Limit: 5000}); err != nil {
		t.Fatal(err)
	}
	list := d.queries[len(d.queries)-1]
	if got := list.Params[len(list.Params)-1].String(); got != "250" {
		t.Errorf("list limit = %s, want capped at 250", got)
	}
}

func TestClearWholeLog(t *testing.T) {
	s, d, _ := newStore(t)
	d.rows = []driver.Row{{driver.Int(3)}}
	n, err := s.Clear(context.Background(), history.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("cleared %d, want 3", n)
	}
	var texts []string
	for _, q := range d.queries {
		texts = append(texts, strings.TrimSpace(q.Text))
	}
	last3 := texts[len(texts)-3:]
	if last3[0] != "SELECT COUNT(*) FROM history" || last3[1] != "DELETE FROM history" || last3[2] != "VACUUM" {
		t.Errorf("statements = %q, want count, delete and vacuum", last3)
	}

	// An empty log deletes and compacts nothing.
	d.rows = []driver.Row{{driver.Int(0)}}
	before := len(d.queries)
	if n, err := s.Clear(context.Background(), history.Filter{}); err != nil || n != 0 {
		t.Fatalf("empty: n=%d err=%v", n, err)
	}
	if got := len(d.queries) - before; got != 1 {
		t.Errorf("an empty clear ran %d statements, want only the count", got)
	}
	var nilStore *history.Store
	if n, err := nilStore.Clear(context.Background(), history.Filter{}); n != 0 || err != nil {
		t.Errorf("nil store: n=%d err=%v", n, err)
	}
}

// TestClearFilteredDeletesWhatListMatches: a filtered clear removes exactly
// the ids the same filter lists, non-ASCII search included, then compacts.
func TestClearFilteredDeletesWhatListMatches(t *testing.T) {
	s, d, _ := newStore(t)
	row := func(id int64, q string) driver.Row {
		return driver.Row{driver.Int(id), driver.String("2026-01-02T03:04:05Z"), driver.String("c"), driver.String("cli"),
			driver.String(q), driver.Int(1), driver.Int(0), driver.Int(0), driver.Null()}
	}
	d.rows = []driver.Row{row(3, "select 'ÉCOLE'"), row(2, "select 1"), row(1, "select 'école'")}
	n, err := s.Clear(context.Background(), history.Filter{Conn: "c", Search: "école", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("cleared %d, want the 2 matches (Limit is ignored)", n)
	}
	q := d.queries[len(d.queries)-2]
	if q.Text != "DELETE FROM history WHERE id IN (?, ?)" || q.Params[0].String() != "3" || q.Params[1].String() != "1" {
		t.Errorf("delete = %s %v", q.Text, q.Params)
	}
	if last := d.queries[len(d.queries)-1].Text; last != "VACUUM" {
		t.Errorf("last statement = %s, want VACUUM", last)
	}
	sel := d.queries[len(d.queries)-3]
	if !strings.Contains(sel.Text, "conn = ?") || sel.Params[0].String() != "c" {
		t.Errorf("the clear did not list with the filter: %s %v", sel.Text, sel.Params)
	}

	// Nothing matching: no delete, no compaction.
	d.rows = nil
	before := len(d.queries)
	if n, err := s.Clear(context.Background(), history.Filter{Search: "nope"}); err != nil || n != 0 {
		t.Fatalf("no match: n=%d err=%v", n, err)
	}
	if got := len(d.queries) - before; got != 1 {
		t.Errorf("a clear with no match ran %d statements, want only the select", got)
	}
}
