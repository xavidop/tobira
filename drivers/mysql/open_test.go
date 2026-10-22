package mysql

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "mysql" || info.Kind != driver.KindSQL || info.Dialect != "mysql" {
		t.Errorf("info = %+v", info)
	}
	if !slices.Equal(info.Schemes, []string{"mysql"}) {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly}
	if !slices.Equal(info.Capabilities, want) {
		t.Errorf("capabilities = %v, want %v", info.Capabilities, want)
	}
	if d, ok := driver.Default.Lookup("mysql"); !ok || d.Info().Name != "mysql" {
		t.Error(`init did not register "mysql"`)
	}
}

func mustConfig(t *testing.T, cfg driver.ConnConfig) *gomysql.Config {
	t.Helper()
	mc, err := buildConfig(cfg)
	if err != nil {
		t.Fatalf("buildConfig(%q): %v", cfg.DSN, err)
	}
	return mc
}

func TestBuildConfigURL(t *testing.T) {
	tests := []struct {
		dsn                         string
		user, pass, net, addr, name string
	}{
		{"mysql://u:p%40ss@db.example:3307/app", "u", "p@ss", "tcp", "db.example:3307", "app"},
		{"mysql://u@h/db", "u", "", "tcp", "h:3306", "db"},
		{"MYSQL://u@h/db", "u", "", "tcp", "h:3306", "db"},
		{"mysql://u@[::1]:3310/db", "u", "", "tcp", "[::1]:3310", "db"},
		{"mysql://u@h", "u", "", "tcp", "h:3306", ""},
		{"mysql://u@h/", "u", "", "tcp", "h:3306", ""},
		{"mysql://u%3Ax:pw@h/my%2Fdb", "u:x", "pw", "tcp", "h:3306", "my/db"},
		{"mysql://u:a/b@c@h/db", "u", "a/b@c", "tcp", "h:3306", "db"},
		{"mysql://u@localhost/db?socket=/tmp/mysql.sock", "u", "", "unix", "/tmp/mysql.sock", "db"},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			mc := mustConfig(t, driver.ConnConfig{DSN: tt.dsn})
			if mc.User != tt.user || mc.Passwd != tt.pass || mc.Net != tt.net || mc.Addr != tt.addr || mc.DBName != tt.name {
				t.Errorf("got user=%q pass=%q net=%q addr=%q db=%q", mc.User, mc.Passwd, mc.Net, mc.Addr, mc.DBName)
			}
		})
	}
}

func TestBuildConfigNative(t *testing.T) {
	mc := mustConfig(t, driver.ConnConfig{DSN: "u:pw@tcp(h:3306)/db?parseTime=false&loc=Local&charset=utf8mb4"})
	if mc.User != "u" || mc.Passwd != "pw" || mc.Addr != "h:3306" || mc.DBName != "db" {
		t.Errorf("native DSN parsed as %+v", mc)
	}
	mc = mustConfig(t, driver.ConnConfig{DSN: "u@tcp(h)/db"})
	if mc.Addr != "h:3306" {
		t.Errorf("addr = %q", mc.Addr)
	}
}

func TestBuildConfigForcedSettings(t *testing.T) {
	for _, dsn := range []string{
		"mysql://u@h/db?parseTime=false&loc=Local&time_zone=%27SYSTEM%27&TIME_ZONE=%27-05:00%27",
		"u@tcp(h)/db?parseTime=false&loc=Local&time_zone=%27SYSTEM%27",
	} {
		mc := mustConfig(t, driver.ConnConfig{DSN: dsn})
		if !mc.ParseTime {
			t.Errorf("%s: parseTime not forced", dsn)
		}
		if mc.Loc != time.UTC {
			t.Errorf("%s: loc = %v, want UTC", dsn, mc.Loc)
		}
		if got := paramValues(mc, "time_zone"); len(got) != 1 || got[0] != "'+00:00'" {
			t.Errorf("%s: time_zone params = %q, want only '+00:00'", dsn, got)
		}
		if _, ok := mc.Logger.(*gomysql.NopLogger); !ok {
			t.Errorf("%s: logger = %T, want a NopLogger so nothing is written to the terminal", dsn, mc.Logger)
		}
		if mc.MultiStatements {
			t.Errorf("%s: multiStatements on", dsn)
		}
	}
}

