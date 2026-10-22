package mssql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "mssql" || info.Kind != driver.KindSQL || info.Dialect == "" {
		t.Errorf("Info = %+v", info)
	}
	for _, s := range []string{"sqlserver", "mssql"} {
		if !strings.Contains(strings.Join(info.Schemes, ","), s) {
			t.Errorf("Info.Schemes %v lacks %q", info.Schemes, s)
		}
	}
	if !driver.HasCapability(info, driver.CapReadOnly) {
		t.Error("Info does not declare readonly")
	}
	for _, scheme := range []string{"sqlserver", "mssql"} {
		if d, ok := driver.Default.ForScheme(scheme); !ok || d.Info().Name != Name {
			t.Errorf("scheme %q is not registered to the mssql driver", scheme)
		}
	}
}

func TestBuildConfig(t *testing.T) {
	for _, tc := range []struct {
		name, dsn            string
		host, db, user, pass string
		port                 uint64
		readOnly, intent     bool
		password             string
	}{
		{name: "url", dsn: "sqlserver://sa:pw@db.example:1444?database=sales", host: "db.example", port: 1444, db: "sales", user: "sa", pass: "pw"},
		{name: "alias scheme", dsn: "mssql://sa:pw@h?database=d", host: "h", db: "d", user: "sa", pass: "pw"},
		{name: "upper-case scheme", dsn: "SQLSERVER://sa@h?database=d", host: "h", db: "d", user: "sa"},
		{name: "ado", dsn: "server=h;user id=sa;password=pw;database=d", host: "h", db: "d", user: "sa", pass: "pw"},
		{name: "odbc", dsn: "odbc:server=h;user id=sa;password=pw;database=d", host: "h", db: "d", user: "sa", pass: "pw"},
		{name: "password override", dsn: "sqlserver://sa:old@h?database=d", host: "h", db: "d", user: "sa", pass: "new", password: "new"},
		{name: "read-only intent", dsn: "sqlserver://sa@h?database=d", host: "h", db: "d", user: "sa", readOnly: true, intent: true},
		{name: "read-only without database", dsn: "sqlserver://sa@h", host: "h", user: "sa", readOnly: true},
		{name: "intent kept on writable", dsn: "sqlserver://sa@h?database=d&ApplicationIntent=ReadOnly", host: "h", db: "d", user: "sa", intent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := driver.ConnConfig{DSN: tc.dsn, ReadOnly: tc.readOnly}
			if tc.password != "" {
				cfg.Password = driver.NewSecret(tc.password)
			}
			p, err := buildConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if p.Host != tc.host || p.Database != tc.db || p.User != tc.user || p.Password != tc.pass || p.ReadOnlyIntent != tc.intent {
				t.Errorf("got host %q db %q user %q pass %q intent %v", p.Host, p.Database, p.User, p.Password, p.ReadOnlyIntent)
			}
			if tc.port != 0 && p.Port != tc.port {
				t.Errorf("port %d, want %d", p.Port, tc.port)
			}
			if p.LogFlags != 0 || p.Encoding.Timezone != time.UTC || p.Encoding.GuidConversion {
				t.Errorf("enforced settings not applied: log %d tz %v guid %v", p.LogFlags, p.Encoding.Timezone, p.Encoding.GuidConversion)
			}
		})
	}
	p, err := buildConfig(driver.ConnConfig{DSN: "sqlserver://sa@h?database=d&log=63&timezone=Europe/Madrid&guid+conversion=true"})
	if err != nil || p.LogFlags != 0 || p.Encoding.Timezone != time.UTC || p.Encoding.GuidConversion {
		t.Errorf("DSN overrides of enforced settings survived: %v %+v", err, p.Encoding)
	}
}

func TestBuildConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		cfg  driver.ConnConfig
		kind error
	}{
		{driver.ConnConfig{DSN: "postgres://h/db"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "sqlserver:h"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "just-a-host"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "sqlserver://sa@h?port=notaport"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "sqlserver://sa@h", Options: map[string]string{"x": "1"}}, driver.ErrUnsupported},
	} {
		_, err := buildConfig(tc.cfg)
		if !errors.Is(err, tc.kind) {
			t.Errorf("buildConfig(%q) = %v, want %v", tc.cfg.DSN, err, tc.kind)
		}
	}
}

