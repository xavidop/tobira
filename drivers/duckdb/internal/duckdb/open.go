package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// lockedConfig starts every instance the plugin opens, read-write ones
// included: no file system or network access from SQL, no extension
// auto-install or auto-load, and every setting locked so none of this can be
// undone. The engine, not only the lexical guard, then stops a query from
// reading a file or URL. A writable DSN may opt out with external_access=1
// (see externalConfig).
const lockedConfig = "enable_external_access=false" +
	"&autoinstall_known_extensions=false&autoload_known_extensions=false&lock_configuration=true"

// readOnlyConfig opens a dedicated read-only instance: lockedConfig with
// the file opened read-only.
const readOnlyConfig = "access_mode=READ_ONLY&" + lockedConfig

// externalConfig is a writable instance that opted in to external access
// (external_access=1): DuckDB's defaults, unlocked. Read-only queries are
// refused on it (errExternalAccess).
const externalConfig = ""

// lockedSettings are what every locked instance must report once open;
// anything else and Open fails closed.
var lockedSettings = map[string]string{
	"enable_external_access":       "false",
	"autoinstall_known_extensions": "false",
	"autoload_known_extensions":    "false",
	"lock_configuration":           "true",
}

// errExternalAccess refuses a read-only query on an instance that allows
// external access: the engine would no longer stop it reading files or URLs.
var errExternalAccess = errors.New("this connection allows external access (external_access=1), so read-only queries are refused: open a separate read-only connection")

func refuseExternal(op string) error {
	return driver.Wrap(driver.ErrReadOnly, Name, op, errExternalAccess)
}

// target is a parsed DSN.
type target struct {
	path     string // absolute; empty for :memory:
	memory   bool
	readOnly bool // access_mode=read_only in the DSN
	external bool // external_access=1 in the DSN
}

// parseDSN reads duckdb:///abs/path.db, duckdb://rel/path.db,
// duckdb:rel.db, a bare path, or :memory: in any of those forms. The only
// parameter is access_mode=read_only|read_write; any other is refused,
// since configuration in the DSN could undo the read-only layers. Paths
// with '#', '%' or NUL are refused: duckdb-go parses its DSN as a URL and
// would drop or decode part of it.
func parseDSN(dsn string) (target, error) {
	var t target
	fail := func(kind error, msg string) (target, error) {
		return target{}, driver.Wrap(kind, Name, "open", errors.New(msg))
	}
	rest := dsn
	switch sc := driver.Scheme(dsn); sc {
	case Name:
		rest = strings.TrimPrefix(dsn[len(sc)+1:], "//")
	case "":
	default:
		return fail(driver.ErrSyntax, "unsupported DSN scheme "+sc+": want duckdb:")
	}
	path, query, hasQuery := strings.Cut(rest, "?")
	if hasQuery {
		params, err := url.ParseQuery(query)
		if err != nil {
			return fail(driver.ErrSyntax, "invalid DSN parameters")
		}
		for _, k := range slices.Sorted(maps.Keys(params)) {
			if k != "access_mode" && k != "external_access" {
				return fail(driver.ErrUnsupported, "unsupported DSN parameter "+safeName(k)+": only access_mode and external_access are accepted")
			}
			if len(params[k]) > 1 {
				return fail(driver.ErrUnsupported, "repeated DSN parameter "+k)
			}
			switch v := strings.ToLower(params.Get(k)); {
			case k == "access_mode" && v == "read_only":
				t.readOnly = true
			case k == "access_mode" && v == "read_write":
			case k == "access_mode":
				return fail(driver.ErrUnsupported, "access_mode must be read_only or read_write")
			case v == "1" || v == "true":
				t.external = true
			case v == "0" || v == "false":
			default:
				return fail(driver.ErrUnsupported, "external_access must be 1 or 0")
			}
		}
		if t.readOnly && t.external {
			return fail(driver.ErrUnsupported, "a read-only connection cannot allow external access")
		}
	}
	if path == ":memory:" {
		if hasQuery {
			return fail(driver.ErrUnsupported, "an in-memory database takes no parameters")
		}
		t.memory = true
		return t, nil
	}
	if path == "" {
		return fail(driver.ErrSyntax, "missing database path: want duckdb:///path/to/file.duckdb or duckdb://:memory:")
	}
	if strings.ContainsAny(path, "#%\x00") {
		return fail(driver.ErrUnsupported, "the database path may not contain '#', '%' or NUL")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail(driver.ErrSyntax, "invalid database path")
	}
	// One spelling per file, so the process never holds two instances of
	// it under different names.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	t.path = abs
	return t, nil
}

