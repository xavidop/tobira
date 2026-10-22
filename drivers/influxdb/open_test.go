package influxdb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "influxdb" || info.Kind != driver.KindTimeSeries {
		t.Errorf("Info = %+v, want name influxdb, kind timeseries", info)
	}
	if !slices.Equal(info.Schemes, []string{"influxdb3", "influxdb"}) {
		t.Errorf("Schemes = %v", info.Schemes)
	}
	for _, c := range []driver.Capability{driver.CapBrowser, driver.CapTimeSeriesReader, driver.CapReadOnly} {
		if !driver.HasCapability(info, c) {
			t.Errorf("Info does not declare %s", c)
		}
	}
	if driver.HasCapability(info, driver.CapMutator) {
		t.Error("Info declares mutator: writes are line protocol and out of scope")
	}
	if _, ok := driver.Default.Lookup(Name); !ok {
		t.Error("the driver is not registered in driver.Default")
	}
}

func TestParseDSN(t *testing.T) {
	for _, tc := range []struct {
		cfg  driver.ConnConfig
		want settings
	}{
		{driver.ConnConfig{DSN: "influxdb3://:tok123@db.example:9000/metrics"},
			settings{host: "https://db.example:9000", token: "tok123", database: "metrics", lang: langSQL, tls: true, tlsDefaulted: true}},
		{driver.ConnConfig{DSN: "influxdb3://db.example/metrics"}, // no token: still TLS off loopback
			settings{host: "https://db.example:8181", database: "metrics", lang: langSQL, tls: true, tlsDefaulted: true}},
		{driver.ConnConfig{DSN: "influxdb3://db.example/metrics?tls=false&allow_plaintext=1"},
			settings{host: "http://db.example:8181", database: "metrics", lang: langSQL}},
		{driver.ConnConfig{DSN: "influxdb3://db.example/metrics?allow_plaintext=1"}, // alone it keeps TLS
			settings{host: "https://db.example:8181", database: "metrics", lang: langSQL, tls: true, tlsDefaulted: true}},
		{driver.ConnConfig{DSN: "influxdb3://:tok@localhost/db"},
			settings{host: "http://localhost:8181", token: "tok", database: "db", lang: langSQL}},
		{driver.ConnConfig{DSN: "influxdb3://:tok@127.0.0.1/db"},
			settings{host: "http://127.0.0.1:8181", token: "tok", database: "db", lang: langSQL}},
		{driver.ConnConfig{DSN: "influxdb3://:tok@[::1]/db"},
			settings{host: "http://[::1]:8181", token: "tok", database: "db", lang: langSQL}},
		{driver.ConnConfig{DSN: "influxdb3://:tok@db.example/db?tls=false&allow_plaintext=true"}, // explicit plaintext (warns)
			settings{host: "http://db.example:8181", token: "tok", database: "db", lang: langSQL}},
		{driver.ConnConfig{DSN: "influxdb3://localhost"},
			settings{host: "http://localhost:8181", lang: langSQL}},
		{driver.ConnConfig{DSN: "influxdb://user:tok@[::1]:8181/db?language=influxql&tls=true"},
			settings{host: "https://[::1]:8181", token: "tok", database: "db", lang: langInfluxQL, tls: true}},
		{driver.ConnConfig{DSN: "INFLUXDB3://h/db?language=SQL&tls=false&allow_plaintext=1"},
			settings{host: "http://h:8181", database: "db", lang: langSQL}},
		{driver.ConnConfig{DSN: "influxdb3://:dsntok@h/db", Password: driver.NewSecret("cfgtok")},
			settings{host: "https://h:8181", token: "cfgtok", database: "db", lang: langSQL, tls: true, tlsDefaulted: true}},
		{driver.ConnConfig{DSN: "influxdb3://h/db", Password: driver.NewSecret("cfgtok")},
			settings{host: "https://h:8181", token: "cfgtok", database: "db", lang: langSQL, tls: true, tlsDefaulted: true}},
		{driver.ConnConfig{DSN: "influxdb3://:p%40ss@h/my%20db"},
			settings{host: "https://h:8181", token: "p@ss", database: "my db", lang: langSQL, tls: true, tlsDefaulted: true}},
	} {
		got, err := parseDSN(tc.cfg)
		if err != nil {
			t.Errorf("parseDSN(%q): %v", tc.cfg.DSN, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDSN(%q) = %+v, want %+v", tc.cfg.DSN, got, tc.want)
		}
	}
}

func TestParseDSNErrors(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		kind error
	}{
		{"postgres://h/db", driver.ErrSyntax},
		{"influxdb3:///db", driver.ErrSyntax},            // no host
		{"influxdb3://h:99999/db", driver.ErrSyntax},     // bad port
		{"influxdb3://h/db/extra", driver.ErrSyntax},     // one path segment only
		{"influxdb3://tokenonly@h/db", driver.ErrSyntax}, // a token in the user position would not be redacted
		{"influxdb3://h/db?language=flux", driver.ErrUnsupported},
		{"influxdb3://h/db?tls=maybe", driver.ErrSyntax},
		{"influxdb3://h/db?token=secret1", driver.ErrUnsupported}, // tokens go in the password position
		{"influxdb3://h/db?nope=1", driver.ErrUnsupported},
		{"influxdb3://h/db?language=sql&language=influxql", driver.ErrSyntax},
		// Plain http off loopback needs the opt-in; InfluxDB Cloud is TLS only.
		{"influxdb3://h/db?tls=false", driver.ErrSyntax},
		{"influxdb3://:tok@h/db?tls=false&allow_plaintext=0", driver.ErrSyntax},
		{"influxdb3://h/db?allow_plaintext=maybe", driver.ErrSyntax},
		{"influxdb3://us-east-1-1.aws.cloud2.influxdata.com/db?tls=false&allow_plaintext=1", driver.ErrSyntax},
		{"influxdb3://Eu-Central-1-1.AWS.Cloud2.InfluxData.COM./db?tls=false&allow_plaintext=1", driver.ErrSyntax},
		{"influxdb3://cluster-id.a.influxdb.io/db?tls=false&allow_plaintext=1", driver.ErrSyntax}, // Cloud Dedicated
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn})
		if !errors.Is(err, tc.kind) {
			t.Errorf("parseDSN(%q) = %v, want %v", tc.dsn, err, tc.kind)
		}
		if err != nil && strings.Contains(err.Error(), "secret1") {
			t.Errorf("parseDSN(%q) error leaks the token: %v", tc.dsn, err)
		}
	}
}

