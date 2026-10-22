package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

const (
	urlPrefix     = "clickhouse://"
	protocolParam = "protocol"
	// allowPlainParam opts in to plain HTTP to a host that is not loopback.
	allowPlainParam = "allow_plaintext"
	readOnlyKey     = "readonly"
	// cancelHTTPKey makes the server stop a read-only HTTP query when the
	// client goes away (a cancelled context closes the request).
	cancelHTTPKey = "cancel_http_readonly_queries_on_client_close"
	// maxExecutionTimeKey is the setting clickhouse-go adds for a deadline.
	maxExecutionTimeKey = "max_execution_time"
)

// Default ports by protocol and TLS.
var defaultPorts = map[ch.Protocol][2]string{
	ch.Native: {"9000", "9440"},
	ch.HTTP:   {"8123", "8443"},
}

// Open builds the client options from cfg, opens the pool and probes the
// server once, so a bad DSN or bad credentials fail here rather than on the
// first query.
func (chDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	opts, err := buildOptions(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	p, defaultNS, err := openPool(ctx, opts, cfg.ReadOnly)
	if err != nil {
		return nil, scrubError(mapError(ctx, "open", err), cfg)
	}
	c := &conn{db: p.db, serverRO: p.serverRO, opts: opts, cfg: cfg, readOnly: cfg.ReadOnly, defaultNS: defaultNS}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// pool is one clickhouse-go pool, and whether its server already runs the
// user read-only (getSetting('readonly') >= 1).
type pool struct {
	db       chdriver.Conn
	serverRO bool
}

// probeSQL reads what a pool needs to know about its server session.
const probeSQL = "SELECT currentDatabase(), toUInt64(getSetting('readonly'))"

// openPool opens a pool on base and probes the server's readonly setting.
// A user whose profile sets readonly (1 or 2) may not change it, nor any
// setting under readonly=1, so then nothing is added: the server already
// refuses every write. Otherwise the pool is reopened with guardSettings,
// which adds readonly=1 on a read-only connection (the server layer of the
// guard) and, over HTTP, the cancel-on-disconnect setting.
func openPool(ctx context.Context, base *ch.Options, readOnly bool) (pool, string, error) {
	ctx, done := detach(ctx)
	defer done()
	var tracked transports
	open := func(o *ch.Options) (chdriver.Conn, error) {
		if o.Protocol == ch.HTTP {
			// clickhouse-go does not release the HTTP client of a
			// connection whose hello failed (bad credentials, on first
			// use): close its idle sockets when opening fails. The
			// tracked transports also drop max_execution_time.
			o.TransportFunc = tracked.track
		}
		db, err := ch.Open(o)
		if err == nil {
			if err = db.Ping(ctx); err != nil {
				_ = db.Close()
			}
		}
		if err != nil {
			tracked.closeIdle()
			return nil, err
		}
		return db, nil
	}
	probe := guardSettings(base, false, true) // the base settings only
	db, err := open(probe)
	if err != nil {
		return pool{}, "", err
	}
	var current string
	var level uint64
	if err := db.QueryRow(ctx, probeSQL).Scan(&current, &level); err != nil {
		_ = db.Close()
		tracked.closeIdle()
		return pool{}, "", err
	}
	serverRO := level >= 1
	final := guardSettings(base, readOnly, serverRO)
	if len(final.Settings) == len(probe.Settings) {
		return pool{db: db, serverRO: serverRO}, current, nil
	}
	_ = db.Close()
	tracked.closeIdle() // the probe pool's HTTP sockets
	if db, err = open(final); err != nil {
		return pool{}, "", err
	}
	return pool{db: db, serverRO: serverRO}, current, nil
}

// guardSettings returns a copy of base with the settings the guard adds
// when the server does not already run the user read-only: readonly=1 on a
// read-only connection, and over HTTP cancel_http_readonly_queries_on_client_close=1.
func guardSettings(base *ch.Options, readOnly, serverRO bool) *ch.Options {
	o := *base
	o.Settings = maps.Clone(base.Settings)
	if o.Settings == nil {
		o.Settings = ch.Settings{}
	}
	if !serverRO {
		if readOnly {
			o.Settings[readOnlyKey] = 1
		}
		if o.Protocol == ch.HTTP {
			o.Settings[cancelHTTPKey] = 1
		}
	}
	return &o
}

// buildOptions turns cfg into clickhouse-go options without connecting. It
// accepts clickhouse://user[:password]@host[:port][,host2[:port]]/db?params,
// where params are clickhouse-go's DSN parameters (secure, dial_timeout,
// compress …), protocol=native|http, allow_plaintext, and any other key as a
// server setting. It then enforces what tobira relies on:
//
//   - a missing port gets the protocol's default (9000, or 9440 with
//     secure=true; 8123 or 8443 over HTTP);
//   - a read-only connection drops any readonly setting of the DSN (Open
//     then adds readonly=1 unless the server already enforces readonly; see
//     openPool);
//   - clickhouse-go never logs (it would write over the terminal);
//   - a non-empty ConnConfig.Password replaces the DSN's password;
//   - without secure=true, over HTTP or over the native protocol with a
//     password, every host is loopback or the DSN says allow_plaintext=1,
//     and no host is ClickHouse Cloud (always TLS).
func buildOptions(cfg driver.ConnConfig) (*ch.Options, error) {
	fail := func(kind error, err error) (*ch.Options, error) {
		return nil, driver.Wrap(kind, Name, "open", err)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the clickhouse driver takes its settings from DSN parameters", k))
	}
	dsn := cfg.DSN
	if len(dsn) < len(urlPrefix) || !strings.EqualFold(dsn[:len(urlPrefix)], urlPrefix) {
		if sc := driver.Scheme(dsn); sc != "" {
			return fail(driver.ErrSyntax, fmt.Errorf("unsupported DSN scheme %q: use clickhouse://", sc))
		}
		return fail(driver.ErrSyntax, errors.New("invalid DSN: use clickhouse://user:password@host:port/database"))
	}
	rest := dsn[len(urlPrefix):]
	proto := ch.Native
	allowPlain := false
	if base, rawQuery, ok := strings.Cut(rest, "?"); ok {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return fail(driver.ErrSyntax, errors.New("invalid DSN: malformed query parameters"))
		}
		for k := range q {
			if !strings.EqualFold(k, protocolParam) {
				continue
			}
			switch v := strings.ToLower(q.Get(k)); v {
			case "native", "tcp":
			case "http":
				proto = ch.HTTP
			default:
				return fail(driver.ErrUnsupported, fmt.Errorf("unsupported protocol %q: use native or http", v))
			}
			q.Del(k)
		}
		for k := range q {
			if !strings.EqualFold(k, allowPlainParam) {
				continue
			}
			b, err := strconv.ParseBool(q.Get(k))
			if err != nil {
				return fail(driver.ErrSyntax, errors.New("invalid DSN: allow_plaintext must be true or false"))
			}
			allowPlain = b
			q.Del(k)
		}
		rest = base
		if enc := q.Encode(); enc != "" {
			rest += "?" + enc
		}
	}
	scheme := "clickhouse://"
	if proto == ch.HTTP {
		scheme = "http://"
		if secureParam(rest) {
			scheme = "https://"
		}
	}
	opts, err := ch.ParseDSN(scheme + rest)
	if err != nil {
		return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: %w", err))
	}
	opts.Protocol = proto
	for i, a := range opts.Addr {
		if opts.Addr[i], err = withPort(a, proto, opts.TLS != nil); err != nil {
			return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: %w", err))
		}
	}
	if len(opts.Addr) == 0 {
		return fail(driver.ErrSyntax, errors.New("invalid DSN: no host"))
	}
	if !cfg.Password.IsZero() {
		opts.Auth.Password = cfg.Password.Reveal()
	}
	// Plain HTTP always falls under the plaintext rule; the plain native
	// protocol only when it carries a password, since it sends it in clear.
	if opts.TLS == nil && (proto == ch.HTTP || opts.Auth.Password != "") {
		what := "plain HTTP"
		if proto == ch.Native {
			what = "a password over the plain native protocol"
		}
		for _, a := range opts.Addr {
			h, _, _ := net.SplitHostPort(a)
			switch netpolicy.CheckPlaintext(h, allowPlain, cloudZones...) {
			case netpolicy.RefusedCloud:
				return fail(driver.ErrSyntax, fmt.Errorf("%s is refused for ClickHouse Cloud (%s): add secure=true", what, h))
			case netpolicy.NeedsOptIn:
				return fail(driver.ErrSyntax, fmt.Errorf("%s to %s (not loopback) needs allow_plaintext=1, or secure=true for TLS", what, h))
			}
		}
	}
	if opts.Settings == nil {
		opts.Settings = ch.Settings{}
	}
	if cfg.ReadOnly {
		for k := range opts.Settings {
			if strings.EqualFold(k, readOnlyKey) {
				delete(opts.Settings, k)
			}
		}
	}
	opts.Debug, opts.Debugf, opts.Logger = false, nil, nil //nolint:staticcheck // the DSN can still set the deprecated Debug; clear it
	opts.ClientInfo.Products = append(opts.ClientInfo.Products, struct{ Name, Version string }{"tobira", version})
	return opts, nil
}

// transports records the HTTP transports clickhouse-go creates during Open.
type transports struct {
	mu  sync.Mutex
	all []*http.Transport
}

func (t *transports) track(rt *http.Transport) (http.RoundTripper, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.all = append(t.all, rt)
	// clickhouse-go's http.Client follows redirects, and Go forwards
	// X-ClickHouse-User and X-ClickHouse-Key (the credentials) to any host,
	// from https to http included. Without Location every 3xx comes back as
	// the reply; the ClickHouse HTTP interface never needs a redirect.
	return netpolicy.DropRedirectLocation(noTimeoutSetting{rt}), nil
}

// cloudZones is ClickHouse Cloud, which only speaks TLS.
var cloudZones = []string{"clickhouse.cloud"}

// noTimeoutSetting drops two settings clickhouse-go adds on its own over
// HTTP, which a user whose profile sets readonly=1 may not change, so every
// request would fail:
//
//   - max_execution_time, derived from a context deadline. The driver's own
//     calls carry no deadline (see detach), but clickhouse-go's HTTP hello
//     runs under its dial timeout. A cancelled context still stops the
//     request.
//   - http_write_exception_in_output_format=0, pinned by QueryFormat. It only
//     matters for formats that can carry an error in-band (JSON, XML), never
//     for the CSVWithNames that Export asks for.
type noTimeoutSetting struct{ rt http.RoundTripper }

// droppedParams are the HTTP parameters noTimeoutSetting removes.
var droppedParams = []string{maxExecutionTimeKey, "http_write_exception_in_output_format"}

// CloseIdleConnections lets http.Client.CloseIdleConnections (clickhouse-go
// calls it when a connection closes) reach the transport.
func (n noTimeoutSetting) CloseIdleConnections() {
	if c, ok := n.rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (n noTimeoutSetting) RoundTrip(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()
	found := false
	for _, k := range droppedParams {
		if q.Has(k) {
			q.Del(k)
			found = true
		}
	}
	if !found {
		return n.rt.RoundTrip(req)
	}
	r := req.Clone(req.Context())
	r.URL.RawQuery = q.Encode()
	return n.rt.RoundTrip(r)
}

func (t *transports) closeIdle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rt := range t.all {
		rt.CloseIdleConnections()
	}
}

// secureParam reports whether the DSN asks for TLS (secure=true).
func secureParam(rest string) bool {
	_, rawQuery, _ := strings.Cut(rest, "?")
	q, _ := url.ParseQuery(rawQuery)
	v, _ := strconv.ParseBool(q.Get("secure"))
	return v
}

// withPort adds the protocol's default port to a host without one and
// checks the port of one that has it.
func withPort(addr string, proto ch.Protocol, tls bool) (string, error) {
	if addr == "" {
		return "", errors.New("empty host")
	}
	h, port, err := net.SplitHostPort(addr)
	if err != nil { // no port
		h = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
		port = defaultPorts[proto][0]
		if tls {
			port = defaultPorts[proto][1]
		}
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
