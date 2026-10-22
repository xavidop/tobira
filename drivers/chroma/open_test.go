package chroma

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "chroma" || info.Kind != driver.KindVector || info.Dialect != "chroma" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "chroma" {
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
	if d, ok := driver.Default.ForScheme("chroma"); !ok || d.Info().Name != Name {
		t.Error("chroma scheme not routed to the driver")
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		name string
		cfg  driver.ConnConfig
		want dsnConfig
	}{
		{"loopback defaults", driver.ConnConfig{DSN: "chroma://localhost"}, dsnConfig{scheme: "http", host: "localhost", port: 8000, authHeader: headerChromaToken}},
		{"empty host", driver.ConnConfig{DSN: "chroma://"}, dsnConfig{scheme: "http", host: "localhost", port: 8000, authHeader: headerChromaToken}},
		{"remote is https", driver.ConnConfig{DSN: "chroma://db.example"}, dsnConfig{scheme: "https", host: "db.example", port: 443, authHeader: headerChromaToken}},
		{"remote port", driver.ConnConfig{DSN: "chroma://db.example:8000"}, dsnConfig{scheme: "https", host: "db.example", port: 8000, authHeader: headerChromaToken}},
		{"remote plaintext", driver.ConnConfig{DSN: "chroma://db.example:8000?tls=false&allow_plaintext=1"}, dsnConfig{scheme: "http", host: "db.example", port: 8000, authHeader: headerChromaToken}},
		{"loopback tls", driver.ConnConfig{DSN: "chroma://127.0.0.1?tls=true"}, dsnConfig{scheme: "https", host: "127.0.0.1", port: 443, authHeader: headerChromaToken}},
		{"ipv6", driver.ConnConfig{DSN: "chroma://[::1]:9000"}, dsnConfig{scheme: "http", host: "::1", port: 9000, authHeader: headerChromaToken}},
		{"tenant and database", driver.ConnConfig{DSN: "chroma://localhost:8000?tenant=acme&database=prod"}, dsnConfig{scheme: "http", host: "localhost", port: 8000, tenant: "acme", database: "prod", authHeader: headerChromaToken}},
		{"token in password", driver.ConnConfig{DSN: "chroma://:tok@localhost"}, dsnConfig{scheme: "http", host: "localhost", port: 8000, token: "tok", authHeader: headerChromaToken}},
		{"token param", driver.ConnConfig{DSN: "chroma://localhost?token=tok"}, dsnConfig{scheme: "http", host: "localhost", port: 8000, token: "tok", authHeader: headerChromaToken}},
		{"password ref wins", driver.ConnConfig{DSN: "chroma://:old@localhost", Password: driver.NewSecret("new")}, dsnConfig{scheme: "http", host: "localhost", port: 8000, token: "new", authHeader: headerChromaToken}},
		{"bearer header", driver.ConnConfig{DSN: "chroma://localhost?auth_header=authorization"}, dsnConfig{scheme: "http", host: "localhost", port: 8000, authHeader: headerAuthorization}},
		{"options", driver.ConnConfig{DSN: "chroma://localhost", Options: map[string]string{"tenant": "t1", "database": "d1", "tls": "false", "auth_header": "x-chroma-token"}},
			dsnConfig{scheme: "http", host: "localhost", port: 8000, tenant: "t1", database: "d1", authHeader: headerChromaToken}},
		{"plaintext option", driver.ConnConfig{DSN: "chroma://db.example:8000", Options: map[string]string{"tls": "false", "allow_plaintext": "1"}},
			dsnConfig{scheme: "http", host: "db.example", port: 8000, authHeader: headerChromaToken}},
		{"allow_plaintext alone keeps TLS", driver.ConnConfig{DSN: "chroma://db.example?allow_plaintext=1"},
			dsnConfig{scheme: "https", host: "db.example", port: 443, authHeader: headerChromaToken}},
		{"remote token plaintext warns", driver.ConnConfig{DSN: "chroma://:tok@db.example:8000?tls=false&allow_plaintext=true"},
			dsnConfig{scheme: "http", host: "db.example", port: 8000, token: "tok", authHeader: headerChromaToken, plaintextToken: true}},
		{"cloud", driver.ConnConfig{DSN: "chroma://api.trychroma.com?tenant=t&database=d", Password: driver.NewSecret("ck-1")},
			dsnConfig{scheme: "https", host: "api.trychroma.com", port: 443, tenant: "t", database: "d", token: "ck-1", authHeader: headerChromaToken}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDSN(tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("parseDSN = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseDSNErrors(t *testing.T) {
	const tok = "s3cr3t-t0ken"
	for _, dsn := range []string{
		"qdrant://localhost",
		"chroma://localhost/db",
		"chroma://localhost?bogus=1",
		"chroma://localhost?tls=maybe",
		"chroma://localhost:0",
		"chroma://localhost:99999",
		"chroma://user:" + tok + "@localhost",
		"chroma://:" + tok + "@localhost?token=" + tok,
		"chroma://localhost?auth_header=cookie&token=" + tok,
		"chroma://localhost?tenant=",
		"chroma://localhost?database=",
		"chroma://localhost?tenant=a%2Fb",
		"chroma://localhost?database=..",
		"chroma://:" + tok + "@api.trychroma.com?tls=false",
		"chroma://:" + tok + "@api.trychroma.com?tls=false&allow_plaintext=1",
		"chroma://:" + tok + "@db.example:8000?tls=false",
		"chroma://db.example:8000?tls=false",
		"chroma://db.example:8000?tls=false&allow_plaintext=0",
		"chroma://db.example:8000?tls=false&allow_plaintext=maybe",
		"chroma://:" + tok + "@eu.TRYCHROMA.com.?tls=false",
		"chroma://%zz:" + tok + "@localhost",
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: dsn})
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseDSN(%s): want ErrSyntax, got %v", dsn, err)
			continue
		}
		if strings.Contains(err.Error(), tok) {
			t.Errorf("parseDSN(%s): the error carries the token: %v", dsn, err)
		}
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "chroma://localhost", Options: map[string]string{"nope": "1"}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("unknown option: %v", err)
	}
}

func TestRedactDSNMasksToken(t *testing.T) {
	for _, dsn := range []string{
		"chroma://:s3cr3t@db.example:8000",
		"chroma://db.example:8000?token=s3cr3t",
		"chroma://db.example:8000?tenant=t&token=s3cr3t&database=d",
	} {
		if got := driver.RedactDSN(dsn); strings.Contains(got, "s3cr3t") {
			t.Errorf("RedactDSN(%s) = %s", dsn, got)
		}
	}
}

func TestOpenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "chroma://127.0.0.1:1", Password: driver.NewSecret("s3cr3t-t0ken")})
	if !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("want ErrConnection, got %v", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-t0ken") {
		t.Errorf("error carries the token: %v", err)
	}
}

func TestOpenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "chroma://127.0.0.1:1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestOpenAuth(t *testing.T) {
	f := newFake(t)
	f.token = "good-token"
	for _, tt := range []struct {
		name string
		cfg  driver.ConnConfig
		want error
	}{
		{"password ref", driver.ConnConfig{DSN: f.dsn(""), Password: driver.NewSecret("good-token")}, nil},
		{"password position", driver.ConnConfig{DSN: strings.Replace(f.dsn(""), "chroma://", "chroma://:good-token@", 1)}, nil},
		{"token param", driver.ConnConfig{DSN: f.dsn("token=good-token")}, nil},
		{"no token", driver.ConnConfig{DSN: f.dsn("")}, driver.ErrAuth},
		{"wrong token", driver.ConnConfig{DSN: f.dsn(""), Password: driver.NewSecret("bad-token-xyz")}, driver.ErrAuth},
		{"wrong header", driver.ConnConfig{DSN: f.dsn("auth_header=authorization"), Password: driver.NewSecret("good-token")}, driver.ErrAuth},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New().Open(t.Context(), tt.cfg)
			if err == nil {
				_ = c.Close()
			}
			if tt.want == nil && err != nil {
				t.Fatalf("Open: %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Open: want %v, got %v", tt.want, err)
			}
			if err != nil && (strings.Contains(err.Error(), "good-token") || strings.Contains(err.Error(), "bad-token-xyz")) {
				t.Errorf("error carries the token: %v", err)
			}
		})
	}
	f.header = "authorization"
	c := openFake(t, f, driver.ConnConfig{DSN: f.dsn("auth_header=authorization"), Password: driver.NewSecret("good-token")})
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Ping with a bearer token: %v", err)
	}
}