// paramValues returns every system-variable param whose name matches name
// case-insensitively.
func paramValues(mc *gomysql.Config, name string) []string {
	var out []string
	for k, v := range mc.Params {
		if strings.EqualFold(k, name) {
			out = append(out, v)
		}
	}
	return out
}

func TestBuildConfigReadOnly(t *testing.T) {
	dsn := "mysql://u@h/db?transaction_read_only=0&TRANSACTION_READ_ONLY=OFF&tx_read_only=0"
	mc := mustConfig(t, driver.ConnConfig{DSN: dsn, ReadOnly: true})
	if got := paramValues(mc, "transaction_read_only"); len(got) != 1 || got[0] != "1" {
		t.Errorf("transaction_read_only params = %q, want only 1", got)
	}
	if got := paramValues(mc, "tx_read_only"); len(got) != 0 {
		t.Errorf("tx_read_only params = %q, want none", got)
	}
	mc = mustConfig(t, driver.ConnConfig{DSN: "mysql://u@h/db"})
	if got := paramValues(mc, "transaction_read_only"); len(got) != 0 {
		t.Errorf("writable connection has transaction_read_only %q", got)
	}
}

func TestBuildConfigPasswordOverride(t *testing.T) {
	for _, dsn := range []string{"mysql://u:dsnpw@h/db", "u:dsnpw@tcp(h)/db"} {
		mc := mustConfig(t, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("resolved")})
		if mc.Passwd != "resolved" {
			t.Errorf("%s: passwd = %q, want the ConnConfig password", dsn, mc.Passwd)
		}
	}
}

func TestBuildConfigRefused(t *testing.T) {
	tests := []struct {
		name string
		cfg  driver.ConnConfig
		kind error
	}{
		{"multiStatements true", driver.ConnConfig{DSN: "mysql://u@h/db?multiStatements=true"}, driver.ErrUnsupported},
		{"multiStatements 1", driver.ConnConfig{DSN: "u@tcp(h)/db?multiStatements=1"}, driver.ErrUnsupported},
		{"multistatements lower case", driver.ConnConfig{DSN: "mysql://u@h/db?multistatements=TRUE"}, driver.ErrUnsupported},
		{"multiStatements yes", driver.ConnConfig{DSN: "mysql://u@h/db?MULTISTATEMENTS=yes"}, driver.ErrUnsupported},
		{"options", driver.ConnConfig{DSN: "mysql://u@h/db", Options: map[string]string{"x": "y"}}, driver.ErrUnsupported},
		{"other scheme", driver.ConnConfig{DSN: "postgres://u@h/db"}, driver.ErrSyntax},
		{"bad native", driver.ConnConfig{DSN: "u:topsecret@tcp(h/db"}, driver.ErrSyntax},
		{"bad url", driver.ConnConfig{DSN: "mysql://u:topsecret@h:port/db"}, driver.ErrSyntax},
		{"bad url params", driver.ConnConfig{DSN: "mysql://u:topsecret@h/db?a=%zz"}, driver.ErrSyntax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildConfig(tt.cfg)
			if !errors.Is(err, tt.kind) {
				t.Fatalf("buildConfig = %v, want %v", err, tt.kind)
			}
			if strings.Contains(err.Error(), "topsecret") {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
	if _, err := buildConfig(driver.ConnConfig{DSN: "mysql://u@h/db?multiStatements=false"}); err != nil {
		t.Errorf("multiStatements=false refused: %v", err)
	}
}

// TestOpenFailureRedactsPassword opens against a closed local port: the error
// must not carry the password, whether it came from the DSN or ConnConfig.
func TestOpenFailureRedactsPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "mysql://u:hunter2secret@127.0.0.1:1/db"},
		{DSN: "u:hunter2secret@tcp(127.0.0.1:1)/db"},
		{DSN: "mysql://u@127.0.0.1:1/db", Password: driver.NewSecret("hunter2secret")},
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
			t.Errorf("Open(%q) error leaks the password: %v", cfg.DSN, err)
		}
	}
}

func TestScrubErrorMasksPassword(t *testing.T) {
	cfg := driver.ConnConfig{DSN: "mysql://u:dsnsecret@h/db", Password: driver.NewSecret("cfgsecret")}
	in := driver.Wrap(driver.ErrConnection, Name, "open", errors.New("dial u:dsnsecret@h failed; also cfgsecret"))
	out := scrubError(in, cfg)
	msg := out.Error()
	if strings.Contains(msg, "dsnsecret") || strings.Contains(msg, "cfgsecret") {
		t.Errorf("scrubError left a secret: %s", msg)
	}
	if !errors.Is(out, driver.ErrConnection) {
		t.Errorf("scrubError lost the kind: %v", out)
	}
}

