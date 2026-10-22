package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

const (
	defaultPort    = "3306"
	readOnlyVar    = "transaction_read_only"
	timeZoneVar    = "time_zone"
	utcTimeZone    = "'+00:00'"
	socketParam    = "socket"
	multiStmtKey   = "multiStatements"
	interpolateKey = "interpolateParams"
)

// Open builds a connector from cfg and pings once, so a bad DSN or bad
// credentials fail here rather than on the first query.
func (d mysqlDriver) Open(ctx context.Context, cfg driver.ConnConfig) (_ driver.Conn, err error) {
	defer func() { err = (&conn{name: d.Info().Name}).named(err) }()
	mc, forcedFlavor, forced, err := d.config(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	connector, err := gomysql.NewConnector(mc)
	if err != nil {
		return nil, scrubError(driver.Wrap(driver.ErrSyntax, Name, "open", err), cfg)
	}
	db := sql.OpenDB(connector)
	var version string
	detected := flavorMySQL
	err = db.PingContext(ctx)
	if err != nil && cfg.ReadOnly {
		if refusal := readOnlyVarRefusal(err); refusal != nil {
			_ = db.Close()
			return nil, scrubError(refusal, cfg)
		}
	}
	if err == nil {
		version, detected, err = detectFlavor(ctx, db, cfg.ReadOnly, cfg.Log())
	}
	if err != nil {
		_ = db.Close()
		return nil, scrubError(mapError(ctx, "open", err), cfg)
	}
	f := detected
	if forced {
		f = forcedFlavor
	}
	if err := checkServer(version, detected, f, cfg.ReadOnly); err != nil {
		_ = db.Close()
		return nil, err
	}
	c := &conn{db: db, readOnly: cfg.ReadOnly, version: version, detected: detected, flavor: f, name: d.Info().Name}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// guardVerified reports whether the read-only guard's lexer was verified
// against this MySQL server: MySQL 8.0 to 8.4 (Percona Server included).
// MySQL 9 and later, anything older or unparsable, and MariaDB versions (see
// mariaDBVerified) are not.
func guardVerified(version string) bool {
	if strings.Contains(strings.ToLower(version), "mariadb") {
		return false
	}
	major, rest, ok := strings.Cut(version, ".")
	if !ok || major != "8" {
		return false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	minor, err := strconv.Atoi(rest[:end])
	return err == nil && minor <= 4
}

// checkServer refuses a read-only connection to a server the read-only
// guard was not verified against (see readOnlyRefusal). Writable connections
// work on any server.
func checkServer(version string, detected, f flavor, readOnly bool) error {
	if !readOnly {
		return nil
	}
	if err := readOnlyRefusal(version, detected, f); err != nil {
		return driver.Wrap(driver.ErrUnsupported, Name, "open", err)
	}
	return nil
}

func unverifiedServer(version string) error {
	return fmt.Errorf("read-only access needs MySQL 8.0 to 8.4: the read-only guard is not verified on server version %q", version)
}

// buildConfig turns cfg into a go-sql-driver config without connecting. It
// accepts mysql://user[:password]@host[:port]/db?params and go-sql-driver's
// own user[:password]@net(addr)/db?params, then enforces what tobira relies
// on:
//
//   - multiStatements must be off (one statement per query);
//   - parseTime=true and loc=UTC, with the session time_zone at '+00:00', so
//     DATETIME and TIMESTAMP values come back as UTC times;
//   - a read-only connection sets transaction_read_only=1 on every session
//     (whatever the DSN said), the second layer of the read-only guard;
//   - interpolateParams must be off, so the server runs the text the
//     read-only guard checked;
//   - the client charset is utf8mb4 (see pinCharset);
//   - go-sql-driver never logs (it would write to the terminal);
//   - a non-empty ConnConfig.Password replaces the DSN's password.
func buildConfig(cfg driver.ConnConfig) (*gomysql.Config, error) {
	mc, _, _, err := parseConfig(cfg)
	return mc, err
}

// config is parseConfig for this driver: the mariadb and singlestore
// drivers force their flavor, and a DSN that names another one is
// ErrUnsupported.
func (d mysqlDriver) config(cfg driver.ConnConfig) (*gomysql.Config, flavor, bool, error) {
	mc, f, forced, err := parseConfig(cfg)
	if err != nil || !d.flavored {
		return mc, f, forced, err
	}
	if forced && f != d.flavor {
		return nil, flavorMySQL, false, driver.Wrap(driver.ErrUnsupported, Name, "open",
			fmt.Errorf("the DSN names flavor %s, but this is the %s driver", f, d.flavor))
	}
	return mc, d.flavor, true, nil
}

// urlSchemes are the URL forms the driver accepts. mariadb:// and
// singlestore:// force their flavor, as flavor= would; mysql:// detects it.
var urlSchemes = []struct {
	prefix string
	flavor flavor
	forced bool
}{
	{"mysql://", flavorMySQL, false},
	{"mariadb://", flavorMariaDB, true},
	{"singlestore://", flavorSingleStore, true},
}

// parseConfig is buildConfig, plus the flavor the DSN forces: forced is
// false when the flavor is left to detection. The flavor= parameter is
// removed from the session variables.
func parseConfig(cfg driver.ConnConfig) (mc *gomysql.Config, f flavor, forced bool, err error) {
	fail := func(kind error, err error) (*gomysql.Config, flavor, bool, error) {
		return nil, flavorMySQL, false, driver.Wrap(kind, Name, "open", err)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the mysql driver takes its settings from DSN parameters", k))
	}
	var rawQuery string
	dsn := cfg.DSN
	parsed := false
	for _, s := range urlSchemes {
		if len(dsn) >= len(s.prefix) && strings.EqualFold(dsn[:len(s.prefix)], s.prefix) {
			mc, rawQuery, err = parseURL(dsn[len(s.prefix):])
			f, forced, parsed = s.flavor, s.forced, true
			break
		}
	}
	switch {
	case parsed:
	case driver.Scheme(dsn) != "" && strings.HasPrefix(dsn[len(driver.Scheme(dsn)):], "://"):
		return fail(driver.ErrSyntax, fmt.Errorf("unsupported DSN scheme %q", driver.Scheme(dsn)))
	default:
		mc, err = gomysql.ParseDSN(dsn)
		if slash := strings.LastIndexByte(dsn, '/'); slash >= 0 {
			_, rawQuery, _ = strings.Cut(dsn[slash+1:], "?")
		}
	}
	if err != nil {
		var de *driver.Error
		if errors.As(err, &de) {
			return nil, flavorMySQL, false, err
		}
		return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: %w", err))
	}
	if mc.Params == nil {
		mc.Params = map[string]string{}
	}
	if f, forced, err = paramFlavor(mc.Params, f, forced); err != nil {
		return fail(driver.ErrUnsupported, err)
	}
	for k, v := range mc.Params { // unknown keys are system variables; catch other spellings
		if strings.EqualFold(k, multiStmtKey) {
			if truthy(v) {
				mc.MultiStatements = true
			}
			delete(mc.Params, k)
		}
	}
	if mc.MultiStatements {
		return fail(driver.ErrUnsupported, errors.New("multiStatements=true is not supported: tobira runs one statement per query"))
	}
	for k, v := range mc.Params {
		if strings.EqualFold(k, interpolateKey) {
			if truthy(v) {
				mc.InterpolateParams = true
			}
			delete(mc.Params, k)
		}
	}
	if mc.InterpolateParams {
		return fail(driver.ErrUnsupported, errors.New("interpolateParams=true is not supported: the server must see exactly the text the read-only guard checked"))
	}
	if err = pinCharset(mc, rawQuery); err != nil {
		return fail(driver.ErrUnsupported, err)
	}
	mc.ParseTime = true
	mc.Loc = time.UTC
	setParam(mc.Params, timeZoneVar, utcTimeZone)
	if cfg.ReadOnly {
		deleteParam(mc.Params, "tx_read_only")
		setParam(mc.Params, readOnlyVar, "1")
	}
	mc.Logger = &gomysql.NopLogger{}
	if !cfg.Password.IsZero() {
		mc.Passwd = cfg.Password.Reveal()
	}
	return mc, f, forced, nil
}

// paramFlavor removes every spelling of the flavor= parameter from params
// and applies it on top of the flavor the scheme gave (f, forced). Spellings
// that disagree, an unknown value, or a value that contradicts a mariadb://
// or singlestore:// scheme are refused.
func paramFlavor(params map[string]string, f flavor, forced bool) (flavor, bool, error) {
	var value string
	seen := false
	for k, v := range params {
		if !strings.EqualFold(k, flavorParam) {
			continue
		}
		delete(params, k)
		if seen && !strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(value)) {
			return f, forced, errors.New("the flavor parameter is given twice with different values")
		}
		value, seen = v, true
	}
	if !seen {
		return f, forced, nil
	}
	pf, err := parseFlavor(value)
	if err != nil {
		return f, forced, err
	}
	if forced && pf != f {
		return f, forced, fmt.Errorf("flavor=%s contradicts the %s:// scheme", pf, f)
	}
	return pf, true, nil
}

// pinCharset makes every session use the utf8mb4 client charset. The
// read-only guard lexes text as ASCII-transparent bytes; under gbk, big5,
// sjis and similar charsets a 0x5c byte can be the second half of a
// character, and the server would end a string where the guard does not. So
// a DSN that asks for another client charset is refused: charset other than
// utf8mb4, a collation outside utf8mb4, and the names, charset,
// character_set_* and collation_* system variables. rawQuery is the DSN's
// query string, where go-sql-driver's charset parameter is visible.
func pinCharset(mc *gomysql.Config, rawQuery string) error {
	refuse := func(what string) error {
		return fmt.Errorf("%s is not supported: tobira pins the client charset to utf8mb4", what)
	}
	for kv := range strings.SplitSeq(rawQuery, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k != "charset" {
			continue
		}
		if u, err := url.QueryUnescape(v); err == nil {
			v = u
		}
		for cs := range strings.SplitSeq(v, ",") {
			if !strings.EqualFold(strings.TrimSpace(cs), "utf8mb4") {
				return refuse("charset=" + v)
			}
		}
	}
	if mc.Collation != "" && !strings.HasPrefix(strings.ToLower(mc.Collation), "utf8mb4_") {
		return refuse("collation=" + mc.Collation)
	}
	for k := range mc.Params {
		if l := strings.ToLower(k); l == "names" || l == "charset" || strings.HasPrefix(l, "character_set_") || strings.HasPrefix(l, "collation_") {
			return refuse("DSN parameter " + strconv.Quote(k))
		}
	}
	return mc.Apply(gomysql.Charset("utf8mb4", mc.Collation))
}

// truthy reports whether a DSN boolean is on, leniently: anything but an
// explicit false is on, so an odd spelling cannot sneak a setting through.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// deleteParam removes every case-insensitive spelling of a system variable.
func deleteParam(params map[string]string, name string) {
	for k := range params {
		if strings.EqualFold(k, name) {
			delete(params, k)
		}
	}
}

// setParam sets a system variable, replacing every other spelling of it.
func setParam(params map[string]string, name, value string) {
	deleteParam(params, name)
	params[name] = value
}

// parseURL parses the part of a mysql:// URL after the scheme. The userinfo
// runs to the last '@' before any '?', so a password may hold unescaped '@',
// '/' and ':' (a '?' must be escaped). The query string holds go-sql-driver
// parameters, plus socket=<path> for a Unix socket.
func parseURL(rest string) (*gomysql.Config, string, error) {
	bad := func(msg string) (*gomysql.Config, string, error) {
		return nil, "", driver.Wrap(driver.ErrSyntax, Name, "open", errors.New("invalid DSN: "+msg))
	}
	var userinfo string
	hasUser := false
	at := strings.LastIndexByte(rest, '@')
	if q := strings.IndexByte(rest, '?'); q >= 0 && q < at {
		at = strings.LastIndexByte(rest[:q], '@')
	}
	if at >= 0 {
		userinfo, rest, hasUser = rest[:at], rest[at+1:], true
	}
	hostPath, rawQuery, _ := strings.Cut(rest, "?")
	host, path, _ := strings.Cut(hostPath, "/")
	params, err := url.ParseQuery(rawQuery)
	if err != nil {
		return bad("malformed query parameters")
	}
	netw, addr := "tcp", ""
	if sock := params.Get(socketParam); sock != "" {
		netw, addr = "unix", sock
	} else if addr, err = hostAddr(host); err != nil {
		return bad(err.Error())
	}
	params.Del(socketParam)
	native := netw + "(" + addr + ")/"
	if enc := params.Encode(); enc != "" {
		native += "?" + enc
	}
	mc, err := gomysql.ParseDSN(native)
	if err != nil {
		return nil, "", err
	}
	if hasUser {
		user, pass, _ := strings.Cut(userinfo, ":")
		if mc.User, err = url.PathUnescape(user); err != nil {
			return bad("malformed user name")
		}
		if mc.Passwd, err = url.PathUnescape(pass); err != nil {
			return bad("malformed password")
		}
	}
	if mc.DBName, err = url.PathUnescape(path); err != nil {
		return bad("malformed database name")
	}
	return mc, params.Encode(), nil
}

// hostAddr turns a URL host ("h", "h:3307", "[::1]:3307", "") into host:port.
func hostAddr(host string) (string, error) {
	if host == "" {
		return net.JoinHostPort("127.0.0.1", defaultPort), nil
	}
	h, port, err := net.SplitHostPort(host)
	if err != nil { // no port
		h, port = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return "", fmt.Errorf("invalid port %q", port)
	}
	return net.JoinHostPort(h, port), nil
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
