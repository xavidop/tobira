package vertica

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

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// dsnConfig is a parsed DSN:
//
//	vertica://user@host[:port][/database][?tlsmode=…&backup_server_node=…&connection_load_balance=…&workload=…&client_label=…&auth=oauth]
type dsnConfig struct {
	user, host, database string
	port                 int
	tlsMode              string // none, prefer (loopback default), server or server-strict (default off loopback)
	backups              string // backup_server_node: host[:port],…
	loadBalance          bool
	workload, label      string
	oauth                bool // the secret is an OAuth access token
}

const defaultPort = 5433

var (
	errMalformedDSN = errors.New("malformed DSN: want vertica://user@host[:port][/database][?tlsmode=…]")
	// errDSNCredentials refuses a DSN that carries a credential. It never
	// quotes the DSN.
	errDSNCredentials = errors.New("credentials must not be in the DSN: put the password or OAuth token in the connection's password (a secret ref)")
)

// dsnParams are the DSN parameters the driver reads. Anything else is
// refused, vertica-sql-go's own included: tobira always uses server-side
// prepared statements and autocommit, and a DSN cannot name a custom TLS
// config, a TOTP code or a token.
var dsnParams = []string{"tlsmode", "backup_server_node", "connection_load_balance", "workload", "client_label", "auth", "allow_plaintext"}

var tlsModes = []string{"none", "prefer", "server", "server-strict"}

// parseDSN parses a vertica:// DSN. Errors never quote the DSN, which
// could hold a pasted credential.
func parseDSN(dsn string) (dsnConfig, error) {
	const scheme = "vertica://"
	if len(dsn) < len(scheme) || !strings.EqualFold(dsn[:len(scheme)], scheme) {
		return dsnConfig{}, errMalformedDSN
	}
	rest := dsn[len(scheme):]
	authority, _, _ := strings.Cut(rest, "/")
	authority, _, _ = strings.Cut(authority, "?")
	if userinfo, _, ok := strings.Cut(authority, "@"); ok && strings.Contains(userinfo, ":") {
		return dsnConfig{}, errDSNCredentials
	}
	u, err := url.Parse(scheme + rest)
	if err != nil { // url errors quote the input
		return dsnConfig{}, errMalformedDSN
	}
	if _, has := u.User.Password(); has {
		return dsnConfig{}, errDSNCredentials
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return dsnConfig{}, errMalformedDSN
	}
	for k := range query {
		if !slices.Contains(dsnParams, strings.ToLower(k)) {
			if credentialKey(k) {
				return dsnConfig{}, errDSNCredentials
			}
			// Not quoted: a stray parameter could be a pasted secret.
			return dsnConfig{}, errors.New("malformed DSN: unknown parameter (known: " + strings.Join(dsnParams, ", ") + ")")
		}
	}
	if u.Hostname() == "" || u.Fragment != "" || strings.HasSuffix(dsn, "#") || u.Opaque != "" {
		return dsnConfig{}, errMalformedDSN
	}
	d := dsnConfig{user: u.User.Username(), host: u.Hostname(), port: defaultPort, tlsMode: "prefer"}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return dsnConfig{}, errors.New("malformed DSN: the port must be 1-65535")
		}
		d.port = n
	}
	if path := strings.TrimPrefix(u.Path, "/"); path != "" {
		if strings.Contains(path, "/") {
			return dsnConfig{}, errors.New("malformed DSN: the path is /database")
		}
		d.database = path
	}
	tlsSet, allowPlain := false, false
	for k, vs := range query {
		name := strings.ToLower(k)
		if len(vs) != 1 || vs[0] == "" {
			return dsnConfig{}, errors.New("malformed DSN: " + name + " must have one non-empty value")
		}
		v := vs[0]
		switch name {
		case "tlsmode":
			if !slices.Contains(tlsModes, strings.ToLower(v)) {
				return dsnConfig{}, errors.New("malformed DSN: tlsmode is none, prefer, server or server-strict")
			}
			d.tlsMode, tlsSet = strings.ToLower(v), true
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return dsnConfig{}, errors.New("malformed DSN: allow_plaintext must be true or false")
			}
			allowPlain = b
		case "backup_server_node":
			for _, hp := range strings.Split(v, ",") {
				if hp == "" || strings.ContainsAny(hp, "/@?#% \t") {
					return dsnConfig{}, errors.New("malformed DSN: backup_server_node is host[:port],…")
				}
			}
			d.backups = v
		case "connection_load_balance":
			if v != "0" && v != "1" {
				return dsnConfig{}, errors.New("malformed DSN: connection_load_balance is 0 or 1")
			}
			d.loadBalance = v == "1"
		case "workload":
			d.workload = v
		case "client_label":
			d.label = v
		case "auth":
			if !strings.EqualFold(v, "oauth") {
				return dsnConfig{}, driver.Wrap(driver.ErrUnsupported, Name, "open", errors.New("unsupported auth: leave it out for a password, or use auth=oauth for an OAuth access token"))
			}
			d.oauth = true
		}
	}
	if d.user == "" && !d.oauth {
		return dsnConfig{}, errors.New("malformed DSN: a user is required (vertica://user@host/database)")
	}
	// Off loopback the credentials go only over verified TLS unless the
	// DSN opts in: server-strict by default, and server, prefer or none
	// (which do not verify the server, or skip TLS) need allow_plaintext=1.
	if hosts := d.hosts(); !allLoopback(hosts) {
		if !tlsSet {
			d.tlsMode = "server-strict"
		}
		if d.tlsMode != "server-strict" {
			for _, h := range hosts {
				if netpolicy.CheckPlaintext(h, allowPlain) != netpolicy.Allowed {
					return dsnConfig{}, fmt.Errorf("malformed DSN: tlsmode=%s does not verify the server, so a host that is not loopback (%s) also needs allow_plaintext=1; or use tlsmode=server-strict", d.tlsMode, h)
				}
			}
		}
	}
	return d, nil
}

