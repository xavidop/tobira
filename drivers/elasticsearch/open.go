package elasticsearch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	es "github.com/elastic/go-elasticsearch/v8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const defaultPort = 9200

// dsnConfig is a parsed elasticsearch:// or opensearch:// DSN.
type dsnConfig struct {
	tls        bool
	host       string
	port       int
	user       string
	password   string // basic auth, with user
	apiKey     string // base64 API key, when there is no user
	caFile     string
	skipVerify bool
	// allowSystem: allow_system_indices=1 lets read-only paths reach
	// system, dot-prefixed and hidden indexes.
	allowSystem bool
	// plaintextCred: credentials go to a remote host without TLS because
	// tls=false was given. Open logs a warning.
	plaintextCred bool
}

func (d dsnConfig) address() string {
	scheme := "https"
	if !d.tls {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(d.host, strconv.Itoa(d.port))
}

// Open builds a client and pings the cluster once, so a bad address or bad
// credentials fail here rather than on the first query. Errors never
// contain the password or the API key.
func (d esDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	name := d.fl.name()
	dc, err := parseDSN(cfg, d.fl)
	if err != nil {
		return nil, redact(err, name, cfg, dc)
	}
	if dc.plaintextCred {
		// The host's --debug log; credentials are never logged.
		cfg.Log().WarnContext(ctx, name+": credentials are sent without TLS because tls=false is set", "connection", cfg.Name, "host", dc.host)
	}
	if dc.skipVerify {
		cfg.Log().WarnContext(ctx, name+": the server certificate is not verified because tls_skip_verify is set", "connection", cfg.Name, "host", dc.host)
	}
	c, err := newConn(d.fl, dc, cfg)
	if err != nil {
		return nil, redact(err, name, cfg, dc)
	}
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		return nil, redact(err, name, cfg, dc)
	}
	return c, nil
}

// newConn creates the client without contacting the server.
func newConn(fl flavor, d dsnConfig, cfg driver.ConnConfig) (*conn, error) {
	name := fl.name()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: d.skipVerify} //nolint:gosec // opt-in tls_skip_verify, warned about in Open
	if d.caFile != "" {
		pem, err := os.ReadFile(d.caFile)
		if err != nil {
			return nil, driver.Wrap(driver.ErrConnection, name, "open", errors.New("cannot read ca_file"))
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, driver.Wrap(driver.ErrConnection, name, "open", errors.New("ca_file holds no PEM certificate"))
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	esCfg := es.Config{
		Addresses: []string{d.address()},
		Username:  d.user,
		Password:  d.password,
		APIKey:    d.apiKey,
		Transport: tr,
		// Retries would replay writes and hide a dead node behind delays;
		// the TUI shows the error and the user retries.
		DisableRetry: true,
	}
	if fl == flavorOpenSearch {
		// go-elasticsearch refuses servers without Elastic's product
		// header. OpenSearch speaks the same REST API for everything
		// this driver sends, so the opensearch driver vouches for it.
		esCfg.Interceptors = []elastictransport.InterceptorFunc{productHeader}
	}
	client, err := es.NewBaseClient(esCfg)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, driver.Wrap(driver.ErrConnection, name, "open", err)
	}
	return &conn{fl: fl, client: client, transport: tr, readOnly: cfg.ReadOnly, allowSystem: d.allowSystem, embedder: cfg.Embedder, pageSize: defaultPageSize}, nil
}

// productHeader marks every successful response as Elasticsearch's, for
// the opensearch driver only.
func productHeader(next elastictransport.RoundTripFunc) elastictransport.RoundTripFunc {
	return func(req *http.Request) (*http.Response, error) {
		res, err := next(req)
		if res != nil && res.Header != nil && res.Header.Get("X-Elastic-Product") == "" {
			res.Header.Set("X-Elastic-Product", "Elasticsearch")
		}
		return res, err
	}
}

