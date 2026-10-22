package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const brokenSeed = `connections:
  - name: good
    driver: postgres
    dsn: postgres://app@localhost/app
  - name: leaky
    driver: postgres
    dsn: postgres://app@localhost/app
    password: hunter2-SUPERSECRET
  - name: good
    driver: redis
  - driver: redis
    dsn: redis://h/0
  - name: "bad\u0085name"
    driver: x
  - name: typo
    driver: x
    pasword: SUPERSECRET
  - {name: nodriver, dsn: ./x.db}
  - name: fine
    driver: redis
`

func TestLoadCollectsBrokenEntries(t *testing.T) {
	cfg, err := Load(writeTemp(t, brokenSeed))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var good []string
	for _, c := range cfg.Connections {
		good = append(good, c.Name)
	}
	if want := []string{"good", "fine"}; !slices.Equal(good, want) {
		t.Fatalf("good = %v, want %v", good, want)
	}
	want := []BrokenConnection{
		{Name: "leaky", Line: 5, Err: "password must be a secret reference (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME)"},
		{Name: "good", Line: 9, Err: "duplicate name"},
		{Name: "", Line: 11, Err: "name is required"},
		{Name: "", Line: 13, Err: "name must not contain control characters"},
		{Name: "typo", Line: 15, Err: `unknown connection key (did you mean "password"?)`},
		{Name: "nodriver", Line: 18, Err: "set driver or use a DSN with a scheme"},
	}
	if len(cfg.Broken) != len(want) {
		t.Fatalf("Broken = %+v, want %d entries", cfg.Broken, len(want))
	}
	for i, w := range want {
		g := cfg.Broken[i]
		if g.Name != w.Name || g.Line != w.Line || g.Err != w.Err {
			t.Errorf("Broken[%d] = {%q %d %q}, want {%q %d %q}", i, g.Name, g.Line, g.Err, w.Name, w.Line, w.Err)
		}
		if strings.Contains(g.Err, "SUPERSECRET") || strings.Contains(g.Err, "hunter") {
			t.Errorf("Broken[%d].Err echoes a value: %q", i, g.Err)
		}
	}
}

func TestLoadWholeFileProblemsStayFatal(t *testing.T) {
	for name, body := range map[string]string{
		"bad yaml":   "connections: [\n",
		"bad theme":  "connections:\n  - {name: a, driver: x}\ntui:\n  theme: neon\n",
		"bad keymap": "tui:\n  keymap: emacs\n",
	} {
		if _, err := Load(writeTemp(t, body)); err == nil {
			t.Errorf("%s: want a fatal error", name)
		}
	}
}

