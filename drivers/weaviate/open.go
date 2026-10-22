package weaviate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"

	"github.com/weaviate/weaviate-go-client/v5/weaviate/connection"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/data"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/db"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/schema"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const (
	defaultHTTPPort = 8080
	// maxResponseBytes caps one response body: a reply bigger than this is
	// an error, not an out-of-memory.
	maxResponseBytes = 64 << 20
	// defaultPageSize is the number of objects a streamed query reads per
	// request.
	defaultPageSize = 500
)

// dsnConfig is a parsed weaviate:// DSN.
type dsnConfig struct {
	scheme string // http or https
	host   string
	port   int // 0: the scheme's default port
	apiKey string
	// plaintextKey: an API key goes to a remote host over plain http
	// because scheme=http was given. Open logs a warning.
	plaintextKey bool
}

// hostPort is the host the client dials, with the port when one is set.
func (d dsnConfig) hostPort() string {
	if d.port == 0 {
		if strings.Contains(d.host, ":") {
			return "[" + d.host + "]"
		}
		return d.host
	}
	return net.JoinHostPort(d.host, strconv.Itoa(d.port))
}

// Open builds a client and reads the server's meta document once, so a bad
// address or a bad API key fail here rather than on the first query. Errors
// never contain the key.
func (weaviateDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	if d.plaintextKey {
		// The host's --debug log (ConnConfig.Logger; discarded when nil).
		// The key itself is never logged.
		cfg.Log().WarnContext(ctx, "weaviate: the API key is sent without TLS because scheme=http is set", "connection", cfg.Name, "host", d.host)
	}
	c := newConn(d, cfg, "")
	if err := c.check(ctx, "open"); err != nil {
		_ = c.Close()
		return nil, redact(err, cfg, d.apiKey)
	}
	version, err := c.meta(ctx, "open")
	if err != nil {
		_ = c.Close()
		return nil, redact(err, cfg, d.apiKey)
	}
	c.version.Store(version)
	// The provider read "" when it was built; refreshing it here, while
	// only Open holds the connection, makes its one write happen before
	// any concurrent use (its empty check reads without the lock).
	c.versions.Refresh()
	if !db.NewDBVersionSupport(db.NewVersionProvider(func() string { return version })).SupportsClassNameNamespacedEndpoints() {
		_ = c.Close()
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "open", fmt.Errorf("weaviate %q is not supported: tobira needs 1.14 or later", version))
	}
	return c, nil
}

// newConn creates the client without contacting the server. version is the
// server version, when already known.
func newConn(d dsnConfig, cfg driver.ConnConfig, version string) *conn {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	headers := map[string]string{}
	if d.apiKey != "" {
		headers["Authorization"] = "Bearer " + d.apiKey
	}
	// No redirect is followed: Go keeps the Authorization header on a
	// redirect to the same host name (any port, and from https to http) or
	// a subdomain, and the Weaviate REST API never needs one.
	con := connection.NewConnection(d.scheme, d.hostPort(), &http.Client{Transport: cappedTransport{tr}, CheckRedirect: netpolicy.RefuseRedirects}, 0, headers)
	// The client's finalizer sends on an unbuffered channel nobody reads
	// (it is meant for an OAuth refresh goroutine this driver never
	// starts), which would block the runtime's finalizer goroutine forever
	// once the connection is collected.
	runtime.SetFinalizer(con, nil)
	c := &conn{con: con, transport: tr, readOnly: cfg.ReadOnly, embedder: cfg.Embedder, pageSize: defaultPageSize}
	c.version.Store(version)
	vp := db.NewVersionProvider(func() string { v, _ := c.version.Load().(string); return v })
	c.versions = vp
	c.schema = schema.New(con, vp)
	c.data = data.New(con, db.NewDBVersionSupport(vp))
	return c
}

// meta reads /v1/meta and returns the server version.
func (c *conn) meta(ctx context.Context, op string) (string, error) {
	body, err := c.rest(ctx, op, http.MethodGet, "/meta", nil)
	if err != nil {
		return "", err
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", driver.Wrap(nil, Name, op, fmt.Errorf("unexpected meta document: %w", err))
	}
	return m.Version, nil
}

// rest sends one REST request through the client's connection and returns
// the body of a 2xx reply; any other status is mapped to a driver error.
func (c *conn) rest(ctx context.Context, op, method, path string, body any) ([]byte, error) {
	resp, err := c.con.RunREST(ctx, path, method, body)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(op, resp.StatusCode, resp.Body)
	}
	return resp.Body, nil
}

