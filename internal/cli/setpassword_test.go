package cli_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets/keyring"
)

// fakeStore is a map-backed keyring.Store.
type fakeStore struct {
	mu      sync.Mutex
	m       map[string]string
	delErr  error
	deleted []string
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string]string{}} }

func (f *fakeStore) Get(service, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[service+"/"+account]
	if !ok {
		return "", gokeyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeStore) Set(service, account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[service+"/"+account] = secret
	return nil
}

func (f *fakeStore) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, account)
	if _, ok := f.m[service+"/"+account]; !ok {
		return gokeyring.ErrNotFound
	}
	delete(f.m, service+"/"+account)
	return nil
}

const pw = "Tr0ub4dor&3-horse"

func (h *harness) stored(name string) (string, bool) {
	v, err := h.keyring().Get(keyring.Service, name)
	return v, err == nil
}

func TestSetPasswordStoresAndUpdatesConfig(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	out, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got, ok := h.stored("local-pg"); !ok || got != pw {
		t.Errorf("stored = %q ok=%v", got, ok)
	}
	cfg, err := config.Load(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := cfg.Find("local-pg")
	if c.Password != "keyring:local-pg" {
		t.Errorf("password ref = %q", c.Password)
	}
	if !strings.Contains(errOut, "Password for local-pg: ") || !strings.Contains(errOut, "Confirm: ") ||
		!strings.Contains(errOut, "stored password for local-pg in the OS keyring") {
		t.Errorf("stderr = %q", errOut)
	}
	if strings.Contains(errOut, "changed from") {
		t.Errorf("no old ref, yet stderr = %q", errOut)
	}
	cfgBytes, _ := os.ReadFile(h.cfg)
	for name, s := range map[string]string{"stdout": out, "stderr": errOut, "config": string(cfgBytes)} {
		if strings.Contains(s, pw) {
			t.Errorf("password leaked into %s", name)
		}
	}
}

func TestSetPasswordMismatchAndEmpty(t *testing.T) {
	for name, pws := range map[string][]string{
		"mismatch": {pw, "other"},
		"empty":    {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.tty = true
			h.pws = pws
			before, _ := os.ReadFile(h.cfg)
			_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg")
			if code != 1 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if _, ok := h.stored("local-pg"); ok {
				t.Error("something was stored")
			}
			if after, _ := os.ReadFile(h.cfg); string(after) != string(before) {
				t.Error("config changed")
			}
			if strings.Contains(errOut, pw) {
				t.Error("password leaked")
			}
		})
	}
}

func TestSetPasswordUnknownConnection(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "ghost")
	if code != 1 || !strings.Contains(errOut, "ghost") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("ghost"); ok {
		t.Error("stored for an unknown connection")
	}
}

const brokenCfg = `connections:
  # keep this comment
  - name: broken
    dsn: no-scheme-here
    password: hunter2-literal
    # and this one
    unknown_field: 42
  - name: good
    driver: fake
    dsn: fake://x   # trailing
    # inside good
    tags: [a]
`

