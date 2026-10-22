package secrets

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/mamori"

	"github.com/xavidop/tobira/internal/config"
)

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"local-pg": "TOBIRA_CONN_LOCAL_PG_PASSWORD",
		"my.db-1":  "TOBIRA_CONN_MY_DB_1_PASSWORD",
		"Prod":     "TOBIRA_CONN_PROD_PASSWORD",
	} {
		if got := EnvName(in); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrecedence(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ref := func(v string, err error) func(context.Context, string) ([]byte, error) {
		return func(context.Context, string) ([]byte, error) { return []byte(v), err }
	}
	tests := []struct {
		name string
		r    Resolver
		c    config.Connection
		want string
	}{
		{"env wins", Resolver{Getenv: env(map[string]string{"TOBIRA_CONN_A_PASSWORD": "fromenv"}), Resolve: ref("fromref", nil)},
			config.Connection{Name: "a", DSN: "postgres://u:indsn@h/d", Password: "env:X"}, "fromenv"},
		{"ref beats dsn", Resolver{Getenv: env(nil), Resolve: ref("fromref", nil)},
			config.Connection{Name: "a", DSN: "postgres://u:indsn@h/d", Password: "env:X"}, "fromref"},
		{"dsn password", Resolver{Getenv: env(nil), Resolve: func(context.Context, string) ([]byte, error) {
			t.Error("Resolve must not be called")
			return nil, nil
		}},
			config.Connection{Name: "a", DSN: "postgres://u:indsn@h/d"}, ""},
		{"trim LF", Resolver{Getenv: env(nil), Resolve: ref("pw\n", nil)},
			config.Connection{Name: "a", Password: "file:///x"}, "pw"},
		{"trim CRLF", Resolver{Getenv: env(nil), Resolve: ref("pw\r\n", nil)},
			config.Connection{Name: "a", Password: "file:///x"}, "pw"},
		{"trim once", Resolver{Getenv: env(nil), Resolve: ref("pw\n\n", nil)},
			config.Connection{Name: "a", Password: "file:///x"}, "pw\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := tt.r.ConnConfig(context.Background(), tt.c)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Password.Reveal(); got != tt.want {
				t.Errorf("password = %q, want %q", got, tt.want)
			}
			if tt.want == "" && !cfg.Password.IsZero() {
				t.Error("password should be zero")
			}
			if cfg.DSN != tt.c.DSN {
				t.Errorf("DSN changed: %q", cfg.DSN)
			}
		})
	}
}

func TestResolveErrorRedacted(t *testing.T) {
	const secret = "s3cr3t-value"
	r := Resolver{
		Getenv:  func(string) string { return "" },
		Resolve: func(context.Context, string) ([]byte, error) { return []byte(secret), errors.New("boom") },
	}
	_, err := r.ConnConfig(context.Background(), config.Connection{Name: "prod", Password: "file:///vault/item/field"})
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	for _, must := range []string{"prod", "(file ref)"} {
		if !strings.Contains(msg, must) {
			t.Errorf("error %q missing %q", msg, must)
		}
	}
	for _, bad := range []string{secret, "vault", "item", "field"} {
		if strings.Contains(msg, bad) {
			t.Errorf("error %q leaks %q", msg, bad)
		}
	}
}

func TestEmptyResolvedValue(t *testing.T) {
	r := Resolver{Getenv: func(string) string { return "" },
		Resolve: func(context.Context, string) ([]byte, error) { return []byte("\n"), nil }}
	_, err := r.ConnConfig(context.Background(), config.Connection{Name: "a", Password: "file:///x"})
	if err == nil || !strings.Contains(err.Error(), "resolved to an empty value") {
		t.Fatalf("err = %v", err)
	}
}

func TestDSNPassthrough(t *testing.T) {
	opts := map[string]string{"sslmode": "disable"}
	c := config.Connection{Name: "n", Driver: "postgres", DSN: "postgres://h/d", ReadOnly: true, Options: opts}
	cfg, err := Resolver{}.ConnConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "n" || cfg.Driver != "postgres" || cfg.DSN != c.DSN || !cfg.ReadOnly || cfg.Options["sslmode"] != "disable" {
		t.Errorf("cfg = %+v", cfg)
	}
	opts["sslmode"] = "require"
	if cfg.Options["sslmode"] != "disable" {
		t.Error("Options aliased")
	}
}

