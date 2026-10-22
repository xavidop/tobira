package snowflake

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// multiStatementCountParam is the session parameter that bounds the
// statements in one request; the driver pins it to 1.
const multiStatementCountParam = "MULTI_STATEMENT_COUNT"

// authKind is how the connection logs in. The secret for every kind comes
// from ConnConfig.Password, which the host resolves from a ref.
type authKind uint8

const (
	authPassword authKind = iota // authenticator=snowflake: user and password
	authKeyPair                  // snowflake_jwt: user and an RSA private key (PEM)
	authOAuth                    // oauth: an OAuth access token
	authPAT                      // programmatic_access_token: user and a PAT
)

var authenticators = map[string]authKind{
	"snowflake": authPassword, "snowflake_jwt": authKeyPair, "oauth": authOAuth, "programmatic_access_token": authPAT,
}

func (k authKind) String() string {
	for name, v := range authenticators {
		if v == k {
			return name
		}
	}
	return "unknown"
}

// dsnConfig is a parsed DSN:
//
//	snowflake://user@account[/database[/schema]][?warehouse=…&role=…&authenticator=…&host=…&port=…&protocol=…&allow_plaintext=…]
type dsnConfig struct {
	account, user, database, schema, warehouse, role string
	auth                                             authKind
	host                                             string // "" = <account>.snowflakecomputing.com
	port                                             int    // 0 = 443
	protocol                                         string // "" = https
}

var (
	errMalformedDSN = errors.New("malformed DSN: want snowflake://user@account[/database[/schema]][?warehouse=…&role=…]")
	// errDSNCredentials refuses a DSN that carries a credential. It never
	// quotes the DSN.
	errDSNCredentials = errors.New("credentials must not be in the DSN: put the password, private key (PEM) or token in the connection's password (a secret ref)")
)

// dsnParams are the DSN parameters the driver reads. Anything else is
// refused: gosnowflake's own DSN parameters are not passed through, so a
// DSN cannot turn on query logging, point at a token file or skip
// certificate checks.
var dsnParams = []string{"warehouse", "role", "authenticator", "host", "port", "protocol", "allow_plaintext"}