func TestSetPasswordBrokenEntryKeepsEverythingElse(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, []byte(brokenCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tty = true
	h.pws = []string{pw, pw}
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "broken")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("broken"); !ok {
		t.Error("nothing stored for the broken entry")
	}
	got, _ := os.ReadFile(h.cfg)
	want := strings.Replace(brokenCfg, "password: hunter2-literal", "password: keyring:broken", 1)
	if string(got) != want {
		t.Errorf("config:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(errOut, "hunter2-literal") {
		t.Error("the old literal was echoed")
	}
}

func TestSetPasswordKeepsCommentsInsideTheEntry(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, []byte(brokenCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tty = true
	h.pws = []string{pw, pw}
	if _, e, c := h.exec(t, context.Background(), "connections", "set-password", "good"); c != 0 {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(h.cfg)
	want := strings.Replace(brokenCfg, "    tags: [a]\n", "    tags: [a]\n    password: keyring:good\n", 1)
	if string(got) != want {
		t.Errorf("config:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetPasswordCancelledAtThePrompt(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	ctx, cancel := context.WithCancel(context.Background())
	h.onRead = func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	before, _ := os.ReadFile(h.cfg)
	_, errOut, code := h.exec(t, ctx, "connections", "set-password", "local-pg")
	if code == 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("local-pg"); ok {
		t.Error("stored after a cancel")
	}
	if after, _ := os.ReadFile(h.cfg); string(after) != string(before) {
		t.Error("config changed after a cancel")
	}
}

func TestSetPasswordConfigChangedWhilePromptingStoresNothing(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	h.onRead = func(context.Context) error {
		f, err := os.OpenFile(h.cfg, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteString("# edited elsewhere\n")
		return errors.Join(err, f.Close())
	}
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg")
	if code != 1 || !strings.Contains(errOut, "changed on disk") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("local-pg"); ok {
		t.Error("stored although the config changed")
	}
}

func TestSetPasswordConfigWriteFailureAfterStoreSaysSo(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	dir := filepath.Dir(h.cfg)
	h.onRead = func(context.Context) error { return os.Chmod(dir, 0o500) }
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg")
	if code != 1 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, want := range []string{"stored in the OS keyring", "not updated", "at keyring:local-pg by hand"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q lacks %q", errOut, want)
		}
	}
	if _, ok := h.stored("local-pg"); !ok {
		t.Error("the password was not stored")
	}
	if strings.Contains(errOut, pw) {
		t.Error("password leaked")
	}
}

func TestSetPasswordStdinRefusedOnATerminal(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.in = pw + "\n"
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg", "--stdin")
	if code != 1 || !strings.Contains(errOut, "--stdin needs piped input") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("local-pg"); ok {
		t.Error("stored")
	}
}

type failingSet struct {
	*fakeStore
	err error
}

func (f failingSet) Set(string, string, string) error { return f.err }

func TestSetPasswordNamesKnownBackendFailures(t *testing.T) {
	for want, err := range map[string]error{
		"too long":        gokeyring.ErrSetDataTooBig,
		"not supported":   gokeyring.ErrUnsupportedPlatform,
		"keyring service": errors.New("dbus said " + pw),
	} {
		t.Run(want, func(t *testing.T) {
			h := newHarness(t)
			h.tty = true
			h.pws = []string{pw, pw}
			h.store = nil
			h.keyringOverride = failingSet{newFakeStore(), err}
			_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg")
			if code != 1 || !strings.Contains(errOut, want) || strings.Contains(errOut, pw) {
				t.Errorf("code=%d stderr=%q, want %q", code, errOut, want)
			}
		})
	}
}

func TestSetPasswordNoConfig(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	before, _ := os.ReadFile(h.cfg)
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "brand.new", "--no-config")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("brand.new"); !ok {
		t.Error("not stored")
	}
	if after, _ := os.ReadFile(h.cfg); string(after) != string(before) {
		t.Error("config touched with --no-config")
	}
}

func TestSetPasswordRejectsBadName(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	_, _, code := h.exec(t, context.Background(), "connections", "set-password", "a b", "--no-config")
	if code != 1 {
		t.Errorf("code=%d", code)
	}
}

func TestSetPasswordNeedsTTYOrStdin(t *testing.T) {
	h := newHarness(t)
	h.in = pw + "\n"
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg")
	if code != 1 || !strings.Contains(errOut, "--stdin") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("local-pg"); ok {
		t.Error("stored without a terminal or --stdin")
	}
	if strings.Contains(errOut, pw) {
		t.Error("password leaked")
	}
}

func TestSetPasswordStdin(t *testing.T) {
	h := newHarness(t)
	h.in = pw + "\r\nignored\n"
	out, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg", "--stdin")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got, _ := h.stored("local-pg"); got != pw {
		t.Errorf("stored %q", got)
	}
	if strings.Contains(errOut, "Password for") || strings.Contains(out+errOut, pw) {
		t.Errorf("out=%q err=%q", out, errOut)
	}
}

func TestSetPasswordStdinEmpty(t *testing.T) {
	h := newHarness(t)
	h.in = "\n"
	_, _, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg", "--stdin")
	if code != 1 {
		t.Errorf("code=%d", code)
	}
	if _, ok := h.stored("local-pg"); ok {
		t.Error("stored an empty password")
	}
}

func TestSetPasswordRefChangedNotice(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	// Give local-pg an env ref first.
	b, _ := os.ReadFile(h.cfg)
	s := strings.Replace(string(b), "dsn: fake://x\n  - name: ro", "dsn: fake://x\n    password: env:OLD_PW\n  - name: ro", 1)
	if err := os.WriteFile(h.cfg, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	h.pws = []string{pw, pw}
	_, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(errOut, "password ref changed from env:OLD_PW to keyring:local-pg") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestSetPasswordNeverLeaksToDebugLog(t *testing.T) {
	h := newHarness(t)
	state := t.TempDir()
	h.env = map[string]string{"XDG_STATE_HOME": state}
	h.tty = true
	h.pws = []string{pw, pw}
	out, errOut, code := h.exec(t, context.Background(), "--debug", "connections", "set-password", "local-pg")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	cfgBytes, _ := os.ReadFile(h.cfg)
	log, _ := os.ReadFile(filepath.Join(state, "tobira", "tobira.log"))
	for name, s := range map[string]string{"stdout": out, "stderr": errOut, "config": string(cfgBytes), "log": string(log)} {
		if strings.Contains(s, pw) {
			t.Errorf("password leaked into %s", name)
		}
	}
}

func TestRmDeletesKeyringEntry(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	if _, e, c := h.exec(t, context.Background(), "connections", "set-password", "local-pg"); c != 0 {
		t.Fatal(e)
	}
	out, errOut, code := h.exec(t, context.Background(), "connections", "rm", "local-pg", "--yes")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if _, ok := h.stored("local-pg"); ok {
		t.Error("keyring entry left behind")
	}
	if strings.Contains(out+errOut, "warning") {
		t.Errorf("out=%q err=%q", out, errOut)
	}
}

func TestRmLeavesForeignRefs(t *testing.T) {
	h := newHarness(t)
	_ = h.keyring().Set(keyring.Service, "ro", "keep")
	_ = h.keyring().Set(keyring.Service, "other", "keep")
	b, _ := os.ReadFile(h.cfg)
	s := strings.Replace(string(b), "read_only: true", "read_only: true\n    password: keyring:other", 1)
	_ = os.WriteFile(h.cfg, []byte(s), 0o600)
	if _, e, c := h.exec(t, context.Background(), "connections", "rm", "ro", "--yes"); c != 0 {
		t.Fatal(e)
	}
	for _, n := range []string{"ro", "other"} {
		if _, ok := h.stored(n); !ok {
			t.Errorf("entry %q was deleted", n)
		}
	}
}

func TestRmKeyringFailureIsAWarning(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.pws = []string{pw, pw}
	if _, e, c := h.exec(t, context.Background(), "connections", "set-password", "local-pg"); c != 0 {
		t.Fatal(e)
	}
	h.store.delErr = os.ErrPermission
	_, errOut, code := h.exec(t, context.Background(), "connections", "rm", "local-pg", "--yes")
	if code != 0 || !strings.Contains(errOut, "warning") || strings.Contains(errOut, pw) {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("local-pg"); ok {
		t.Error("connection not removed")
	}
}

func TestRmKeepsAnEntryAnotherConnectionStillUses(t *testing.T) {
	for name, extra := range map[string]string{
		"loaded": "  - name: sharer\n    driver: fake\n    dsn: fake://s\n    password: keyring:local-pg\n",
		"broken": "  - name: sharer\n    dsn: no-scheme\n    password: keyring:local-pg\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			b, _ := os.ReadFile(h.cfg)
			s := strings.Replace(string(b), "    dsn: fake://x\n  - name: ro", "    dsn: fake://x\n    password: keyring:local-pg\n  - name: ro", 1) + extra
			if err := os.WriteFile(h.cfg, []byte(s), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = h.keyring().Set(keyring.Service, "local-pg", "keep")
			if _, e, c := h.exec(t, context.Background(), "connections", "rm", "local-pg", "--yes"); c != 0 {
				t.Fatal(e)
			}
			if _, ok := h.stored("local-pg"); !ok {
				t.Error("a shared keyring entry was deleted")
			}
		})
	}
}

// A renamed connection keeps keyring:<old name>. set-password <old name> for
// a new connection must not silently change the password the renamed one
// authenticates with.
func TestSetPasswordRefusesAnEntryAnotherConnectionUses(t *testing.T) {
	for name, extra := range map[string]string{
		"loaded": "  - name: prod-old\n    driver: fake\n    dsn: fake://s\n    password: keyring:local-pg\n",
		"broken": "  - name: prod-old\n    dsn: no-scheme\n    password: keyring:local-pg\n",
	} {
		for _, noConfig := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-config=%v", name, noConfig), func(t *testing.T) {
				h := newHarness(t)
				h.tty = true
				b, _ := os.ReadFile(h.cfg)
				if err := os.WriteFile(h.cfg, append(b, extra...), 0o600); err != nil {
					t.Fatal(err)
				}
				_ = h.keyring().Set(keyring.Service, "local-pg", "old-secret")
				before, _ := os.ReadFile(h.cfg)
				args := []string{"connections", "set-password", "local-pg"}
				if noConfig {
					args = append(args, "--no-config")
				}

				h.pws = []string{pw, pw}
				_, errOut, code := h.exec(t, context.Background(), args...)
				if code != 1 || !strings.Contains(errOut, "keyring:local-pg is used by connection prod-old; pass --force to overwrite") {
					t.Fatalf("code=%d stderr=%q", code, errOut)
				}
				if strings.Contains(errOut, "Password for") {
					t.Error("prompted before refusing")
				}
				if got, _ := h.stored("local-pg"); got != "old-secret" {
					t.Errorf("stored = %q, want the old secret kept", got)
				}
				if after, _ := os.ReadFile(h.cfg); string(after) != string(before) {
					t.Error("config changed")
				}

				h.pws = []string{pw, pw}
				_, errOut, code = h.exec(t, context.Background(), append(args, "--force")...)
				if code != 0 {
					t.Fatalf("--force: code=%d stderr=%q", code, errOut)
				}
				if got, _ := h.stored("local-pg"); got != pw {
					t.Errorf("--force: stored = %q", got)
				}
			})
		}
	}
}

// The connection's own reference is not another user.
func TestSetPasswordOwnReferenceIsNotShared(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	b, _ := os.ReadFile(h.cfg)
	s := strings.Replace(string(b), "    dsn: fake://x\n  - name: ro", "    dsn: fake://x\n    password: keyring:local-pg\n  - name: ro", 1)
	if err := os.WriteFile(h.cfg, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = h.keyring().Set(keyring.Service, "local-pg", "old-secret")
	h.pws = []string{pw, pw}
	if _, errOut, code := h.exec(t, context.Background(), "connections", "set-password", "local-pg"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got, _ := h.stored("local-pg"); got != pw {
		t.Errorf("stored = %q", got)
	}
}
