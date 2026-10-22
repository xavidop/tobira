package pinecone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

// cloudControlPlane is the control plane of the Pinecone service.
const cloudControlPlane = "https://api.pinecone.io"

// maxResponseBytes caps one control-plane response body.
const maxResponseBytes = 8 << 20

// The index metrics, as Pinecone spells them.
const (
	metricCosine    = "cosine"
	metricEuclidean = "euclidean"
	metricDot       = "dotproduct"
)

// dsnConfig is a parsed pinecone:// DSN. Exactly one of control and host is
// set: control is the control-plane URL (a project: every index is listed),
// host the URL of one index (host mode: no control-plane call is made).
type dsnConfig struct {
	control string
	host    string
	index   string // host mode: the namespace name of the index
	metric  string // host mode: the index metric, when the server does not report it
	apiKey  string
	// plaintextKey: an API key goes to a remote host without TLS because
	// tls=false was given explicitly. Open logs a warning.
	plaintextKey bool
}

// Open builds the client and pings it once, so a bad address or a bad API
// key fail here rather than on the first query. Errors never contain the
// key.
func (pineconeDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	if d.plaintextKey {
		// The host's --debug log (ConnConfig.Logger; discarded when nil).
		// The key itself is never logged.
		cfg.Log().WarnContext(ctx, "pinecone: the API key is sent without TLS because tls=false is set", "connection", cfg.Name)
	}
	c, err := newConn(d, cfg)
	if err != nil {
		return nil, redact(mapError(ctx, "open", err), cfg, d.apiKey)
	}
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		return nil, redact(err, cfg, d.apiKey)
	}
	return c, nil
}

// newConn creates the clients without contacting the server.
func newConn(d dsnConfig, cfg driver.ConnConfig) (*conn, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Control-plane calls are rare (the index list, one lookup per index),
	// so no connection is kept alive: none outlives Close, even one a
	// response hands back to the pool after Close has run.
	transport.DisableKeepAlives = true
	// No redirect is followed: Go forwards custom headers such as Api-Key
	// to the target, even on another host or over plain http, and the
	// Pinecone REST API never needs one. (The data plane is gRPC.)
	hc := &http.Client{Transport: cappedTransport{transport}, CheckRedirect: netpolicy.RefuseRedirects}
	headers := map[string]string{}
	if d.apiKey != "" {
		headers["Api-Key"] = d.apiKey
	}
	control := d.control
	if control == "" {
		control = cloudControlPlane // never called in host mode
	}
	// NewClientBase, not NewClient: it never falls back to the
	// PINECONE_API_KEY environment variable, and Host is always given, so
	// PINECONE_CONTROLLER_HOST is ignored too.
	client, err := pinecone.NewClientBase(pinecone.NewClientBaseParams{Host: control, Headers: headers, RestClient: hc})
	if err != nil {
		return nil, err
	}
	return &conn{
		dsn: d, cfg: cfg, apiKey: d.apiKey, client: client, http: hc,
		readOnly: cfg.ReadOnly, embedder: cfg.Embedder, indexes: map[string]*indexHandle{},
	}, nil
}

// cappedTransport fails a response body that grows past maxResponseBytes.
type cappedTransport struct{ next http.RoundTripper }

func (t cappedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, left: maxResponseBytes}
	return resp, nil
}

type cappedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, driver.Wrap(driver.ErrLimit, Name, "read", fmt.Errorf("the server response is too large (over %d MiB)", maxResponseBytes>>20))
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// parseDSN reads
//
//	pinecone://[:apikey@][control-host[:port]][?tls=…&allow_plaintext=…&api_key=…]
//	pinecone://[:apikey@]?host=index-host[:port][&index=name][&metric=…][&tls=…][&allow_plaintext=…]
//
// The first form is a project, reached through its control plane
// (api.pinecone.io when no host is given); the second is one index host,
// with no control-plane call. The API key may sit in the password position,
// in an api_key (or apikey) parameter, both of which every redactor masks,
// or in the password field, which wins. ConnConfig.Options may set tls,
// allow_plaintext, host, index and metric. TLS is on unless tls=false; plain
// http to a host that is not loopback also needs allow_plaintext=1, and
// Pinecone's cloud (*.pinecone.io) is always TLS.
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if driver.Scheme(cfg.DSN) != "pinecone" {
		return fail("DSN must start with pinecone://")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, key included
	}
	if u.Path != "" && u.Path != "/" {
		return fail("a pinecone DSN has no path: name the index with host= or in the query")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fail("invalid port %q", p)
		}
	}
	var d dsnConfig
	if pw, ok := u.User.Password(); ok {
		d.apiKey = pw
	}
	tls, allowPlain := true, false
	var host, index, metric string
	var hostSet, indexSet, metricSet bool
	set := func(k, v string) error {
		switch k {
		case "tls":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("tls must be true or false")
			}
			tls = b
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("allow_plaintext must be true or false")
			}
			allowPlain = b
		case "host":
			host, hostSet = v, true
		case "index":
			index, indexSet = v, true
		case "metric":
			metric, metricSet = v, true
		default:
			return fmt.Errorf("unknown option %q (tls, allow_plaintext, host, index and metric are accepted)", k)
		}
		return nil
	}
	for k, vs := range u.Query() {
		v := vs[len(vs)-1]
		switch lk := strings.ToLower(k); lk {
		case "api_key", "apikey", "api-key":
			d.apiKey = v
		case "tls", "allow_plaintext", "host", "index", "metric":
			if err := set(lk, v); err != nil {
				return fail("%v", err)
			}
		default:
			return fail("unknown DSN parameter %q (tls, allow_plaintext, host, index, metric and api_key are accepted)", k)
		}
	}
	for k, v := range cfg.Options {
		if err := set(k, v); err != nil {
			return fail("%v", err)
		}
	}
	if !cfg.Password.IsZero() {
		d.apiKey = cfg.Password.Reveal()
	}
	scheme := "https://"
	if !tls {
		scheme = "http://"
	}
	target := u.Hostname()
	switch {
	case hostSet && u.Host != "":
		return fail("give a control-plane host or host= (one index), not both")
	case hostSet:
		if host == "" || strings.Contains(host, "://") || strings.ContainsAny(host, "/?#@") {
			return fail("give host= as host[:port], without a scheme or path (tls=false for plaintext)")
		}
		h, port, err := net.SplitHostPort(host)
		if err != nil {
			h, port = host, ""
		}
		if port != "" {
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return fail("invalid port %q in host=", port)
			}
		}
		target = h
		d.host = scheme + host
		d.index = "default"
		if indexSet {
			if index == "" {
				return fail("index must not be empty")
			}
			d.index = index
		}
		if metricSet {
			switch metric {
			case metricCosine, metricEuclidean, metricDot:
				d.metric = metric
			default:
				return fail("metric must be cosine, euclidean or dotproduct")
			}
		}
	case indexSet || metricSet:
		return fail("index and metric go with host= (one index host); a project reads them from the control plane")
	case u.Host != "":
		d.control = scheme + u.Host
	default:
		d.control = strings.Replace(cloudControlPlane, "https://", scheme, 1)
		target = "api.pinecone.io"
	}
	if !tls {
		switch netpolicy.CheckPlaintext(target, allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return fail("tls=false is refused for Pinecone's cloud (*.pinecone.io): the API key would cross the network in clear")
		case netpolicy.NeedsOptIn:
			return fail("plain http to %s (not loopback) needs allow_plaintext=1 as well as tls=false", target)
		}
	}
	d.plaintextKey = d.apiKey != "" && !tls && !netpolicy.IsLoopback(target)
	return d, nil
}

// cloudZones is Pinecone's cloud, which only speaks TLS.
var cloudZones = []string{"pinecone.io"}

// redact masks the DSN, the password and the API key in err's message,
// keeping its kind, code and driver attribution.
func redact(err error, cfg driver.ConnConfig, apiKey string) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	var inner *driver.Error
	if errors.As(err, &inner) && inner.Err != nil {
		text = inner.Err.Error() // without the "pinecone: op:" prefix and kind, which out adds again
	}
	msg := driver.RedactMessage(text, cfg)
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
