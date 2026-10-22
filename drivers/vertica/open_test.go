package vertica

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	d, err := parseDSN("vertica://dbadmin@db.example.com:5444/analytics?tlsmode=server-strict&workload=reports")
	if err != nil {
		t.Fatal(err)
	}
	want := dsnConfig{user: "dbadmin", host: "db.example.com", port: 5444, database: "analytics", tlsMode: "server-strict", workload: "reports"}
	if d.user != want.user || d.host != want.host || d.port != want.port || d.database != want.database ||
		d.tlsMode != want.tlsMode || d.workload != want.workload || d.oauth {
		t.Errorf("parseDSN = %+v, want %+v", d, want)
	}

	d, err = parseDSN("VERTICA://u@[::1]/db")
	if err != nil || d.host != "::1" || d.port != 5433 || d.tlsMode != "prefer" {
		t.Errorf("parseDSN(ipv6, defaults) = %+v, %v; want host ::1, port 5433, tlsmode prefer", d, err)
	}

	d, err = parseDSN("vertica://h?auth=oauth")
	if err != nil || !d.oauth || d.user != "" {
		t.Errorf("parseDSN(oauth) = %+v, %v; want oauth without a user", d, err)
	}

	d, err = parseDSN("vertica://u@h/db?backup_server_node=h2:5433,h3&connection_load_balance=1&client_label=me")
	if err != nil || d.backups != "h2:5433,h3" || !d.loadBalance || d.label != "me" {
		t.Errorf("parseDSN(backups) = %+v, %v", d, err)
	}

	d, err = parseDSN("vertica://u@h")
	if err != nil || d.database != "" {
		t.Errorf("parseDSN(no database) = %+v, %v; want the user's default database", d, err)
	}
}

func TestParseDSNRefuses(t *testing.T) {
	const secret = "hunter2-secret"
	for _, dsn := range []string{
		"",
		"postgres://u@h/db",
		"vertica://",
		"vertica://u@/db",
		"vertica://u:" + secret + "@h/db",
		"vertica://u:@h/db",
		"vertica://u@h/db?password=" + secret,
		"vertica://u@h/db?oauth_access_token=" + secret,
		"vertica://u@h/db?pwd=" + secret,
		"vertica://u@h/db?use_prepared_statements=0",
		"vertica://u@h/db?autocommit=0",
		"vertica://u@h/db?totp=123456",
		"vertica://u@h/db?tlsmode=myCustomConfig",
		"vertica://u@h/db?tlsmode=",
		"vertica://u@h/db?unknown=" + secret,
		"vertica://u@h/db?workload=a&workload=b",
		"vertica://u@h/db?auth=kerberos",
		"vertica://u@h:0/db",
		"vertica://u@h:99999/db",
		"vertica://u@h:x/db",
		"vertica://u@h/db/extra",
		"vertica://u@h/db#frag",
		"vertica://h/db", // no user and no oauth
		"vertica://u@h/db?backup_server_node=h2:5433/x",
		"vertica:u@h/db",
	} {
		_, err := parseDSN(dsn)
		if err == nil {
			t.Errorf("parseDSN(%q) = nil error, want a refusal", dsn)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("parseDSN(%q) error quotes the secret: %v", dsn, err)
		}
	}
}

// buildURL is the only place the password meets the connection string. It
// is escaped there, and the settings tobira relies on are forced whatever
// the DSN says: server-side prepared statements (no client interpolation),
// autocommit on, a tobira client label.
func TestBuildURL(t *testing.T) {
	d, err := parseDSN("vertica://ada@h:5444/db?tlsmode=server&allow_plaintext=1&backup_server_node=h2:5433&connection_load_balance=1&workload=w")
	if err != nil {
		t.Fatal(err)
	}
	raw := buildURL(d, "p@ss:w/rd?&#")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Scheme != "vertica" || u.User.Username() != "ada" || pw != "p@ss:w/rd?&#" || u.Host != "h:5444" || u.Path != "/db" {
		t.Errorf("buildURL = %q: wrong scheme, user, password, host or path", raw)
	}
	q := u.Query()
	for k, v := range map[string]string{
		"use_prepared_statements": "1", "autocommit": "1", "tlsmode": "server",
		"backup_server_node": "h2:5433", "connection_load_balance": "1", "workload": "w",
	} {
		if q.Get(k) != v {
			t.Errorf("buildURL: %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if !strings.HasPrefix(q.Get("client_label"), "tobira") {
		t.Errorf("buildURL: client_label = %q, want a tobira label", q.Get("client_label"))
	}

	d, _ = parseDSN("vertica://h/db?auth=oauth")
	u, _ = url.Parse(buildURL(d, "tok-123"))
	if u.Query().Get("oauth_access_token") != "tok-123" || u.User != nil {
		t.Errorf("buildURL(oauth) = %q, want the token as oauth_access_token and no user", u.Redacted())
	}
}

func TestOpenRefusesOptionsAndMissingSecret(t *testing.T) {
	ctx := context.Background()
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "vertica://u@127.0.0.1:1/db", Password: driver.NewSecret("x"), Options: map[string]string{"sslmode": "x"}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Open(with Options) = %v, want ErrUnsupported", err)
	}
	_, err = New().Open(ctx, driver.ConnConfig{DSN: "vertica://u@127.0.0.1:1/db"})
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("Open(no password) = %v, want ErrAuth", err)
	}
	_, err = New().Open(ctx, driver.ConnConfig{DSN: "vertica://u:pw-in-dsn-123@127.0.0.1:1/db", Password: driver.NewSecret("x")})
	if !errors.Is(err, driver.ErrSyntax) || strings.Contains(err.Error(), "pw-in-dsn-123") {
		t.Errorf("Open(password in DSN) = %v, want ErrSyntax without the password", err)
	}
}

// A failed dial is ErrConnection and never shows the password.
func TestOpenDialFailureRedacts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const pw = "s3cret-pw-9876"
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "vertica://u@127.0.0.1:1/db?tlsmode=none", Password: driver.NewSecret(pw)})
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Open(closed port) = %v, want ErrConnection", err)
	}
	if err != nil && strings.Contains(err.Error(), pw) {
		t.Errorf("Open error shows the password: %v", err)
	}
}

// vertica-sql-go dials and logs in without a context; Open still returns
// when ctx ends, here against a server that accepts and never answers.
func TestOpenHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	defer func() {
		_ = ln.Close()
		for c := range accepted { // the abandoned login sees EOF and ends
			_ = c.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = New().Open(ctx, driver.ConnConfig{DSN: "vertica://u@" + ln.Addr().String() + "/db?tlsmode=none", Password: driver.NewSecret("x")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Open(silent server) = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Open(silent server) took %v", d)
	}

	pre, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	if _, err := New().Open(pre, driver.ConnConfig{DSN: "vertica://u@" + ln.Addr().String() + "/db", Password: driver.NewSecret("x")}); !errors.Is(err, context.Canceled) {
		t.Errorf("Open(cancelled ctx) = %v, want context.Canceled", err)
	}
}

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "vertica" || info.Kind != driver.KindSQL || len(info.Schemes) != 1 || info.Schemes[0] != "vertica" {
		t.Errorf("Info = %+v", info)
	}
	for _, c := range []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly} {
		found := false
		for _, have := range info.Capabilities {
			found = found || have == c
		}
		if !found {
			t.Errorf("Info.Capabilities lacks %s", c)
		}
	}
}
