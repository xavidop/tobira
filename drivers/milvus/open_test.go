package milvus

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  driver.ConnConfig
		want dsnConfig
	}{
		{"defaults", driver.ConnConfig{DSN: "milvus://"}, dsnConfig{host: "localhost", port: 19530, db: "default"}},
		{"host port db", driver.ConnConfig{DSN: "milvus://10.0.0.5:29530/analytics"}, dsnConfig{host: "10.0.0.5", port: 29530, db: "analytics", tls: true}},
		{"trailing slash", driver.ConnConfig{DSN: "milvus://localhost/"}, dsnConfig{host: "localhost", port: 19530, db: "default"}},
		{"user and password", driver.ConnConfig{DSN: "milvus://root:Milvus@localhost"}, dsnConfig{host: "localhost", port: 19530, db: "default", token: "root:Milvus"}},
		{"password ref replaces dsn password", driver.ConnConfig{DSN: "milvus://root:old@localhost", Password: driver.NewSecret("new")}, dsnConfig{host: "localhost", port: 19530, db: "default", token: "root:new"}},
		{"api key in password ref", driver.ConnConfig{DSN: "milvus://localhost", Password: driver.NewSecret("api-key-123")}, dsnConfig{host: "localhost", port: 19530, db: "default", token: "api-key-123"}},
		{"token as user:pass ref", driver.ConnConfig{DSN: "milvus://localhost", Password: driver.NewSecret("bob:pw")}, dsnConfig{host: "localhost", port: 19530, db: "default", token: "bob:pw"}},
		{"tls default on with a token off loopback", driver.ConnConfig{DSN: "milvus://milvus.internal", Password: driver.NewSecret("k")}, dsnConfig{host: "milvus.internal", port: 19530, db: "default", token: "k", tls: true}},
		{"tls default on off loopback without a token", driver.ConnConfig{DSN: "milvus://milvus.internal"}, dsnConfig{host: "milvus.internal", port: 19530, db: "default", tls: true}},
		{"tls default off on loopback", driver.ConnConfig{DSN: "milvus://127.0.0.1", Password: driver.NewSecret("k")}, dsnConfig{host: "127.0.0.1", port: 19530, db: "default", token: "k"}},
		{"remote plaintext needs allow_plaintext", driver.ConnConfig{DSN: "milvus://milvus.internal?tls=false&allow_plaintext=1", Password: driver.NewSecret("k")}, dsnConfig{host: "milvus.internal", port: 19530, db: "default", token: "k", plaintextToken: true}},
		{"remote plaintext by options", driver.ConnConfig{DSN: "milvus://milvus.internal", Options: map[string]string{"tls": "false", "allow_plaintext": "true"}}, dsnConfig{host: "milvus.internal", port: 19530, db: "default"}},
		{"tls on loopback by option", driver.ConnConfig{DSN: "milvus://127.0.0.1", Options: map[string]string{"tls": "true"}}, dsnConfig{host: "127.0.0.1", port: 19530, db: "default", tls: true}},
		{"ipv6", driver.ConnConfig{DSN: "milvus://[::1]:19531/db1"}, dsnConfig{host: "::1", port: 19531, db: "db1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDSN(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("parseDSN = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDSNAddress(t *testing.T) {
	if got := (dsnConfig{host: "::1", port: 1}).address(); got != "[::1]:1" {
		t.Errorf("address = %q", got)
	}
	if got := (dsnConfig{host: "h", port: 19530}).address(); got != "h:19530" {
		t.Errorf("address = %q", got)
	}
}

func TestParseDSNErrors(t *testing.T) {
	for _, tc := range []struct {
		name, dsn string
		opts      map[string]string
	}{
		{"wrong scheme", "qdrant://localhost", nil},
		{"bad port", "milvus://localhost:99999", nil},
		{"port not a number", "milvus://localhost:abc", nil},
		{"nested path", "milvus://localhost/db/coll", nil},
		{"token parameter", "milvus://localhost?token=abc", nil},
		{"api key parameter", "milvus://localhost?api_key=abc", nil},
		{"password parameter", "milvus://localhost?password=abc", nil},
		{"unknown parameter", "milvus://localhost?foo=1", nil},
		{"bad tls", "milvus://localhost?tls=maybe", nil},
		{"unknown option", "milvus://localhost", map[string]string{"foo": "1"}},
		{"plaintext to zilliz cloud", "milvus://:key@in03-abc.api.gcp-us-west1.zillizcloud.com?tls=false", nil},
		{"plaintext to zilliz cloud (case, dot)", "milvus://:key@IN03-ABC.ZILLIZCLOUD.COM.?tls=false", nil},
		{"bad database name", "milvus://localhost/bad%20name", nil},
		{"remote plaintext without allow_plaintext", "milvus://milvus.internal?tls=false", nil},
		{"remote plaintext without allow_plaintext, with a token", "milvus://:abc@milvus.internal?tls=false", nil},
		{"allow_plaintext alone is not plaintext", "milvus://milvus.internal?allow_plaintext=maybe", nil},
		{"zilliz cloud even with allow_plaintext", "milvus://:key@x.zillizcloud.com?tls=false&allow_plaintext=1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn, Options: tc.opts})
			if !errors.Is(err, driver.ErrSyntax) {
				t.Fatalf("parseDSN(%q) = %v, want ErrSyntax", tc.dsn, err)
			}
			if strings.Contains(err.Error(), "abc") || strings.Contains(err.Error(), "key@") {
				t.Errorf("the error echoes the credential: %v", err)
			}
		})
	}
}

// Open errors never carry the token, in any of its spellings.
func TestOpenNeverLeaksToken(t *testing.T) {
	const secret = "s3cr3t/tok+en"
	for _, cfg := range []driver.ConnConfig{
		{DSN: "milvus://root:" + url.QueryEscape(secret) + "@127.0.0.1:1"},
		{DSN: "milvus://127.0.0.1:1", Password: driver.NewSecret(secret)},
		{DSN: "milvus://user@127.0.0.1:1", Password: driver.NewSecret(secret)},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		c, err := New().Open(ctx, cfg)
		cancel()
		if err == nil {
			_ = c.Close()
			t.Fatal("Open on a closed port succeeded")
		}
		msg := err.Error()
		for _, form := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret), base64.StdEncoding.EncodeToString([]byte("root:" + secret))} {
			if strings.Contains(msg, form) {
				t.Errorf("Open error carries the token (%q): %v", form, msg)
			}
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("Open error is not a milvus driver.Error: %#v", err)
		}
	}
}

func TestRedactMasksEveryForm(t *testing.T) {
	const tok = "a/b+c:d"
	for _, form := range []string{tok, url.QueryEscape(tok), url.PathEscape(tok), base64.StdEncoding.EncodeToString([]byte(tok))} {
		err := redact(errors.New("boom "+form+" end"), driver.ConnConfig{}, tok)
		if strings.Contains(err.Error(), form) {
			t.Errorf("redact left %q in %q", form, err)
		}
	}
}

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "milvus" || info.Kind != driver.KindVector || info.Dialect != "milvus" {
		t.Errorf("Info = %+v", info)
	}
	for _, cp := range []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapVectorSearcher, driver.CapReadOnly} {
		if !driver.HasCapability(info, cp) {
			t.Errorf("Info lacks %s", cp)
		}
	}
	if driver.HasCapability(info, driver.CapTransactor) {
		t.Error("Milvus has no transactions")
	}
}
