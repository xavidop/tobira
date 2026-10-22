package keyring_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/secrets/keyring"
)

// fake is a map-backed Store; no test touches the real OS keychain.
type fake struct {
	mu sync.Mutex
	m  map[string]string
}

func newFake() *fake { return &fake{m: map[string]string{}} }

func (f *fake) Get(service, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[service+"/"+account]
	if !ok {
		return "", gokeyring.ErrNotFound
	}
	return v, nil
}

func (f *fake) Set(service, account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[service+"/"+account] = secret
	return nil
}

func (f *fake) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[service+"/"+account]; !ok {
		return gokeyring.ErrNotFound
	}
	delete(f.m, service+"/"+account)
	return nil
}

func TestProviderResolves(t *testing.T) {
	f := newFake()
	_ = f.Set(keyring.Service, "local-pg", "s3cret-pw")
	p := keyring.NewProvider(f)
	if p.Scheme() != "keyring" {
		t.Fatalf("scheme = %q", p.Scheme())
	}
	ref, err := mamori.ParseRef(keyring.Ref("local-pg"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil || string(v.Bytes) != "s3cret-pw" || !v.Sensitive {
		t.Fatalf("v=%+v err=%v", v, err)
	}
}

func TestProviderNotFound(t *testing.T) {
	p := keyring.NewProvider(newFake())
	ref, _ := mamori.ParseRef("keyring:missing")
	_, err := p.Resolve(context.Background(), ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

type failing struct{ *fake }

func (failing) Get(string, string) (string, error) {
	return "", errors.New("backend exploded with s3cret-pw inside")
}

func TestProviderWrapsErrorsWithoutSecret(t *testing.T) {
	p := keyring.NewProvider(failing{newFake()})
	ref, _ := mamori.ParseRef("keyring:abc")
	_, err := p.Resolve(context.Background(), ref)
	if err == nil || strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("err = %v, want an error without the secret", err)
	}
	if !strings.Contains(err.Error(), "abc") {
		t.Errorf("err = %v, want it to name the account", err)
	}
}

func TestProviderRefusesBadNames(t *testing.T) {
	p := keyring.NewProvider(newFake())
	refused := 0
	for _, raw := range []string{"keyring:", "keyring:a b", "keyring:a/b", "keyring:a\x01"} {
		ref, err := mamori.ParseRef(raw)
		if err != nil {
			continue // refused earlier, by the parser
		}
		if _, err := p.Resolve(context.Background(), ref); err == nil || errors.Is(err, mamori.ErrNotFound) {
			t.Errorf("%q: err = %v, want a refusal", raw, err)
		}
		refused++
	}
	if refused < 2 {
		t.Errorf("only %d names reached Resolve; the test checks nothing", refused)
	}
}

func TestValidName(t *testing.T) {
	if err := keyring.ValidName("ok.name_1-x"); err != nil {
		t.Error(err)
	}
	for _, n := range []string{"", "a b", "a:b", "é"} {
		if keyring.ValidName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestOSStoreRoundTripWithMock(t *testing.T) {
	gokeyring.MockInit()
	s := keyring.OS()
	if err := s.Set(keyring.Service, "n", "pw"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(keyring.Service, "n"); err != nil || got != "pw" {
		t.Fatalf("got %q, %v", got, err)
	}
	// The provider maps the go-keyring not-found error.
	p := keyring.NewProvider(s)
	ref, _ := mamori.ParseRef("keyring:nope")
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if err := s.Delete(keyring.Service, "n"); err != nil {
		t.Fatal(err)
	}
}

func cleanupCfg(t *testing.T, text string) *config.Config {
	t.Helper()
	register()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCleanupOnRemove(t *testing.T) {
	const head = "connections:\n  - name: a\n    driver: d\n    dsn: d://x\n    password: keyring:a\n"
	cases := []struct {
		name, text, ref string
		wantDeleted     bool
	}{
		{"exact", head, "keyring:a", true},
		{"different ref", head, "keyring:other", false},
		{"env ref", head, "env:A", false},
		{"shared with a loaded entry", head + "  - name: b\n    driver: d\n    dsn: d://y\n    password: keyring:a\n", "keyring:a", false},
		{"shared with a broken entry", head + "  - name: b\n    dsn: nodriver\n    password: keyring:a\n", "keyring:a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			_ = f.Set(keyring.Service, "a", "pw")
			cfg := cleanupCfg(t, tc.text)
			if w := keyring.CleanupOnRemove(f, cfg, "a", tc.ref); w != "" {
				t.Errorf("warning %q", w)
			}
			if _, err := f.Get(keyring.Service, "a"); (err != nil) != tc.wantDeleted {
				t.Errorf("deleted = %v, want %v", err != nil, tc.wantDeleted)
			}
		})
	}
}

func TestCleanupOnRemoveWarnsWithoutBackendText(t *testing.T) {
	cfg := cleanupCfg(t, "connections:\n  - name: a\n    driver: d\n    dsn: d://x\n    password: keyring:a\n")
	w := keyring.CleanupOnRemove(failingDelete{newFake()}, cfg, "a", "keyring:a")
	if w == "" || strings.Contains(w, "s3cret-pw") {
		t.Errorf("warning = %q", w)
	}
	if keyring.CleanupOnRemove(nil, cfg, "a", "keyring:a") != "" {
		t.Error("nil store warned")
	}
	// A missing entry is not a problem.
	if w := keyring.CleanupOnRemove(newFake(), cfg, "a", "keyring:a"); w != "" {
		t.Errorf("warning for a missing entry: %q", w)
	}
}

type failingDelete struct{ *fake }

func (failingDelete) Delete(string, string) error { return errors.New("locked s3cret-pw") }

var once sync.Once

func register() { once.Do(func() { mamori.Register(keyring.NewProvider(newFake())) }) }

func TestRefValidationWithProviderRegistered(t *testing.T) {
	register()
	if err := config.ValidatePasswordRef("keyring:x"); err != nil {
		t.Errorf("writer rejects keyring:x: %v", err)
	}
	if err := config.ValidateLoadedPasswordRef("keyring:x"); err != nil {
		t.Errorf("loader rejects keyring:x: %v", err)
	}
	if config.ValidatePasswordRef("keyring:") == nil {
		t.Error("writer accepts an empty keyring name")
	}
	if config.ValidateLoadedPasswordRef("keyring:") == nil {
		t.Error("loader accepts an empty keyring name")
	}
}

func TestResolverUsesKeyring(t *testing.T) {
	f := newFake()
	_ = f.Set(keyring.Service, "c1", "pw-from-keyring\n")
	p := keyring.NewProvider(f)
	r := secrets.Resolver{
		Getenv: func(string) string { return "" },
		Resolve: func(ctx context.Context, raw string) ([]byte, error) {
			ref, err := mamori.ParseRef(raw)
			if err != nil {
				return nil, err
			}
			v, err := p.Resolve(ctx, ref)
			return v.Bytes, err
		},
	}
	cfg, err := r.ConnConfig(context.Background(), config.Connection{Name: "c1", Driver: "d", DSN: "d://x", Password: "keyring:c1"})
	if err != nil || cfg.Password.Reveal() != "pw-from-keyring" {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
}