func TestZeroResolverDefaults(t *testing.T) {
	t.Setenv("TOBIRA_CONN_Z_PASSWORD", "envpw")
	cfg, err := Resolver{}.ConnConfig(context.Background(), config.Connection{Name: "z"})
	if err != nil || cfg.Password.Reveal() != "envpw" {
		t.Fatalf("cfg=%v err=%v", cfg.Password.Reveal(), err)
	}
	t.Setenv("ZPW", "viaref\n")
	cfg, err = Resolver{Getenv: func(string) string { return "" }}.ConnConfig(context.Background(), config.Connection{Name: "z", Password: "env:ZPW"})
	if err != nil || cfg.Password.Reveal() != "viaref" {
		t.Fatalf("cfg=%v err=%v", cfg.Password.Reveal(), err)
	}
}

func TestRealMamoriErrorsRedacted(t *testing.T) {
	tests := []struct {
		name, ref string
		notFound  bool
	}{
		{"file", "file:///some/secret/path", true},
		{"env", "env:TOBIRA_TEST_NOPE_VAR", true},
		{"dotenv", "dotenv:///some/secret/.env#KEY", false},
		{"chain", "env:TOBIRA_TEST_NOPE_VAR,file:///some/secret/path", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolver{}.ConnConfig(context.Background(), config.Connection{Name: "c", Password: tt.ref})
			if err == nil {
				t.Fatal("want error")
			}
			for _, bad := range []string{"/some/secret", "TOBIRA_TEST_NOPE_VAR", "#KEY", ".env"} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %q leaks %q", err, bad)
				}
			}
			if tt.notFound && !errors.Is(err, mamori.ErrNotFound) {
				t.Errorf("errors.Is ErrNotFound false: %v", err)
			}
			t.Log(err)
		})
	}
}

func TestChainSchemeAndContext(t *testing.T) {
	r := Resolver{Getenv: func(string) string { return "" },
		Resolve: func(context.Context, string) ([]byte, error) { return nil, context.Canceled }}
	_, err := r.ConnConfig(context.Background(), config.Connection{Name: "c", Password: "env:A,file:///x"})
	if !strings.Contains(err.Error(), "(env ref): canceled") || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestUnregisteredSchemeNotPrinted(t *testing.T) {
	r := Resolver{Getenv: func(string) string { return "" },
		Resolve: func(context.Context, string) ([]byte, error) { return nil, errors.New("boom") }}
	for _, pw := range []string{"hunter:2rest", "op://vault/item/field"} {
		_, err := r.ConnConfig(context.Background(), config.Connection{Name: "c", Password: pw})
		if err == nil {
			t.Fatalf("%s: want error", pw)
		}
		msg := err.Error()
		if !strings.Contains(msg, "(password ref)") {
			t.Errorf("error %q, want \"(password ref)\"", msg)
		}
		for _, bad := range []string{"hunter", "rest", "op ", "vault"} {
			if strings.Contains(msg, bad) {
				t.Errorf("error %q leaks %q", msg, bad)
			}
		}
	}
	r.Resolve = func(context.Context, string) ([]byte, error) { return []byte("\n"), nil }
	_, err := r.ConnConfig(context.Background(), config.Connection{Name: "c", Password: "hunter:2rest"})
	if err == nil || strings.Contains(err.Error(), "hunter") || !strings.Contains(err.Error(), "(password ref)") {
		t.Errorf("empty value err = %v", err)
	}
}

func TestSecretResolvesRefAndTrims(t *testing.T) {
	r := Resolver{Resolve: func(_ context.Context, ref string) ([]byte, error) {
		if ref != "file:///k" {
			t.Fatalf("ref = %q", ref)
		}
		return []byte("sk-abc\n"), nil
	}}
	s, err := r.Secret(context.Background(), "ai.api_key", "file:///k")
	if err != nil {
		t.Fatal(err)
	}
	if s.Reveal() != "sk-abc" {
		t.Fatalf("secret = %q", s.Reveal())
	}
}

func TestSecretErrorsNeverHoldRefOrValue(t *testing.T) {
	r := Resolver{Resolve: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("open /run/secret/gemini-key-path: no such file")
	}}
	_, err := r.Secret(context.Background(), "ai.api_key", "file:///run/secret/gemini-key-path")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "gemini-key-path") {
		t.Fatalf("error leaks the ref path: %q", err)
	}
	if !strings.Contains(err.Error(), "ai.api_key") || !strings.Contains(err.Error(), "file ref") {
		t.Fatalf("error %q should name what and the scheme", err)
	}

	empty := Resolver{Resolve: func(context.Context, string) ([]byte, error) { return []byte("\n"), nil }}
	if _, err := empty.Secret(context.Background(), "ai.api_key", "env:K"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty secret err = %v", err)
	}
}

