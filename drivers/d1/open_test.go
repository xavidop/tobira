package d1

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	tok := driver.NewSecret("tok")
	good := []struct {
		dsn     string
		pw      driver.Secret
		wantURL string
		wantTok string
	}{
		{"d1://" + fakeAccount + "/" + fakeDatabase, tok,
			"https://api.cloudflare.com/client/v4/accounts/" + fakeAccount + "/d1/database/" + fakeDatabase + "/raw", "tok"},
		{"d1://" + fakeAccount + "/" + fakeDatabase + "/", tok,
			"https://api.cloudflare.com/client/v4/accounts/" + fakeAccount + "/d1/database/" + fakeDatabase + "/raw", "tok"},
		{"d1://:urltok@acct/db-1", driver.Secret{}, "https://api.cloudflare.com/client/v4/accounts/acct/d1/database/db-1/raw", "urltok"},
		{"d1://:urltok@acct/db-1", tok, "https://api.cloudflare.com/client/v4/accounts/acct/d1/database/db-1/raw", "tok"},
		{"d1://acct/db?endpoint=https%3A%2F%2Fproxy.example.com%2Fcf%2Fv4%2F", tok, "https://proxy.example.com/cf/v4/accounts/acct/d1/database/db/raw", "tok"},
		{"d1://acct/db?endpoint=http%3A%2F%2F127.0.0.1%3A8787%2Fclient%2Fv4", tok, "http://127.0.0.1:8787/client/v4/accounts/acct/d1/database/db/raw", "tok"},
		{"d1://acct/db?endpoint=http%3A%2F%2Flocalhost%3A8787", tok, "http://localhost:8787/accounts/acct/d1/database/db/raw", "tok"},
		// A remote plaintext proxy needs the explicit opt-in.
		{"d1://acct/db?allow_plaintext=1&endpoint=http%3A%2F%2Fproxy.internal%3A8787", tok, "http://proxy.internal:8787/accounts/acct/d1/database/db/raw", "tok"},
		{"d1://acct/db?endpoint=https%3A%2F%2Fproxy.example.com&allow_plaintext=true", tok, "https://proxy.example.com/accounts/acct/d1/database/db/raw", "tok"},
	}
	for _, tc := range good {
		d, err := parseDSN(driver.ConnConfig{DSN: tc.dsn, Password: tc.pw})
		if err != nil {
			t.Errorf("parseDSN(%q): %v", tc.dsn, err)
			continue
		}
		if d.url != tc.wantURL || d.token != tc.wantTok {
			t.Errorf("parseDSN(%q) = %q token %q, want %q token %q", tc.dsn, d.url, d.token, tc.wantURL, tc.wantTok)
		}
	}

	bad := []struct {
		dsn  string
		kind error
	}{
		{"sqlite:/tmp/x.db", driver.ErrSyntax},
		{"https://api.cloudflare.com/x", driver.ErrSyntax},
		{"d1://", driver.ErrSyntax},
		{"d1://acct", driver.ErrSyntax},
		{"d1://acct/", driver.ErrSyntax},
		{"d1://acct/db/extra", driver.ErrSyntax},
		{"d1://ac%2Fct/db", driver.ErrSyntax},
		{"d1://acct/d%2Fb", driver.ErrSyntax},
		{"d1://acct/db..x", driver.ErrSyntax},
		{"d1://acct:9/db", driver.ErrSyntax},
		{"d1://acct/db#frag", driver.ErrSyntax},
		{"d1://acct/db?token=abc", driver.ErrSyntax},
		{"d1://acct/db?api_token=abc", driver.ErrSyntax},
		{"d1://acct/db?apiToken=abc", driver.ErrSyntax},
		{"d1://acct/db?bogus=1", driver.ErrSyntax},
		{"d1://acct/db?endpoint=x&endpoint=y", driver.ErrSyntax},
		{"d1://acct/db?endpoint=https%3A%2F%2Fa.example.com&Endpoint=https%3A%2F%2Fb.example.com", driver.ErrSyntax},
		{"d1://acct/db?endpoint=http%3A%2F%2Fevil.example.com", driver.ErrSyntax}, // token in clear to a remote host
		{"d1://acct/db?endpoint=ftp%3A%2F%2F127.0.0.1", driver.ErrSyntax},
		{"d1://acct/db?endpoint=https%3A%2F%2Fu%3Ap%40proxy.example.com", driver.ErrSyntax},
		{"d1://acct/db?endpoint=https%3A%2F%2Fproxy.example.com%3Fa%3D1", driver.ErrSyntax},
		{"d1://acct/db?endpoint=https%3A%2F%2F", driver.ErrSyntax},
		{"d1://user@acct/db", driver.ErrSyntax},
		{"d1://acct/db?endpoint=http%3A%2F%2Fevil.example.com&allow_plaintext=0", driver.ErrSyntax},
		{"d1://acct/db?allow_plaintext=maybe", driver.ErrSyntax},
		{"d1://acct/db?allow_plaintext=1&allow_plaintext=1", driver.ErrSyntax},
		// Cloudflare's own API is always TLS.
		{"d1://acct/db?allow_plaintext=1&endpoint=http%3A%2F%2Fapi.cloudflare.com%2Fclient%2Fv4", driver.ErrSyntax},
		{"d1://acct/db?allow_plaintext=1&endpoint=http%3A%2F%2FAPI.CloudFlare.com.%2Fclient%2Fv4", driver.ErrSyntax},
		{"d1://acct/db?allow_plaintext=1&endpoint=http%3A%2F%2Fcloudflare.com", driver.ErrSyntax},
	}
	for _, tc := range bad {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn, Password: tok})
		if !errors.Is(err, tc.kind) {
			t.Errorf("parseDSN(%q) = %v, want %v", tc.dsn, err, tc.kind)
		}
	}

	if _, err := parseDSN(driver.ConnConfig{DSN: "d1://acct/db"}); !errors.Is(err, driver.ErrAuth) {
		t.Errorf("no token: %v, want ErrAuth", err)
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "d1://acct/db", Password: driver.NewSecret("a\r\nX-Evil: 1")}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("token with CR/LF: %v, want ErrSyntax", err)
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "d1://acct/db", Password: tok, Options: map[string]string{"x": "y"}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("options: %v, want ErrSyntax", err)
	}
}

