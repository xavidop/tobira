package pinecone

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "pinecone" || info.Kind != driver.KindVector || info.Dialect != "pinecone" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "pinecone" {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := map[driver.Capability]bool{driver.CapBrowser: true, driver.CapMutator: true, driver.CapVectorSearcher: true, driver.CapReadOnly: true}
	if len(info.Capabilities) != len(want) {
		t.Fatalf("caps = %v", info.Capabilities)
	}
	for _, c := range info.Capabilities {
		if !want[c] {
			t.Errorf("unexpected capability %v", c)
		}
	}
	if _, ok := driver.Default.Lookup(Name); !ok {
		t.Error("not registered in driver.Default")
	}
	if d, ok := driver.Default.ForScheme("pinecone"); !ok || d.Info().Name != Name {
		t.Error("pinecone scheme not routed to the driver")
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		name string
		cfg  driver.ConnConfig
		want dsnConfig
	}{
		{"cloud default", driver.ConnConfig{DSN: "pinecone://"}, dsnConfig{control: "https://api.pinecone.io"}},
		{"cloud key in password", driver.ConnConfig{DSN: "pinecone://:s3cret@"}, dsnConfig{control: "https://api.pinecone.io", apiKey: "s3cret"}},
		{"password field wins", driver.ConnConfig{DSN: "pinecone://:fromdsn@?api_key=fromparam", Password: driver.NewSecret("override")},
			dsnConfig{control: "https://api.pinecone.io", apiKey: "override"}},
		{"api_key param", driver.ConnConfig{DSN: "pinecone://?api_key=pk-1"}, dsnConfig{control: "https://api.pinecone.io", apiKey: "pk-1"}},
		{"apikey param", driver.ConnConfig{DSN: "pinecone://?apikey=pk-2"}, dsnConfig{control: "https://api.pinecone.io", apiKey: "pk-2"}},
		{"param beats userinfo", driver.ConnConfig{DSN: "pinecone://:u@?apikey=p"}, dsnConfig{control: "https://api.pinecone.io", apiKey: "p"}},
		{"local control plane", driver.ConnConfig{DSN: "pinecone://localhost:5080?tls=false"}, dsnConfig{control: "http://localhost:5080"}},
		{"control plane host", driver.ConnConfig{DSN: "pinecone://api.example.com"}, dsnConfig{control: "https://api.example.com"}},
		{"index host", driver.ConnConfig{DSN: "pinecone://?host=docs-abc.svc.aped.pinecone.io&index=docs"},
			dsnConfig{host: "https://docs-abc.svc.aped.pinecone.io", index: "docs"}},
		{"index host default name", driver.ConnConfig{DSN: "pinecone://?host=localhost:5081&tls=false&metric=cosine"},
			dsnConfig{host: "http://localhost:5081", index: "default", metric: "cosine"}},
		{"index host options", driver.ConnConfig{DSN: "pinecone://", Options: map[string]string{"host": "127.0.0.1:5081", "tls": "false", "index": "v", "metric": "dotproduct"}},
			dsnConfig{host: "http://127.0.0.1:5081", index: "v", metric: "dotproduct"}},
		{"key to remote, explicit plaintext", driver.ConnConfig{DSN: "pinecone://:k@db.example?tls=false&allow_plaintext=1"},
			dsnConfig{control: "http://db.example", apiKey: "k", plaintextKey: true}},
		{"allow_plaintext as an option", driver.ConnConfig{DSN: "pinecone://db.example", Options: map[string]string{"tls": "false", "allow_plaintext": "true"}},
			dsnConfig{control: "http://db.example"}},
		{"allow_plaintext alone keeps TLS", driver.ConnConfig{DSN: "pinecone://db.example?allow_plaintext=1"},
			dsnConfig{control: "https://db.example"}},
		{"key to remote index host, plaintext", driver.ConnConfig{DSN: "pinecone://:k@?host=db.example:5081&tls=false&allow_plaintext=1"},
			dsnConfig{host: "http://db.example:5081", index: "default", apiKey: "k", plaintextKey: true}},
		{"key to loopback, plaintext", driver.ConnConfig{DSN: "pinecone://:k@127.0.0.1:5080?tls=false"},
			dsnConfig{control: "http://127.0.0.1:5080", apiKey: "k"}},
		{"bracketed IPv6 loopback index host without a port", driver.ConnConfig{DSN: "pinecone://:k@?host=[::1]&tls=false"},
			dsnConfig{host: "http://[::1]", index: "default", apiKey: "k"}},
	}
	for _, tc := range tests {
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

func TestParseDSNErrors(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"qdrant://x", "pinecone://"},
		{"pinecone://h/path", "no path"},
		{"pinecone://h?bogus=1", "unknown DSN parameter"},
		{"pinecone://h:99999", "invalid port"},
		{"pinecone://h?tls=maybe", "tls must be"},
		{"pinecone://api.example.com?host=idx.example.com", "not both"},
		{"pinecone://?host=https://idx.example.com", "without a scheme"},
		{"pinecone://?host=idx.example.com/x", "without a scheme"},
		{"pinecone://?metric=cosine", "host="},
		{"pinecone://?index=docs", "host="},
		{"pinecone://?host=h&metric=manhattan", "metric must be"},
		{"pinecone://?host=h&index=", "index"},
		// Pinecone's cloud is never reached in clear.
		{"pinecone://?tls=false", "pinecone.io"},
		{"pinecone://api.pinecone.io?tls=false", "pinecone.io"},
		{"pinecone://API.Pinecone.IO.:443?tls=false", "pinecone.io"},
		{"pinecone://?host=docs-abc.svc.aped-4627.pinecone.io&tls=false", "pinecone.io"},
		{"pinecone://api.pinecone.io?tls=false&allow_plaintext=1", "pinecone.io"},
		// Plain http to a host that is not loopback needs the explicit opt-in.
		{"pinecone://db.example?tls=false", "allow_plaintext=1"},
		{"pinecone://:k@?host=db.example:5081&tls=false", "allow_plaintext=1"},
		{"pinecone://db.example?tls=false&allow_plaintext=0", "allow_plaintext=1"},
		{"pinecone://db.example?allow_plaintext=maybe", "allow_plaintext must be"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn})
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDSN(%q) = %v, want ErrSyntax containing %q", tc.dsn, err, tc.want)
		}
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "pinecone://", Options: map[string]string{"nope": "1"}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("unknown option: %v", err)
	}
}

