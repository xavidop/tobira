package oracle

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestBuildURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  driver.ConnConfig
		want string
	}{
		{"full", driver.ConnConfig{DSN: "oracle://scott:tiger@db.example:1522/ORCLPDB1"}, "oracle://scott:tiger@db.example:1522/ORCLPDB1"},
		{"default port", driver.ConnConfig{DSN: "oracle://scott:tiger@db.example/ORCLPDB1"}, "oracle://scott:tiger@db.example:1521/ORCLPDB1"},
		{"ipv6 default port", driver.ConnConfig{DSN: "oracle://scott@[::1]/FREE"}, "oracle://scott@[::1]:1521/FREE"},
		{"upper-case scheme", driver.ConnConfig{DSN: "ORACLE://scott@h:1521/FREE"}, "oracle://scott@h:1521/FREE"},
		{"password from config", driver.ConnConfig{DSN: "oracle://scott:old@h:1521/FREE", Password: driver.NewSecret("n:e/w@?")}, "oracle://scott:" + url.QueryEscape("n:e/w@?") + "@h:1521/FREE"},
		{"password without one in the DSN", driver.ConnConfig{DSN: "oracle://scott@h:1521/FREE", Password: driver.NewSecret("pw")}, "oracle://scott:pw@h:1521/FREE"},
		{"options kept", driver.ConnConfig{DSN: "oracle://scott@h:1521/FREE?SSL=true&PREFETCH_ROWS=100"}, "oracle://scott@h:1521/FREE?PREFETCH_ROWS=100&SSL=true"},
		{"lob fetch inline is fine", driver.ConnConfig{DSN: "oracle://scott@h:1521/FREE?LOB+FETCH=INLINE"}, "oracle://scott@h:1521/FREE?LOB+FETCH=INLINE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildURL(tc.cfg)
			if err != nil {
				t.Fatalf("buildURL: %v", err)
			}
			gu, _ := url.Parse(got)
			wu, _ := url.Parse(tc.want)
			gp, _ := gu.User.Password()
			wp, _ := wu.User.Password()
			if gu.Scheme != wu.Scheme || gu.Host != wu.Host || gu.Path != wu.Path || gu.User.Username() != wu.User.Username() || gp != wp || gu.Query().Encode() != wu.Query().Encode() {
				t.Errorf("buildURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildURLRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  driver.ConnConfig
		kind error
	}{
		{"other scheme", driver.ConnConfig{DSN: "postgres://u@h/db"}, driver.ErrSyntax},
		{"no scheme", driver.ConnConfig{DSN: "scott/tiger@h:1521/FREE"}, driver.ErrSyntax},
		{"no host", driver.ConnConfig{DSN: "oracle:///FREE"}, driver.ErrSyntax},
		{"bad port", driver.ConnConfig{DSN: "oracle://u@h:99999/FREE"}, driver.ErrSyntax},
		{"bad escape", driver.ConnConfig{DSN: "oracle://u:%zz@h/FREE"}, driver.ErrSyntax},
		{"options map", driver.ConnConfig{DSN: "oracle://u@h/FREE", Options: map[string]string{"x": "y"}}, driver.ErrUnsupported},
		{"trace file", driver.ConnConfig{DSN: "oracle://u@h/FREE?TRACE+FILE=/tmp/t.log"}, driver.ErrUnsupported},
		{"trace dir lower case", driver.ConnConfig{DSN: "oracle://u@h/FREE?trace%20dir=/tmp"}, driver.ErrUnsupported},
		{"trace folder", driver.ConnConfig{DSN: "oracle://u@h/FREE?TRACE+FOLDER=/tmp"}, driver.ErrUnsupported},
		{"trace directory", driver.ConnConfig{DSN: "oracle://u@h/FREE?TRACE+DIRECTORY=/tmp"}, driver.ErrUnsupported},
		{"lob stream", driver.ConnConfig{DSN: "oracle://u@h/FREE?LOB+FETCH=STREAM"}, driver.ErrUnsupported},
		{"lob post", driver.ConnConfig{DSN: "oracle://u@h/FREE?lob+fetch=post"}, driver.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildURL(tc.cfg)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("buildURL(%q) = %v, want %v", tc.cfg.DSN, err, tc.kind)
			}
		})
	}
}

// A failed open never shows the password, whether it came from the DSN or
// from the config.
func TestOpenErrorIsRedacted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "oracle://scott:s3cretDSN@127.0.0.1:1/FREE"},
		{DSN: "oracle://scott@127.0.0.1:1/FREE", Password: driver.NewSecret("s3cretCFG")},
		{DSN: "oracle://scott:s3cretBAD@h:99999/FREE"},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%q) succeeded", cfg.DSN)
		}
		for _, secret := range []string{"s3cretDSN", "s3cretCFG", "s3cretBAD"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Open error leaks the password: %v", err)
			}
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("Open error is %T, want a *driver.Error: %v", err, err)
		}
	}
}

// A DSN with a connect descriptor (connStr) never leaks a password: the
// userinfo or config password is redacted, and a PASSWORD inside the
// descriptor (go-ora would forward it to the listener as is) is refused
// without echoing it.
func TestConnStrRedaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	desc := url.QueryEscape("(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=127.0.0.1)(PORT=1))(CONNECT_DATA=(SERVICE_NAME=FREE)))")
	for _, cfg := range []driver.ConnConfig{
		{DSN: "oracle://scott:s3cretURL@127.0.0.1:1/?connStr=" + desc},
		{DSN: "oracle://scott@127.0.0.1:1/?connStr=" + desc, Password: driver.NewSecret("s3cretCFG")},
		{DSN: "oracle://scott:s3cretURL@127.0.0.1:1/?connStr=" + url.QueryEscape("(DESCRIPTION=(ADDRESS=(HOST=127.0.0.1)(PORT=1))(PASSWORD=s3cretCS))")},
		{DSN: "oracle://scott@127.0.0.1:1/?CONNSTR=" + url.QueryEscape("(DESCRIPTION=(ADDRESS=(HOST=h)(PORT=1))(password = s3cretCS))")},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%q) succeeded", cfg.DSN)
		}
		for _, secret := range []string{"s3cretURL", "s3cretCFG", "s3cretCS"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Open(%q) error leaks %s: %v", cfg.DSN, secret, err)
			}
		}
	}
	_, err := buildURL(driver.ConnConfig{DSN: "oracle://scott@h/?connStr=" + url.QueryEscape("(DESCRIPTION=(PASSWORD=s3cretCS))")})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("PASSWORD in connStr: want ErrUnsupported, got %v", err)
	}
}

func TestGuardVerified(t *testing.T) {
	for v, want := range map[string]bool{
		"23.26.3.0.0": true,
		"23.0.0.0.0":  true,
		"21.3.0.0.0":  true,
		"19.24.0.0.0": true,
		"12.2.0.1.0":  true,
		"11.2.0.4.0":  false,
		"26.1.0.0.0":  false,
		"":            false,
		"x.y":         false,
	} {
		if got := guardVerified(v); got != want {
			t.Errorf("guardVerified(%q) = %v, want %v", v, got, want)
		}
	}
}
