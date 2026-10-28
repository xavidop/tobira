package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

// cloudEndpoint is the REST API of github.com.
const cloudEndpoint = "https://api.github.com"

// defaultTimeout bounds one request.
const defaultTimeout = 30 * time.Second

// cloudZones are GitHub's own hosts, which only speak TLS.
var cloudZones = []string{"github.com", "githubusercontent.com"}

// A GitHub login is 1 to 39 letters, digits and single hyphens, neither
// starting nor ending with a hyphen; a repository name is letters, digits,
// ".", "_" and "-", up to 100.
var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9])*$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// validOwner reports a well-formed GitHub login.
func validOwner(s string) bool { return len(s) <= 39 && ownerRe.MatchString(s) }

// dsnConfig is a parsed github:// DSN.
type dsnConfig struct {
	endpoint string // REST API base, no trailing slash
	graphql  string // GraphQL endpoint
	owner    string // "" = the token's own repositories
	repo     string // "" = every repository of the owner
	token    string
	timeout  time.Duration
	// plaintextToken: the token goes to a remote host without TLS because
	// an http:// endpoint was given explicitly. Open logs a warning.
	plaintextToken bool
}

// repoName is the namespace name of the pinned repository, or "".
func (d dsnConfig) repoName() string {
	if d.repo == "" {
		return ""
	}
	return d.owner + "/" + d.repo
}

// Open builds the client and pings it once, so a bad endpoint or a bad
// token fail here rather than on the first query. Errors never contain the
// token.
func (githubDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	log := cfg.Log()
	if d.plaintextToken {
		// The host's --debug log (ConnConfig.Logger; discarded when nil).
		// The token itself is never logged.
		log.WarnContext(ctx, "github: the token is sent without TLS because the endpoint is http://", "connection", cfg.Name)
	}
	if d.token == "" {
		log.WarnContext(ctx, "github: no token: only public data is visible and GitHub allows 60 requests per hour", "connection", cfg.Name)
	}
	c := newConn(d, cfg)
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		return nil, redact(err, cfg, d.token)
	}
	return c, nil
}

// newConn creates the connection without contacting the server.
func newConn(d dsnConfig, cfg driver.ConnConfig) *conn {
	c := &conn{dsn: d, cfg: cfg, client: newClient(d.endpoint, d.graphql, d.token, d.timeout), readOnly: cfg.ReadOnly}
	c.repos = newRepoCache(c)
	c.catalog = newNameCache(c)
	return c
}

// parseDSN reads
//
//	github://[:token@][owner[/repo]][?endpoint=…&allow_plaintext=…&timeout=…]
//
// Without an owner the connection lists the token's own repositories; with
// an owner, that user's or organization's; with a repository, that one. The
// token may sit in the password position, which every redactor masks, or in
// ConnConfig.Password, which wins; a token parameter is refused because it
// would be printed. ConnConfig.Options may set endpoint, allow_plaintext
// and timeout. The endpoint is https://api.github.com unless given (a
// GitHub Enterprise Server's https://host/api/v3). Plain http to a host
// that is not loopback also needs allow_plaintext=1, and GitHub's own hosts
// are always TLS.
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if driver.Scheme(cfg.DSN) != "github" {
		return fail("DSN must start with github://")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, token included
	}
	d := dsnConfig{endpoint: cloudEndpoint, timeout: defaultTimeout}
	if pw, ok := u.User.Password(); ok {
		d.token = pw
	}
	if u.User != nil && u.User.Username() != "" {
		return fail("a github DSN has no user name: give the token as github://:TOKEN@… or in password:")
	}
	if u.Port() != "" {
		return fail("a github DSN has no port: the authority is the owner (set endpoint= for another server)")
	}
	d.owner = u.Hostname()
	if d.owner != "" && !validOwner(d.owner) {
		return fail("%q is not a GitHub user or organization name", d.owner)
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		if d.owner == "" {
			return fail("a repository needs its owner: github://owner/repo")
		}
		if strings.Contains(p, "/") || !repoRe.MatchString(p) {
			return fail("%q is not a repository name: the DSN is github://owner/repo", p)
		}
		d.repo = p
	}
	endpoint, allowPlain := "", false
	set := func(k, v string) error {
		switch k {
		case "endpoint":
			endpoint = v
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("allow_plaintext must be true or false")
			}
			allowPlain = b
		case "timeout":
			t, err := time.ParseDuration(v)
			if err != nil || t <= 0 {
				return errors.New("timeout must be a positive duration such as 30s")
			}
			d.timeout = t
		case "token", "password", "api_key", "access_token":
			return errors.New("a token in the DSN query would be printed: give it as github://:TOKEN@… or in the password field")
		default:
			return fmt.Errorf("unknown option %q (endpoint, allow_plaintext and timeout are accepted)", k)
		}
		return nil
	}
	for k, vs := range u.Query() {
		if err := set(strings.ToLower(k), vs[len(vs)-1]); err != nil {
			return fail("%v", err)
		}
	}
	for k, v := range cfg.Options {
		if err := set(strings.ToLower(k), v); err != nil {
			return fail("%v", err)
		}
	}
	if !cfg.Password.IsZero() {
		d.token = cfg.Password.Reveal()
	}
	if endpoint != "" {
		eu, err := url.Parse(endpoint)
		if err != nil || eu.Host == "" || (eu.Scheme != "http" && eu.Scheme != "https") || eu.RawQuery != "" || eu.Fragment != "" || eu.User != nil {
			return fail("endpoint must be an http(s) URL with a host and no query, fragment or user")
		}
		d.endpoint = strings.TrimRight(eu.String(), "/")
		host := eu.Hostname()
		if eu.Scheme == "http" {
			switch netpolicy.CheckPlaintext(host, allowPlain, cloudZones...) {
			case netpolicy.RefusedCloud:
				return fail("an http:// endpoint is refused for GitHub's own hosts: the token would cross the network in clear")
			case netpolicy.NeedsOptIn:
				return fail("plain http to %s (not loopback) needs allow_plaintext=1", host)
			}
			d.plaintextToken = d.token != "" && !netpolicy.IsLoopback(host)
		}
	}
	d.graphql = graphqlEndpoint(d.endpoint)
	return d, nil
}

// graphqlEndpoint derives the GraphQL URL from the REST one: github.com
// serves it at /graphql, an Enterprise Server at /api/graphql next to
// /api/v3.
func graphqlEndpoint(rest string) string {
	if strings.HasSuffix(rest, "/api/v3") {
		return strings.TrimSuffix(rest, "/api/v3") + "/api/graphql"
	}
	return rest + "/graphql"
}
