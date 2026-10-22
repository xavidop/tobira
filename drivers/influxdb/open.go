package influxdb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const defaultPort = "8181"

// noToken is sent to the Flight API when the connection has no token: the
// client library requires one, and a server running without auth ignores it.
const noToken = "tobira-no-token"

// maxBody caps how much of an HTTP response the driver reads.
const maxBody = 1 << 20

// settings is a parsed DSN.
type settings struct {
	host     string // scheme://host:port for the client
	token    string // "" = none
	database string // default database; "" = none
	lang     language
	tls      bool
	// tlsDefaulted: TLS is on only because the host is not loopback and
	// the DSN has no tls=, so a handshake failure gets a hint.
	tlsDefaulted bool
}

// parseDSN reads influxdb3://[user:token@]host[:port][/database][?params]
// (or influxdb://). The token is the userinfo password; a non-empty
// ConnConfig.Password replaces it. Parameters: language=sql|influxql,
// tls=true|false and allow_plaintext=true|false. TLS is on unless the host
// is loopback or tls=false is given; plain http to a host that is not
// loopback also needs allow_plaintext=1, and InfluxDB Cloud
// (*.influxdata.com, and Cloud Dedicated's *.influxdb.io) is always TLS. Anything else is refused, so a typo is
// never ignored.
func parseDSN(cfg driver.ConnConfig) (settings, error) {
	var s settings
	bad := func(kind error, format string, args ...any) (settings, error) {
		return settings{}, driver.Wrap(kind, Name, "open", fmt.Errorf(format, args...))
	}
	sc := driver.Scheme(cfg.DSN)
	if sc != "influxdb3" && sc != "influxdb" {
		return bad(driver.ErrSyntax, "DSN must start with influxdb3:// or influxdb://")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return bad(driver.ErrSyntax, "cannot parse the DSN")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return bad(driver.ErrSyntax, "DSN has no host: use influxdb3://host[:port]/database")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return bad(driver.ErrSyntax, "invalid port %q", port)
	}
	if u.User != nil {
		pw, ok := u.User.Password()
		if !ok {
			return bad(driver.ErrSyntax, "put the token in the password position (influxdb3://:TOKEN@host), not the user name")
		}
		s.token = pw
	}
	if pw := cfg.Password.Reveal(); pw != "" {
		s.token = pw
	}
	db := strings.TrimPrefix(u.Path, "/")
	if strings.Contains(db, "/") {
		return bad(driver.ErrSyntax, "the DSN path is one database name")
	}
	s.database = db
	tlsSet, allowPlain := false, false
	for key, vals := range u.Query() {
		if len(vals) != 1 {
			return bad(driver.ErrSyntax, "parameter %q is given more than once", key)
		}
		v := vals[0]
		switch strings.ToLower(key) {
		case "language":
			switch strings.ToLower(v) {
			case "sql":
				s.lang = langSQL
			case "influxql":
				s.lang = langInfluxQL
			default:
				return bad(driver.ErrUnsupported, "language must be sql or influxql")
			}
		case "tls":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return bad(driver.ErrSyntax, "tls must be true or false")
			}
			s.tls = b
			tlsSet = true
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return bad(driver.ErrSyntax, "allow_plaintext must be true or false")
			}
			allowPlain = b
		case "token":
			return bad(driver.ErrUnsupported, "a token parameter would not be redacted: put the token in the password position (influxdb3://:TOKEN@host) or in the connection's password")
		default:
			return bad(driver.ErrUnsupported, "unknown DSN parameter %q", key)
		}
	}
	// Nothing crosses the network in cleartext unless asked: TLS is the
	// default for anything but a loopback host.
	loopback := netpolicy.IsLoopback(u.Hostname())
	if !tlsSet {
		s.tls, s.tlsDefaulted = !loopback, !loopback
	}
	if !s.tls {
		switch netpolicy.CheckPlaintext(u.Hostname(), allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return bad(driver.ErrSyntax, "tls=false is refused for InfluxDB Cloud (*.influxdata.com, *.influxdb.io)")
		case netpolicy.NeedsOptIn:
			return bad(driver.ErrSyntax, "plain http to %s (not loopback) needs allow_plaintext=1 as well as tls=false", u.Hostname())
		}
		if !loopback && s.token != "" {
			cfg.Log().Warn("influxdb: tls=false sends the token in cleartext", "connection", cfg.Name, "host", u.Hostname())
		}
	}
	scheme := "http"
	if s.tls {
		scheme = "https"
	}
	s.host = scheme + "://" + net.JoinHostPort(u.Hostname(), port)
	return s, nil
}