// A failed open never echoes the password, from the DSN or the secret.
func TestOpenErrorRedactsPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "sqlserver://sa:Sup3r-S3cret-Pw@127.0.0.1:1?database=d"},
		{DSN: "server=127.0.0.1;port=1;user id=sa;password=Sup3r-S3cret-Pw"},
		{DSN: "sqlserver://sa@127.0.0.1:1?port=bad-Sup3r-S3cret-Pw", Password: driver.NewSecret("Sup3r-S3cret-Pw")},
	} {
		_, err := New().Open(ctx, cfg)
		if err == nil {
			t.Fatalf("Open(%q) succeeded", cfg.DSN)
		}
		if strings.Contains(err.Error(), "Sup3r-S3cret-Pw") {
			t.Errorf("Open error leaks the password: %v", err)
		}
	}
}

// An ODBC password, braced or not, with '@' in it or in the user name.
func TestODBCPasswordWithAtIsRedacted(t *testing.T) {
	for _, dsn := range []string{
		"odbc:server=h;uid=me@corp.com;pwd=Secret1xyz;database=d",
		"odbc:server=h;uid=me@corp.com;pwd={Sec;ret1xyz};database=d",
		"odbc:server=h;uid=sa;pwd=Sec@ret1xyz;database=d",
		"odbc:server=h;uid=me@corp.com;password=Sec@ret1xyz;database=d",
	} {
		cfg := driver.ConnConfig{DSN: dsn}
		for _, part := range []string{"Secret1xyz", "Sec", "ret1xyz"} {
			if got := driver.RedactDSN(dsn); strings.Contains(got, part) {
				t.Errorf("RedactDSN(%q) = %q leaks %q", dsn, got, part)
			}
			if got := driver.RedactMessage("open "+dsn+" failed", cfg); strings.Contains(got, part) {
				t.Errorf("RedactMessage(%q) = %q leaks %q", dsn, got, part)
			}
		}
	}
}

// An ODBC braced password may hold ';' and '}}'. Every redaction path the
// driver relies on masks all of it, whatever the key's case.
func TestODBCBracedPasswordIsRedacted(t *testing.T) {
	const secret = "a;Sup3r}}S3cret;b"
	for _, form := range []struct{ key, uid string }{
		{"pwd", "sa"}, {"PWD", "sa"}, {"Pwd", "sa"}, {"password", "sa"}, {"PassWord", "sa"},
		{"pwd", "me@corp.com"}, {"password", "me@corp.com"},
	} {
		key := form.key
		dsn := "odbc:server=127.0.0.1;port=1;uid=" + form.uid + ";" + key + "={" + secret + "};database=d"
		cfg := driver.ConnConfig{DSN: dsn}
		for _, part := range []string{"a;Sup3r", "S3cret;b", "Sup3r", "S3cret"} {
			if got := driver.RedactDSN(dsn); strings.Contains(got, part) {
				t.Errorf("RedactDSN(%s) = %q leaks %q", key, got, part)
			}
			if got := driver.RedactMessage("open "+dsn+" failed", cfg); strings.Contains(got, part) {
				t.Errorf("RedactMessage(%s) = %q leaks %q", key, got, part)
			}
		}
		p, err := buildConfig(cfg)
		if err != nil {
			t.Fatalf("buildConfig(%s): %v", key, err)
		}
		if strings.HasPrefix(strings.ToLower(key), "password") && p.Password != "a;Sup3r}S3cret;b" {
			t.Errorf("%s: parsed password %q, want the braced value unescaped", key, p.Password)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = New().Open(ctx, cfg)
		cancel()
		if err == nil {
			t.Fatal("Open on a closed port succeeded")
		}
		if strings.Contains(err.Error(), "Sup3r") || strings.Contains(err.Error(), "S3cret") {
			t.Errorf("Open error leaks the password: %v", err)
		}
	}
}