// parseDSN parses a snowflake:// DSN. Errors never quote the DSN, which
// could hold a pasted credential.
func parseDSN(dsn string) (dsnConfig, error) {
	const scheme = "snowflake://"
	if len(dsn) < len(scheme) || !strings.EqualFold(dsn[:len(scheme)], scheme) {
		return dsnConfig{}, errMalformedDSN
	}
	rest := dsn[len(scheme):]
	authority, _, _ := strings.Cut(rest, "/")
	authority, _, _ = strings.Cut(authority, "?")
	if userinfo, _, ok := strings.Cut(authority, "@"); ok && strings.Contains(userinfo, ":") {
		return dsnConfig{}, errDSNCredentials
	}
	u, err := url.Parse(scheme + rest)
	if err != nil { // url errors quote the input
		return dsnConfig{}, errMalformedDSN
	}
	if _, has := u.User.Password(); has {
		return dsnConfig{}, errDSNCredentials
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return dsnConfig{}, errMalformedDSN
	}
	for k := range query {
		if !slices.Contains(dsnParams, strings.ToLower(k)) {
			if credentialKey(k) {
				return dsnConfig{}, errDSNCredentials
			}
			// Not quoted: a stray parameter could be a pasted secret.
			return dsnConfig{}, errors.New("malformed DSN: unknown parameter (known: " + strings.Join(dsnParams, ", ") + ")")
		}
	}
	if u.Host == "" || u.Port() != "" || u.Fragment != "" || strings.HasSuffix(dsn, "#") || u.Opaque != "" {
		if u.Port() != "" {
			return dsnConfig{}, errors.New("malformed DSN: the account takes no port; set port= for a custom endpoint")
		}
		return dsnConfig{}, errMalformedDSN
	}
	d := dsnConfig{account: u.Hostname(), user: u.User.Username()}
	if !validAccount(d.account) {
		return dsnConfig{}, errors.New("malformed DSN: the account holds letters, digits, '-', '_' and '.' only")
	}
	if p := strings.Trim(u.EscapedPath(), "/"); p != "" {
		parts := strings.Split(p, "/")
		if len(parts) > 2 {
			return dsnConfig{}, errors.New("malformed DSN: the path is /database[/schema]")
		}
		names := make([]string, len(parts))
		for i, part := range parts {
			if names[i], err = url.PathUnescape(part); err != nil || names[i] == "" {
				return dsnConfig{}, errMalformedDSN
			}
		}
		d.database = names[0]
		if len(names) == 2 {
			d.schema = names[1]
		}
	}
	allowPlain := false
	for k, vs := range query {
		name := strings.ToLower(k)
		if len(vs) != 1 || vs[0] == "" {
			return dsnConfig{}, errors.New("malformed DSN: " + name + " must have one non-empty value")
		}
		v := vs[0]
		switch name {
		case "warehouse":
			d.warehouse = v
		case "role":
			d.role = v
		case "authenticator":
			kind, ok := authenticators[strings.ToLower(v)]
			if !ok {
				return dsnConfig{}, driver.Wrap(driver.ErrUnsupported, Name, "open",
					errors.New("unsupported authenticator: use snowflake (password), snowflake_jwt (key pair), oauth or programmatic_access_token"))
			}
			d.auth = kind
		case "host":
			if strings.ContainsAny(v, "/@?#:% \t") {
				return dsnConfig{}, errors.New("malformed DSN: host must be a host name")
			}
			d.host = v
		case "port":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 65535 {
				return dsnConfig{}, errors.New("malformed DSN: port must be 1-65535")
			}
			d.port = n
		case "protocol":
			if v = strings.ToLower(v); v != "http" && v != "https" {
				return dsnConfig{}, errors.New("malformed DSN: protocol must be http or https")
			}
			d.protocol = v
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return dsnConfig{}, errors.New("malformed DSN: allow_plaintext must be true or false")
			}
			allowPlain = b
		}
	}
	if d.protocol == "http" {
		v := netpolicy.CheckPlaintext(d.host, allowPlain, snowflakeZones...)
		switch {
		case d.host == "" || v == netpolicy.RefusedCloud:
			return dsnConfig{}, errors.New("malformed DSN: protocol=http is refused for Snowflake's own endpoints (*.snowflakecomputing.com, *.snowflakecomputing.cn)")
		case v == netpolicy.NeedsOptIn:
			return dsnConfig{}, errors.New("malformed DSN: plain http to a host that is not loopback needs allow_plaintext=1 as well as protocol=http")
		}
	}
	if d.user == "" && d.auth != authOAuth {
		return dsnConfig{}, errors.New("malformed DSN: a user is required (snowflake://user@account)")
	}
	return d, nil
}

// snowflakeZones are Snowflake's own endpoints (accounts and private link,
// global and China regions), which are never reached in clear.
var snowflakeZones = []string{"snowflakecomputing.com", "snowflakecomputing.cn"}

// newTransport is the HTTP transport of one connection: Go's defaults (the
// environment's proxy, the system roots) with TLS 1.2 at least.
func newTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return tr
}

func validAccount(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isWordStart(c) && !isDigit(c) && c != '-' && c != '.' {
			return false
		}
	}
	return s != ""
}