// The API key never appears in an Open error, in whichever DSN position
// it was given, and the shared DSN redactor masks api_key and apikey.
func TestOpenNeverLeaksKey(t *testing.T) {
	const key = "pcsk_SUPERSECRET_9f8e7d"
	for _, dsn := range []string{
		"pinecone://?api_key=" + key + "&bogus=1",
		"pinecone://?apikey=" + key + "&bogus=1",
		"pinecone://:" + key + "@h?bogus=1",
		"pinecone://:" + key + "@127.0.0.1:1?tls=false",
		"pinecone://?api_key=" + key + "&host=127.0.0.1:1&tls=false",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn})
		cancel()
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%s) succeeded", driver.RedactDSN(dsn))
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("Open error leaks the key: %v", err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("Open error is not a pinecone driver.Error: %#v", err)
		}
		if red := driver.RedactDSN(dsn); strings.Contains(red, key) {
			t.Errorf("RedactDSN(%q) = %q keeps the key", dsn, red)
		}
		if msg := driver.RedactMessage("failed: "+dsn, driver.ConnConfig{DSN: dsn}); strings.Contains(msg, key) {
			t.Errorf("RedactMessage keeps the key: %q", msg)
		}
	}
	// The password field is masked too.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "pinecone://127.0.0.1:1?tls=false&bogus=" + key, Password: driver.NewSecret(key)})
	if err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("Open with the key in the password field: %v", err)
	}
}

// An unreachable control plane fails Open with ErrConnection.
func TestOpenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "pinecone://127.0.0.1:1?tls=false"})
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Open(closed port) = %v, want ErrConnection", err)
	}
}

// A redirect is never followed: Go forwards custom headers such as Api-Key
// to the target, even on another host or over plain http, so the second
// server must never see a request.
func TestRedirectIsNotFollowed(t *testing.T) {
	var hits, keyed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Api-Key") != "" || r.Header.Get("Authorization") != "" {
			keyed.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"indexes":[]}`))
	}))
	t.Cleanup(target.Close)
	targetURL := strings.Replace(target.URL, "127.0.0.1", "localhost", 1) // another host
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetURL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	dsn := "pinecone://" + strings.TrimPrefix(redirector.URL, "http://") + "?tls=false"
	c, err := New().Open(t.Context(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("s3cr3t-k3y")})
	if err == nil {
		_ = c.Close()
		t.Error("Open through a redirect succeeded")
	}
	if err != nil && strings.Contains(err.Error(), "s3cr3t-k3y") {
		t.Errorf("the error carries the key: %v", err)
	}
	if n := keyed.Load(); n != 0 {
		t.Errorf("the redirect target got the API key %d times", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}
