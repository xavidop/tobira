package sshtunnel_test

import (
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/sshtunnel"
)

func TestTarget(t *testing.T) {
	for dsn, want := range map[string]string{
		"postgres://app@db.internal:5433/app?sslmode=disable": "db.internal:5433",
		"postgres://app:pw@db.internal/app":                   "db.internal:5432",
		"mysql://root@10.0.0.5":                               "10.0.0.5:3306",
		"redis://cache:6380/0":                                "cache:6380",
		"mongodb://u:p@mongo.internal/db?authSource=admin":    "mongo.internal:27017",
		"neo4j://graph:7688":                                  "graph:7688",
		"bolt://graph":                                        "graph:7687",
		"https://search.internal/":                            "search.internal:443",
		"qdrant://[fd00::1]:6334":                             "[fd00::1]:6334",
		"clickhouse://[fd00::1]/default":                      "[fd00::1]:9000",
		"sqlserver://sa@db.internal?database=x":               "db.internal:1433",
	} {
		got, err := sshtunnel.Target(dsn)
		if err != nil || got != want {
			t.Errorf("Target(%q) = %q, %v; want %q", dsn, got, err, want)
		}
	}
}

func TestTargetErrors(t *testing.T) {
	for dsn, want := range map[string]string{
		"sqlite:./app.db":                         "scheme://host:port",
		"server=db;user id=sa":                    "scheme://host:port",
		"mongodb://a.internal,b.internal/db":      "several hosts",
		"mongodb+srv://u:p@cluster.example.com/x": "+srv",
		"weird://db.internal/x":                   "no default; add the port",
		"postgres:///app":                         "no host",
	} {
		_, err := sshtunnel.Target(dsn)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Target(%q) = %v, want an error containing %q", dsn, err, want)
		}
	}
}

func TestRewrite(t *testing.T) {
	for dsn, want := range map[string]string{
		"postgres://app:pw@db.internal:5433/app?sslmode=disable": "postgres://app:pw@127.0.0.1:50000/app?sslmode=disable",
		"redis://cache/0":                   "redis://127.0.0.1:50000/0",
		"https://search.internal":           "https://127.0.0.1:50000",
		"qdrant://[fd00::1]:6334?tls=false": "qdrant://127.0.0.1:50000?tls=false",
	} {
		got, err := sshtunnel.Rewrite(dsn, "127.0.0.1:50000")
		if err != nil || got != want {
			t.Errorf("Rewrite(%q) = %q, %v; want %q", dsn, got, err, want)
		}
	}
	if _, err := sshtunnel.Rewrite("sqlite:./x", "127.0.0.1:1"); err == nil {
		t.Error("a non-URL DSN was rewritten")
	}
}

func TestConfigAddr(t *testing.T) {
	for _, tt := range []struct {
		cfg  sshtunnel.Config
		want string
	}{
		{sshtunnel.Config{Host: "bastion"}, "bastion:22"},
		{sshtunnel.Config{Host: "bastion:2222"}, "bastion:2222"},
		{sshtunnel.Config{Host: "bastion:2222", Port: 22}, "bastion:22"},
		{sshtunnel.Config{Host: "[fd00::2]", Port: 2200}, "[fd00::2]:2200"},
		{sshtunnel.Config{Host: "10.0.0.1"}, "10.0.0.1:22"},
	} {
		if got := tt.cfg.Addr(); got != tt.want {
			t.Errorf("%q/%d: Addr = %q, want %q", tt.cfg.Host, tt.cfg.Port, got, tt.want)
		}
	}
}