// TestOpenFailureRedactsToken opens against a closed local port: the error
// must be ErrConnection and must not carry the token, whether it came from
// the DSN or from ConnConfig.Password.
func TestOpenFailureRedactsToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "influxdb3://:hunter2secret@127.0.0.1:1/db"},
		{DSN: "influxdb3://127.0.0.1:1/db", Password: driver.NewSecret("hunter2secret")},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%q) succeeded against a closed port", cfg.DSN)
		}
		if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("Open(%q) = %v, want ErrConnection", cfg.DSN, err)
		}
		if strings.Contains(err.Error(), "hunter2secret") {
			t.Errorf("Open(%q) error leaks the token: %v", cfg.DSN, err)
		}
	}
}

// TestOpenAuthAndToken checks that Open sends the token as a bearer token
// and maps a rejected one to ErrAuth without echoing it.
func TestOpenAuthAndToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer goodtoken" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad token: ` + r.Header.Get("Authorization") + `"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New().Open(ctx, driver.ConnConfig{DSN: "influxdb3://" + hostport + "/db", Password: driver.NewSecret("goodtoken")})
	if err != nil {
		t.Fatalf("Open with the right token: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := c.Ping(ctx); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping after Close = %v, want ErrConnection", err)
	}

	_, err = New().Open(ctx, driver.ConnConfig{DSN: "influxdb3://:wrongtoken1@" + hostport + "/db"})
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("Open with a wrong token = %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "wrongtoken1") {
		t.Errorf("Open error leaks the token: %v", err)
	}
}

func TestOpenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "influxdb3://127.0.0.1:1/db"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Open with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestScrubErrorHidesOriginal checks that a masked message cannot be
// recovered by unwrapping: the chain holds only the kind.
func TestScrubErrorHidesOriginal(t *testing.T) {
	cfg := driver.ConnConfig{Password: driver.NewSecret("supersecret9")}
	in := driver.Wrap(driver.ErrAuth, Name, "open", fmt.Errorf("outer: %w", errors.New("token supersecret9 rejected")))
	out := scrubError(in, cfg)
	if !errors.Is(out, driver.ErrAuth) {
		t.Errorf("the kind was lost: %v", out)
	}
	var walk func(err error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "supersecret9") {
			t.Errorf("an error in the chain shows the token: %T %q", err, err.Error())
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				walk(e)
			}
		}
	}
	walk(out)
}
