package bigquery

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "bigquery" || info.Kind != driver.KindSQL || info.Dialect != "googlesql" {
		t.Errorf("info = %+v", info)
	}
	if !slices.Equal(info.Schemes, []string{"bigquery"}) {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := []driver.Capability{driver.CapBrowser, driver.CapExplainer, driver.CapCompleter, driver.CapReadOnly}
	if !slices.Equal(info.Capabilities, want) {
		t.Errorf("capabilities = %v, want %v", info.Capabilities, want)
	}
	if d, ok := driver.Default.Lookup("bigquery"); !ok || d.Info().Name != "bigquery" {
		t.Error(`init did not register "bigquery"`)
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		dsn  string
		want dsnConfig
	}{
		{"bigquery://my-project", dsnConfig{project: "my-project", maxBytesBilled: defaultMaxBytesBilled}},
		{"BIGQUERY://my-project/", dsnConfig{project: "my-project", maxBytesBilled: defaultMaxBytesBilled}},
		{"bigquery://my-project/sales", dsnConfig{project: "my-project", dataset: "sales", maxBytesBilled: defaultMaxBytesBilled}},
		{"bigquery://my-project/sales?location=EU", dsnConfig{project: "my-project", dataset: "sales", location: "EU", maxBytesBilled: defaultMaxBytesBilled}},
		{"bigquery://p/d?max_bytes_billed=5000", dsnConfig{project: "p", dataset: "d", maxBytesBilled: 5000}},
		{"bigquery://p/d?max_bytes_billed=0", dsnConfig{project: "p", dataset: "d", maxBytesBilled: 0}},
		{"bigquery://test/ds?emulator=localhost:9050", dsnConfig{project: "test", dataset: "ds", emulator: "localhost:9050", maxBytesBilled: defaultMaxBytesBilled}},
		{"bigquery://test?emulator=127.0.0.1:9050&location=US", dsnConfig{project: "test", emulator: "127.0.0.1:9050", location: "US", maxBytesBilled: defaultMaxBytesBilled}},
	}
	for _, tt := range tests {
		got, err := parseDSN(tt.dsn)
		if err != nil {
			t.Errorf("parseDSN(%q): %v", tt.dsn, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseDSN(%q) = %+v, want %+v", tt.dsn, got, tt.want)
		}
	}
}

func TestParseDSNRefuses(t *testing.T) {
	for _, dsn := range []string{
		"",
		"postgres://h/db",
		"bigquery://",
		"bigquery:///ds",
		"bigquery://p/ds/extra",
		"bigquery://p/ds?unknown=1",
		"bigquery://p/ds?max_bytes_billed=-1",
		"bigquery://p/ds?max_bytes_billed=lots",
		"bigquery://p/ds?location=",
		"bigquery://p/ds?emulator=",
		"bigquery://p/ds?emulator=http://h:1",
		"bigquery://p/ds?location=EU&location=US",
		"bigquery://p:9050/ds",
		"bigquery://p/ds#frag",
		"bigquery://p/ds?%zz",
	} {
		if _, err := parseDSN(dsn); err == nil {
			t.Errorf("parseDSN(%q) accepted", dsn)
		}
	}
}

// Credentials only ever come from ConnConfig.Password (a secret ref): a DSN
// that carries one is refused, and the refusal never quotes it.
func TestParseDSNRefusesCredentials(t *testing.T) {
	const secret = "PRIVATEKEYMATERIAL-123456"
	for _, dsn := range []string{
		"bigquery://user:" + secret + "@p/ds",
		"bigquery://" + secret + "@p/ds",
		"bigquery://p/ds?credentials=" + secret,
		"bigquery://p/ds?CREDENTIALS=" + secret,
		"bigquery://p/ds?credentials_json=" + secret,
		"bigquery://p/ds?credentials_file=/tmp/" + secret,
		"bigquery://p/ds?password=" + secret,
		"bigquery://p/ds?private_key=" + secret,
		"bigquery://p/ds?key=" + secret,
		"bigquery://p/ds?token=" + secret,
		"bigquery://p/ds?access_token=" + secret,
	} {
		_, err := parseDSN(dsn)
		if err == nil {
			t.Errorf("parseDSN(%q) accepted a credential", driver.RedactDSN(dsn))
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("parseDSN error quotes the credential: %v", err)
		}
		cerr := openErr(t, driver.ConnConfig{DSN: dsn})
		if !errors.Is(cerr, driver.ErrAuth) && !errors.Is(cerr, driver.ErrUnsupported) {
			t.Errorf("Open(%s): want ErrAuth or ErrUnsupported, got %v", driver.RedactDSN(dsn), cerr)
		}
		if strings.Contains(cerr.Error(), secret) {
			t.Errorf("Open error quotes the credential: %v", cerr)
		}
	}
}

// The documented DSN forms hold no secret, so RedactDSN leaves them as
// they are; a password pasted into the userinfo anyway is masked by the
// shared redaction (and refused by Open, above).
func TestDSNRedaction(t *testing.T) {
	for _, dsn := range []string{
		"bigquery://my-project",
		"bigquery://my-project/sales?location=EU",
		"bigquery://my-project/sales?location=EU&max_bytes_billed=1073741824",
		"bigquery://test/ds?emulator=localhost:9050",
	} {
		if got := driver.RedactDSN(dsn); got != dsn {
			t.Errorf("RedactDSN(%q) = %q, want it unchanged (the form carries no secret)", dsn, got)
		}
		if driver.HasPassword(dsn) {
			t.Errorf("HasPassword(%q) = true", dsn)
		}
	}
	const pw = "hunter2-secret"
	for _, dsn := range []string{
		"bigquery://user:" + pw + "@my-project/sales",
		"bigquery://my-project/sales?password=" + pw,
	} {
		if got := driver.RedactDSN(dsn); strings.Contains(got, pw) {
			t.Errorf("RedactDSN(%q) = %q, still holds the password", dsn, got)
		}
		if got := driver.RedactMessage("open failed for "+dsn, driver.ConnConfig{DSN: dsn}); strings.Contains(got, pw) {
			t.Errorf("RedactMessage leaves the password: %q", got)
		}
	}
}

func openErr(t *testing.T, cfg driver.ConnConfig) error {
	t.Helper()
	c, err := New().Open(context.Background(), cfg)
	if err == nil {
		_ = c.Close()
		t.Fatalf("Open(%s) succeeded", driver.RedactDSN(cfg.DSN))
	}
	return err
}

func TestOpenRefusesBadCredentials(t *testing.T) {
	for name, pw := range map[string]string{
		"not json":          "this is not json at all",
		"wrong type":        `{"type":"authorized_user","client_id":"x","client_secret":"SECRETSECRET","refresh_token":"r"}`,
		"external account":  `{"type":"external_account","audience":"a","credential_source":{"executable":{"command":"/bin/sh -c id"}}}`,
		"no private key":    `{"type":"service_account","client_email":"a@b.iam.gserviceaccount.com"}`,
		"truncated":         `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nSECRETSECRET`,
		"empty json object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, dsn := range []string{"bigquery://p/ds", "bigquery://p/ds?emulator=127.0.0.1:1"} {
				err := openErr(t, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(pw)})
				if !errors.Is(err, driver.ErrAuth) {
					t.Errorf("Open(%s): want ErrAuth, got %v", dsn, err)
				}
				if strings.Contains(err.Error(), "SECRETSECRET") || strings.Contains(err.Error(), "/bin/sh") {
					t.Errorf("error quotes the credentials: %v", err)
				}
			}
		})
	}
}

func TestSecretFragments(t *testing.T) {
	key := testServiceAccountKey(t)
	pw := serviceAccountJSON(t, key, "https://oauth2.example/token")
	sa, err := parseServiceAccount(pw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := driver.ConnConfig{DSN: "bigquery://p/ds", Password: driver.NewSecret(pw)}
	frags := secretFragments(sa)
	for _, msg := range []string{
		"failed: " + sa.PrivateKey,
		"failed: key id " + sa.PrivateKeyID,
		"failed: " + strings.Split(sa.PrivateKey, "\n")[1], // one PEM line
		"failed: " + pw,
	} {
		got := scrubMessage(msg, cfg, frags)
		if strings.Contains(got, sa.PrivateKeyID) || strings.Contains(got, strings.Split(sa.PrivateKey, "\n")[1]) {
			t.Errorf("scrubMessage left key material: %q", got)
		}
	}
}