// hosts lists the DSN's host and its backup_server_node hosts, without
// ports.
func (d dsnConfig) hosts() []string {
	out := []string{d.host}
	if d.backups == "" {
		return out
	}
	for _, hp := range strings.Split(d.backups, ",") {
		h := hp
		if host, _, err := net.SplitHostPort(hp); err == nil {
			h = host
		}
		out = append(out, h)
	}
	return out
}

func allLoopback(hosts []string) bool {
	for _, h := range hosts {
		if !netpolicy.IsLoopback(h) {
			return false
		}
	}
	return true
}

// credentialKey reports whether a DSN parameter name looks like it carries
// a credential. Any unknown parameter is refused anyway; these get the
// error that says where credentials go.
func credentialKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"credential", "password", "passwd", "pwd", "secret", "token", "key", "pass", "totp"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// buildURL renders vertica-sql-go's connection string for d with secret
// as the password (or, with auth=oauth, the access token). It always sets
// use_prepared_statements=1 (parameters are bound by the server, never
// interpolated), autocommit=1 and a tobira client label unless the DSN
// gave one.
func buildURL(d dsnConfig, secret string) string {
	q := url.Values{}
	q.Set("use_prepared_statements", "1")
	q.Set("autocommit", "1")
	q.Set("tlsmode", d.tlsMode)
	label := d.label
	if label == "" {
		label = "tobira-" + version
	}
	q.Set("client_label", label)
	if d.backups != "" {
		q.Set("backup_server_node", d.backups)
	}
	if d.loadBalance {
		q.Set("connection_load_balance", "1")
	}
	if d.workload != "" {
		q.Set("workload", d.workload)
	}
	u := url.URL{Scheme: "vertica", Host: net.JoinHostPort(d.host, strconv.Itoa(d.port)), Path: "/" + d.database}
	switch {
	case d.oauth:
		q.Set("oauth_access_token", secret)
		if d.user != "" {
			u.User = url.User(d.user)
		}
	default:
		u.User = url.UserPassword(d.user, secret)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// openPool opens a database/sql pool over connector. It does not connect.
func openPool(rawURL string, setup []string) *sql.DB {
	db := sql.OpenDB(connector{url: rawURL, setup: setup})
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(30 * time.Minute)
	return db
}

// Open parses the DSN, logs in and reads the session's schema, so bad
// credentials or an unknown host fail here rather than on the first query.
func (vDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	fail := func(kind, err error) (driver.Conn, error) {
		var de *driver.Error
		if !errors.As(err, &de) {
			err = driver.Wrap(kind, Name, "open", err)
		}
		return nil, scrubError(err, cfg)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the vertica driver takes its settings from DSN parameters", k))
	}
	d, err := parseDSN(cfg.DSN)
	if err != nil {
		return fail(driver.ErrSyntax, err)
	}
	secret := cfg.Password.Reveal()
	if secret == "" {
		what := "password"
		if d.oauth {
			what = "OAuth access token"
		}
		return fail(driver.ErrAuth, errors.New("no "+what+": set the connection's password (a secret ref)"))
	}
	if d.tlsMode == "none" || d.tlsMode == "prefer" {
		cfg.Log().Warn("vertica: the connection may send its credentials without verified TLS (tlsmode="+d.tlsMode+"); use tlsmode=server-strict", "host", d.host)
	}
	c := &conn{url: buildURL(d, secret), cfg: cfg, readOnly: cfg.ReadOnly}
	c.db = openPool(c.url, c.sessionSetup(""))
	if err := c.probe(ctx); err != nil {
		_ = c.db.Close()
		return nil, scrubError(err, cfg)
	}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// probe logs in and reads the session's current schema.
func (c *conn) probe(ctx context.Context) error {
	s, err := c.acquire(ctx, "open", c.db)
	if err != nil {
		return err
	}
	defer s.release()
	var schema sql.NullString
	if err := s.queryRow(ctx, "SELECT CURRENT_SCHEMA()", nil, &schema); err != nil {
		return mapError(ctx, "open", err)
	}
	c.defaultSchema = schema.String
	return nil
}
