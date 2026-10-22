package bigquery

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/auth/credentials"
	bq "cloud.google.com/go/bigquery"
	bqapi "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// defaultMaxBytesBilled is the maximumBytesBilled every query job carries
// unless the DSN sets max_bytes_billed: 1 GiB.
const defaultMaxBytesBilled = 1 << 30

// dsnConfig is a parsed DSN:
//
//	bigquery://project[/dataset][?location=…&max_bytes_billed=…&emulator=host:port]
type dsnConfig struct {
	project        string
	dataset        string // default dataset; "" = none
	location       string // job location; "" = BigQuery's choice
	emulator       string // host:port of an emulator: plain HTTP, no credentials
	maxBytesBilled int64  // 0 = no cap
}

var (
	errMalformedDSN = errors.New("malformed DSN: want bigquery://project[/dataset][?location=…&max_bytes_billed=…]")
	// errDSNCredentials refuses a DSN that carries a credential. It never
	// quotes the DSN.
	errDSNCredentials = errors.New("credentials must not be in the DSN: put the service-account JSON in the connection's password (a secret ref)")
)

// parseDSN parses a bigquery:// DSN. Errors never quote the DSN, which
// could hold a pasted credential.
func parseDSN(dsn string) (dsnConfig, error) {
	const scheme = "bigquery://"
	if len(dsn) < len(scheme) || !strings.EqualFold(dsn[:len(scheme)], scheme) {
		return dsnConfig{}, errMalformedDSN
	}
	authority, _, _ := strings.Cut(dsn[len(scheme):], "/")
	authority, _, _ = strings.Cut(authority, "?")
	if strings.Contains(authority, "@") {
		return dsnConfig{}, errDSNCredentials
	}
	u, err := url.Parse("bigquery://" + dsn[len(scheme):])
	if err != nil { // url errors quote the input
		return dsnConfig{}, errMalformedDSN
	}
	if u.User != nil {
		return dsnConfig{}, errDSNCredentials
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return dsnConfig{}, errMalformedDSN
	}
	for k := range query {
		if credentialKey(k) {
			return dsnConfig{}, errDSNCredentials
		}
	}
	if u.Host == "" || u.Port() != "" || u.Fragment != "" || strings.HasSuffix(dsn, "#") || u.Opaque != "" {
		return dsnConfig{}, errMalformedDSN
	}
	d := dsnConfig{project: u.Hostname(), dataset: strings.Trim(u.Path, "/"), maxBytesBilled: defaultMaxBytesBilled}
	if strings.Contains(d.dataset, "/") {
		return dsnConfig{}, errors.New("malformed DSN: the path holds one dataset at most")
	}
	for k, vs := range query {
		name := strings.ToLower(k)
		if name != "location" && name != "max_bytes_billed" && name != "emulator" {
			// Not quoted: a stray parameter could be a pasted secret.
			return dsnConfig{}, errors.New("malformed DSN: unknown parameter (known: location, max_bytes_billed, emulator)")
		}
		if len(vs) != 1 || vs[0] == "" {
			return dsnConfig{}, errors.New("malformed DSN: " + name + " must have one non-empty value")
		}
		v := vs[0]
		switch name {
		case "location":
			d.location = v
		case "max_bytes_billed":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return dsnConfig{}, errors.New("malformed DSN: max_bytes_billed must be a byte count (0 for no cap)")
			}
			d.maxBytesBilled = n
		case "emulator":
			if host, port, err := net.SplitHostPort(v); err != nil || host == "" || port == "" || strings.ContainsAny(v, "/@?#") {
				return dsnConfig{}, errors.New("malformed DSN: emulator must be host:port")
			}
			d.emulator = v
		}
	}
	return d, nil
}

