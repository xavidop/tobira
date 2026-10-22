package qdrant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const defaultPort = 6334

// dsnConfig is a parsed qdrant:// DSN.
type dsnConfig struct {
	host   string
	port   int
	apiKey string
	tls    bool
	// plaintextKey: an API key goes to a remote host without TLS because
	// tls=false was given explicitly. Open logs a warning.
	plaintextKey bool
	// tlsDefaulted: TLS is on only because the host is not loopback and
	// no tls was given, so a handshake failure gets a hint.
	tlsDefaulted bool
}

// Open builds a gRPC client and pings it once, so a bad address or a bad API
// key fail here rather than on the first query. Errors never contain the
// key.
func (qdrantDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	if d.plaintextKey {
		// The host's --debug log (ConnConfig.Logger; discarded when nil).
		// The key itself is never logged.
		cfg.Log().WarnContext(ctx, "qdrant: the API key is sent without TLS because tls=false is set", "connection", cfg.Name, "host", d.host)
	}
	c, err := newConn(d, cfg)
	if err != nil {
		return nil, redact(mapError(ctx, "open", err), cfg, d.apiKey)
	}
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		return nil, redact(d.tlsHint(err), cfg, d.apiKey)
	}
	return c, nil
}

// newConn creates the client without contacting the server (gRPC connects
// lazily).
func newConn(d dsnConfig, cfg driver.ConnConfig) (*conn, error) {
	client, err := pb.NewClient(&pb.Config{
		Host:   d.host,
		Port:   d.port,
		APIKey: d.apiKey,
		UseTLS: d.tls,
		// One connection is plenty for an interactive client, and the
		// version probe would add a round trip and a log line to every Open.
		PoolSize:               1,
		SkipCompatibilityCheck: true,
		// The client's warnings (an API key without TLS) must never reach
		// the terminal.
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return nil, err
	}
	return &conn{client: client, readOnly: cfg.ReadOnly, embedder: cfg.Embedder}, nil
}

// parseDSN reads qdrant://[user][:apikey@]host[:port][?tls=…&allow_plaintext=…].
// The API key sits in the password position, where every redactor masks it;
// a non-empty cfg.Password replaces it. ConnConfig.Options may set "tls" and
// "allow_plaintext". TLS is on unless the host is loopback or tls=false is
// given; plain gRPC to a host that is not loopback also needs
// allow_plaintext=1, and Qdrant Cloud (*.cloud.qdrant.io) is always TLS.
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if driver.Scheme(cfg.DSN) != "qdrant" {
		return fail("DSN must start with qdrant://")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, key included
	}
	d := dsnConfig{host: u.Hostname(), port: defaultPort}
	if d.host == "" {
		d.host = "localhost"
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fail("invalid port %q", p)
		}
		d.port = n
	}
	if u.Path != "" && u.Path != "/" {
		return fail("a qdrant DSN has no path: name the collection in the query")
	}
	if pw, ok := u.User.Password(); ok {
		d.apiKey = pw
	}
	if !cfg.Password.IsZero() {
		d.apiKey = cfg.Password.Reveal()
	}
	tlsSet, allowPlain := false, false
	setAllow := func(v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("allow_plaintext must be true or false")
		}
		allowPlain = b
		return nil
	}
	setTLS := func(v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("tls must be true or false")
		}
		d.tls, tlsSet = b, true
		return nil
	}
	for k, vs := range u.Query() {
		switch strings.ToLower(k) {
		case "tls":
			if err := setTLS(vs[len(vs)-1]); err != nil {
				return fail("%v", err)
			}
		case "allow_plaintext":
			if err := setAllow(vs[len(vs)-1]); err != nil {
				return fail("%v", err)
			}
		case "api_key", "api-key", "apikey", "key":
			return fail("put the API key in the password position (qdrant://:KEY@host) or the password field, not in parameter %q", k)
		default:
			return fail("unknown DSN parameter %q (tls and allow_plaintext are accepted)", k)
		}
	}
	for k, v := range cfg.Options {
		var err error
		switch k {
		case "tls":
			err = setTLS(v)
		case "allow_plaintext":
			err = setAllow(v)
		default:
			err = fmt.Errorf("unknown option %q (tls and allow_plaintext are accepted)", k)
		}
		if err != nil {
			return fail("%v", err)
		}
	}
	loopback := netpolicy.IsLoopback(d.host)
	if !tlsSet {
		d.tls, d.tlsDefaulted = !loopback, !loopback
	}
	if !d.tls {
		switch netpolicy.CheckPlaintext(d.host, allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return fail("tls=false is refused for Qdrant Cloud (%s)", d.host)
		case netpolicy.NeedsOptIn:
			return fail("plain gRPC to %s (not loopback) needs allow_plaintext=1 as well as tls=false", d.host)
		}
	}
	d.plaintextKey = d.apiKey != "" && !d.tls && !loopback
	return d, nil
}

// cloudZones is Qdrant Cloud (any provider and region), which only speaks
// TLS.
var cloudZones = []string{"cloud.qdrant.io"}

// tlsHint adds the opt-in to an Open error that looks like a TLS handshake
// against a plain gRPC server, when TLS was only the default: a DSN that
// reached such a server in clear before TLS became the default for remote
// hosts fails here, and the error says how to opt back in.
func (d dsnConfig) tlsHint(err error) error {
	var de *driver.Error
	if !d.tlsDefaulted || !netpolicy.IsTLSMismatch(err) || !errors.As(err, &de) {
		return err
	}
	out := *de
	out.Err = fmt.Errorf("%w; TLS is the default for a host that is not loopback: if the server speaks plain gRPC, add tls=false&allow_plaintext=1", de.Err)
	return &out
}

// redact masks the DSN, the password and the API key in err's message,
// keeping its kind and driver attribution.
func redact(err error, cfg driver.ConnConfig, apiKey string) error {
	if err == nil {
		return nil
	}
	msg := driver.RedactMessage(err.Error(), cfg)
	if apiKey != "" {
		msg = strings.ReplaceAll(msg, apiKey, "xxxxx")
		msg = strings.ReplaceAll(msg, url.QueryEscape(apiKey), "xxxxx")
	}
	out := &driver.Error{Kind: driver.KindOf(err), Driver: Name, Op: "open", Err: errors.New(msg)}
	var de *driver.Error
	if errors.As(err, &de) {
		out.Code = de.Code
		if de.Op != "" {
			out.Op = de.Op
		}
		// Keep context identity (errors.Is(err, context.Canceled)).
		if ctxErr := contextCause(de.Err); ctxErr != nil {
			out.Err = errors.Join(out.Err, ctxErr)
		}
	}
	return out
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