func TestSchemeAndFailureClass(t *testing.T) {
	for ref, want := range map[string]string{
		"env:DB_PW":                "env",
		"file:///vault/item/field": "file",
		"hunter2:rest":             "password",
		"":                         "password",
	} {
		if got := Scheme(ref); got != want {
			t.Errorf("Scheme(%q) = %q, want %q", ref, got, want)
		}
	}
	r := Resolver{Getenv: func(string) string { return "" },
		Resolve: func(context.Context, string) ([]byte, error) { return nil, context.DeadlineExceeded }}
	_, err := r.Secret(context.Background(), "x", "file:///vault/item")
	if got := FailureClass(err); got != "deadline exceeded" {
		t.Errorf("FailureClass = %q, want deadline exceeded", got)
	}
	r.Resolve = func(context.Context, string) ([]byte, error) { return []byte("\n"), nil }
	_, err = r.Secret(context.Background(), "x", "file:///vault/item")
	if got := FailureClass(err); got != "resolved to an empty value" {
		t.Errorf("FailureClass = %q, want resolved to an empty value", got)
	}
	if got := FailureClass(errors.New("vault/item")); got != "unknown error" {
		t.Errorf("FailureClass(other) = %q", got)
	}
}

func TestIsLocal(t *testing.T) {
	for ref, want := range map[string]bool{
		"env:A":                      true,
		"file:///x":                  true,
		"dotenv://.env#K":            true,
		"keyring:pg":                 true,
		"env:A,keyring:pg":           true,
		"op://vault/item":            false,
		"env:A,vault://kv/data/x#pw": false,
		"":                           false,
	} {
		if got := IsLocal(ref); got != want {
			t.Errorf("IsLocal(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestKnownSchemes(t *testing.T) {
	for ref, want := range map[string]bool{
		"env:A":              true,
		"file:///x":          true,
		"dotenv://.env#K":    true,
		"env:A,file:///x":    true,
		"keyring:pg":         false, // cmd/tobira registers it; this test binary does not
		"op://vault/item":    false, // not compiled into this build
		"env:A,nosuch://x/y": false,
		"":                   false,
	} {
		if got := KnownSchemes(ref); got != want {
			t.Errorf("KnownSchemes(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestCachedResolvesEachRefOnce(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	r := Resolver{Resolve: func(_ context.Context, ref string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[ref]++
		if ref == "env:BAD" {
			return nil, errors.New("not set")
		}
		return []byte("v-" + ref + "\n"), nil
	}}.Cached()
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if s, err := r.Secret(ctx, "x", "env:A"); err != nil || s.Reveal() != "v-env:A" {
				t.Errorf("Secret = %q, %v", s.Reveal(), err)
			}
			if _, err := r.Secret(ctx, "x", "env:BAD"); err == nil {
				t.Error("a failed ref resolved")
			}
		})
	}
	wg.Wait()
	cfg, err := r.ConnConfig(ctx, config.Connection{Name: "c", Password: "env:A"})
	if err != nil || cfg.Password.Reveal() != "v-env:A" {
		t.Fatalf("ConnConfig = %q, %v", cfg.Password.Reveal(), err)
	}
	if calls["env:A"] != 1 || calls["env:BAD"] != 1 {
		t.Errorf("calls = %v, want each ref resolved once", calls)
	}
}
