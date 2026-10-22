package snowflake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	d, err := parseDSN("snowflake://ada@myorg-acct/SALES/PUBLIC?warehouse=WH&role=ANALYST")
	if err != nil {
		t.Fatal(err)
	}
	want := dsnConfig{account: "myorg-acct", user: "ada", database: "SALES", schema: "PUBLIC", warehouse: "WH", role: "ANALYST", auth: authPassword}
	if d != want {
		t.Errorf("parseDSN = %+v, want %+v", d, want)
	}

	d, err = parseDSN("SNOWFLAKE://svc@xy12345.us-east-1/db?authenticator=snowflake_jwt&host=127.0.0.1&port=8080&protocol=http")
	if err != nil {
		t.Fatal(err)
	}
	want = dsnConfig{account: "xy12345.us-east-1", user: "svc", database: "db", auth: authKeyPair, host: "127.0.0.1", port: 8080, protocol: "http"}
	if d != want {
		t.Errorf("parseDSN = %+v, want %+v", d, want)
	}

	for in, auth := range map[string]authKind{
		"snowflake://u@a?authenticator=SNOWFLAKE":                 authPassword,
		"snowflake://u@a?authenticator=oauth":                     authOAuth,
		"snowflake://u@a?authenticator=programmatic_access_token": authPAT,
		"snowflake://a?authenticator=oauth":                       authOAuth, // OAuth needs no user
	} {
		d, err := parseDSN(in)
		if err != nil || d.auth != auth {
			t.Errorf("parseDSN(%q) = %+v, %v; want auth %v", in, d, err, auth)
		}
	}

	// Quoted (percent-encoded) names keep their case and dots.
	d, err = parseDSN("snowflake://u@a/my%2Edb/Mixed%20Case")
	if err != nil || d.database != "my.db" || d.schema != "Mixed Case" {
		t.Errorf("parseDSN(escaped path) = %+v, %v", d, err)
	}
}

func TestParseDSNRefuses(t *testing.T) {
	const secret = "hunter2-secret"
	for _, dsn := range []string{
		"",
		"postgres://u@a/db",
		"snowflake://",
		"snowflake://u@",          // no account
		"snowflake://a",           // password auth needs a user
		"snowflake://u@a/db/s/x",  // too many path parts
		"snowflake://u@a:443/db",  // the port goes in port=
		"snowflake://u@a/db#frag", // fragment
		"snowflake://u@a?bogus=1", // unknown parameter
		"snowflake://u@a?warehouse=a&warehouse=b",
		"snowflake://u@a?warehouse=",
		"snowflake://u@a?port=0",
		"snowflake://u@a?port=70000",
		"snowflake://u@a?port=x",
		"snowflake://u@a?protocol=ftp",
		"snowflake://u@a?host=h/x",
		"snowflake://u@a?authenticator=externalbrowser",
		"snowflake://u@a?authenticator=okta",
		"snowflake://u@a?authenticator=username_password_mfa",
		"snowflake://u@a?%zz",
	} {
		if _, err := parseDSN(dsn); err == nil {
			t.Errorf("parseDSN(%q) = nil error, want a refusal", dsn)
		}
	}
	// A credential in the DSN is refused with an error that does not quote
	// it, wherever it sits.
	for _, dsn := range []string{
		"snowflake://u:" + secret + "@a/db",
		"snowflake://u@a/db?password=" + secret,
		"snowflake://u@a/db?token=" + secret,
		"snowflake://u@a/db?privateKey=" + secret,
		"snowflake://u@a/db?private_key=" + secret,
		"snowflake://u@a/db?passcode=" + secret,
		"snowflake://u@a/db?oauthClientSecret=" + secret,
		"snowflake://u@a/db?tokenFilePath=" + secret,
		"snowflake://u@a/db?" + secret + "=1",
	} {
		_, err := parseDSN(dsn)
		if err == nil {
			t.Errorf("parseDSN(%q) = nil error, want a refusal", dsn)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("parseDSN(%q) error quotes the credential: %v", dsn, err)
		}
	}
}

