package redis

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "redis" || info.Kind != driver.KindKeyValue || info.Dialect != "redis" {
		t.Errorf("info = %+v", info)
	}
	if !slices.Equal(info.Schemes, []string{"redis", "rediss"}) {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapWatcher, driver.CapReadOnly}
	if !slices.Equal(info.Capabilities, want) {
		t.Errorf("capabilities = %v, want %v", info.Capabilities, want)
	}
	if d, ok := driver.Default.Lookup("redis"); !ok || d.Info().Name != "redis" {
		t.Error(`init did not register "redis"`)
	}
}

func mustOptions(t *testing.T, cfg driver.ConnConfig) *goredis.Options {
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
		addr, user, pw string
		db             int
		tls            bool
	}{
		{"redis://localhost:6379", "localhost:6379", "", "", 0, false},
		{"redis://h", "h:6379", "", "", 0, false},
		{"redis://:secret@h:7000/3", "h:7000", "", "secret", 3, false},
		{"redis://alice:p%40ss@h/2", "h:6379", "alice", "p@ss", 2, false},
		{"rediss://h:6380/1", "h:6380", "", "", 1, true},
		{"REDIS://h/4", "h:6379", "", "", 4, false},
		{"redis://h?db=5", "h:6379", "", "", 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			o := mustOptions(t, driver.ConnConfig{DSN: tt.dsn})
			if o.Addr != tt.addr || o.Username != tt.user || o.Password != tt.pw || o.DB != tt.db || (o.TLSConfig != nil) != tt.tls {
				t.Errorf("options = addr %q user %q pw %q db %d tls %v", o.Addr, o.Username, o.Password, o.DB, o.TLSConfig != nil)
			}
			if o.Protocol != 2 {
				t.Errorf("Protocol = %d, want 2 (RESP2 replies are what the renderer reads)", o.Protocol)
			}
			if !o.DisableIdentity {
				t.Error("DisableIdentity is off: tobira does not announce itself with CLIENT SETINFO")
			}
			if o.MaintNotificationsConfig == nil || o.MaintNotificationsConfig.Mode != "disabled" {
				t.Error("maintenance notifications must be disabled")
			}
		})
	}
}

func TestBuildOptionsOverrides(t *testing.T) {
	o := mustOptions(t, driver.ConnConfig{DSN: "redis://:dsnpw@h/0?protocol=3", Password: driver.NewSecret("override")})
	if o.Password != "override" {
		t.Errorf("Password = %q, want the ConnConfig password", o.Password)
	}
	if o.Protocol != 2 {
		t.Errorf("Protocol = %d: protocol=3 in the DSN must be overridden", o.Protocol)
	}
	ro := mustOptions(t, driver.ConnConfig{DSN: "redis://h", ReadOnly: true})
	if ro.OnConnect == nil {
		t.Error("a read-only connection needs the OnConnect hook that sends READONLY to cluster replicas")
	}
}

func TestBuildOptionsRefused(t *testing.T) {
	for _, tt := range []struct {
		cfg  driver.ConnConfig
		kind error
	}{
		{driver.ConnConfig{DSN: "postgres://h/db"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "redis://h:notaport"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "redis://h/notadb"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "redis://h?bogus=1"}, driver.ErrSyntax},
		{driver.ConnConfig{DSN: "redis://h", Options: map[string]string{"x": "y"}}, driver.ErrUnsupported},
		{driver.ConnConfig{DSN: "unix:///tmp/redis.sock"}, nil}, // accepted
	} {
		_, err := buildOptions(tt.cfg)
		if tt.kind == nil {
			if err != nil {
				t.Errorf("buildOptions(%q): %v", tt.cfg.DSN, err)
			}
			continue
		}
		if !errors.Is(err, tt.kind) {
			t.Errorf("buildOptions(%q) = %v, want %v", tt.cfg.DSN, err, tt.kind)
		}
	}
}

// TestOpenRedactsPassword proves an Open error never shows the password,
// whether it sits in the DSN or in ConnConfig.Password.
func TestOpenRedactsPassword(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "redis://:hunter2secret@h:notaport/0"},
		{DSN: "redis://:hunter2secret@127.0.0.1:1/0"},
		{DSN: "redis://u:hunter2secret@h/0?bogus=hunter2secret"},
		{DSN: "redis://127.0.0.1:1/0", Password: driver.NewSecret("hunter2secret")},
		{DSN: "redis://:hunter2secret@h/notadb"},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%q) succeeded", cfg.DSN)
		}
		if strings.Contains(err.Error(), "hunter2secret") {
			t.Errorf("Open error leaks the password: %v", err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Op != "open" {
			t.Errorf("Open error = %#v, want a *driver.Error for redis open", err)
		}
	}
}

func TestParseClusterReplica(t *testing.T) {
	for _, tt := range []struct {
		info string
		want bool
	}{
		{"# Cluster\r\ncluster_enabled:1\r\n# Replication\r\nrole:slave\r\n", true},
		{"# Cluster\r\ncluster_enabled:1\r\n# Replication\r\nrole:master\r\n", false},
		{"# Cluster\r\ncluster_enabled:0\r\n# Replication\r\nrole:slave\r\n", false},
		{"", false},
	} {
		if got := isClusterReplica(tt.info); got != tt.want {
			t.Errorf("isClusterReplica(%q) = %v, want %v", tt.info, got, tt.want)
		}
	}
}

func TestNotifyFlags(t *testing.T) {
	for _, tt := range []struct {
		flags            string
		keyspace, events bool
	}{
		{"", false, false},
		{"KEA", true, true},
		{"Kg$", true, false},
		{"Ex", false, true},
		{"K", false, false}, // no event class: nothing is delivered
		{"E", false, false},
		{"AK", true, false},
		{"Kh", true, false},
	} {
		ks, ev := notifyModes(tt.flags)
		if ks != tt.keyspace || ev != tt.events {
			t.Errorf("notifyModes(%q) = %v, %v; want %v, %v", tt.flags, ks, ev, tt.keyspace, tt.events)
		}
	}
}