// cloudZones are InfluxDB Cloud (Serverless and the older regions on
// influxdata.com) and InfluxDB Cloud Dedicated (influxdb.io), which only
// speak TLS.
var cloudZones = []string{"influxdata.com", "influxdb.io"}

// tlsHint adds the opt-in to an Open error that looks like a TLS handshake
// against a plain-HTTP server, when TLS was only the default: a DSN that
// reached such a server in clear before TLS became the default for remote
// hosts fails here, and the error says how to opt back in.
func (s settings) tlsHint(err error) error {
	var de *driver.Error
	if !s.tlsDefaulted || !netpolicy.IsTLSMismatch(err) || !errors.As(err, &de) {
		return err
	}
	out := *de
	out.Err = fmt.Errorf("%w; TLS is the default for a host that is not loopback: if the server speaks plain HTTP, add tls=false&allow_plaintext=1", de.Err)
	return &out
}

func (influxDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, driver.Wrap(nil, Name, "open", err)
	}
	set, err := parseDSN(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	redact := cfg
	if set.token != "" {
		redact.Password = driver.NewSecret(set.token)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	// No redirect is followed: Go keeps the Authorization header (the
	// token) on a redirect to the same host name, another port or from
	// https to http included, and the InfluxDB HTTP API never needs one.
	// Queries go over Flight (gRPC), which has no redirects.
	hc := &http.Client{Transport: tr, CheckRedirect: netpolicy.RefuseRedirects}
	token := set.token
	if token == "" {
		token = noToken
	}
	client, err := influxdb3.New(influxdb3.ClientConfig{Host: set.host, Token: token, Database: set.database, HTTPClient: hc})
	if err != nil {
		tr.CloseIdleConnections()
		return nil, scrubError(driver.Wrap(driver.ErrConnection, Name, "open", err), redact)
	}
	c := &conn{client: client, http: hc, set: set, redact: redact, readOnly: cfg.ReadOnly, rangeCap: maxRangePoints}
	pctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if _, err := c.get(pctx, "open", "/ping"); err != nil {
		_ = client.Close()
		return nil, set.tlsHint(err)
	}
	return c, nil
}

// get runs an authenticated GET on the HTTP API and returns the body. A
// failure is classified by status: 401 is ErrAuth, 403 ErrPermission, 404
// ErrNotFound, anything else (and a transport failure) ErrConnection. The
// message is scrubbed of the token.
func (c *conn) get(ctx context.Context, op, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.set.host+path, nil)
	if err != nil {
		return nil, c.scrub(driver.Wrap(driver.ErrSyntax, Name, op, err))
	}
	if c.set.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.set.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, driver.Wrap(nil, Name, op, cerr)
		}
		return nil, c.scrub(driver.Wrap(driver.ErrConnection, Name, op, err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, driver.Wrap(nil, Name, op, cerr)
		}
		return nil, c.scrub(driver.Wrap(driver.ErrConnection, Name, op, err))
	}
	if resp.StatusCode/100 == 2 {
		return body, nil
	}
	kind := driver.ErrConnection
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		kind = driver.ErrAuth
	case http.StatusForbidden:
		kind = driver.ErrPermission
	case http.StatusNotFound:
		kind = driver.ErrNotFound
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		msg = resp.Status
	}
	return nil, c.scrub(&driver.Error{Kind: kind, Driver: Name, Op: op, Code: strconv.Itoa(resp.StatusCode),
		Err: fmt.Errorf("%s %s: %s", http.MethodGet, path, msg)})
}

func (c *conn) scrub(err error) error { return scrubError(err, c.redact) }

// scrubError masks every credential of cfg in the message of a
// *driver.Error, keeping its kind and code. A masked message replaces the
// original error entirely: like the AI providerError, it unwraps only to
// the kind (and a context error), never to the unredacted original.
func scrubError(err error, cfg driver.ConnConfig) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := de.Err.Error()
	if clean := driver.RedactMessage(msg, cfg); clean != msg {
		var kinds []error
		if de.Kind != nil {
			kinds = append(kinds, de.Kind)
		}
		for _, c := range []error{context.Canceled, context.DeadlineExceeded} {
			if errors.Is(de.Err, c) {
				kinds = append(kinds, c)
			}
		}
		de.Err = maskedError{msg: clean, kinds: kinds}
	}
	return err
}

// maskedError shows a masked message; it unwraps only to its kinds.
type maskedError struct {
	msg   string
	kinds []error
}

func (m maskedError) Error() string   { return m.msg }
func (m maskedError) Unwrap() []error { return m.kinds }

// queryTimeout bounds the catalog reads the driver issues on its own.
const queryTimeout = 30 * time.Second
