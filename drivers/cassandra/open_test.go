package cassandra

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	s, err := parseDSN(driver.ConnConfig{DSN: "cassandra://h1:9043,h2/ks?consistency=one&serial_consistency=SERIAL&timeout=5s&connect_timeout=2s&dc=eu&proto=4&system_keyspaces=true&discovery=false"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"h1:9043", "h2:9042"}; !slices.Equal(s.hosts, want) {
		t.Errorf("hosts = %q, want %q", s.hosts, want)
	}
	if s.keyspace != "ks" || s.consistency != gocql.One || s.serial != gocql.Serial || s.timeout != 5*time.Second ||
		s.connectTimeout != 2*time.Second || s.dc != "eu" || s.proto != 4 || !s.systemKeyspaces || s.discovery {
		t.Errorf("settings = %+v", s)
	}
}

func TestParseDSNDefaults(t *testing.T) {
	s, err := parseDSN(driver.ConnConfig{DSN: "cassandra://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.hosts, []string{"localhost:9042"}) || s.keyspace != "" || s.consistency != gocql.LocalQuorum ||
		s.serial != gocql.LocalSerial || s.timeout != defaultTimeout || s.connectTimeout != defaultConnectTimeout ||
		s.proto != 0 || s.systemKeyspaces || !s.discovery || s.tls {
		t.Errorf("defaults = %+v", s)
	}
}

func TestParseDSNSchemesAndHosts(t *testing.T) {
	for dsn, want := range map[string][]string{
		"scylla://a:1,b:2/ks":       {"a:1", "b:2"},
		"CASSANDRA://[::1]:9050/ks": {"[::1]:9050"},
		"cassandra://[::1]/ks":      {"[::1]:9042"},
		"cassandra://u@h/ks":        {"h:9042"},
	} {
		s, err := parseDSN(driver.ConnConfig{DSN: dsn})
		if err != nil {
			t.Errorf("parseDSN(%q): %v", dsn, err)
			continue
		}
		if !slices.Equal(s.hosts, want) {
			t.Errorf("parseDSN(%q) hosts = %q, want %q", dsn, s.hosts, want)
		}
	}
}

func TestParseDSNCredentials(t *testing.T) {
	s, err := parseDSN(driver.ConnConfig{DSN: "cassandra://alice:s%40cret@h/ks?tls=true"})
	if err != nil {
		t.Fatal(err)
	}
	if s.user != "alice" || s.password != "s@cret" {
		t.Errorf("user/password = %q/%q", s.user, s.password)
	}
	s, err = parseDSN(driver.ConnConfig{DSN: "cassandra://alice:old@localhost/ks", Password: driver.NewSecret("new")})
	if err != nil {
		t.Fatal(err)
	}
	if s.password != "new" {
		t.Errorf("ConnConfig.Password did not replace the DSN's: %q", s.password)
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "cassandra://h/ks", Password: driver.NewSecret("pw")}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("password without a user: got %v, want ErrSyntax", err)
	}
}

func TestParseDSNTLS(t *testing.T) {
	s, err := parseDSN(driver.ConnConfig{DSN: "cassandra://h/ks?tls=true&tls_ca=/ca.pem&tls_server_name=db&tls_insecure=true"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.tls || s.tlsCA != "/ca.pem" || s.tlsServerName != "db" || !s.tlsInsecure {
		t.Errorf("tls settings = %+v", s)
	}
	// Any tls_* option implies TLS.
	s, err = parseDSN(driver.ConnConfig{DSN: "cassandra://h/ks?tls_ca=/ca.pem"})
	if err != nil || !s.tls {
		t.Errorf("tls_ca alone: tls=%v err=%v, want TLS on", s.tls, err)
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "cassandra://h/ks?tls=false&tls_ca=/ca.pem"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("tls=false with tls_ca: got %v, want ErrSyntax", err)
	}
}

func TestParseDSNErrors(t *testing.T) {
	for dsn, kind := range map[string]error{
		"mongodb://h/ks":                        driver.ErrSyntax,
		"cassandra:///ks":                       driver.ErrSyntax,
		"cassandra://h/ks/extra":                driver.ErrSyntax,
		"cassandra://h/bad-name":                driver.ErrSyntax,
		"cassandra://h:notaport/ks":             driver.ErrSyntax,
		"cassandra://h:0/ks":                    driver.ErrSyntax,
		"cassandra://h,/ks":                     driver.ErrSyntax,
		"cassandra://h/ks?bogus=1":              driver.ErrUnsupported,
		"cassandra://h/ks?consistency=nope":     driver.ErrSyntax,
		"cassandra://h/ks?serial_consistency=1": driver.ErrSyntax,
		"cassandra://h/ks?timeout=soon":         driver.ErrSyntax,
		"cassandra://h/ks?timeout=-1s":          driver.ErrSyntax,
		"cassandra://h/ks?proto=2":              driver.ErrSyntax,
		"cassandra://h/ks?tls=maybe":            driver.ErrSyntax,
		"cassandra://h/ks?dc=a&dc=b":            driver.ErrSyntax,
		"cassandra://h/ks#frag":                 driver.ErrSyntax,
	} {
		if _, err := parseDSN(driver.ConnConfig{DSN: dsn}); !errors.Is(err, kind) {
			t.Errorf("parseDSN(%q) = %v, want %v", dsn, err, kind)
		}
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "cassandra://h/ks", Options: map[string]string{"x": "1"}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Options: got %v, want ErrUnsupported", err)
	}
}

// An error from Open never shows the password, from the DSN or from
// ConnConfig.Password.
func TestOpenErrorRedactsPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "cassandra://alice:hunter2pw@127.0.0.1:1/ks?connect_timeout=300ms&timeout=300ms"},
		{DSN: "cassandra://alice@127.0.0.1:1/ks?connect_timeout=300ms&timeout=300ms", Password: driver.NewSecret("hunter2pw")},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Fatal("Open on a closed port succeeded")
		}
		if strings.Contains(err.Error(), "hunter2pw") {
			t.Errorf("Open error leaks the password: %v", err)
		}
		if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("Open on a closed port: got %v, want ErrConnection", err)
		}
	}
}

func TestOpenRefusesBadDSNBeforeDialling(t *testing.T) {
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "cassandra://h/ks?bogus=hunter2pw"})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "cassandra" || info.Kind != driver.KindWideColumn || info.Dialect != "cql" {
		t.Errorf("Info = %+v", info)
	}
	for _, c := range []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapCompleter, driver.CapReadOnly} {
		if !driver.HasCapability(info, c) {
			t.Errorf("Info lacks %s", c)
		}
	}
	for _, c := range []driver.Capability{driver.CapTransactor, driver.CapExplainer, driver.CapWatcher} {
		if driver.HasCapability(info, c) {
			t.Errorf("Info claims %s, which Cassandra does not have", c)
		}
	}
}
