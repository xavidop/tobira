package firestore

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xavidop/tobira/driver"
)

// openTimeout bounds Open's first call to the server (a variable so tests
// can lower it).
var openTimeout = 10 * time.Second

// dsnConfig is a parsed DSN:
//
//	firestore://project[?database=(default)&emulator=host:port]
type dsnConfig struct {
	project  string
	database string // database id; "(default)" unless the DSN names one
	emulator string // host:port of an emulator: plain gRPC, no credentials
}

var (
	errMalformedDSN = errors.New("malformed DSN: want firestore://project[?database=…&emulator=host:port]")
	// errDSNCredentials refuses a DSN that carries a credential. It never
	// quotes the DSN.
	errDSNCredentials = errors.New("credentials must not be in the DSN: put the service-account JSON in the connection's password (a secret ref)")
)

// parseDSN parses a firestore:// DSN. Errors never quote the DSN, which
// could hold a pasted credential.
func parseDSN(dsn string) (dsnConfig, error) {
	const scheme = "firestore://"
	if len(dsn) < len(scheme) || !strings.EqualFold(dsn[:len(scheme)], scheme) {
		return dsnConfig{}, errMalformedDSN
	}
	rest := dsn[len(scheme):]
	authority, _, _ := strings.Cut(rest, "/")
	authority, _, _ = strings.Cut(authority, "?")
	if strings.Contains(authority, "@") {
		return dsnConfig{}, errDSNCredentials
	}
	u, err := url.Parse("firestore://" + rest)
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
	if u.Host == "" || u.Port() != "" || u.Fragment != "" || strings.HasSuffix(dsn, "#") || u.Opaque != "" ||
		strings.Trim(u.Path, "/") != "" {
		return dsnConfig{}, errMalformedDSN
	}
	d := dsnConfig{project: u.Hostname(), database: fs.DefaultDatabaseID}
	for k, vs := range query {
		name := strings.ToLower(k)
		if name != "database" && name != "emulator" {
			// Not quoted: a stray parameter could be a pasted secret.
			return dsnConfig{}, errors.New("malformed DSN: unknown parameter (known: database, emulator)")
		}
		if len(vs) != 1 || vs[0] == "" {
			return dsnConfig{}, errors.New("malformed DSN: " + name + " must have one non-empty value")
		}
		v := vs[0]
		switch name {
		case "database":
			if strings.ContainsAny(v, "/?#@ ") {
				return dsnConfig{}, errors.New("malformed DSN: database must be a database id, such as (default)")
			}
			d.database = v
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

// parseServiceAccount checks that pw is a service-account key whose
// private key parses. Only that type is accepted: other credential files
// (external_account, for one) can make the auth library run a program or
// call arbitrary URLs. Errors never quote the key.
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
	b, _ := pem.Decode([]byte(sa.PrivateKey))
	if b == nil {
		return serviceAccount{}, errors.New("the service-account private_key is not a PEM key")
	}
	if _, err := x509.ParsePKCS8PrivateKey(b.Bytes); err != nil {
		if _, err := x509.ParsePKCS1PrivateKey(b.Bytes); err != nil {
			return serviceAccount{}, errors.New("the service-account private_key is not an RSA private key")
		}
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
// error does not unwrap to the original, whose nested parts were not
// masked; only a context error stays reachable, so errors.Is(err,
// context.Canceled) still holds. Kind and Code stay on the *driver.Error.
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
// requests when GOOGLE_SDK_GO_LOGGING_LEVEL is set, to stderr by default,
// which would draw over the TUI and could print request details.
var discard = slog.New(slog.DiscardHandler)

// emulatorCreds is the bearer token the Firestore emulator expects from an
// admin client, as the SDK sends it when FIRESTORE_EMULATOR_HOST is set: it
// bypasses security rules, and it is no secret.
type emulatorCreds struct{}

func (emulatorCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer owner"}, nil
}

func (emulatorCreds) RequireTransportSecurity() bool { return false }

// emulatorEnv is the variable the client library reads to send every RPC
// to an emulator, whatever the options say.
const emulatorEnv = "FIRESTORE_EMULATOR_HOST"

// envEmulator returns the emulator address the client library will use
// from emulatorEnv, read and normalised exactly as the library does: any
// non-empty value sets it (set), spaces included and never trimmed, and
// only an http:// or https:// prefix is dropped from addr.
func envEmulator() (addr string, set bool) {
	v := os.Getenv(emulatorEnv)
	addr = strings.TrimPrefix(v, "http://")
	return strings.TrimPrefix(addr, "https://"), v != ""
}

// checkEmulatorEnv refuses a DSN the environment would retarget: with
// FIRESTORE_EMULATOR_HOST set, the client library sends every RPC there in
// plain text, so a DSN without emulator= (meant for Google) is refused, and
// one with emulator= must name the same address. It reports whether the
// library will dial the emulator itself.
func checkEmulatorEnv(d dsnConfig) (libraryDials bool, err error) {
	env, set := envEmulator()
	switch {
	case !set:
		return false, nil
	case d.emulator == "":
		return false, fmt.Errorf("%s is set, so the client library would send this connection to that emulator instead of Google: unset it, or add emulator=host:port to the DSN", emulatorEnv)
	case env != d.emulator:
		return false, fmt.Errorf("%s names another emulator than the DSN's emulator=: unset it, or make them the same", emulatorEnv)
	}
	return true, nil
}

// openWith opens a connection; extra client options are for tests.
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
	frags := secretFragments(sa)
	libraryDials, err := checkEmulatorEnv(d)
	if err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "open", err)
	}
	opts := []option.ClientOption{
		option.WithLogger(discard),
		option.WithTelemetryDisabled(),
		option.WithUserAgent("tobira/" + version),
	}
	var gconn *grpc.ClientConn
	switch {
	case libraryDials:
		// The library dials FIRESTORE_EMULATOR_HOST (the same address) with
		// the emulator's admin token and owns that connection; a second one
		// from the driver would replace it and leak it.
		if has {
			cfg.Log().Warn("firestore: the emulator takes no credentials; the service-account key is not sent")
		}
	case d.emulator != "":
		gconn, err = grpc.NewClient("passthrough:///"+d.emulator,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(emulatorCreds{}),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)))
		if err != nil {
			return nil, driver.Wrap(driver.ErrConnection, Name, "open", errors.New("emulator address: "+scrubMessage(err.Error(), cfg, frags)))
		}
		opts = append(opts, option.WithGRPCConn(gconn), option.WithoutAuthentication())
		if has {
			cfg.Log().Warn("firestore: the emulator takes no credentials; the service-account key is not sent")
		}
	case has:
		opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, []byte(cfg.Password.Reveal())))
	}
	opts = append(opts, extra...)
	client, err := fs.NewClientWithDatabase(ctx, d.project, d.database, opts...)
	if err != nil {
		if gconn != nil {
			_ = gconn.Close()
		}
		return nil, scrubError(mapError(ctx, "open", err), cfg, frags)
	}
	c := &conn{client: client, dsn: d, cfg: cfg, secrets: frags, readOnly: cfg.ReadOnly}
	c.catalog = newCatalogCache(connCatalog{c})
	pctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	if err := c.Ping(pctx); err != nil {
		_ = c.Close()
		if ctx.Err() == nil && pctx.Err() != nil {
			// Our own bound ran out, not the caller's context.
			return nil, driver.Wrap(driver.ErrConnection, Name, "open",
				fmt.Errorf("no answer from Firestore within %s", openTimeout))
		}
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
