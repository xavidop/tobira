package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

var errClosed = errors.New("connection is closed")

// dsnOptions are the settings parsed from a DSN's query string.
type dsnOptions struct{ readOnly bool }

// parseDSN maps a sqlite:, sqlite3: or file: DSN (or a bare path) to an
// absolute filesystem path. The query string is split off for every scheme:
// only mode=ro|rw|rwc is understood; mode=memory and any other parameter are
// ErrUnsupported. In-memory databases are unsupported because the read-only
// guard needs a second handle onto the same database.
func parseDSN(dsn string) (string, dsnOptions, error) {
	var opts dsnOptions
	fail := func(kind error, msg string) (string, dsnOptions, error) {
		return "", opts, driver.Wrap(kind, Name, "open", errors.New(msg))
	}
	rest, isFile := dsn, false
	switch sc := driver.Scheme(dsn); sc {
	case "sqlite", "sqlite3":
		rest = dsn[len(sc)+1:]
	case "file":
		rest, isFile = dsn[len("file:"):], true
	case "":
	default:
		return fail(driver.ErrSyntax, "unsupported DSN scheme "+strconv.Quote(sc))
	}
	rest = strings.TrimPrefix(rest, "//")
	path, query, hasQuery := strings.Cut(rest, "?")
	if hasQuery {
		params, err := url.ParseQuery(query)
		if err != nil {
			return fail(driver.ErrSyntax, "invalid DSN parameters")
		}
		for _, k := range slices.Sorted(maps.Keys(params)) {
			if len(params[k]) > 1 {
				return fail(driver.ErrUnsupported, "repeated DSN parameter "+strconv.Quote(k))
			}
			if k != "mode" {
				return fail(driver.ErrUnsupported, "unsupported DSN parameter "+strconv.Quote(k))
			}
			switch m := params.Get(k); m {
			case "ro":
				opts.readOnly = true
			case "rw", "rwc":
			case "memory":
				path = ":memory:"
			default:
				return fail(driver.ErrUnsupported, "unsupported DSN parameter mode="+strconv.Quote(m))
			}
		}
	}
	if isFile {
		if p, err := url.PathUnescape(path); err == nil {
			path = p
		}
	}
	if path == "" {
		return fail(driver.ErrSyntax, "missing database path")
	}
	if strings.HasPrefix(path, ":memory:") {
		return fail(driver.ErrUnsupported, "in-memory databases are not supported: read-only guard needs a second handle")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail(driver.ErrSyntax, "invalid database path")
	}
	return abs, opts, nil
}

var uriEscaper = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

func fileURI(abs, params string) string {
	return "file:" + uriEscaper.Replace(abs) + "?" + params
}

// Open opens the database named by cfg.DSN. The read-write handle is omitted
// when cfg.ReadOnly; the read-only handle always exists.
func (sqliteDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	abs, dsnOpts, err := parseDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	readOnly := cfg.ReadOnly || dsnOpts.readOnly
	redacted := driver.RedactDSN(abs)
	busy := "5000"
	if v, err := strconv.Atoi(cfg.Options["busy_timeout"]); err == nil && v >= 0 {
		busy = strconv.Itoa(v)
	}
	c := &conn{path: abs}
	if readOnly {
		if _, err := os.Stat(abs); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, driver.Wrap(driver.ErrNotFound, Name, "open", errors.New("database file not found: "+redacted))
			}
			return nil, driver.Wrap(driver.ErrConnection, Name, "open", errors.New("cannot access "+redacted))
		}
	} else {
		rw, err := sql.Open("sqlite", fileURI(abs, "_pragma=busy_timeout("+busy+")&_pragma=foreign_keys(1)"))
		if err != nil {
			return nil, mapError("open", err)
		}
		if err := rw.PingContext(ctx); err != nil { // creates the file when missing
			_ = rw.Close()
			return nil, mapError("open", err)
		}
		c.rw = rw
	}
	ro, err := sql.Open("sqlite", fileURI(abs, "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout("+busy+")"))
	if err == nil {
		err = ro.PingContext(ctx)
	}
	if err != nil {
		if ro != nil {
			_ = ro.Close()
		}
		if c.rw != nil {
			_ = c.rw.Close()
		}
		return nil, mapError("open", err)
	}
	c.ro = ro
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}
