package chroma

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

// Chroma's defaults when the DSN names no tenant or database and the
// server's identity does not say.
const (
	defaultTenant   = "default_tenant"
	defaultDatabase = "default_database"
)

// The headers a token may travel in: Chroma's own (Chroma Cloud and the
// token provider of self-hosted servers) or a bearer token.
const (
	headerChromaToken   = "x-chroma-token"
	headerAuthorization = "authorization"
)

// dsnConfig is a parsed chroma:// DSN.
type dsnConfig struct {
	scheme     string // http or https
	host       string
	port       int
	tenant     string // "" = from the server's identity, else default_tenant
	database   string // "" = from the identity when it lists one, else default_database
	token      string
	authHeader string
	// plaintextToken: a token goes to a remote host without TLS because
	// tls=false was given explicitly. Open logs a warning.
	plaintextToken bool
}

func (d dsnConfig) baseURL() string {
	return d.scheme + "://" + net.JoinHostPort(d.host, strconv.Itoa(d.port))
}

// Open builds the HTTP client, resolves the tenant and database, and reads
// the database once, so a bad address, token or database fails here rather
// than on the first query. Errors never contain the token.
func (chromaDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	if d.plaintextToken {
		// The host's --debug log (ConnConfig.Logger; discarded when nil).
		// The token itself is never logged.
		cfg.Log().WarnContext(ctx, "chroma: the token is sent without TLS because tls=false is set", "connection", cfg.Name, "host", d.host)
	}
	c := newConn(d, cfg)
	if err := c.resolve(ctx, d); err != nil {
		_ = c.Close()
		return nil, redact(err, cfg, d.token)
	}
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		return nil, redact(err, cfg, d.token)
	}
	return c, nil
}

// newConn creates the client without contacting the server. The tenant and
// database are the DSN's, or Chroma's defaults until resolve runs.
func newConn(d dsnConfig, cfg driver.ConnConfig) *conn {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment, // HTTPS_PROXY and friends; never for loopback
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	c := &conn{
		// No redirect is followed: Go forwards x-chroma-token to any host,
		// over plain http included, and the REST API never needs one.
		api:      &apiClient{base: d.baseURL(), token: d.token, header: d.authHeader, http: &http.Client{Transport: transport, CheckRedirect: netpolicy.RefuseRedirects}},
		tenant:   d.tenant,
		database: d.database,
		token:    d.token,
		readOnly: cfg.ReadOnly,
		embedder: cfg.Embedder,
	}
	if c.tenant == "" {
		c.tenant = defaultTenant
	}
	if c.database == "" {
		c.database = defaultDatabase
	}
	return c
}

// resolve fills the tenant and database the DSN left out from the
// server's identity: its tenant, and its database when it lists exactly
// one. A server without the identity endpoint keeps Chroma's defaults.
func (c *conn) resolve(ctx context.Context, d dsnConfig) error {
	if d.tenant != "" && d.database != "" {
		return nil
	}
	if err := c.check(ctx, "open"); err != nil {
		return err
	}
	id, err := c.api.identity(ctx)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			return nil
		}
		return c.fail(ctx, "open", err)
	}
	if d.tenant == "" && id.Tenant != "" {
		if !validName(id.Tenant) {
			return driver.Wrap(nil, Name, "open", fmt.Errorf("the server's identity names an invalid tenant %q", id.Tenant))
		}
		c.tenant = id.Tenant
	}
	if d.database == "" && len(id.Databases) == 1 && id.Databases[0] != "" {
		if !validName(id.Databases[0]) {
			return driver.Wrap(nil, Name, "open", fmt.Errorf("the server's identity names an invalid database %q", id.Databases[0]))
		}
		c.database = id.Databases[0]
	}
	return nil
}

