package d1

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

var errClosed = errors.New("connection is closed")

// defaultEndpoint is the Cloudflare API base URL.
const defaultEndpoint = "https://api.cloudflare.com/client/v4"

// idPattern is what an account or database id may hold: Cloudflare's are
// hex and UUIDs. Anything else could reshape the request path.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// dsnConfig is a parsed d1 DSN.
type dsnConfig struct {
	url   string // the database's /raw endpoint
	token string
}

// parseDSN reads d1://<account_id>/<database_id>[?endpoint=<api base>[&allow_plaintext=1]]. The
// API token comes from cfg.Password (a password ref) or the URL's password
// position (d1://:TOKEN@account/database), where every redactor masks it; a
// token in a query parameter is refused, because parameters are echoed
// unmasked. endpoint overrides the API base URL (for a proxy or a local
// stand-in); it must be https unless it is a loopback address or
// allow_plaintext=1 is given, so the token never travels in clear to another
// machine by accident, and Cloudflare's own API is always https.
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if sc := driver.Scheme(cfg.DSN); sc != "d1" {
		return fail("a d1 DSN is d1://<account_id>/<database_id>, got scheme %q", sc)
	}
	if strings.Contains(cfg.DSN, "#") {
		return fail("the DSN must not have a fragment")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, token included
	}
	if u.User != nil && u.User.Username() != "" {
		return fail("the DSN takes no user name: put the API token in the password field (a password ref)")
	}
	account := u.Host
	database := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	if u.RawPath != "" || strings.Contains(u.Host, "%") {
		return fail("the account and database ids must not be percent-encoded")
	}
	if !idPattern.MatchString(account) || !idPattern.MatchString(database) {
		return fail("a d1 DSN is d1://<account_id>/<database_id> (letters, digits and dashes)")
	}
	endpoint, rawEndpoint, allowPlain := defaultEndpoint, "", false
	seen := map[string]bool{}
	for k, vs := range u.Query() {
		lk := strings.ToLower(k)
		if seen[lk] {
			return fail("repeated DSN parameter %q", lk)
		}
		seen[lk] = true
		switch lk {
		case "endpoint":
			if len(vs) != 1 {
				return fail("repeated DSN parameter %q", k)
			}
			rawEndpoint = vs[0]
		case "allow_plaintext":
			if len(vs) != 1 {
				return fail("repeated DSN parameter %q", k)
			}
			b, err := strconv.ParseBool(vs[0])
			if err != nil {
				return fail("allow_plaintext must be true or false")
			}
			allowPlain = b
		case "token", "api_token", "apitoken", "authtoken", "auth_token", "key", "api_key", "apikey":
			return fail("put the API token in the password field (a password ref) or the URL's password position (d1://:TOKEN@account/database), not in parameter %q", k)
		default:
			return fail("unknown DSN parameter %q (endpoint and allow_plaintext are accepted)", k)
		}
	}
	if rawEndpoint != "" {
		if endpoint, err = parseEndpoint(rawEndpoint, allowPlain); err != nil {
			return fail("%v", err)
		}
	}
	for k := range cfg.Options {
		return fail("unknown option %q: the d1 driver takes none", k)
	}
	d := dsnConfig{url: endpoint + "/accounts/" + account + "/d1/database/" + database + "/raw"}
	if pw, ok := u.User.Password(); ok {
		d.token = pw
	}
	if !cfg.Password.IsZero() {
		d.token = cfg.Password.Reveal()
	}
	if d.token == "" {
		return dsnConfig{}, driver.Wrap(driver.ErrAuth, Name, "open", errors.New("an API token is required: set the connection's password (a password ref) to a Cloudflare API token with D1 access"))
	}
	for i := 0; i < len(d.token); i++ {
		if c := d.token[i]; c < 0x21 || c > 0x7e {
			return fail("the API token holds a character a token never has")
		}
	}
	return d, nil
}

// parseEndpoint validates an API base URL override and returns it without a
// trailing slash. Plain http is accepted for loopback, or for another host
// when allowPlain is set, but never for Cloudflare's own API.
func parseEndpoint(raw string, allowPlain bool) (string, error) {
	e, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", errors.New("endpoint is not a URL")
	case e.Scheme != "https" && e.Scheme != "http":
		return "", errors.New("endpoint must be an https:// URL")
	case e.Hostname() == "":
		return "", errors.New("endpoint has no host")
	case e.User != nil, e.RawQuery != "", e.ForceQuery, e.Fragment != "":
		return "", errors.New("endpoint must not have user info, a query or a fragment")
	}
	if e.Scheme == "http" {
		switch netpolicy.CheckPlaintext(e.Hostname(), allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return "", errors.New("refusing plain HTTP to Cloudflare's API (*.cloudflare.com): use an https:// endpoint")
		case netpolicy.NeedsOptIn:
			return "", errors.New("refusing to send the API token over plain HTTP to a remote host: use an https:// endpoint, or add allow_plaintext=1")
		}
	}
	return e.Scheme + "://" + e.Host + strings.TrimRight(e.EscapedPath(), "/"), nil
}

// cloudZones is Cloudflare's API, which only speaks TLS.
var cloudZones = []string{"cloudflare.com"}

// Open parses the DSN and runs SELECT 1, so a bad token, account or
// database fails here. Errors never contain the token.
func (d1Driver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	c := &conn{client: newClient(d.url, d.token), readOnly: cfg.ReadOnly}
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
		msg = strings.ReplaceAll(msg, url.PathEscape(token), "xxxxx")
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
