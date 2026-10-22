package cassandra

import (
	"context"
	"crypto/tls"
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

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

const (
	defaultPort           = "9042"
	defaultTimeout        = 10 * time.Second
	defaultConnectTimeout = 10 * time.Second
	// maxProto is the newest native protocol gocql speaks.
	maxProto = 5
)

// settings is a parsed DSN.
type settings struct {
	hosts           []string // host:port
	user, password  string
	keyspace        string // "" = none: tables must be qualified
	consistency     gocql.Consistency
	serial          gocql.Consistency
	timeout         time.Duration
	connectTimeout  time.Duration
	dc              string
	tls             bool
	tlsCA           string
	tlsServerName   string
	tlsInsecure     bool
	proto           int // 0 = negotiate
	systemKeyspaces bool
	discovery       bool
	allowPlaintext  bool // a password may go to a remote host without TLS
}

// keyspaceName is what an unquoted keyspace name may hold.
var keyspaceName = regexp.MustCompile(`^[A-Za-z0-9_]{1,48}$`)

// dsnParams are the DSN query parameters the driver reads.
var dsnParams = []string{
	"consistency", "serial_consistency", "timeout", "connect_timeout", "dc", "tls", "tls_ca",
	"tls_server_name", "tls_insecure", "proto", "system_keyspaces", "discovery", "allow_plaintext",
}

// cloudZones are the managed Cassandra services that only speak TLS:
// Amazon Keyspaces (cassandra.<region>.amazonaws.com, and China).
var cloudZones = []string{"amazonaws.com", "amazonaws.com.cn"}

// parseDSN reads
//
//	cassandra://[user[:password]@]host[:port][,host[:port]…][/keyspace][?param=value&…]
//
// (or scylla://). The port defaults to 9042; IPv6 hosts go in brackets. A
// non-empty ConnConfig.Password replaces the DSN's password, and needs a
// user. ConnConfig.Options are refused: settings come from the DSN.
func parseDSN(cfg driver.ConnConfig) (settings, error) {
	fail := func(kind error, format string, args ...any) (settings, error) {
		return settings{}, driver.Wrap(kind, Name, "open", fmt.Errorf(format, args...))
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, "unsupported option %q: the cassandra driver takes its settings from the DSN", k)
	}
	scheme := strings.ToLower(driver.Scheme(cfg.DSN))
	rest, ok := strings.CutPrefix(cfg.DSN[len(scheme):], "://")
	if (scheme != "cassandra" && scheme != "scylla") || !ok {
		return fail(driver.ErrSyntax, "the DSN must be a cassandra:// or scylla:// URL")
	}
	if strings.Contains(rest, "#") {
		return fail(driver.ErrSyntax, "the DSN must not have a fragment")
	}
	rest, query, _ := strings.Cut(rest, "?")
	authority, path, _ := strings.Cut(rest, "/")
	s := settings{
		consistency: gocql.LocalQuorum, serial: gocql.LocalSerial,
		timeout: defaultTimeout, connectTimeout: defaultConnectTimeout, discovery: true,
	}
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
	for h := range strings.SplitSeq(authority, ",") {
		hp, err := hostPort(h)
		if err != nil {
			return fail(driver.ErrSyntax, "invalid host %q: %v", h, err)
		}
		s.hosts = append(s.hosts, hp)
	}
	if path != "" {
		if !keyspaceName.MatchString(path) {
			return fail(driver.ErrSyntax, "invalid keyspace %q: use letters, digits and '_' (at most 48)", path)
		}
		s.keyspace = path
	}
	if err := s.applyParams(query); err != nil {
		return settings{}, err
	}
	// The password authenticator sends the password as is: without TLS it
	// goes to a remote host only with allow_plaintext=1, and never to a
	// TLS-only service.
	if s.password != "" && !s.tls {
		for _, hp := range s.hosts {
			h, _, _ := net.SplitHostPort(hp)
			switch netpolicy.CheckPlaintext(h, s.allowPlaintext, cloudZones...) {
			case netpolicy.RefusedCloud:
				return fail(driver.ErrSyntax, "%s only speaks TLS: add tls=true", h)
			case netpolicy.NeedsOptIn:
				return fail(driver.ErrSyntax, "the password would go to %s (not loopback) without TLS: add tls=true, or allow_plaintext=1 to send it in clear", h)
			}
		}
	}
	return s, nil
}

// hostPort normalises one host of the DSN to host:port.
func hostPort(h string) (string, error) {
	host, port := h, defaultPort
	switch {
	case strings.HasPrefix(h, "["):
		end := strings.IndexByte(h, ']')
		if end < 0 {
			return "", errors.New("missing ']'")
		}
		host = h[1:end]
		if tail := h[end+1:]; tail != "" {
			p, ok := strings.CutPrefix(tail, ":")
			if !ok {
				return "", errors.New("junk after ']'")
			}
			port = p
		}
	case strings.Contains(h, ":"):
		host, port, _ = strings.Cut(h, ":")
	}
	if host == "" {
		return "", errors.New("empty host")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", errors.New("the port must be 1 to 65535")
	}
	return net.JoinHostPort(host, port), nil
}