func TestBrokenWrongTypeNeverEchoes(t *testing.T) {
	cfg, err := Load(writeTemp(t, "connections:\n  - name: a\n    driver: x\n    read_only: SUPERSECRET\n"))
	if err != nil || len(cfg.Broken) != 1 {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
	if strings.Contains(cfg.Broken[0].Err, "SUPERSECRET") {
		t.Fatalf("Err echoes the value: %q", cfg.Broken[0].Err)
	}
}

func TestBrokenPartialHoldsOnlySafeFields(t *testing.T) {
	cfg, err := Load(writeTemp(t, brokenSeed))
	if err != nil {
		t.Fatal(err)
	}
	b, ok := cfg.FindBroken("leaky")
	if !ok {
		t.Fatal("leaky not broken")
	}
	want := Connection{Name: "leaky", Driver: "postgres", DSN: "postgres://app@localhost/app"}
	if !reflect.DeepEqual(b.Partial, want) {
		t.Fatalf("Partial = %+v, want %+v (no password)", b.Partial, want)
	}
	if _, ok := cfg.FindBroken("good"); !ok {
		t.Fatal("duplicate not findable")
	}
	if _, ok := cfg.Find("leaky"); ok {
		t.Fatal("Find returns a broken entry")
	}
}

func TestEmptyRefsRejected(t *testing.T) {
	for _, ref := range []string{"env:", "file://", "dotenv://", "env:A,env:"} {
		if err := ValidatePasswordRef(ref); err == nil {
			t.Errorf("ValidatePasswordRef(%q) accepted", ref)
		}
		if err := ValidateLoadedPasswordRef(ref); err == nil {
			t.Errorf("ValidateLoadedPasswordRef(%q) accepted", ref)
		}
	}
	if err := ValidateLoadedPasswordRef("op://"); err == nil {
		t.Error("bare op:// accepted")
	}
	for _, ref := range []string{"env:A", "file:///p", "dotenv://p#K"} {
		if err := ValidatePasswordRef(ref); err != nil {
			t.Errorf("ValidatePasswordRef(%q): %v", ref, err)
		}
	}
}

func TestUpdateRepairsBrokenEntry(t *testing.T) {
	p := writeTemp(t, brokenSeed)
	_, st := loaded(t, p)
	fixed := Connection{Name: "leaky", Driver: "postgres", DSN: "postgres://app@localhost/app", Password: "env:PW"}
	if _, err := UpdateConnection(p, "leaky", fixed, st); err != nil {
		t.Fatalf("UpdateConnection on a broken entry: %v", err)
	}
	cfg, _ := loaded(t, p)
	if got, ok := cfg.Find("leaky"); !ok || got.Password != "env:PW" {
		t.Fatalf("leaky = %+v, %v", got, ok)
	}
	if _, ok := cfg.FindBroken("leaky"); ok {
		t.Fatal("leaky still broken")
	}
	if len(cfg.Broken) != 5 {
		t.Fatalf("other broken entries changed: %+v", cfg.Broken)
	}
}

func TestUpdateRefusesToStayBroken(t *testing.T) {
	p := writeTemp(t, brokenSeed)
	_, st := loaded(t, p)
	bad := Connection{Name: "leaky", Driver: "postgres", DSN: "postgres://app@localhost/app", Password: "hunter2"}
	if _, err := UpdateConnection(p, "leaky", bad, st); err == nil {
		t.Fatal("want an error for a password literal")
	}
}

func TestRemoveBrokenEntryAndAddBesideBroken(t *testing.T) {
	p := writeTemp(t, brokenSeed)
	_, st := loaded(t, p)
	st, err := RemoveConnection(p, "leaky", st)
	if err != nil {
		t.Fatalf("RemoveConnection on a broken entry: %v", err)
	}
	if _, err := AddConnection(p, Connection{Name: "new", Driver: "redis", DSN: "redis://h/1"}, st); err != nil {
		t.Fatalf("AddConnection beside broken entries: %v", err)
	}
	cfg, _ := loaded(t, p)
	if _, ok := cfg.Find("new"); !ok {
		t.Fatal("new not added")
	}
	if _, ok := cfg.FindBroken("leaky"); ok {
		t.Fatal("leaky not removed")
	}
}

func TestWriteThroughDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real", "config.yaml")
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := AddConnection(link, webConn, Stat{}); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced: %v, %v", fi, err)
	}
	if !strings.Contains(readFile(t, target), "name: web") {
		t.Fatalf("target not written: %q", readFile(t, target))
	}
}

func TestRemoveKeepsCommentOfNextEntry(t *testing.T) {
	for _, indent := range []string{"  ", "    ", "      "} {
		body := "connections:\n  - name: a\n    driver: x\n" + indent + "# about b\n  - name: b\n    driver: y\n"
		p := writeTemp(t, body)
		_, st := loaded(t, p)
		if _, err := RemoveConnection(p, "a", st); err != nil {
			t.Fatal(err)
		}
		want := "connections:\n" + indent + "# about b\n  - name: b\n    driver: y\n"
		if got := readFile(t, p); got != want {
			t.Errorf("indent %d: remove:\n%q\nwant\n%q", len(indent), got, want)
		}
		p = writeTemp(t, body)
		_, st = loaded(t, p)
		if _, err := UpdateConnection(p, "a", Connection{Name: "a", Driver: "z", DSN: "z://h"}, st); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, p); !strings.Contains(got, indent+"# about b\n  - name: b") {
			t.Errorf("indent %d: replace lost the comment:\n%s", len(indent), got)
		}
	}
}

func TestNameListsAreStable(t *testing.T) {
	if want := []string{"auto", "tobira-dark", "tobira-light"}; !slices.Equal(ThemeNames(), want) {
		t.Errorf("ThemeNames = %v", ThemeNames())
	}
	if want := []string{"default", "vim"}; !slices.Equal(KeymapNames(), want) {
		t.Errorf("KeymapNames = %v", KeymapNames())
	}
}