func TestBuildConfigClientCharset(t *testing.T) {
	for _, dsn := range []string{
		"mysql://u@h/db?charset=gbk",
		"mysql://u@h/db?charset=utf8mb4,gbk",
		"u@tcp(h)/db?charset=big5",
		"mysql://u@h/db?collation=gbk_chinese_ci",
		"u@tcp(h)/db?collation=sjis_japanese_ci",
		"mysql://u@h/db?character_set_client=gbk",
		"u@tcp(h)/db?CHARACTER_SET_CONNECTION=%27gbk%27",
		"mysql://u@h/db?character_set_results=gbk",
		"mysql://u@h/db?names=gbk",
		"u@tcp(h)/db?NAMES=gbk",
		"mysql://u@h/db?collation_connection=gbk_bin",
	} {
		_, err := buildConfig(driver.ConnConfig{DSN: dsn})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("buildConfig(%q) = %v, want ErrUnsupported", dsn, err)
		}
	}
	for _, dsn := range []string{
		"mysql://u@h/db",
		"mysql://u@h/db?charset=utf8mb4",
		"u@tcp(h)/db?charset=utf8mb4&collation=utf8mb4_bin",
	} {
		mc := mustConfig(t, driver.ConnConfig{DSN: dsn})
		if f := mc.FormatDSN(); !strings.Contains(f, "charset=utf8mb4") {
			t.Errorf("%s: client charset not pinned to utf8mb4: %s", dsn, f)
		}
	}
}

func TestBuildConfigInterpolateParams(t *testing.T) {
	for _, dsn := range []string{
		"mysql://u@h/db?interpolateParams=true",
		"u@tcp(h)/db?interpolateParams=1",
		"mysql://u@h/db?INTERPOLATEPARAMS=yes",
	} {
		_, err := buildConfig(driver.ConnConfig{DSN: dsn})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("buildConfig(%q) = %v, want ErrUnsupported", dsn, err)
		}
	}
	mc := mustConfig(t, driver.ConnConfig{DSN: "mysql://u@h/db?interpolateParams=false&interpolateparams=0"})
	if mc.InterpolateParams || len(paramValues(mc, "interpolateParams")) != 0 {
		t.Errorf("interpolateParams = %v, params %v", mc.InterpolateParams, mc.Params)
	}
}

func TestGuardVerified(t *testing.T) {
	tests := []struct {
		version string
		ok      bool
	}{
		{"8.4.3", true},
		{"8.0.36", true},
		{"8.0.36-28", true}, // Percona Server
		{"8.1.0", true},
		{"8.4.0-log", true},
		{"9.1.0", false},
		{"10.11.6-MariaDB-1:10.11.6+maria~ubu2204", false},
		{"11.4.2-MariaDB", false},
		{"8.0.0-MariaDB", false},
		{"5.7.44", false},
		{"8.5.0", false},
		{"", false},
		{"garbage", false},
		{"8", false},
	}
	for _, tt := range tests {
		if got := guardVerified(tt.version); got != tt.ok {
			t.Errorf("guardVerified(%q) = %v, want %v", tt.version, got, tt.ok)
		}
	}
}

func TestReadOnlyRefusedOnUnverifiedServer(t *testing.T) {
	if err := checkServer("9.1.0", flavorMySQL, flavorMySQL, true); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("read-only open on 9.1.0 = %v, want ErrUnsupported", err)
	}
	if err := checkServer("10.11.6-MariaDB", flavorMariaDB, flavorMariaDB, false); err != nil {
		t.Errorf("writable open on MariaDB = %v, want nil", err)
	}
	if err := checkServer("8.4.3", flavorMySQL, flavorMySQL, true); err != nil {
		t.Errorf("read-only open on 8.4.3 = %v, want nil", err)
	}
	c := &conn{version: "9.1.0"}
	_, err := c.Query(context.Background(), driver.Query{Text: "SELECT 1", ReadOnly: true})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("per-call ReadOnly on 9.1.0 = %v, want ErrUnsupported", err)
	}
}