func TestOpenTenantAndDatabase(t *testing.T) {
	f := newFake(t)
	f.tenant, f.dbs = "acme", []string{"only_db"}
	// Neither given: both come from the identity, the database because the
	// token sees exactly one.
	c := openFake(t, f, driver.ConnConfig{})
	if c.tenant != "acme" || c.database != "only_db" {
		t.Errorf("tenant/database = %q/%q, want acme/only_db", c.tenant, c.database)
	}
	// A missing database fails Open.
	_, err := New().Open(t.Context(), driver.ConnConfig{DSN: f.dsn("database=nope")})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing database: want ErrNotFound, got %v", err)
	}
	// Several databases and none named: Chroma's default.
	f.dbs = []string{"a", "default_database"}
	c = openFake(t, f, driver.ConnConfig{})
	if c.database != "default_database" {
		t.Errorf("database = %q, want default_database", c.database)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newFake(t)
	c := openFake(t, f, driver.ConnConfig{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(t.Context()); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping after Close: want ErrConnection, got %v", err)
	}
	if _, err := c.Query(t.Context(), driver.Query{Text: `{"collection":"x"}`}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Query after Close: want ErrConnection, got %v", err)
	}
}

// The client never follows a redirect: Go forwards custom headers such as
// x-chroma-token to the redirect target, even on another host or over plain
// http. A redirect is an error that names the problem, and the target never
// sees a request.
func TestRedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	host := strings.TrimPrefix(redirector.URL, "http://")
	for _, dsn := range []string{
		"chroma://" + host + "?tenant=t&database=d",
		"chroma://" + host,
		"chroma://" + host + "?auth_header=authorization",
	} {
		_, err := New().Open(t.Context(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("s3cr3t-t0ken")})
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Errorf("Open(%s) through a redirect: want a redirect error, got %v", dsn, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t-t0ken") {
			t.Errorf("the error carries the token: %v", err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

func TestIdentityNamesAreValidated(t *testing.T) {
	for _, tc := range []struct{ tenant, db string }{{"..", ""}, {"ok", ".."}, {"a/b", ""}, {"ok", "x/y"}} {
		f := newFake(t)
		f.tenant = tc.tenant
		if tc.db != "" {
			f.dbs = []string{tc.db}
		}
		_, err := New().Open(t.Context(), driver.ConnConfig{DSN: f.dsn("")})
		if err == nil || !strings.Contains(err.Error(), "identity") {
			t.Errorf("identity %+v: want a refusal naming the identity, got %v", tc, err)
		}
	}
}

// A reply over the cap is a size limit (ErrLimit), so the CLI and TUI show
// the narrowing hint.
func TestOversizedReplyIsErrLimit(t *testing.T) {
	f := newFake(t)
	coll := f.addColl("default_database", "things", "l2", 2)
	for i := range 50 {
		coll.add(strconv.Itoa(i), []float64{1, 0}, strings.Repeat("x", 100), nil)
	}
	c := openFake(t, f, driver.ConnConfig{})
	c.api.maxBody = 1000
	_, err := c.Query(t.Context(), driver.Query{Text: `{"collection":"things"}`})
	if !errors.Is(err, driver.ErrLimit) || errors.Is(err, driver.ErrConnection) || !strings.Contains(err.Error(), "fewer records") {
		t.Errorf("oversized reply: want ErrLimit with the hint, got %v", err)
	}
}