// parseDSN reads
//
//	chroma://[:token@]host[:port][?tenant=…&database=…&tls=…&allow_plaintext=…&auth_header=…&token=…]
//
// The token sits in the password position or the token parameter, where
// every redactor masks it; a non-empty cfg.Password replaces it.
// ConnConfig.Options may set tenant, database, tls, allow_plaintext and
// auth_header. TLS is on unless the host is loopback or tls=false is given.
// Plain http to a host that is not loopback also needs allow_plaintext=1,
// and it is refused toward Chroma Cloud whatever the options.
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if driver.Scheme(cfg.DSN) != "chroma" {
		return fail("DSN must start with chroma://")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, token included
	}
	d := dsnConfig{host: u.Hostname(), authHeader: headerChromaToken}
	if d.host == "" {
		d.host = "localhost"
	}
	if u.Path != "" && u.Path != "/" {
		return fail("a chroma DSN has no path: name the database with ?database=")
	}
	if u.User != nil {
		if u.User.Username() != "" {
			return fail("a chroma DSN takes no user name: put the token in the password position (chroma://:TOKEN@host)")
		}
		d.token, _ = u.User.Password()
	}
	tls, tlsSet, allowPlain := false, false, false
	set := func(k, v, from string) error {
		switch strings.ToLower(k) {
		case "tenant", "database":
			if !validName(v) {
				return fmt.Errorf("invalid %s name %q", k, v)
			}
			if strings.ToLower(k) == "tenant" {
				d.tenant = v
			} else {
				d.database = v
			}
		case "tls":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("tls must be true or false")
			}
			tls, tlsSet = b, true
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("allow_plaintext must be true or false")
			}
			allowPlain = b
		case "auth_header":
			switch strings.ToLower(v) {
			case headerChromaToken, headerAuthorization:
				d.authHeader = strings.ToLower(v)
			default:
				return errors.New("auth_header must be x-chroma-token or authorization")
			}
		case "token":
			if from != "dsn" {
				return errors.New("put the token in the password field, not in options")
			}
			if d.token != "" {
				return errors.New("the token is given twice (password position and token parameter)")
			}
			d.token = v
		default:
			return fmt.Errorf("unknown %s %q (accepted: tenant, database, tls, allow_plaintext, auth_header, token)", map[string]string{"dsn": "DSN parameter", "options": "option"}[from], k)
		}
		return nil
	}
	for k, vs := range u.Query() {
		if err := set(k, vs[len(vs)-1], "dsn"); err != nil {
			return fail("%v", err)
		}
	}
	for k, v := range cfg.Options {
		if err := set(k, v, "options"); err != nil {
			return fail("%v", err)
		}
	}
	if !cfg.Password.IsZero() {
		d.token = cfg.Password.Reveal()
	}
	loopback := netpolicy.IsLoopback(d.host)
	if !tlsSet {
		tls = !loopback
	}
	if !tls {
		switch netpolicy.CheckPlaintext(d.host, allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return fail("tls=false is refused for Chroma Cloud (%s)", d.host)
		case netpolicy.NeedsOptIn:
			return fail("plain http to %s (not loopback) needs allow_plaintext=1 as well as tls=false", d.host)
		}
	}
	d.scheme, d.port = "http", 8000
	if tls {
		d.scheme, d.port = "https", 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fail("invalid port %q", p)
		}
		d.port = n
	}
	d.plaintextToken = d.token != "" && !tls && !loopback
	return d, nil
}

// validName accepts a tenant or database name that is safe in a URL path:
// not empty, not . or .., and without a path or query separator.
func validName(v string) bool {
	return v != "" && v != "." && v != ".." && !strings.ContainsAny(v, "/\\?#")
}

// cloudZones is Chroma Cloud, which only speaks TLS.
var cloudZones = []string{"trychroma.com"}

// redact masks the DSN, the password and the token in err's message,
// keeping its kind, code and context identity.
func redact(err error, cfg driver.ConnConfig, token string) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	var de *driver.Error
	if errors.As(err, &de) && de.Err != nil {
		text = de.Err.Error() // without the "chroma: op:" prefix, which out adds again
	}
	msg := maskToken(driver.RedactMessage(text, cfg), token)
	out := &driver.Error{Kind: driver.KindOf(err), Driver: Name, Op: "open", Err: errors.New(msg)}
	if de != nil {
		out.Code = de.Code
		if de.Op != "" {
			out.Op = de.Op
		}
		if ctxErr := contextCause(de.Err); ctxErr != nil {
			out.Err = errors.Join(out.Err, ctxErr)
		}
	}
	return out
}

// maskToken replaces the token, raw or URL-escaped, with xxxxx.
func maskToken(msg, token string) string {
	if token == "" {
		return msg
	}
	for _, s := range []string{token, url.QueryEscape(token), url.PathEscape(token)} {
		msg = strings.ReplaceAll(msg, s, "xxxxx")
	}
	return msg
}

// contextCause returns context.Canceled or context.DeadlineExceeded when err
// wraps one.
func contextCause(err error) error {
	for _, c := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, c) {
			return c
		}
	}
	return nil
}
