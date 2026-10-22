package cosmos

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// testKey is a syntactically valid account key (base64) used only by the
// unit tests; no server ever accepts it.
const testKey = "dG9iaXJhLXVuaXQtdGVzdC1rZXktbm90LWEtcmVhbC1zZWNyZXQtMDEyMzQ1Njc4OQ=="

func TestParseDSN(t *testing.T) {
	for _, tc := range []struct {
		dsn      string
		endpoint string
		db, key  string
		insecure bool
	}{
		{"cosmos://acct.documents.azure.com?database=app", "https://acct.documents.azure.com:443/", "app", "", false},
		{"cosmos://acct.documents.azure.com:443", "https://acct.documents.azure.com:443/", "", "", false},
		{"cosmos://acct.documents.azure.com/?database=app", "https://acct.documents.azure.com:443/", "app", "", false},
		{"cosmos://localhost:8081?database=app&tls=false", "http://localhost:8081/", "app", "", true},
		{"COSMOS://localhost:8081?tls=true", "https://localhost:8081/", "", "", false},
		{"cosmos://h:1?key=abc%2Bdef%3D%3D", "https://h:1/", "", "abc+def==", false},
		{"cosmos://h:1?AccountKey=abc", "https://h:1/", "", "abc", false},
		{"cosmos://[::1]:8081?tls=false", "http://[::1]:8081/", "", "", true},
		{"cosmos://127.0.0.1:8081?tls=false", "http://127.0.0.1:8081/", "", "", true},
		{"cosmos://LocalHost?tls=false", "http://LocalHost:8081/", "", "", true},
		// A remote emulator in clear needs the explicit opt-in.
		{"cosmos://10.0.0.5:8081?tls=false&allow_plaintext=1", "http://10.0.0.5:8081/", "", "", true},
		{"cosmos://emu.example?allow_plaintext=true&tls=false", "http://emu.example:8081/", "", "", true},
		{"cosmos://emu.example?allow_plaintext=1", "https://emu.example:443/", "", "", false},
	} {
		got, err := parseDSN(tc.dsn)
		if err != nil {
			t.Errorf("parseDSN(%q): %v", tc.dsn, err)
			continue
		}
		if got.endpoint != tc.endpoint || got.database != tc.db || got.key != tc.key || got.insecure != tc.insecure {
			t.Errorf("parseDSN(%q) = %+v, want endpoint %q db %q key %q insecure %v", tc.dsn, got, tc.endpoint, tc.db, tc.key, tc.insecure)
		}
	}
}

func TestParseDSNRefusals(t *testing.T) {
	const secret = "S3cretKeyValue=="
	for _, dsn := range []string{
		"mongodb://h?database=x",
		"cosmos://",
		"cosmos://?database=x",
		"cosmos://user:" + secret + "@h?database=x",
		"cosmos://h/app",
		"cosmos://h?database=x&database=y",
		"cosmos://h?tls=maybe",
		"cosmos://h?bogus=1&key=" + secret,
		"cosmos://h?key=" + secret + "&accountkey=" + secret,
		"cosmos://h?key=" + secret + "&key=" + secret,
		"cosmos://h#frag",
		"cosmos://h?database=",
		"cosmos://h:notaport",
		// Plain http only to the emulator on this machine: the signed
		// requests and the data would cross the network in clear.
		"cosmos://acct.documents.azure.com?tls=false",
		"cosmos://acct.documents.azure.com:443?database=x&tls=false&key=" + secret,
		"cosmos://10.0.0.5:8081?tls=false",
		"cosmos://localhost.example.com:8081?tls=false",
		"cosmos://10.0.0.5:8081?tls=false&allow_plaintext=0",
		"cosmos://h?allow_plaintext=maybe",
		"cosmos://h?allow_plaintext=1&allow_plaintext=1",
		// Azure's Cosmos DB endpoints are always TLS.
		"cosmos://acct.documents.azure.com?tls=false&allow_plaintext=1",
		"cosmos://ACCT.Documents.Azure.COM.:443?tls=false&allow_plaintext=1",
	} {
		_, err := parseDSN(dsn)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseDSN(%q): want ErrSyntax, got %v", dsn, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("parseDSN(%q): the error quotes the key: %v", dsn, err)
		}
	}
}

