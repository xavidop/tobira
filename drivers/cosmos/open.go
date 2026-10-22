package cosmos

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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const (
	// openTimeout bounds Open's first request when ctx has no deadline.
	openTimeout = 15 * time.Second
	// requestTimeout caps one HTTP attempt, as the SDK's own client does.
	requestTimeout = 65 * time.Second
	// maxRetries bounds the SDK's retries of one call, so an unreachable
	// account fails in seconds rather than minutes.
	maxRetries = 2
)

// dsnConfig is a parsed cosmos:// DSN.
type dsnConfig struct {
	endpoint string // https://host:port/ (http:// with tls=false)
	database string // the default database; "" when the DSN names none
	key      string // the account key from key= or accountkey=, if any
	insecure bool   // tls=false
}

// errDSN is the generic DSN error: url.Parse quotes the whole DSN, key
// included, so its message is never shown.
var errDSN = errors.New("the DSN must look like cosmos://host[:port][?database=name][&tls=false]")

// parseDSN reads cosmos://host[:port][/][?database=…&tls=false&allow_plaintext=1&key=…].
// tls=false (plain http) is allowed for localhost or a loopback address,
// where the emulator runs; any other host also needs allow_plaintext=1, and
// Azure's Cosmos DB endpoints (*.documents.azure.com and the sovereign
// clouds' zones) are always TLS.
// The account key may come from key= or accountkey= (any case), though a
// password ref is the documented way. No error quotes a value from the
// DSN.
func parseDSN(dsn string) (dsnConfig, error) {
	fail := func(err error) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", err)
	}
	if !strings.EqualFold(driver.Scheme(dsn), "cosmos") {
		return fail(errDSN)
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Opaque != "" {
		return fail(errDSN)
	}
	switch {
	case u.User != nil:
		return fail(errors.New("the DSN must not carry credentials before the host: give the account key as a password ref"))
	case u.Host == "" || u.Hostname() == "":
		return fail(errors.New("the DSN names no account host"))
	case u.Path != "" && u.Path != "/":
		return fail(errors.New("the DSN path must be empty: name the database with ?database="))
	case u.Fragment != "" || strings.Contains(dsn, "#"):
		return fail(errors.New("the DSN must not have a #fragment"))
	}
	d := dsnConfig{}
	seen := map[string]bool{}
	tls, allowPlain := "", ""
	for kv := range strings.SplitSeq(u.RawQuery, "&") {
		if kv == "" {
			continue
		}
		rk, rv, _ := strings.Cut(kv, "=")
		k, err := url.PathUnescape(rk)
		if err != nil {
			return fail(errors.New("the DSN has a badly escaped parameter name"))
		}
		v, err := url.PathUnescape(rv)
		if err != nil {
			return fail(fmt.Errorf("parameter %q is badly escaped", k))
		}
		k = strings.ToLower(k)
		canon := k
		if k == "accountkey" {
			canon = "key"
		}
		if seen[canon] {
			return fail(fmt.Errorf("parameter %q is given twice", canon))
		}
		seen[canon] = true
		switch k {
		case "database":
			if v == "" {
				return fail(errors.New("parameter database is empty"))
			}
			d.database = v
		case "tls":
			tls = v
		case "allow_plaintext":
			allowPlain = v
		case "key", "accountkey":
			d.key = v
		default:
			return fail(fmt.Errorf("unknown parameter %q (known: database, tls, allow_plaintext, key)", k))
		}
	}
	plain := false
	if allowPlain != "" {
		b, err := strconv.ParseBool(allowPlain)
		if err != nil {
			return fail(errors.New("parameter allow_plaintext must be true or false"))
		}
		plain = b
	}
	scheme := "https"
	switch strings.ToLower(tls) {
	case "", "true":
	case "false":
		switch netpolicy.CheckPlaintext(u.Hostname(), plain, azureZones...) {
		case netpolicy.RefusedCloud:
			return fail(errors.New("tls=false is refused for Azure Cosmos DB endpoints: the signed requests and the data would cross the network unencrypted"))
		case netpolicy.NeedsOptIn:
			return fail(errors.New("tls=false to a host that is not this machine also needs allow_plaintext=1: the signed requests and the data would cross the network unencrypted"))
		}
		scheme, d.insecure = "http", true
	default:
		return fail(errors.New("parameter tls must be true or false"))
	}
	host := u.Host
	if u.Port() == "" {
		port := "443"
		if d.insecure {
			port = "8081"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	d.endpoint = scheme + "://" + host + "/"
	return d, nil
}

// azureZones are the DNS zones of Azure's Cosmos DB endpoints (public,
// China and US Government clouds). An account's regional endpoints live in
// the same zone as the account endpoint.
var azureZones = []string{"documents.azure.com", "documents.azure.cn", "documents.azure.us"}

// azureZone returns the Cosmos DB zone host belongs to, or "" when it is
// not an Azure endpoint. The comparison ignores case and a trailing dot.
func azureZone(host string) string {
	for _, z := range azureZones {
		if netpolicy.InCloudZone(host, z) {
			return z
		}
	}
	return ""
}

// endpointGuard is the transport of every connection. The SDK sends
// requests to the regional endpoints the account read names, and each one
// carries the account-key signature, so a request may only go to the DSN's
// own scheme and host:port, or, for an Azure account, to another https
// host in the same Cosmos DB zone. Any other target is refused before a
// byte is sent, whatever the server advertised.
type endpointGuard struct {
	next   http.RoundTripper
	scheme string
	host   string // origin host:port, see originHost
	zone   string // azureZone of the endpoint; "" outside Azure
}

func newEndpointGuard(endpoint *url.URL, next http.RoundTripper) endpointGuard {
	return endpointGuard{next: next, scheme: endpoint.Scheme, host: originHost(endpoint), zone: azureZone(endpoint.Hostname())}
}

// originHost is u's host:port in lower case, with the scheme's default
// port (443 for https, 80 for http) filled in, so https://h/ and
// https://h:443/ compare as the same origin.
func originHost(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// allows reports whether a signed request may go to u.
func (g endpointGuard) allows(u *url.URL) bool {
	if u.Scheme != g.scheme {
		return false
	}
	if originHost(u) == g.host {
		return true
	}
	return g.zone != "" && u.Scheme == "https" && azureZone(u.Hostname()) == g.zone
}

func (g endpointGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if !g.allows(r.URL) {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, fmt.Errorf("refusing to send a signed request to %s://%s: it is not the account endpoint", r.URL.Scheme, r.URL.Host)
	}
	return g.next.RoundTrip(r)
}

// Open builds a client from cfg and makes one request (the default
// database's read, or a one-database listing), so a bad endpoint or a
// wrong key fails here. The account key is ConnConfig.Password, or the
// DSN's key=; without one Open fails with ErrAuth (Entra ID sign-in is
// not supported yet).
func (cosmosDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := open(ctx, cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	return c, nil
}

func open(ctx context.Context, cfg driver.ConnConfig) (*conn, error) {
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "open", fmt.Errorf("unsupported option %q: the cosmos driver takes its settings from the DSN", k))
	}
	d, err := parseDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	key := d.key
	if !cfg.Password.IsZero() {
		key = cfg.Password.Reveal()
	}
	if key == "" {
		return nil, driver.Wrap(driver.ErrAuth, Name, "open", errors.New("no account key: set the connection's password ref to the Cosmos DB account key"))
	}
	cred, err := azcosmos.NewKeyCredential(key)
	if err != nil {
		// The SDK's message may quote the key: never show it.
		return nil, driver.Wrap(driver.ErrAuth, Name, "open", errors.New("the account key is not valid base64"))
	}
	endpointURL, err := url.Parse(d.endpoint)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "open", errDSN)
	}
	if d.insecure {
		cfg.Log().Warn("cosmos: requests are signed with the account key but sent without TLS (tls=false)", "endpoint", d.endpoint)
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	// No redirect is followed: Go keeps the Authorization header (the
	// account-key signature) on a redirect to the same host name, and the
	// Cosmos DB REST API never needs one.
	opts := &azcosmos.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: &http.Client{Transport: newEndpointGuard(endpointURL, tr), CheckRedirect: netpolicy.RefuseRedirects, Timeout: requestTimeout},
		Telemetry: policy.TelemetryOptions{Disabled: true},
		Retry:     policy.RetryOptions{MaxRetries: maxRetries},
	}}
	client, err := azcosmos.NewClientWithKey(d.endpoint, cred, opts)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, driver.Wrap(driver.ErrConnection, Name, "open", errors.New("cannot build a client for the endpoint"))
	}
	pctx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, openTimeout)
		defer cancel()
	}
	if err := ping(pctx, client, d.database); err != nil {
		client.Close()
		tr.CloseIdleConnections()
		if errors.Is(err, driver.ErrNotFound) && d.database != "" {
			var de *driver.Error
			if errors.As(err, &de) {
				de.Op = "open"
				de.Err = fmt.Errorf("database %q does not exist: %w", d.database, de.Err)
			}
		}
		return nil, err
	}
	c := &conn{client: client, transport: tr, defaultDB: d.database, readOnly: cfg.ReadOnly, embedder: cfg.Embedder}
	c.catalog = newCatalogCache(connCatalog{c}, catalogTTL)
	c.props = newPropsCache()
	return c, nil
}

// scrubError masks the account key (from the password or the DSN, in any
// spelling) and the DSN's other secrets in the message of a *driver.Error,
// keeping its kind and code. The masked error unwraps only to a context
// error, never to the original, whose text may hold the key.
func scrubError(err error, cfg driver.ConnConfig) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	rc := cfg
	if rc.Password.IsZero() {
		if d, perr := parseDSN(cfg.DSN); perr == nil && d.key != "" {
			rc.Password = driver.NewSecret(d.key)
		}
	}
	msg := de.Err.Error()
	clean := driver.RedactMessage(msg, rc)
	if clean != msg {
		de.Err = maskedError{msg: clean, ctx: contextErr(de.Err)}
	}
	return err
}

// contextErr returns the context error err wraps, if any.
func contextErr(err error) error {
	for _, ce := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, ce) {
			return ce
		}
	}
	return nil
}

// maskedError shows a masked message; it unwraps only to a context error.
type maskedError struct {
	msg string
	ctx error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.ctx }
