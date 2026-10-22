package firestore

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "firestore" || info.Kind != driver.KindDocument || info.Dialect != "firestore-json" {
		t.Errorf("info = %+v", info)
	}
	if !slices.Equal(info.Schemes, []string{"firestore"}) {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapCompleter, driver.CapWatcher, driver.CapReadOnly}
	if !slices.Equal(info.Capabilities, want) {
		t.Errorf("capabilities = %v, want %v", info.Capabilities, want)
	}
	if d, ok := driver.Default.Lookup("firestore"); !ok || d.Info().Name != "firestore" {
		t.Error(`init did not register "firestore"`)
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		dsn  string
		want dsnConfig
	}{
		{"firestore://my-project", dsnConfig{project: "my-project", database: "(default)"}},
		{"FIRESTORE://my-project/", dsnConfig{project: "my-project", database: "(default)"}},
		{"firestore://my-project?database=orders", dsnConfig{project: "my-project", database: "orders"}},
		{"firestore://my-project?database=(default)", dsnConfig{project: "my-project", database: "(default)"}},
		{"firestore://my-project?database=%28default%29", dsnConfig{project: "my-project", database: "(default)"}},
		{"firestore://demo?emulator=localhost:8080", dsnConfig{project: "demo", database: "(default)", emulator: "localhost:8080"}},
		{"firestore://demo?emulator=127.0.0.1:8080&database=other", dsnConfig{project: "demo", database: "other", emulator: "127.0.0.1:8080"}},
		{"firestore://demo?emulator=[::1]:8080", dsnConfig{project: "demo", database: "(default)", emulator: "[::1]:8080"}},
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
		"firestore://",
		"firestore:///x",
		"firestore://p/extra",
		"firestore://p?unknown=1",
		"firestore://p?database=",
		"firestore://p?database=a&database=b",
		"firestore://p?database=a/b",
		"firestore://p?emulator=",
		"firestore://p?emulator=http://h:1",
		"firestore://p?emulator=hostonly",
		"firestore://p:8080",
		"firestore://p#frag",
		"firestore://p?%zz",
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
		"firestore://user:" + secret + "@p",
		"firestore://" + secret + "@p",
		"firestore://p?credentials=" + secret,
		"firestore://p?CREDENTIALS=" + secret,
		"firestore://p?credentials_file=/tmp/" + secret,
		"firestore://p?password=" + secret,
		"firestore://p?private_key=" + secret,
		"firestore://p?key=" + secret,
		"firestore://p?token=" + secret,
		"firestore://p?unknown=" + secret,
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
// they are.
func TestDSNRedaction(t *testing.T) {
	for _, dsn := range []string{
		"firestore://my-project",
		"firestore://my-project?database=orders",
		"firestore://demo?emulator=localhost:8080",
	} {
		if got := driver.RedactDSN(dsn); got != dsn {
			t.Errorf("RedactDSN(%q) = %q, want it unchanged", dsn, got)
		}
		if driver.HasPassword(dsn) {
			t.Errorf("HasPassword(%q) = true", dsn)
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
		"not a key":         `{"type":"service_account","client_email":"a@b","private_key":"-----BEGIN PRIVATE KEY-----\nU0VDUkVUU0VDUkVU\n-----END PRIVATE KEY-----\n"}`,
		"empty json object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, dsn := range []string{"firestore://p", "firestore://p?emulator=127.0.0.1:1"} {
				err := openErr(t, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(pw)})
				if !errors.Is(err, driver.ErrAuth) {
					t.Errorf("Open(%s): want ErrAuth, got %v", dsn, err)
				}
				var de *driver.Error
				if !errors.As(err, &de) || de.Driver != Name {
					t.Errorf("Open(%s): want a *driver.Error from %s, got %T", dsn, Name, err)
				}
				for _, s := range []string{"SECRETSECRET", "/bin/sh", "U0VDUkVUU0VDUkVU"} {
					if strings.Contains(err.Error(), s) {
						t.Errorf("error quotes the credentials: %v", err)
					}
				}
			}
		})
	}
}

func TestParseServiceAccount(t *testing.T) {
	pw := serviceAccountJSON(t, testKey(t))
	sa, err := parseServiceAccount(pw)
	if err != nil {
		t.Fatal(err)
	}
	if sa.ClientEmail != "tobira@test.iam.gserviceaccount.com" || sa.PrivateKeyID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("sa = %+v", sa)
	}
}

func TestSecretFragments(t *testing.T) {
	pw := serviceAccountJSON(t, testKey(t))
	sa, err := parseServiceAccount(pw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := driver.ConnConfig{DSN: "firestore://p", Password: driver.NewSecret(pw)}
	frags := secretFragments(sa)
	line := strings.Split(sa.PrivateKey, "\n")[1]
	for _, msg := range []string{
		"failed: " + sa.PrivateKey,
		"failed: " + strings.ReplaceAll(sa.PrivateKey, "\n", `\n`),
		"failed: key id " + sa.PrivateKeyID,
		"failed: " + line,
		"failed: " + pw,
	} {
		got := scrubMessage(msg, cfg, frags)
		if strings.Contains(got, sa.PrivateKeyID) || strings.Contains(got, line) {
			t.Errorf("scrubMessage left key material: %q", got)
		}
	}
	// scrubError masks a *driver.Error and keeps its kind and context
	// identity, without unwrapping to the unmasked original.
	orig := errors.New("rpc failed: " + line)
	err = scrubError(driver.Wrap(driver.ErrAuth, Name, "open", errors.Join(orig, context.Canceled)), cfg, frags)
	if strings.Contains(err.Error(), line) || errors.Is(err, orig) {
		t.Errorf("scrubError kept the key: %v", err)
	}
	if !errors.Is(err, driver.ErrAuth) || !errors.Is(err, context.Canceled) {
		t.Errorf("scrubError lost the kind or the context error: %v", err)
	}
}

// A good key opens a client (no RPC is made against the emulator address
// until the ping, which fails fast on a closed port) and never shows up in
// the error.
func TestOpenWithKeyNeverLeaks(t *testing.T) {
	pw := serviceAccountJSON(t, testKey(t))
	sa, _ := parseServiceAccount(pw)
	defer func(d time.Duration) { openTimeout = d }(openTimeout)
	openTimeout = 2 * time.Second
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "firestore://p?emulator=127.0.0.1:1", Password: driver.NewSecret(pw)})
	if err == nil {
		_ = c.Close()
		t.Fatal("Open against a closed port succeeded")
	}
	for _, f := range secretFragments(sa) {
		if strings.Contains(err.Error(), f) {
			t.Fatalf("error holds key material: %v", err)
		}
	}
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("want ErrConnection, got %v", err)
	}
}

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func serviceAccountJSON(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "p",
		"private_key_id": "0123456789abcdef0123456789abcdef",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "tobira@test.iam.gserviceaccount.com",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