// The account key may sit in the DSN as key= or accountkey=: tobira's DSN
// redaction masks both, so a DSN echoed in a list, a log or an error never
// shows it.
func TestDSNRedactionMasksKey(t *testing.T) {
	const secret = "C2y6yDjf5R+ob0N8A7Cgv30VRDJIWEHLM+4QDU5DE2nQ9nDuVTqobD4b8mGGyPMbIZnqyMsEcaGQy67XIw/Jw=="
	for _, dsn := range []string{
		"cosmos://localhost:8081?database=app&tls=false&key=" + secret,
		"cosmos://localhost:8081?key=" + secret + "&database=app",
		"cosmos://localhost:8081?database=app&accountkey=" + secret,
		"cosmos://localhost:8081?database=app&AccountKey=" + secret,
	} {
		if red := driver.RedactDSN(dsn); strings.Contains(red, secret) || strings.Contains(red, "ob0N8A7") {
			t.Errorf("RedactDSN(%q) = %q: the key is visible", dsn, red)
		}
		if !driver.HasPassword(dsn) {
			t.Errorf("HasPassword(%q) = false, want true", dsn)
		}
		msg := driver.RedactMessage("open "+dsn+" failed; key "+secret, driver.ConnConfig{DSN: dsn})
		if strings.Contains(msg, secret) {
			t.Errorf("RedactMessage leaves the key in %q", msg)
		}
	}
}

func TestOpenRefusesBadCredentials(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  driver.ConnConfig
		kind error
	}{
		{"no key", driver.ConnConfig{DSN: "cosmos://127.0.0.1:1?database=d&tls=false"}, driver.ErrAuth},
		{"key not base64", driver.ConnConfig{DSN: "cosmos://127.0.0.1:1?database=d&tls=false", Password: driver.NewSecret("not base64 at all!!")}, driver.ErrAuth},
		{"dsn key not base64", driver.ConnConfig{DSN: "cosmos://127.0.0.1:1?database=d&tls=false&key=not-base64!!"}, driver.ErrAuth},
		{"options", driver.ConnConfig{DSN: "cosmos://127.0.0.1:1?database=d&tls=false", Password: driver.NewSecret(testKey), Options: map[string]string{"x": "y"}}, driver.ErrUnsupported},
		{"bad dsn", driver.ConnConfig{DSN: "cosmos://h?nope=1", Password: driver.NewSecret(testKey)}, driver.ErrSyntax},
	} {
		c, err := New().Open(ctx, tc.cfg)
		if err == nil {
			_ = c.Close()
		}
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.kind, err)
			continue
		}
		if strings.Contains(err.Error(), "not base64 at all") || strings.Contains(err.Error(), "not-base64!!") {
			t.Errorf("%s: the error quotes the key: %v", tc.name, err)
		}
	}
}

// fakeAccount serves the account read the SDK makes first and answers
// every other request with status (and a Cosmos-style error body).
func fakeAccount(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/" || r.URL.Path == "" {
			loc := `[{"name":"local","databaseAccountEndpoint":"` + srv.URL + `/"}]`
			_, _ = w.Write([]byte(`{"id":"fake","writableLocations":` + loc + `,"readableLocations":` + loc +
				`,"enableMultipleWriteLocations":false,"userConsistencyPolicy":{"defaultConsistencyLevel":"Session"}}`))
			return
		}
		if r.Header.Get("Authorization") == "" {
			t.Errorf("request %s %s carries no Authorization header", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeDSN(srv *httptest.Server, db string) string {
	return "cosmos://" + strings.TrimPrefix(srv.URL, "http://") + "?tls=false&database=" + db
}

func TestOpenMapsServerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		kind   error
	}{
		{"wrong key", http.StatusUnauthorized, `{"code":"Unauthorized","message":"The input authorization token can't serve the request."}`, driver.ErrAuth},
		{"forbidden", http.StatusForbidden, `{"code":"Forbidden","message":"Request blocked by network firewall"}`, driver.ErrPermission},
		{"no database", http.StatusNotFound, `{"code":"NotFound","message":"Resource Not Found"}`, driver.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeAccount(t, tc.status, tc.body)
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: fakeDSN(srv, "app"), Password: driver.NewSecret(testKey)})
			if err == nil {
				_ = c.Close()
			}
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Driver != Name {
				t.Errorf("want a *driver.Error from %q, got %#v", Name, err)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the error quotes the key: %v", err)
			}
		})
	}
}

func TestOpenAndCloseAgainstFake(t *testing.T) {
	srv := fakeAccount(t, http.StatusOK, `{"id":"app","_rid":"x","_self":"dbs/app/"}`)
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: fakeDSN(srv, "app"), Password: driver.NewSecret(testKey)})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping after Close: want ErrConnection, got %v", err)
	}
}