// safeName quotes a DSN parameter name for an error, cut short; values are
// never echoed.
func safeName(k string) string {
	if len(k) > 40 {
		k = k[:40] + "…"
	}
	return `"` + strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, k) + `"`
}

// Open opens the database cfg.DSN names. Every instance starts locked
// (lockedConfig) unless a writable DSN opts in with external_access=1. A
// read-only conn (cfg.ReadOnly or access_mode=read_only) gets a dedicated
// read-only instance, or shares the instance this process already holds for
// the file (see conn).
func (duckDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	t, err := parseDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	readOnly := cfg.ReadOnly || t.readOnly
	if readOnly && t.external {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "open", errors.New("a read-only connection cannot allow external access"))
	}
	c := &conn{readOnly: readOnly, external: t.external}
	writable := lockedConfig
	if t.external {
		writable = externalConfig
	}
	switch {
	case t.memory && readOnly:
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "open", errors.New("a read-only in-memory database would always be empty"))
	case t.memory:
		c.db, err = openDB(ctx, ":memory:", writable, !t.external)
	case readOnly:
		if _, statErr := os.Stat(t.path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, driver.Wrap(driver.ErrNotFound, Name, "open", errors.New("database file not found: "+driver.RedactDSN(t.path)))
			}
			return nil, driver.Wrap(driver.ErrConnection, Name, "open", errors.New("cannot access "+driver.RedactDSN(t.path)))
		}
		err = c.openReadOnly(ctx, t.path)
	default:
		c.db, err = openDB(ctx, t.path, writable, !t.external)
		if isConfigConflict(err) {
			err = driver.Wrap(driver.ErrConflict, Name, "open", errors.New(
				"the database file is open in this process with another configuration (a read-only connection, or one with a different external_access), "+
					"and DuckDB allows one configuration per file: close the other connection first"))
		}
	}
	if err != nil {
		return nil, mapError("open", err)
	}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// openReadOnly opens the dedicated read-only instance of path. When this
// process already holds the file with another configuration, it shares
// that instance instead, and marks the conn shared: the locked writable
// instance, whose engine still stops file and URL reads, or one that opted
// in to external access (c.external), on which every query is refused.
//
// The read-only open is retried once before sharing, in case the other
// instance closed in between; if it closes after that, the shared open
// creates a locked writable instance for this read-only conn, which is
// still marked shared and still runs every statement in a READ ONLY
// transaction.
func (c *conn) openReadOnly(ctx context.Context, path string) error {
	var err error
	for range 2 {
		if c.db, err = openDB(ctx, path, readOnlyConfig, true); !isConfigConflict(err) {
			return err
		}
	}
	c.shared = true
	if c.db, err = openDB(ctx, path, lockedConfig, true); !isConfigConflict(err) {
		return err
	}
	if c.db, err = openDB(ctx, path, externalConfig, false); !isConfigConflict(err) {
		c.external = true
		return err
	}
	return driver.Wrap(driver.ErrConflict, Name, "open", errors.New("the database file is open in this process with a configuration a read-only connection cannot share"))
}

// openDB opens a pool over the DuckDB instance of path with config (DSN
// parameters), checks it answers, and with locked verifies the instance
// really has every lockedSettings value (and access_mode for a read-only
// config), so a configuration DuckDB silently ignored fails closed.
func openDB(ctx context.Context, path, config string, locked bool) (*sql.DB, error) {
	dsn := path
	if config != "" {
		dsn += "?" + config
	}
	connector, err := duckdb.NewConnector(dsn, nil)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if locked {
		want := maps.Clone(lockedSettings)
		if strings.HasPrefix(config, "access_mode=READ_ONLY") {
			want["access_mode"] = "read_only"
		}
		if err := verifySettings(ctx, db, want); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

// isConfigConflict reports DuckDB's refusal to open a second instance of a
// file with another configuration. It matches DuckDB's English error text
// (1.5.6): a rewording in a DuckDB upgrade turns the shared fallback into a
// hard error, which TestReadOnlySharesReadWriteInstance would catch.
func isConfigConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "different configuration than existing connections")
}

// verifySettings checks each setting of want against the instance.
func verifySettings(ctx context.Context, db *sql.DB, want map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(want)) {
		var got string
		if err := db.QueryRowContext(ctx, "SELECT lower(current_setting(?)::VARCHAR)", name).Scan(&got); err != nil {
			return err
		}
		if got != want[name] {
			return driver.Wrap(driver.ErrConnection, Name, "open", errors.New("the instance reports "+name+"="+got+", want "+want[name]))
		}
	}
	return nil
}