// credentialKey reports whether a DSN parameter name looks like it carries
// a credential. Any unknown parameter is refused anyway; these get the
// error that says where credentials go.
func credentialKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"credential", "password", "passwd", "pwd", "secret", "token", "key", "pass", "auth"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// buildConfig turns cfg into a gosnowflake config without connecting. On
// top of the DSN it always sets:
//
//   - the secret from ConnConfig.Password: the password, the PEM private key
//     (key pair) or the token (OAuth, PAT), by authenticator;
//   - MULTI_STATEMENT_COUNT=1, so the server runs one statement per request;
//   - no credential caching in the OS keychain (ID and MFA tokens);
//   - Application "tobira".
//
// Query text and parameter logging, token files and OCSP overrides are
// never set.
func buildConfig(cfg driver.ConnConfig) (*sf.Config, dsnConfig, error) {
	fail := func(kind, err error) (*sf.Config, dsnConfig, error) {
		var de *driver.Error
		if errors.As(err, &de) {
			return nil, dsnConfig{}, err
		}
		return nil, dsnConfig{}, driver.Wrap(kind, Name, "open", err)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the snowflake driver takes its settings from DSN parameters", k))
	}
	d, err := parseDSN(cfg.DSN)
	if err != nil {
		return fail(driver.ErrSyntax, err)
	}
	secret := cfg.Password.Reveal()
	if secret == "" {
		what := map[authKind]string{authPassword: "password", authKeyPair: "private key", authOAuth: "OAuth token", authPAT: "programmatic access token"}[d.auth]
		return fail(driver.ErrAuth, errors.New("no "+what+": set the connection's password (a secret ref)"))
	}
	one := "1"
	c := &sf.Config{
		Account: d.account, User: d.user, Database: d.database, Schema: d.schema,
		Warehouse: d.warehouse, Role: d.role,
		Host: d.host, Port: d.port, Protocol: d.protocol,
		Application:                    "tobira",
		ClientStoreTemporaryCredential: sf.ConfigBoolFalse,
		ClientRequestMfaToken:          sf.ConfigBoolFalse,
		Params:                         map[string]*string{multiStatementCountParam: &one},
	}
	switch d.auth {
	case authPassword:
		c.Authenticator, c.Password = sf.AuthTypeSnowflake, secret
	case authKeyPair:
		key, err := parsePrivateKey(secret)
		if err != nil {
			return fail(driver.ErrAuth, err)
		}
		c.Authenticator, c.PrivateKey = sf.AuthTypeJwt, key
	case authOAuth:
		c.Authenticator, c.Token = sf.AuthTypeOAuth, secret
	case authPAT:
		c.Authenticator, c.Token = sf.AuthTypePat, secret
	}
	return c, d, nil
}

// parsePrivateKey reads an unencrypted RSA private key in PEM (PKCS #8,
// "PRIVATE KEY", or PKCS #1, "RSA PRIVATE KEY"). Errors never quote it.
func parsePrivateKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("the private key is not PEM: put the PEM text of an unencrypted RSA key in the connection's password")
	}
	switch block.Type {
	case "ENCRYPTED PRIVATE KEY":
		return nil, errors.New("the private key is encrypted: store it unencrypted in the secret store (openssl pkcs8 -topk8 -nocrypt)")
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("the private key cannot be read as PKCS #8")
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("key-pair authentication needs an RSA private key")
		}
		return rk, nil
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("the private key cannot be read as PKCS #1")
		}
		return k, nil
	}
	return nil, errors.New("the PEM block is not a private key")
}

// openPool opens a database/sql pool of gosnowflake sessions for cfg. It
// does not connect.
func openPool(cfg sf.Config) *sql.DB {
	db := sql.OpenDB(sf.NewConnector(sf.SnowflakeDriver{}, cfg))
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(30 * time.Minute)
	return db
}

// Open builds the config, logs in and reads the session's database and
// schema, so bad credentials or an unknown account fail here rather than
// on the first query.
func (sfDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	sfcfg, d, err := buildConfig(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	if d.protocol == "http" {
		cfg.Log().Warn("snowflake: the connection sends its credentials without TLS (protocol=http)", "host", d.host)
	}
	tr := newTransport()
	// gosnowflake builds its own http.Clients, which follow redirects: Go
	// replays a POST body (the login request holds the password or token)
	// on a 307 or 308 to any host, and keeps Authorization on a redirect to
	// the same host name, from https to http included. Without Location a
	// 3xx comes back as the reply, which gosnowflake reports as an error.
	sfcfg.Transporter = netpolicy.DropRedirectLocation(tr)
	c := &conn{db: openPool(*sfcfg), base: *sfcfg, transport: tr, cfg: cfg, readOnly: cfg.ReadOnly}
	if err := c.probe(ctx); err != nil {
		_ = c.db.Close()
		tr.CloseIdleConnections()
		return nil, scrubError(err, cfg)
	}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// probe logs in and reads the session's current database and schema.
func (c *conn) probe(ctx context.Context) error {
	var db, schema sql.NullString
	err := c.db.QueryRowContext(sf.WithMultiStatement(ctx, 1), "SELECT CURRENT_DATABASE(), CURRENT_SCHEMA()").Scan(&db, &schema)
	if err != nil {
		return mapError(ctx, "open", err)
	}
	c.defaultDB, c.defaultSchema = db.String, schema.String
	return nil
}