// applyParams reads the DSN's query parameters into s. Each may appear
// once; an unknown one is ErrUnsupported.
func (s *settings) applyParams(query string) error {
	bad := func(format string, args ...any) error {
		return driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	vals, err := url.ParseQuery(query)
	if err != nil {
		return bad("invalid DSN parameters")
	}
	tlsSet := false
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		if !slices.Contains(dsnParams, k) {
			return driver.Wrap(driver.ErrUnsupported, Name, "open", fmt.Errorf("unsupported DSN parameter %q", k))
		}
		if len(vals[k]) != 1 {
			return bad("the DSN parameter %q is given more than once", k)
		}
		v := vals[k][0]
		switch k {
		case "consistency":
			c, err := gocql.ParseConsistencyWrapper(strings.ToUpper(v))
			if err != nil || c == gocql.Serial || c == gocql.LocalSerial {
				return bad("invalid consistency %q", v)
			}
			s.consistency = c
		case "serial_consistency":
			switch strings.ToUpper(v) {
			case "SERIAL":
				s.serial = gocql.Serial
			case "LOCAL_SERIAL":
				s.serial = gocql.LocalSerial
			default:
				return bad("serial_consistency must be SERIAL or LOCAL_SERIAL")
			}
		case "timeout", "connect_timeout":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return bad("%s must be a positive duration such as 10s", k)
			}
			if k == "timeout" {
				s.timeout = d
			} else {
				s.connectTimeout = d
			}
		case "dc":
			s.dc = v
		case "tls", "tls_insecure", "system_keyspaces", "discovery", "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return bad("%s must be true or false", k)
			}
			switch k {
			case "tls":
				s.tls, tlsSet = b, true
			case "tls_insecure":
				s.tlsInsecure = b
			case "system_keyspaces":
				s.systemKeyspaces = b
			case "allow_plaintext":
				s.allowPlaintext = b
			default:
				s.discovery = b
			}
		case "tls_ca":
			s.tlsCA = v
		case "tls_server_name":
			s.tlsServerName = v
		case "proto":
			n, err := strconv.Atoi(v)
			if err != nil || n < 3 || n > maxProto {
				return bad("proto must be 3, 4 or 5")
			}
			s.proto = n
		}
	}
	if s.tlsCA != "" || s.tlsServerName != "" || s.tlsInsecure {
		if tlsSet && !s.tls {
			return bad("tls=false contradicts the tls_* parameters")
		}
		s.tls = true
	}
	return nil
}

// allowedAuthenticators are the server authenticators the password is sent
// to: gocql v1's list, which v2 no longer enforces by default.
var allowedAuthenticators = []string{
	"org.apache.cassandra.auth.PasswordAuthenticator",
	"com.instaclustr.cassandra.auth.SharedSecretAuthenticator",
	"com.datastax.bdp.cassandra.auth.DseAuthenticator",
	"io.aiven.cassandra.auth.AivenAuthenticator",
	"com.ericsson.bss.cassandra.ecaudit.auth.AuditAuthenticator",
	"com.amazon.helenus.auth.HelenusAuthenticator",
	"com.ericsson.bss.cassandra.ecaudit.auth.AuditPasswordAuthenticator",
	"com.scylladb.auth.SaslauthdAuthenticator",
	"com.scylladb.auth.TransitionalAuthenticator",
	"com.instaclustr.cassandra.auth.InstaclustrPasswordAuthenticator",
}

// cluster builds the gocql configuration for s. The client never logs.
// Pages are never prefetched in the background, so a result that is
// closed early leaves nothing running. Each host gets one connection,
// opened synchronously by CreateSession: a connection multiplexes
// thousands of requests, and gocql fills any further ones in background
// goroutines that outlive Session.Close while they retry.
func (s settings) cluster() *gocql.ClusterConfig {
	cl := gocql.NewCluster(s.hosts...)
	cl.Keyspace = s.keyspace
	cl.Consistency = s.consistency
	cl.SerialConsistency = s.serial
	cl.Timeout = s.timeout
	cl.ConnectTimeout = s.connectTimeout
	cl.ProtoVersion = s.proto
	cl.DisableInitialHostLookup = !s.discovery
	cl.NextPagePrefetch = 0
	cl.NumConns = 1
	cl.Logger = discardLogger{}
	if s.dc != "" {
		cl.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(gocql.DCAwareRoundRobinPolicy(s.dc))
	}
	if s.user != "" {
		cl.Authenticator = gocql.PasswordAuthenticator{
			Username: s.user, Password: s.password, AllowedAuthenticators: allowedAuthenticators,
		}
	}
	if s.tls {
		cl.SslOpts = &gocql.SslOptions{
			Config:                 &tls.Config{ServerName: s.tlsServerName, InsecureSkipVerify: s.tlsInsecure}, //nolint:gosec // tls_insecure is the user's explicit choice
			CaPath:                 s.tlsCA,
			EnableHostVerification: !s.tlsInsecure,
		}
	}
	return cl
}

// discardLogger drops gocql's log messages: they would draw over the TUI.
type discardLogger struct{}

