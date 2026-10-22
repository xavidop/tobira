package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	go_ora "github.com/sijms/go-ora/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

const (
	defaultPort = "1521"
	urlPrefix   = "oracle://"
)

// Open builds a connector from cfg and pings once, so a bad DSN or bad
// credentials fail here rather than on the first query. It reads the server
// version (which gates the read-only path) and the login schema.
func (oracleDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	dsn, err := buildURL(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	db := sql.OpenDB(go_ora.NewConnector(dsn))
	var version, user string
	err = db.PingContext(ctx)
	if err == nil {
		version, err = serverVersion(ctx, db)
	}
	if err == nil {
		err = db.QueryRowContext(ctx, "SELECT USER FROM DUAL").Scan(&user)
	}
	if err != nil {
		_ = db.Close()
		return nil, scrubError(mapError(ctx, "open", err), cfg)
	}
	if cfg.ReadOnly && !guardVerified(version) {
		_ = db.Close()
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "open", unverifiedServer(version))
	}
	c := &conn{db: db, readOnly: cfg.ReadOnly, version: version, user: user}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// serverVersion reads the database version, VERSION_FULL where the server
// has it (18c and later), VERSION otherwise.
func serverVersion(ctx context.Context, db *sql.DB) (string, error) {
	var v sql.NullString
	err := db.QueryRowContext(ctx, "SELECT MAX(version_full) FROM product_component_version WHERE product LIKE 'Oracle%'").Scan(&v)
	if err != nil {
		err = db.QueryRowContext(ctx, "SELECT MAX(version) FROM product_component_version WHERE product LIKE 'Oracle%'").Scan(&v)
	}
	return v.String, err
}

// guardVerified reports whether the read-only guard's lexer applies to this
// server version: Oracle 12c to 23ai (version 23.x, including the 23.26
// "26ai" releases). Q-quoting and the identifier rules date from 10g and the
// WITH FUNCTION clause from 12c; a later major version could add syntax the
// guard does not know, so it is refused until verified.
func guardVerified(version string) bool {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 12 && n <= 23
}

func unverifiedServer(version string) error {
	return fmt.Errorf("read-only access needs Oracle Database 12c to 23ai: the read-only guard is not verified on server version %q", version)
}

// descriptorPassword finds a PASSWORD entry in a connect descriptor.
var descriptorPassword = regexp.MustCompile(`(?i)\(\s*PASSWORD\s*=`)

// buildURL turns cfg into a go-ora URL without connecting. It accepts
// oracle://user[:password]@host[:port]/service?options, go-ora's own form
// (the port defaults to 1521; options are go-ora's, such as SSL, WALLET,
// PREFETCH_ROWS or connStr), then enforces what tobira relies on:
//
//   - ConnConfig.Options are refused: settings come from DSN options;
//   - TRACE FILE and TRACE DIR are refused: go-ora would write the wire
//     protocol, credentials included, to disk;
//   - a PASSWORD inside connStr is refused: go-ora ignores it and forwards
//     the descriptor to the listener as is;
//   - LOB FETCH must stay INLINE (the default), so LOB values arrive with
//     their row;
//   - a non-empty ConnConfig.Password replaces the DSN's password.
func buildURL(cfg driver.ConnConfig) (string, error) {
	fail := func(kind error, err error) (string, error) {
		return "", driver.Wrap(kind, Name, "open", err)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the oracle driver takes its settings from DSN options", k))
	}
	dsn := cfg.DSN
	if len(dsn) < len(urlPrefix) || !strings.EqualFold(dsn[:len(urlPrefix)], urlPrefix) {
		if s := driver.Scheme(dsn); s != "" {
			return fail(driver.ErrSyntax, fmt.Errorf("unsupported DSN scheme %q: use oracle://user@host:port/service", s))
		}
		return fail(driver.ErrSyntax, errors.New("invalid DSN: use oracle://user@host:port/service"))
	}
	u, err := url.Parse(urlPrefix + dsn[len(urlPrefix):])
	if err != nil {
		return fail(driver.ErrSyntax, errors.New("invalid DSN: malformed URL"))
	}
	if u.Hostname() == "" {
		return fail(driver.ErrSyntax, errors.New("invalid DSN: no host"))
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: invalid port %q", port))
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	q := u.Query()
	for k, vs := range q {
		key := strings.ToUpper(strings.Join(strings.Fields(k), " "))
		switch {
		case strings.HasPrefix(key, "TRACE "):
			return fail(driver.ErrUnsupported, fmt.Errorf("DSN option %q is not supported: go-ora would write the wire protocol, credentials included, to disk", k))
		case key == "CONNSTR":
			for _, v := range vs {
				if descriptorPassword.MatchString(v) {
					return fail(driver.ErrUnsupported, errors.New("a PASSWORD in connStr is not supported: go-ora would send it to the listener in plain text; put the password in the URL or a secret ref"))
				}
			}
		case key == "LOB FETCH":
			for _, v := range vs {
				if l := strings.ToUpper(strings.TrimSpace(v)); l != "INLINE" && l != "PRE" {
					return fail(driver.ErrUnsupported, fmt.Errorf("LOB FETCH=%s is not supported: tobira reads LOBs inline", v))
				}
			}
		}
	}
	if !cfg.Password.IsZero() {
		u.User = url.UserPassword(u.User.Username(), cfg.Password.Reveal())
	}
	out := u.String()
	if _, err := go_ora.ParseConfig(out); err != nil {
		return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: %w", err))
	}
	return out, nil
}

// scrubError masks every password of cfg in the message of a *driver.Error,
// keeping its kind, code and the original error (through Unwrap).
func scrubError(err error, cfg driver.ConnConfig) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := de.Err.Error()
	if clean := driver.RedactMessage(msg, cfg); clean != msg {
		de.Err = maskedError{msg: clean, err: de.Err}
	}
	return err
}

// maskedError shows a masked message but keeps the original error reachable.
type maskedError struct {
	msg string
	err error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.err }