// TestOpenErrorsNeverHoldTheToken: every Open failure is redacted, whatever
// echoes the token (the server's message, a URL error).
func TestOpenErrorsNeverHoldTheToken(t *testing.T) {
	f := newFakeD1(t)
	const secret = "s3cret-token-value"
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		writeEnvelope(w, http.StatusBadRequest, nil, cfError{7500, "bad header " + r.Header.Get("Authorization")})
		return true
	})
	cfg := f.cfg(false)
	cfg.Password = driver.NewSecret(secret)
	_, err := New().Open(context.Background(), cfg)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("Open error leaks the token or is nil: %v", err)
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Driver != Name {
		t.Errorf("Open error is not a d1 driver.Error: %#v", err)
	}

	cfg.DSN = "d1://:" + secret + "@acct/db?endpoint=http%3A%2F%2F127.0.0.1%3A1" // nothing listens on port 1
	cfg.Password = driver.Secret{}
	_, err = New().Open(context.Background(), cfg)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("connection error leaks the token or is nil: %v", err)
	}
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("unreachable endpoint: %v, want ErrConnection", err)
	}
}

func TestOpenBadAuthAndWrongDatabase(t *testing.T) {
	f := newFakeD1(t)
	cfg := f.cfg(false)
	cfg.Password = driver.NewSecret("nope")
	if _, err := New().Open(context.Background(), cfg); !errors.Is(err, driver.ErrAuth) {
		t.Errorf("bad token: %v, want ErrAuth", err)
	}
	cfg = f.cfg(false)
	cfg.DSN = strings.Replace(cfg.DSN, fakeDatabase, "00000000-0000-0000-0000-000000000000", 1)
	if _, err := New().Open(context.Background(), cfg); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown database: %v, want ErrNotFound", err)
	}
}

func TestRegisteredByScheme(t *testing.T) {
	d, ok := driver.Default.ForScheme(driver.Scheme("d1://acct/db"))
	if !ok || d.Info().Name != Name {
		t.Fatalf("scheme lookup: %v, %v", d, ok)
	}
	f := newFakeD1(t)
	c, err := driver.OpenConfig(context.Background(), f.cfg(true))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

// Plain http to a host that is not loopback needs allow_plaintext=1, and
// Cloudflare's own API (*.cloudflare.com) is never reached in clear.
func TestEndpointPlaintextRule(t *testing.T) {
	tok := driver.NewSecret("tok")
	for _, tc := range []struct{ dsn, want string }{
		{"d1://acct/db?endpoint=http%3A%2F%2Fevil.example.com", "allow_plaintext=1"},
		{"d1://acct/db?endpoint=http%3A%2F%2Fevil.example.com&allow_plaintext=0", "allow_plaintext=1"},
		{"d1://acct/db?allow_plaintext=maybe", "allow_plaintext must be"},
		{"d1://acct/db?allow_plaintext=1&endpoint=http%3A%2F%2Fapi.cloudflare.com%2Fclient%2Fv4", "Cloudflare"},
		{"d1://acct/db?allow_plaintext=1&endpoint=http%3A%2F%2FAPI.CloudFlare.com.%2Fclient%2Fv4", "Cloudflare"},
		{"d1://acct/db?allow_plaintext=1&endpoint=http%3A%2F%2Fcloudflare.com", "Cloudflare"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn, Password: tok})
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDSN(%q) = %v, want ErrSyntax containing %q", tc.dsn, err, tc.want)
		}
	}
}