func (discardLogger) Error(string, ...gocql.LogField)   {}
func (discardLogger) Warning(string, ...gocql.LogField) {}
func (discardLogger) Info(string, ...gocql.LogField)    {}
func (discardLogger) Debug(string, ...gocql.LogField)   {}

// Open parses the DSN, connects and reads the node's protocol version, so
// a bad DSN, an unreachable cluster or bad credentials fail here rather
// than on the first query. ctx bounds the connect: gocql's own dial cannot
// be cancelled, so a cancelled Open returns at once and closes the session
// in the background once gocql gives up (connect_timeout at most).
func (cassandraDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	s, err := parseDSN(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	if err := ctx.Err(); err != nil {
		return nil, mapError(ctx, "open", err)
	}
	if s.password != "" && !s.tls && !loopbackOnly(s.hosts) {
		cfg.Log().WarnContext(ctx, "cassandra: the password is sent without TLS because tls is off",
			"connection", cfg.Name, "hosts", strings.Join(s.hosts, ","))
	}
	cl := s.cluster()
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left > 0 && left < cl.ConnectTimeout {
			cl.ConnectTimeout = left
		}
	}
	sess, err := createSession(ctx, cl)
	if err != nil {
		err = openError(ctx, err)
		if s.keyspace != "" && strings.Contains(err.Error(), gocql.ErrNoConnectionsStarted.Error()) {
			err = missingKeyspace(ctx, cl, s.keyspace, err)
		}
		return nil, scrubError(err, cfg)
	}
	c := &conn{session: sess, set: s, readOnly: cfg.ReadOnly}
	if c.proto, err = c.protocol(ctx); err != nil {
		sess.Close()
		return nil, scrubError(err, cfg)
	}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	c.schemas = newSchemaCache(schemaTTL)
	return c, nil
}

// createSession runs gocql's CreateSession, which cannot be cancelled,
// bounded by ctx: when ctx ends first it returns ctx's error at once and
// closes the session in the background once gocql gives up.
func createSession(ctx context.Context, cl *gocql.ClusterConfig) (*gocql.Session, error) {
	type opened struct {
		session *gocql.Session
		err     error
	}
	done := make(chan opened, 1)
	go func() {
		sess, err := cl.CreateSession()
		done <- opened{sess, err}
	}()
	select {
	case o := <-done:
		return o.session, o.err
	case <-ctx.Done():
		go func() {
			if o := <-done; o.session != nil {
				o.session.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// missingKeyspace explains a failed connect to a keyspace: gocql reports
// a keyspace that does not exist as "no connections were made". It
// connects once more without the keyspace and, when the cluster answers
// that the keyspace is missing, returns ErrNotFound; otherwise orig.
func missingKeyspace(ctx context.Context, cl *gocql.ClusterConfig, ks string, orig error) error {
	bare := *cl
	bare.Keyspace = ""
	sess, err := createSession(ctx, &bare)
	if err != nil {
		return orig
	}
	defer sess.Close()
	var n int
	if err := sess.Query("SELECT count(*) FROM system_schema.keyspaces WHERE keyspace_name = ?", ks).ScanContext(ctx, &n); err != nil || n > 0 {
		return orig
	}
	return driver.Wrap(driver.ErrNotFound, Name, "open", fmt.Errorf("keyspace %q does not exist", ks))
}

// protocol returns the native protocol version the session speaks: the
// DSN's proto, else the newer of the node's version and gocql's newest.
func (c *conn) protocol(ctx context.Context) (int, error) {
	if c.set.proto != 0 {
		return c.set.proto, nil
	}
	var v string
	if err := c.session.Query("SELECT native_protocol_version FROM system.local").ScanContext(ctx, &v); err != nil {
		return 0, mapError(ctx, "open", err)
	}
	i := 0
	for i < len(v) && isDigit(v[i]) {
		i++
	}
	n, err := strconv.Atoi(v[:i])
	if err != nil {
		return 0, driver.Wrap(driver.ErrUnsupported, Name, "open", fmt.Errorf("unreadable native_protocol_version %q", v))
	}
	return min(n, maxProto), nil
}

// loopbackOnly reports whether every host is a loopback address or
// localhost.
func loopbackOnly(hosts []string) bool {
	for _, hp := range hosts {
		h, _, _ := net.SplitHostPort(hp)
		if !netpolicy.IsLoopback(h) {
			return false
		}
	}
	return true
}

// openError classifies a failed CreateSession. gocql flattens the cause
// into the message, so the kind comes from a server error when one is
// still reachable, else from the text.
func openError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return mapError(ctx, "open", err)
	}
	var re gocql.RequestError
	if errors.As(err, &re) {
		return mapError(ctx, "open", err)
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "password are incorrect"), strings.Contains(msg, "bad credentials"),
		strings.Contains(msg, "authentication"), strings.Contains(msg, "unexpected authenticator"):
		return driver.Wrap(driver.ErrAuth, Name, "open", err)
	case strings.Contains(msg, "keyspace") && strings.Contains(msg, "does not exist"):
		return driver.Wrap(driver.ErrNotFound, Name, "open", err)
	}
	return driver.Wrap(driver.ErrConnection, Name, "open", err)
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