func TestBuildConfigPassword(t *testing.T) {
	cfg, d, err := buildConfig(driver.ConnConfig{
		DSN:      "snowflake://ada@acct/DB/SCH?warehouse=WH&role=R",
		Password: driver.NewSecret("pw-123456"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.database != "DB" || cfg.Account != "acct" || cfg.User != "ada" || cfg.Password != "pw-123456" ||
		cfg.Database != "DB" || cfg.Schema != "SCH" || cfg.Warehouse != "WH" || cfg.Role != "R" {
		t.Errorf("buildConfig = %+v", cfg)
	}
	if cfg.Authenticator != sf.AuthTypeSnowflake {
		t.Errorf("Authenticator = %v, want snowflake", cfg.Authenticator)
	}
	checkHardening(t, cfg)
}

// checkHardening asserts what every config carries, whatever the DSN.
func checkHardening(t *testing.T, cfg *sf.Config) {
	t.Helper()
	if v := cfg.Params[multiStatementCountParam]; v == nil || *v != "1" {
		t.Errorf("MULTI_STATEMENT_COUNT session parameter = %v, want 1", v)
	}
	if cfg.ClientStoreTemporaryCredential != sf.ConfigBoolFalse || cfg.ClientRequestMfaToken != sf.ConfigBoolFalse {
		t.Errorf("credential caching = %v/%v, want both off", cfg.ClientStoreTemporaryCredential, cfg.ClientRequestMfaToken)
	}
	if cfg.Application != "tobira" {
		t.Errorf("Application = %q, want tobira", cfg.Application)
	}
	if cfg.LogQueryText || cfg.LogQueryParameters || cfg.Tracing != "" || //nolint:staticcheck // the deprecated field must stay unset too
		cfg.TokenFilePath != "" || cfg.ClientConfigFile != "" {
		t.Error("query logging, tracing, a token file or a client config file is set")
	}
	if cfg.DisableOCSPChecks {
		t.Error("OCSP checks are disabled")
	}
}

func TestBuildConfigNeedsSecret(t *testing.T) {
	for _, dsn := range []string{
		"snowflake://u@a",
		"snowflake://u@a?authenticator=snowflake_jwt",
		"snowflake://u@a?authenticator=oauth",
		"snowflake://u@a?authenticator=programmatic_access_token",
	} {
		if _, _, err := buildConfig(driver.ConnConfig{DSN: dsn}); !errors.Is(err, driver.ErrAuth) {
			t.Errorf("buildConfig(%q, no password) = %v, want ErrAuth", dsn, err)
		}
	}
}

func TestBuildConfigRefusesOptions(t *testing.T) {
	_, _, err := buildConfig(driver.ConnConfig{DSN: "snowflake://u@a", Password: driver.NewSecret("x"), Options: map[string]string{"x": "y"}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("buildConfig with Options = %v, want ErrUnsupported", err)
	}
}

func TestBuildConfigTokens(t *testing.T) {
	cfg, _, err := buildConfig(driver.ConnConfig{DSN: "snowflake://u@a?authenticator=oauth", Password: driver.NewSecret("oauth-token")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Authenticator != sf.AuthTypeOAuth || cfg.Token != "oauth-token" || cfg.Password != "" {
		t.Errorf("oauth config = auth %v token %q password %q", cfg.Authenticator, cfg.Token, cfg.Password)
	}
	checkHardening(t, cfg)

	cfg, _, err = buildConfig(driver.ConnConfig{DSN: "snowflake://u@a?authenticator=programmatic_access_token", Password: driver.NewSecret("pat-token")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Authenticator != sf.AuthTypePat || cfg.Token != "pat-token" || cfg.Password != "" {
		t.Errorf("PAT config = auth %v token %q password %q", cfg.Authenticator, cfg.Token, cfg.Password)
	}
}

func testKeyPEM(t *testing.T, typ string) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var der []byte
	switch typ {
	case "PRIVATE KEY":
		if der, err = x509.MarshalPKCS8PrivateKey(key); err != nil {
			t.Fatal(err)
		}
	case "RSA PRIVATE KEY":
		der = x509.MarshalPKCS1PrivateKey(key)
	default:
		der = []byte("not a key")
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})), key
}

func TestBuildConfigKeyPair(t *testing.T) {
	for _, typ := range []string{"PRIVATE KEY", "RSA PRIVATE KEY"} {
		pemText, key := testKeyPEM(t, typ)
		cfg, _, err := buildConfig(driver.ConnConfig{DSN: "snowflake://svc@a?authenticator=snowflake_jwt", Password: driver.NewSecret(pemText)})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if cfg.Authenticator != sf.AuthTypeJwt || cfg.PrivateKey == nil || !cfg.PrivateKey.Equal(key) || cfg.Password != "" {
			t.Errorf("%s: key-pair config = auth %v key set %v password %q", typ, cfg.Authenticator, cfg.PrivateKey != nil, cfg.Password)
		}
		checkHardening(t, cfg)
	}
}

func TestBuildConfigKeyPairRefusesBadKeys(t *testing.T) {
	encrypted := "-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIBvTBXBgkqhkiG9w0BBQ0wSjApBgkqhkiG9w0BBQwwHAQI\n-----END ENCRYPTED PRIVATE KEY-----\n"
	garbage, _ := testKeyPEM(t, "PRIVATE KEY BUT WRONG")
	for name, pw := range map[string]string{
		"not PEM":   "secret-not-a-pem-value",
		"encrypted": encrypted,
		"garbage":   garbage,
		"ec key":    ecKeyPEM(t),
	} {
		_, _, err := buildConfig(driver.ConnConfig{DSN: "snowflake://svc@a?authenticator=snowflake_jwt", Password: driver.NewSecret(pw)})
		if !errors.Is(err, driver.ErrAuth) {
			t.Errorf("%s: buildConfig = %v, want ErrAuth", name, err)
			continue
		}
		if strings.Contains(err.Error(), "secret-not-a-pem-value") || strings.Contains(err.Error(), "MII") {
			t.Errorf("%s: the error quotes the key: %v", name, err)
		}
	}
}

func TestBuildConfigEndpoint(t *testing.T) {
	cfg, _, err := buildConfig(driver.ConnConfig{DSN: "snowflake://u@a?host=127.0.0.1&port=8080&protocol=http", Password: driver.NewSecret("x")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8080 || cfg.Protocol != "http" {
		t.Errorf("endpoint = %s://%s:%d", cfg.Protocol, cfg.Host, cfg.Port)
	}
}

func TestNamespaceNames(t *testing.T) {
	for _, c := range []struct{ db, schema, name string }{
		{"SALES", "PUBLIC", "SALES.PUBLIC"},
		{"my.db", "s", `"my.db".s`},
		{"DB", `we"ird`, `DB."we""ird"`},
		{"DB", "", "DB"},
	} {
		if got := namespaceName(c.db, c.schema); got != c.name {
			t.Errorf("namespaceName(%q, %q) = %q, want %q", c.db, c.schema, got, c.name)
		}
		db, schema, err := splitNamespace(c.name)
		if err != nil || db != c.db || schema != c.schema {
			t.Errorf("splitNamespace(%q) = %q, %q, %v; want %q, %q", c.name, db, schema, err, c.db, c.schema)
		}
	}
	for _, bad := range []string{"a.b.c", `"a`, `a."b`, ".b", "a.", `"a"x.b`} {
		if _, _, err := splitNamespace(bad); err == nil {
			t.Errorf("splitNamespace(%q) = nil error, want one", bad)
		}
	}
}

func ecKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// The documented DSN forms carry no secret, so tobira's DSN redaction
// leaves them as they are.
func TestDocumentedDSNsHoldNoPassword(t *testing.T) {
	for _, dsn := range []string{
		"snowflake://ada@myorg-myaccount/SALES/PUBLIC?warehouse=COMPUTE_WH&role=ANALYST",
		"snowflake://svc@xy12345.eu-west-1/SALES?authenticator=snowflake_jwt",
		"snowflake://ada@acct/SALES/PUBLIC?host=127.0.0.1&port=8080&protocol=http",
		"snowflake://acct?authenticator=oauth",
	} {
		if driver.HasPassword(dsn) || driver.RedactDSN(dsn) != dsn {
			t.Errorf("%s: HasPassword=%v RedactDSN=%s", dsn, driver.HasPassword(dsn), driver.RedactDSN(dsn))
		}
	}
	if red := driver.RedactDSN("snowflake://u:hunter2-secret@a/db"); strings.Contains(red, "hunter2-secret") {
		t.Errorf("RedactDSN kept an inline password: %s", red)
	}
}