// parseDSN reads
//
//	elasticsearch://[user[:password]@]host[:port][?tls=…&allow_plaintext=…&ca_file=…&tls_skip_verify=…]
//
// (opensearch:// for the opensearch driver). With a user the password is
// for basic auth; without one, a password is an API key (its base64 form,
// or id:key, which is encoded). A non-empty cfg.Password replaces the
// DSN's. TLS is on unless tls=false; plain http to a host that is not
// loopback also needs allow_plaintext=1, and the managed services (Elastic
// Cloud, Amazon OpenSearch) are always TLS. ConnConfig.Options may set tls,
// allow_plaintext, ca_file and tls_skip_verify too.
func parseDSN(cfg driver.ConnConfig, fl flavor) (dsnConfig, error) {
	name := fl.name()
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, name, "open", fmt.Errorf(format, args...))
	}
	if driver.Scheme(cfg.DSN) != name {
		return fail("DSN must start with %s://", name)
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, password included
	}
	d := dsnConfig{tls: true, host: u.Hostname(), port: defaultPort}
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
		return fail("a %s DSN has no path: name the index in the query", name)
	}
	secret := ""
	if u.User != nil {
		d.user = u.User.Username()
		secret, _ = u.User.Password()
	}
	if !cfg.Password.IsZero() {
		secret = cfg.Password.Reveal()
	}
	if d.user != "" {
		d.password = secret
	} else if secret != "" {
		d.apiKey = secret
		if strings.Contains(secret, ":") {
			d.apiKey = base64.StdEncoding.EncodeToString([]byte(secret))
		}
	}
	allowPlain := false
	set := func(k, v string) error {
		switch strings.ToLower(k) {
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("allow_plaintext must be true or false")
			}
			allowPlain = b
		case "tls":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("tls must be true or false")
			}
			d.tls = b
		case "ca_file":
			d.caFile = v
		case "tls_skip_verify":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("tls_skip_verify must be true or false")
			}
			d.skipVerify = b
		case "allow_system_indices":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("allow_system_indices must be 1, 0, true or false")
			}
			d.allowSystem = b
		case "api_key", "apikey", "api-key", "key", "password", "token":
			return fmt.Errorf("put the secret in the password position or the password field, not in parameter %q", k)
		default:
			return fmt.Errorf("unknown parameter %q (tls, allow_plaintext, ca_file, tls_skip_verify and allow_system_indices are accepted)", k)
		}
		return nil
	}
	for k, vs := range u.Query() {
		if err := set(k, vs[len(vs)-1]); err != nil {
			return fail("%v", err)
		}
	}
	for k, v := range cfg.Options {
		if err := set(k, v); err != nil {
			return fail("%v", err)
		}
	}
	if !d.tls && (d.caFile != "" || d.skipVerify) {
		return fail("ca_file and tls_skip_verify need TLS, but tls=false is set")
	}
	if !d.tls {
		switch netpolicy.CheckPlaintext(d.host, allowPlain, managedZones...) {
		case netpolicy.RefusedCloud:
			return fail("tls=false is refused for a managed service (%s)", d.host)
		case netpolicy.NeedsOptIn:
			return fail("plain http to %s (not loopback) needs allow_plaintext=1 as well as tls=false", d.host)
		}
	}
	if !d.tls && secret != "" && !netpolicy.IsLoopback(d.host) {
		d.plaintextCred = true
	}
	return d, nil
}

// managedZones are the DNS zones of the managed services, which only speak
// TLS: Elastic Cloud (elastic-cloud.com, the older cloud.es.io, and
// Serverless on elastic.cloud) and Amazon OpenSearch Service (global and
// China regions) and Serverless.
var managedZones = []string{"elastic-cloud.com", "cloud.es.io", "elastic.cloud", "es.amazonaws.com", "es.amazonaws.com.cn", "aoss.amazonaws.com"}

// redact masks the DSN, the password and the API key in err's message,
// keeping its kind, code and driver attribution.
func redact(err error, name string, cfg driver.ConnConfig, d dsnConfig) error {
	if err == nil {
		return nil
	}
	msg := driver.RedactMessage(err.Error(), cfg)
	for _, s := range []string{d.password, d.apiKey, cfg.Password.Reveal()} {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "xxxxx")
			msg = strings.ReplaceAll(msg, url.QueryEscape(s), "xxxxx")
		}
	}
	out := &driver.Error{Kind: driver.KindOf(err), Driver: name, Op: "open", Err: errors.New(msg)}
	var de *driver.Error
	if errors.As(err, &de) {
		out.Code = de.Code
		if de.Op != "" {
			out.Op = de.Op
		}
		if ctxErr := contextCause(de.Err); ctxErr != nil {
			out.Err = errors.Join(out.Err, ctxErr)
		}
	}
	return out
}

// contextCause returns context.Canceled or context.DeadlineExceeded when
// err wraps one.
func contextCause(err error) error {
	for _, c := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, c) {
			return c
		}
	}
	return nil
}