// cappedTransport fails a response whose body grows past maxResponseBytes.
type cappedTransport struct{ base http.RoundTripper }

func (t cappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
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

var errTooBig = fmt.Errorf("the response is larger than %d MiB: ask for fewer objects (limit) or fewer fields", maxResponseBytes>>20)

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errTooBig
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// parseDSN reads weaviate://[:apikey@]host[:port][?scheme=http|https]. The
// API key sits in the password position or in an api_key/apikey parameter,
// where every redactor masks it; a non-empty cfg.Password (a secret ref)
// replaces it. The scheme defaults to http on loopback and https elsewhere,
// and the port to 8080 for http and the scheme's own port for https. Plain
// http to a host that is not loopback also needs allow_plaintext=1, and
// Weaviate Cloud (*.weaviate.cloud, *.weaviate.network) is always https.
// ConnConfig.Options may set "scheme" and "allow_plaintext".
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if driver.Scheme(cfg.DSN) != "weaviate" {
		return fail("DSN must start with weaviate://")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN (check the escaping)") // url errors quote the DSN, key included
	}
	d := dsnConfig{host: u.Hostname()}
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
		return fail("a weaviate DSN has no path: name the collection in the query")
	}
	if u.User != nil {
		if u.User.Username() != "" {
			return fail("weaviate has no user names: put the API key in the password position (weaviate://:KEY@host) or the password field")
		}
		d.apiKey, _ = u.User.Password()
	}
	allowPlain := false
	setAllow := func(v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("allow_plaintext must be true or false")
		}
		allowPlain = b
		return nil
	}
	setScheme := func(v string) error {
		switch v = strings.ToLower(v); v {
		case "http", "https":
			d.scheme = v
			return nil
		}
		return errors.New("scheme must be http or https")
	}
	for k, vs := range u.Query() {
		switch strings.ToLower(k) {
		case "scheme":
			if err := setScheme(vs[len(vs)-1]); err != nil {
				return fail("%v", err)
			}
		case "allow_plaintext":
			if err := setAllow(vs[len(vs)-1]); err != nil {
				return fail("%v", err)
			}
		case "api_key", "apikey":
			if d.apiKey != "" || len(vs) > 1 {
				return fail("give the API key once")
			}
			d.apiKey = vs[0]
		default:
			return fail("unknown DSN parameter %q (scheme, allow_plaintext, api_key and apikey are accepted)", k)
		}
	}
	for k, v := range cfg.Options {
		var err error
		switch k {
		case "scheme":
			err = setScheme(v)
		case "allow_plaintext":
			err = setAllow(v)
		default:
			err = fmt.Errorf("unknown option %q (scheme and allow_plaintext are accepted)", k)
		}
		if err != nil {
			return fail("%v", err)
		}
	}
	if !cfg.Password.IsZero() {
		d.apiKey = cfg.Password.Reveal()
	}
	loopback := netpolicy.IsLoopback(d.host)
	if d.scheme == "" {
		d.scheme = "https"
		if loopback {
			d.scheme = "http"
		}
	}
	if d.scheme == "http" {
		switch netpolicy.CheckPlaintext(d.host, allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return fail("scheme=http is refused for Weaviate Cloud (%s)", d.host)
		case netpolicy.NeedsOptIn:
			return fail("plain http to %s (not loopback) needs allow_plaintext=1 as well as scheme=http", d.host)
		}
	}
	if d.scheme == "http" && d.port == 0 {
		d.port = defaultHTTPPort
	}
	d.plaintextKey = d.apiKey != "" && d.scheme == "http" && !loopback
	return d, nil
}

// cloudZones is Weaviate Cloud (weaviate.cloud and the older
// weaviate.network), which only speaks TLS.
var cloudZones = []string{"weaviate.cloud", "weaviate.network"}

// redact masks the DSN, the password and the API key in err's message,
// keeping its kind, code and driver attribution.
func redact(err error, cfg driver.ConnConfig, apiKey string) error {
	if err == nil {
		return nil
	}
	msg := driver.RedactMessage(err.Error(), cfg)
	if apiKey != "" {
		for _, form := range []string{apiKey, url.QueryEscape(apiKey), url.PathEscape(apiKey)} {
			msg = strings.ReplaceAll(msg, form, "xxxxx")
		}
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
