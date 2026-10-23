package neo4j

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const (
	defaultPort           = "7687"
	defaultConnectTimeout = 10 * time.Second
)

// settings is a parsed DSN.
type settings struct {
	scheme         string // the scheme handed to the Go driver: neo4j, neo4j+s, …
	hostPort       string
	user, password string
	database       string // "" = the server's default database
	connectTimeout time.Duration
	timeout        time.Duration // per-transaction server timeout; 0 = the server's
	allowPlaintext bool
}

// uri is the target the Go driver dials.
func (s settings) uri() string { return s.scheme + "://" + s.hostPort }

// tls reports whether the scheme encrypts the connection.
func (s settings) tls() bool { return strings.Contains(s.scheme, "+s") }

// databaseName is what a database name may hold: Neo4j allows letters,
// digits, dots and dashes, starting with a letter, up to 63 characters.
var databaseName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.-]{2,62}$`)

// dsnParams are the DSN query parameters the driver reads.
var dsnParams = []string{"tls", "timeout", "connect_timeout", "allow_plaintext"}

// schemes maps every accepted scheme to the one the Go driver gets.
var schemes = map[string]string{
	"neo4j": "neo4j", "neo4j+s": "neo4j+s", "neo4j+ssc": "neo4j+ssc",
	"bolt": "bolt", "bolt+s": "bolt+s", "bolt+ssc": "bolt+ssc",
}

// cloudZones are the managed services that only speak TLS: Neo4j Aura.
var cloudZones = []string{"neo4j.io"}

// parseDSN reads
//
//	neo4j://[user[:password]@]host[:port][/database][?param=value&…]
//
// with any of the schemes neo4j, neo4j+s, neo4j+ssc, bolt, bolt+s and
// bolt+ssc, which mean to Neo4j's driver what they mean everywhere: +s
// encrypts and verifies the certificate, +ssc encrypts and trusts any
// certificate. tls=true turns a plain scheme into its +s form. The port
// defaults to 7687. A non-empty ConnConfig.Password replaces the DSN's
// password, and needs a user. ConnConfig.Options are refused: settings come
// from the DSN.
func parseDSN(cfg driver.ConnConfig) (settings, error) {
	fail := func(kind error, format string, args ...any) (settings, error) {
		return settings{}, driver.Wrap(kind, Name, "open", fmt.Errorf(format, args...))
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, "unsupported option %q: the neo4j driver takes its settings from the DSN", k)
	}
	scheme := strings.ToLower(driver.Scheme(cfg.DSN))
	rest, ok := strings.CutPrefix(cfg.DSN[len(scheme):], "://")
	if _, known := schemes[scheme]; !known || !ok {
		return fail(driver.ErrSyntax, "the DSN must be a neo4j://, neo4j+s://, neo4j+ssc://, bolt://, bolt+s:// or bolt+ssc:// URL")
	}
	if strings.Contains(rest, "#") {
		return fail(driver.ErrSyntax, "the DSN must not have a fragment")
	}
	rest, query, _ := strings.Cut(rest, "?")
	authority, path, _ := strings.Cut(rest, "/")
	s := settings{scheme: schemes[scheme], connectTimeout: defaultConnectTimeout}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		user, pass, hasPass := strings.Cut(authority[:at], ":")
		var err error
		if s.user, err = url.PathUnescape(user); err != nil {
			return fail(driver.ErrSyntax, "invalid user in the DSN")
		}
		if hasPass {
			if s.password, err = url.PathUnescape(pass); err != nil {
				return fail(driver.ErrSyntax, "invalid password in the DSN")
			}
		}
		authority = authority[at+1:]
	}
	if !cfg.Password.IsZero() {
		s.password = cfg.Password.Reveal()
	}
	if s.password != "" && s.user == "" {
		return fail(driver.ErrSyntax, "a password was given but the DSN names no user")
	}
	if authority == "" {
		return fail(driver.ErrSyntax, "the DSN names no host")
	}
	if strings.Contains(authority, ",") {
		return fail(driver.ErrSyntax, "the DSN names several hosts: name one, the neo4j:// scheme discovers the cluster from it")
	}
	hp, err := hostPort(authority)
	if err != nil {
		return fail(driver.ErrSyntax, "invalid host %q: %v", authority, err)
	}
	s.hostPort = hp
	if path != "" {
		db, err := url.PathUnescape(path)
		if err != nil || !databaseName.MatchString(db) {
			return fail(driver.ErrSyntax, "invalid database %q: a name starts with a letter and holds letters, digits, '.' and '-' (3 to 63 characters)", path)
		}
		s.database = db
	}
	if err := s.applyParams(query); err != nil {
		return settings{}, err
	}
	// Bolt sends the password in the handshake: without TLS it goes to a
	// remote host only with allow_plaintext=1, and never to a TLS-only
	// service.
	if s.password != "" && !s.tls() {
		h, _, _ := net.SplitHostPort(s.hostPort)
		switch netpolicy.CheckPlaintext(h, s.allowPlaintext, cloudZones...) {
		case netpolicy.RefusedCloud:
			return fail(driver.ErrSyntax, "%s only speaks TLS: use the neo4j+s:// scheme", h)
		case netpolicy.NeedsOptIn:
			return fail(driver.ErrSyntax, "the password would go to %s unencrypted: use neo4j+s://, or add allow_plaintext=1 to accept that", h)
		}
	}
	return s, nil
}

// hostPort completes a host with the default port; IPv6 hosts go in
// brackets.
func hostPort(h string) (string, error) {
	if h == "" {
		return "", errors.New("empty host")
	}
	if host, port, err := net.SplitHostPort(h); err == nil {
		if host == "" || port == "" {
			return "", errors.New("empty host or port")
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return "", fmt.Errorf("invalid port %q", port)
		}
		return net.JoinHostPort(host, port), nil
	}
	if strings.HasPrefix(h, "[") {
		return "", errors.New("an IPv6 host needs a port after the closing bracket, or none")
	}
	if strings.ContainsAny(h, "/ ") {
		return "", errors.New("the host holds a '/' or a space")
	}
	return net.JoinHostPort(h, defaultPort), nil
}

// applyParams reads the DSN query parameters.
func (s *settings) applyParams(query string) error {
	fail := func(format string, args ...any) error {
		return driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if query == "" {
		return nil
	}
	vals, err := url.ParseQuery(query)
	if err != nil {
		return fail("invalid DSN parameters: %v", err)
	}
	for k := range vals {
		if !slices.Contains(dsnParams, k) {
			return fail("unknown DSN parameter %q (known: %s)", k, strings.Join(dsnParams, ", "))
		}
	}
	bools := func(k string) (bool, error) {
		v := vals.Get(k)
		if v == "" {
			return false, nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false, fail("%s must be true or false, not %q", k, v)
		}
		return b, nil
	}
	durations := func(k string) (time.Duration, error) {
		v := vals.Get(k)
		if v == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return 0, fail("%s must be a duration such as 30s, not %q", k, v)
		}
		return d, nil
	}
	tls, err := bools("tls")
	if err != nil {
		return err
	}
	if vals.Has("tls") {
		switch {
		case tls && !s.tls():
			s.scheme += "+s"
		case !tls && s.tls():
			return fail("tls=false contradicts the %s:// scheme", s.scheme)
		}
	}
	if s.allowPlaintext, err = bools("allow_plaintext"); err != nil {
		return err
	}
	if d, err := durations("connect_timeout"); err != nil {
		return err
	} else if d > 0 {
		s.connectTimeout = d
	}
	if s.timeout, err = durations("timeout"); err != nil {
		return err
	}
	return nil
}

// noLog keeps the Go driver quiet: tobira draws the terminal, so a driver
// never writes to stderr.
type noLog struct{}

func (noLog) Error(string, string, error)           {}
func (noLog) Warnf(string, string, string, ...any)  {}
func (noLog) Infof(string, string, string, ...any)  {}
func (noLog) Debugf(string, string, string, ...any) {}

// Open parses the DSN and connects. The connection is verified at once,
// so bad credentials and an unreachable host fail here, as ErrAuth and
// ErrConnection. ConnConfig.ReadOnly makes every call run in a read-mode
// transaction, which the server enforces.
func (neo4jDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	const op = "open"
	s, err := parseDSN(cfg)
	if err != nil {
		return nil, err
	}
	auth := neo4j.NoAuth()
	if s.user != "" {
		auth = neo4j.BasicAuth(s.user, s.password, "")
	}
	drv, err := neo4j.NewDriverWithContext(s.uri(), auth, func(c *config.Config) {
		c.Log = noLog{}
		c.UserAgent = "tobira"
		c.SocketConnectTimeout = s.connectTimeout
	})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if err := drv.VerifyConnectivity(ctx); err != nil {
		_ = drv.Close(context.Background())
		return nil, mapError(ctx, op, err)
	}
	return &conn{drv: drv, set: s, readOnly: cfg.ReadOnly, schemas: newSchemaCache()}, nil
}
