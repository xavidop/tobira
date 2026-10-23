package neo4j

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		want settings
	}{
		{"neo4j://localhost", settings{scheme: "neo4j", hostPort: "localhost:7687", connectTimeout: defaultConnectTimeout}},
		{"bolt://user:p%40ss@127.0.0.1:7688/app", settings{scheme: "bolt", hostPort: "127.0.0.1:7688", user: "user", password: "p@ss", database: "app", connectTimeout: defaultConnectTimeout}},
		{"neo4j+s://u:p@xxx.databases.neo4j.io", settings{scheme: "neo4j+s", hostPort: "xxx.databases.neo4j.io:7687", user: "u", password: "p", connectTimeout: defaultConnectTimeout}},
		{"neo4j://u:p@db.internal?tls=true&timeout=30s&connect_timeout=2s", settings{scheme: "neo4j+s", hostPort: "db.internal:7687", user: "u", password: "p", timeout: 30 * time.Second, connectTimeout: 2 * time.Second}},
		{"neo4j://u:p@db.internal?allow_plaintext=1", settings{scheme: "neo4j", hostPort: "db.internal:7687", user: "u", password: "p", allowPlaintext: true, connectTimeout: defaultConnectTimeout}},
		{"bolt+ssc://[::1]:7687/neo4j", settings{scheme: "bolt+ssc", hostPort: "[::1]:7687", database: "neo4j", connectTimeout: defaultConnectTimeout}},
	} {
		got, err := parseDSN(driver.ConnConfig{DSN: tc.dsn})
		if err != nil {
			t.Errorf("%s: %v", tc.dsn, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.dsn, got, tc.want)
		}
	}
}

func TestParseDSNPasswordOverride(t *testing.T) {
	s, err := parseDSN(driver.ConnConfig{DSN: "neo4j://u:old@localhost", Password: driver.NewSecret("new")})
	if err != nil || s.password != "new" {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestParseDSNRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  driver.ConnConfig
		kind error
		msg  string
	}{
		{"scheme", driver.ConnConfig{DSN: "postgres://localhost"}, driver.ErrSyntax, "neo4j://"},
		{"fragment", driver.ConnConfig{DSN: "neo4j://localhost#x"}, driver.ErrSyntax, "fragment"},
		{"no host", driver.ConnConfig{DSN: "neo4j:///db"}, driver.ErrSyntax, "no host"},
		{"two hosts", driver.ConnConfig{DSN: "neo4j://a,b"}, driver.ErrSyntax, "several hosts"},
		{"bad port", driver.ConnConfig{DSN: "neo4j://localhost:abc"}, driver.ErrSyntax, "invalid host"},
		{"bad database", driver.ConnConfig{DSN: "neo4j://localhost/1db"}, driver.ErrSyntax, "invalid database"},
		{"password without user", driver.ConnConfig{DSN: "neo4j://localhost", Password: driver.NewSecret("x")}, driver.ErrSyntax, "names no user"},
		{"unknown param", driver.ConnConfig{DSN: "neo4j://localhost?nope=1"}, driver.ErrSyntax, "unknown DSN parameter"},
		{"bad tls", driver.ConnConfig{DSN: "neo4j://localhost?tls=maybe"}, driver.ErrSyntax, "true or false"},
		{"tls contradiction", driver.ConnConfig{DSN: "neo4j+s://localhost?tls=false"}, driver.ErrSyntax, "contradicts"},
		{"bad timeout", driver.ConnConfig{DSN: "neo4j://localhost?timeout=soon"}, driver.ErrSyntax, "duration"},
		{"options", driver.ConnConfig{DSN: "neo4j://localhost", Options: map[string]string{"x": "1"}}, driver.ErrUnsupported, "unsupported option"},
		{"plaintext remote", driver.ConnConfig{DSN: "neo4j://u:p@db.internal"}, driver.ErrSyntax, "allow_plaintext"},
		{"plaintext aura", driver.ConnConfig{DSN: "neo4j://u:p@xxx.databases.neo4j.io?allow_plaintext=1"}, driver.ErrSyntax, "only speaks TLS"},
	} {
		_, err := parseDSN(tc.cfg)
		if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: got %v, want %v with %q", tc.name, err, tc.kind, tc.msg)
		}
	}
}

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != Name || info.Kind != driver.KindGraph || info.Dialect != "cypher" {
		t.Fatalf("Info = %+v", info)
	}
	for _, s := range []string{"neo4j", "neo4j+s", "neo4j+ssc", "bolt", "bolt+s", "bolt+ssc"} {
		if _, ok := schemes[s]; !ok {
			t.Errorf("scheme %s unknown", s)
		}
	}
	if d, ok := driver.Default.Lookup(Name); !ok || d.Info().Name != Name {
		t.Fatal("the driver is not registered")
	}
}
