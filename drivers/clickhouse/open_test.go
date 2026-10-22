package clickhouse

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "clickhouse" || info.Kind != driver.KindSQL || info.Dialect != "clickhouse" {
		t.Errorf("info = %+v", info)
	}
	if !slices.Equal(info.Schemes, []string{"clickhouse"}) {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := []driver.Capability{driver.CapBrowser, driver.CapExplainer, driver.CapCompleter, driver.CapExporter, driver.CapReadOnly}
	if !slices.Equal(info.Capabilities, want) {
		t.Errorf("capabilities = %v, want %v", info.Capabilities, want)
	}
	if d, ok := driver.Default.Lookup("clickhouse"); !ok || d.Info().Name != "clickhouse" {
		t.Error(`init did not register "clickhouse"`)
	}
}

func mustOptions(t *testing.T, cfg driver.ConnConfig) *ch.Options {
	t.Helper()
	o, err := buildOptions(cfg)
	if err != nil {
		t.Fatalf("buildOptions(%q): %v", cfg.DSN, err)
	}
	return o
}

func TestBuildOptions(t *testing.T) {
	tests := []struct {
		dsn            string
		user, pass, db string
		addr           []string
		proto          ch.Protocol
		tls            bool
	}{
		{"clickhouse://u:p%40ss@db.example:9001/app?allow_plaintext=1", "u", "p@ss", "app", []string{"db.example:9001"}, ch.Native, false},
		{"clickhouse://h", "", "", "", []string{"h:9000"}, ch.Native, false}, // clickhouse-go then uses "default"
		{"CLICKHOUSE://u@h/db", "u", "", "db", []string{"h:9000"}, ch.Native, false},
		{"clickhouse://u@h/db?secure=true", "u", "", "db", []string{"h:9440"}, ch.Native, true},
		{"clickhouse://u@h/db?protocol=http&allow_plaintext=1", "u", "", "db", []string{"h:8123"}, ch.HTTP, false},
		{"clickhouse://u@h/db?protocol=http&secure=true", "u", "", "db", []string{"h:8443"}, ch.HTTP, true},
		{"clickhouse://u@h:18123/db?protocol=HTTP&ALLOW_PLAINTEXT=true", "u", "", "db", []string{"h:18123"}, ch.HTTP, false},
		{"clickhouse://u@[::1]:9010/db", "u", "", "db", []string{"[::1]:9010"}, ch.Native, false},
		{"clickhouse://u@[::1]/db", "u", "", "db", []string{"[::1]:9000"}, ch.Native, false},
		{"clickhouse://u@a,b:9001/db", "u", "", "db", []string{"a:9000", "b:9001"}, ch.Native, false},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			o := mustOptions(t, driver.ConnConfig{DSN: tt.dsn})
			if o.Auth.Username != tt.user || o.Auth.Password != tt.pass || o.Auth.Database != tt.db {
				t.Errorf("auth = %+v", o.Auth)
			}
			if !slices.Equal(o.Addr, tt.addr) {
				t.Errorf("addr = %v, want %v", o.Addr, tt.addr)
			}
			if o.Protocol != tt.proto {
				t.Errorf("protocol = %v, want %v", o.Protocol, tt.proto)
			}
			if (o.TLS != nil) != tt.tls {
				t.Errorf("tls = %v, want %v", o.TLS != nil, tt.tls)
			}
			if _, ok := o.Settings["protocol"]; ok {
				t.Error("protocol leaked into the server settings")
			}
		})
	}
}

func TestBuildOptionsPasswordOverride(t *testing.T) {
	o := mustOptions(t, driver.ConnConfig{DSN: "clickhouse://u:old@h/db?secure=true", Password: driver.NewSecret("new")})
	if o.Auth.Password != "new" {
		t.Errorf("password = %q, want the ConnConfig one", o.Auth.Password)
	}
}

func TestBuildOptionsReadOnlyDropsDSNSetting(t *testing.T) {
	for _, dsn := range []string{
		"clickhouse://u@h/db?readonly=0",
		"clickhouse://u@h/db?readonly=2",
		"clickhouse://u@h/db?READONLY=0&readonly=0",
	} {
		o := mustOptions(t, driver.ConnConfig{DSN: dsn, ReadOnly: true})
		for k := range o.Settings {
			if strings.EqualFold(k, "readonly") {
				t.Errorf("%s: the DSN's %s survived on a read-only connection", dsn, k)
			}
		}
	}
	o := mustOptions(t, driver.ConnConfig{DSN: "clickhouse://u@h/db?readonly=1"})
	if o.Settings["readonly"] != 1 {
		t.Error("a writable connection keeps the DSN's settings")
	}
}

func TestGuardSettings(t *testing.T) {
	native := mustOptions(t, driver.ConnConfig{DSN: "clickhouse://u@h/db?max_threads=2"})
	http := mustOptions(t, driver.ConnConfig{DSN: "clickhouse://u@localhost/db?protocol=http"})
	tests := []struct {
		name               string
		base               *ch.Options
		readOnly, serverRO bool
		want               map[string]any
	}{
		{"native writable", native, false, false, map[string]any{"max_threads": 2}},
		{"native read-only", native, true, false, map[string]any{"max_threads": 2, "readonly": 1}},
		{"native read-only, server read-only", native, true, true, map[string]any{"max_threads": 2}},
		{"http writable", http, false, false, map[string]any{cancelHTTPKey: 1}},
		{"http read-only", http, true, false, map[string]any{"readonly": 1, cancelHTTPKey: 1}},
		{"http read-only, server read-only", http, true, true, map[string]any{}},
		{"http writable, server read-only", http, false, true, map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.base.Settings)
			o := guardSettings(tt.base, tt.readOnly, tt.serverRO)
			if len(o.Settings) != len(tt.want) {
				t.Errorf("settings = %v, want %v", o.Settings, tt.want)
			}
			for k, v := range tt.want {
				if o.Settings[k] != v {
					t.Errorf("%s = %v, want %v", k, o.Settings[k], v)
				}
			}
			if len(tt.base.Settings) != before {
				t.Error("guardSettings changed its input")
			}
		})
	}
}