func TestScrubErrorMasksKey(t *testing.T) {
	cfg := driver.ConnConfig{DSN: "cosmos://h?key=" + testKey, Password: driver.NewSecret(testKey)}
	err := scrubError(driver.Wrap(driver.ErrConnection, Name, "open", errors.New("dial failed for key "+testKey)), cfg)
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("scrubError left the key: %v", err)
	}
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("scrubError lost the kind: %v", err)
	}
}

// A redirect is never followed: Go keeps the Authorization header on a
// redirect to the same host name (another port, or https to http), so the
// signed request must never reach the second server.
func TestRedirectIsNotFollowed(t *testing.T) {
	var hits, signed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			signed.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"app"}`))
	}))
	t.Cleanup(target.Close)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "" {
			w.Header().Set("Content-Type", "application/json")
			loc := `[{"name":"local","databaseAccountEndpoint":"` + srv.URL + `/"}]`
			_, _ = w.Write([]byte(`{"id":"fake","writableLocations":` + loc + `,"readableLocations":` + loc +
				`,"enableMultipleWriteLocations":false,"userConsistencyPolicy":{"defaultConsistencyLevel":"Session"}}`))
			return
		}
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: fakeDSN(srv, "app"), Password: driver.NewSecret(testKey)})
	if err == nil {
		_ = c.Close()
		t.Error("Open through a redirect succeeded")
	}
	if n := signed.Load(); n != 0 {
		t.Errorf("the redirect target got %d signed requests", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

// The SDK sends requests to the regional endpoints the account read names.
// A signed request never goes to a host or scheme other than the DSN's
// (outside Azure's own Cosmos DB zone), whatever the server advertises.
func TestSignedRequestsStayOnTheEndpoint(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"app"}`))
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		loc := `[{"name":"elsewhere","databaseAccountEndpoint":"` + other.URL + `/"}]`
		_, _ = w.Write([]byte(`{"id":"fake","writableLocations":` + loc + `,"readableLocations":` + loc +
			`,"enableMultipleWriteLocations":false,"userConsistencyPolicy":{"defaultConsistencyLevel":"Session"}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: fakeDSN(srv, "app"), Password: driver.NewSecret(testKey)})
	if err == nil {
		_ = c.Close()
		t.Error("Open succeeded through an advertised endpoint on another host")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the advertised endpoint got %d signed requests", n)
	}
}

func TestEndpointGuardAllows(t *testing.T) {
	for _, tc := range []struct {
		endpoint, target string
		want             bool
	}{
		{"https://acct.documents.azure.com:443/", "https://acct.documents.azure.com:443/dbs/x", true},
		{"https://acct.documents.azure.com:443/", "https://acct-westus.documents.azure.com:443/dbs/x", true},
		{"https://acct.documents.azure.com:443/", "https://ACCT-WESTUS.Documents.Azure.Com/dbs/x", true},
		{"https://acct.documents.azure.com:443/", "http://acct-westus.documents.azure.com:443/dbs/x", false},
		{"https://acct.documents.azure.com:443/", "http://acct.documents.azure.com:443/dbs/x", false},
		{"https://acct.documents.azure.com:443/", "https://evil.example/dbs/x", false},
		{"https://acct.documents.azure.com:443/", "https://documents.azure.com.evil.example/dbs/x", false},
		{"http://localhost:8081/", "http://localhost:8081/dbs/x", true},
		{"http://localhost:8081/", "http://localhost:9999/dbs/x", false},
		{"http://localhost:8081/", "http://127.0.0.1:8081/dbs/x", false},
		{"https://emu.example:443/", "https://other.documents.azure.com:443/", false},
		// The same origin with or without its default port.
		{"https://emu.example:443/", "https://emu.example/dbs/x", true},
		{"https://emu.example:443/", "https://EMU.example/dbs/x", true},
		{"https://emu.example/", "https://emu.example:443/dbs/x", true},
		{"http://emu.example:80/", "http://emu.example/dbs/x", true},
		{"https://emu.example:443/", "https://emu.example:8443/dbs/x", false},
		{"https://emu.example:443/", "http://emu.example/dbs/x", false},
		{"http://emu.example:8081/", "http://emu.example/dbs/x", false},
	} {
		d, err := url.Parse(tc.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(tc.target)
		if err != nil {
			t.Fatal(err)
		}
		if got := newEndpointGuard(d, nil).allows(u); got != tc.want {
			t.Errorf("guard(%s).allows(%s) = %v, want %v", tc.endpoint, tc.target, got, tc.want)
		}
	}
}