// credentialKey reports whether a DSN parameter name looks like it carries
// a credential. Any unknown parameter is refused anyway; these get the
// error that says where credentials go.
func credentialKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"credential", "password", "passwd", "pwd", "secret", "token", "key", "auth", "pass"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// serviceAccount is the part of a service-account key the driver checks.
type serviceAccount struct {
	Type         string `json:"type"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	ClientEmail  string `json:"client_email"`
}

// parseServiceAccount checks that pw is a service-account key. Only that
// type is accepted: other credential files (external_account, for one)
// can make the auth library run a program or call arbitrary URLs. Errors
// never quote the key.
func parseServiceAccount(pw string) (serviceAccount, error) {
	var sa serviceAccount
	if err := json.Unmarshal([]byte(pw), &sa); err != nil {
		return serviceAccount{}, errors.New("the password is not service-account JSON")
	}
	if sa.Type != "service_account" {
		return serviceAccount{}, errors.New(`the password must be a service-account key ("type": "service_account")`)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return serviceAccount{}, errors.New("the service-account key lacks client_email or private_key")
	}
	if b, _ := pem.Decode([]byte(sa.PrivateKey)); b == nil {
		return serviceAccount{}, errors.New("the service-account private_key is not a PEM key")
	}
	return sa, nil
}

// secretFragments lists the parts of a key that must never reach an error
// message: the key id, the PEM key, its JSON-escaped form and each of its
// base64 lines.
func secretFragments(sa serviceAccount) []string {
	if sa.PrivateKey == "" {
		return nil
	}
	out := []string{sa.PrivateKey, strings.ReplaceAll(sa.PrivateKey, "\n", `\n`)}
	if sa.PrivateKeyID != "" {
		out = append(out, sa.PrivateKeyID)
	}
	for line := range strings.SplitSeq(sa.PrivateKey, "\n") {
		if line = strings.TrimSpace(line); len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			out = append(out, line)
		}
	}
	return out
}

// scrubMessage masks the DSN's and the password's credentials
// (driver.RedactMessage) and every key fragment in msg.
func scrubMessage(msg string, cfg driver.ConnConfig, frags []string) string {
	msg = driver.RedactMessage(msg, cfg)
	for _, f := range frags {
		if len(f) >= 8 {
			msg = strings.ReplaceAll(msg, f, "xxxxx")
		}
	}
	return msg
}

// scrubError masks credentials in a *driver.Error's message. The masked
// error does not unwrap to the original, whose nested parts (an API
// error's raw Body, for one) were not masked; only a context error stays
// reachable, so errors.Is(err, context.Canceled) still holds. Kind and Code
// stay on the *driver.Error.
func scrubError(err error, cfg driver.ConnConfig, frags []string) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	if _, done := de.Err.(maskedError); done {
		return err
	}
	m := maskedError{msg: scrubMessage(de.Err.Error(), cfg, frags)}
	for _, ce := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(de.Err, ce) {
			m.ctx = ce
		}
	}
	de.Err = m
	return err
}

// maskedError shows a masked message; it unwraps only to a context error.
type maskedError struct {
	msg string
	ctx error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.ctx }

// discard is the logger handed to the Google client libraries: they log
// HTTP traffic when GOOGLE_SDK_GO_LOGGING_LEVEL is set, to stderr by
// default, which would draw over the TUI and could print request details.
var discard = slog.New(slog.DiscardHandler)

// openWith opens a connection; extra client options are for tests (an API
// endpoint with real credentials).
func openWith(ctx context.Context, cfg driver.ConnConfig, extra ...option.ClientOption) (driver.Conn, error) {
	d, err := parseDSN(cfg.DSN)
	if err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "open", err)
	}
	var (
		sa  serviceAccount
		has = cfg.Password.Reveal() != ""
	)
	if has {
		if sa, err = parseServiceAccount(cfg.Password.Reveal()); err != nil {
			return nil, driver.Wrap(driver.ErrAuth, Name, "open", err)
		}
	}
	// The driver owns the HTTP transport, so Close can drop its idle
	// connections; the auth layer wraps it.
	base := http.DefaultTransport.(*http.Transport).Clone()
	authOpts := []option.ClientOption{
		option.WithLogger(discard),
		option.WithTelemetryDisabled(),
		option.WithUserAgent("tobira/" + version),
		option.WithScopes(bq.Scope),
	}
	var clientOpts []option.ClientOption
	switch {
	case d.emulator != "":
		authOpts = append(authOpts, option.WithoutAuthentication())
		clientOpts = append(clientOpts, option.WithEndpoint("http://"+d.emulator))
		if has {
			cfg.Log().Warn("bigquery: the emulator takes no credentials; the service-account key is not sent")
		}
	}
	frags := secretFragments(sa)
	fail := func(err error) (driver.Conn, error) {
		base.CloseIdleConnections()
		return nil, scrubError(mapError(ctx, "open", err), cfg, frags)
	}
	if d.emulator == "" && has {
		// The token exchange (a signed JWT assertion POSTed to the key's
		// token_uri) goes through a client that never follows a redirect.
		creds, err := credentials.NewCredentialsFromJSON(credentials.ServiceAccount, []byte(cfg.Password.Reveal()), &credentials.DetectOptions{
			Scopes: []string{bq.Scope},
			Client: &http.Client{Transport: base, CheckRedirect: netpolicy.RefuseRedirects, Timeout: 30 * time.Second},
			Logger: discard,
		})
		if err != nil {
			return nil, driver.Wrap(driver.ErrAuth, Name, "open", errors.New(scrubMessage(err.Error(), cfg, frags)))
		}
		authOpts = append(authOpts, option.WithAuthCredentials(creds))
	}
	rt, err := htransport.NewTransport(ctx, base, authOpts...)
	if err != nil {
		return fail(err)
	}
	// The auth transport adds the bearer token to every request it sends,
	// a redirected one included, so no redirect is ever followed.
	clientOpts = append(clientOpts, option.WithHTTPClient(&http.Client{Transport: rt, CheckRedirect: netpolicy.RefuseRedirects}))
	clientOpts = append(clientOpts, extra...)
	client, err := bq.NewClient(ctx, d.project, clientOpts...)
	if err != nil {
		return fail(err)
	}
	if d.location != "" {
		client.Location = d.location
	}
	svc, err := bqapi.NewService(ctx, clientOpts...)
	if err != nil {
		_ = client.Close()
		return fail(err)
	}
	c := &conn{client: client, svc: svc, transport: base, dsn: d, cfg: cfg, secrets: frags, readOnly: cfg.ReadOnly}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, 0)
	if err := c.ping(ctx); err != nil {
		_ = c.Close()
		return nil, withOp(err, "open")
	}
	return c, nil
}

// withOp renames the operation of a *driver.Error.
func withOp(err error, op string) error {
	var de *driver.Error
	if errors.As(err, &de) {
		de.Op = op
	}
	return err
}
