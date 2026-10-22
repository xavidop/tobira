package libsql

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

var errClosed = errors.New("connection is closed")

// dsnConfig is a parsed libsql DSN.
type dsnConfig struct {
	url   string // http(s)://host[:port][/path], no trailing slash
	token string
	// plaintextToken: a token goes to a remote host over plain HTTP. Open
	// logs a warning.
	plaintextToken bool
}

// parseDSN reads libsql://, https://, http://, wss:// or ws:// URLs. The
// driver always speaks Hrana over HTTP: libsql:// means https:// (or http://
// with tls=0), wss:// means https:// and ws:// means http://, which sqld and
// Turso serve on the same endpoint. Plain HTTP to a host that is not
// loopback needs allow_plaintext=1 (whatever the scheme), and Turso
// (*.turso.io) is always TLS. The auth token comes from cfg.Password
// (a password ref) or the URL's password position (libsql://:TOKEN@host),
// where every redactor masks it; a token in a query parameter is refused,
// because query parameters are echoed unmasked.
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	sc := driver.Scheme(cfg.DSN)
	switch sc {
	case "libsql", "https", "http", "wss", "ws":
	case "file", "sqlite", "sqlite3", "":
		return fail("libsql needs a server URL (libsql://, https://, http://, wss:// or ws://); open local SQLite files with the sqlite driver")
	default:
		return fail("unsupported DSN scheme %q", sc)
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, token included
	}
	if u.Host == "" || u.Hostname() == "" {
		return fail("the DSN has no host")
	}
	if u.Fragment != "" || strings.Contains(cfg.DSN, "#") {
		return fail("the DSN must not have a fragment")
	}
	scheme := map[string]string{"libsql": "https", "https": "https", "http": "http", "wss": "https", "ws": "http"}[sc]
	allowPlain := false
	for k, vs := range u.Query() {
		switch strings.ToLower(k) {
		case "allow_plaintext":
			if len(vs) != 1 {
				return fail("repeated DSN parameter %q", k)
			}
			b, err := strconv.ParseBool(vs[0])
			if err != nil {
				return fail("allow_plaintext must be 0, 1, true or false")
			}
			allowPlain = b
		case "tls":
			if sc != "libsql" {
				return fail("tls applies only to libsql:// URLs; use http:// or https://")
			}
			if len(vs) != 1 {
				return fail("repeated DSN parameter %q", k)
			}
			on, err := strconv.ParseBool(vs[0])
			if err != nil {
				return fail("tls must be 0, 1, true or false")
			}
			if !on {
				scheme = "http"
			}
		case "authtoken", "auth_token", "jwt", "token":
			return fail("put the auth token in the password field (a password ref) or the URL's password position (libsql://:TOKEN@host), not in parameter %q", k)
		default:
			return fail("unknown DSN parameter %q (tls and allow_plaintext are accepted)", k)
		}
	}
	for k := range cfg.Options {
		return fail("unknown option %q: the libsql driver takes none", k)
	}
	d := dsnConfig{url: scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/")}
	if pw, ok := u.User.Password(); ok {
		d.token = pw
	}
	if !cfg.Password.IsZero() {
		d.token = cfg.Password.Reveal()
	}
	if scheme == "http" {
		switch netpolicy.CheckPlaintext(u.Hostname(), allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return fail("plain HTTP is refused for Turso (*.turso.io): use libsql:// or https://")
		case netpolicy.NeedsOptIn:
			return fail("plain HTTP to %s (not loopback) needs allow_plaintext=1: use https:// (or libsql://), or add allow_plaintext=1 to opt in", u.Hostname())
		}
	}
	d.plaintextToken = d.token != "" && scheme == "http" && !netpolicy.IsLoopback(u.Hostname())
	return d, nil
}

// cloudZones is Turso, which only speaks TLS.
var cloudZones = []string{"turso.io"}

// Open parses the DSN and pings the server once, so a bad address or a bad
// token fails here. Errors never contain the token.
func (libsqlDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	if d.plaintextToken {
		// The host's --debug log (ConnConfig.Logger; discarded when nil).
		cfg.Log().WarnContext(ctx, "libsql: the auth token is sent without TLS", "connection", cfg.Name, "url", d.url)
	}
	cl, err := newClient(d.url, d.token)
	if err != nil {
		return nil, redact(driver.Wrap(driver.ErrSyntax, Name, "open", errors.New("invalid DSN")), cfg, d.token)
	}
	c := &conn{client: cl, readOnly: cfg.ReadOnly}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		return nil, redact(err, cfg, d.token)
	}
	return c, nil
}

// redact masks the DSN, the password and the token in err's message, keeping
// its kind, code and context identity.
func redact(err error, cfg driver.ConnConfig, token string) error {
	if err == nil {
		return nil
	}
	msg := driver.RedactMessage(err.Error(), cfg)
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "xxxxx")
		msg = strings.ReplaceAll(msg, url.QueryEscape(token), "xxxxx")
	}
	out := &driver.Error{Kind: driver.KindOf(err), Driver: Name, Op: "open", Err: errors.New(msg)}
	var de *driver.Error
	if errors.As(err, &de) {
		out.Code = de.Code
		if de.Op != "" {
			out.Op = de.Op
		}
	}
	for _, c := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, c) {
			out.Err = errors.Join(out.Err, c)
			break
		}
	}
	return out
}