func TestBuildOptionsQuiet(t *testing.T) {
	o := mustOptions(t, driver.ConnConfig{DSN: "clickhouse://u@h/db?debug=true"})
	if o.Debug || o.Debugf != nil || o.Logger != nil { //nolint:staticcheck // checks the deprecated field is cleared
		t.Error("the driver must never log: it would write over the terminal")
	}
}

func TestBuildOptionsRefusals(t *testing.T) {
	tests := []struct {
		name string
		cfg  driver.ConnConfig
		kind error
	}{
		{"other scheme", driver.ConnConfig{DSN: "mysql://u@h/db"}, driver.ErrSyntax},
		{"no scheme", driver.ConnConfig{DSN: "u@h/db"}, driver.ErrSyntax},
		{"no host", driver.ConnConfig{DSN: "clickhouse:///db"}, driver.ErrSyntax},
		{"bad protocol", driver.ConnConfig{DSN: "clickhouse://h/db?protocol=grpc"}, driver.ErrUnsupported},
		{"bad port", driver.ConnConfig{DSN: "clickhouse://h:99999/db"}, driver.ErrSyntax},
		{"options", driver.ConnConfig{DSN: "clickhouse://h/db", Options: map[string]string{"x": "y"}}, driver.ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildOptions(tt.cfg)
			if !errors.Is(err, tt.kind) {
				t.Fatalf("err = %v, want %v", err, tt.kind)
			}
		})
	}
}

func TestOpenErrorIsRedacted(t *testing.T) {
	const secret = "s3cr3t-pass"
	for _, cfg := range []driver.ConnConfig{
		{DSN: "clickhouse://u:" + secret + "@127.0.0.1:1/db?dial_timeout=200ms"},
		{DSN: "clickhouse://u:" + secret + "@127.0.0.1:1/db?protocol=grpc"},
		{DSN: "clickhouse://u@127.0.0.1:1/db?dial_timeout=200ms", Password: driver.NewSecret(secret)},
		{DSN: "clickhouse://u:" + secret + "@h:bad/db"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := New().Open(ctx, cfg)
		cancel()
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%q) succeeded", driver.RedactDSN(cfg.DSN))
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("Open error leaks the password: %v", err)
		}
	}
}

func TestScrubError(t *testing.T) {
	cfg := driver.ConnConfig{DSN: "clickhouse://u:hunter22@h/db"}
	inner := errors.New("dial clickhouse://u:hunter22@h/db: refused")
	err := scrubError(driver.Wrap(driver.ErrConnection, Name, "open", inner), cfg)
	if strings.Contains(err.Error(), "hunter22") {
		t.Errorf("scrubbed error still shows the password: %v", err)
	}
	if !errors.Is(err, driver.ErrConnection) || !errors.Is(err, inner) {
		t.Errorf("scrubbing lost the kind or the cause: %v", err)
	}
}

func TestNamespacePoolErrorIsRedacted(t *testing.T) {
	const secret = "pool-s3cr3t"
	cfg := driver.ConnConfig{DSN: "clickhouse://u:" + secret + "@127.0.0.1:1/db?dial_timeout=200ms"}
	opts, err := buildOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &conn{opts: opts, defaultNS: "db", cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.pool(ctx, "other")
	if err == nil {
		t.Fatal("a pool on an unreachable server opened")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("pool error leaks the password: %v", err)
	}
}

func TestCatalogListingsAreCapped(t *testing.T) {
	for name, q := range map[string]string{"namespaces": namespacesSQL, "collections": collectionsSQL} {
		if !strings.Contains(q, "LIMIT 10000") {
			t.Errorf("%s query is not capped: %s", name, q)
		}
	}
}

type recordRT struct{ got string }

func (r *recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req.URL.RawQuery
	return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
}

func TestNoTimeoutSetting(t *testing.T) {
	rec := &recordRT{}
	rt := noTimeoutSetting{rec}
	req, _ := http.NewRequest(http.MethodPost, "http://h/?max_execution_time=35&readonly=1&query_id=x&http_write_exception_in_output_format=0", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if strings.Contains(rec.got, "max_execution_time") || strings.Contains(rec.got, "http_write_exception") || !strings.Contains(rec.got, "readonly=1") {
		t.Errorf("sent query = %q", rec.got)
	}
	if !strings.Contains(req.URL.RawQuery, "max_execution_time") {
		t.Error("the caller's request was modified")
	}
}

type idleRT struct {
	recordRT
	closed bool
}

func (i *idleRT) CloseIdleConnections() { i.closed = true }

func TestNoTimeoutSettingForwardsCloseIdle(t *testing.T) {
	inner := &idleRT{}
	(&http.Client{Transport: noTimeoutSetting{inner}}).CloseIdleConnections()
	if !inner.closed {
		t.Error("CloseIdleConnections did not reach the transport: pooled HTTP sockets would leak on Close")
	}
}
