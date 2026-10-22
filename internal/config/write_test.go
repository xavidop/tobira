package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

var testDrivers = []driver.Info{
	{Name: "postgres", Schemes: []string{"postgres", "postgresql"}},
	{Name: "redis", Schemes: []string{"redis"}},
	{Name: "sqlite", Schemes: []string{"sqlite", "file"}},
}

const seed = `# tobira config
# keep me

connections:
  # the local one
  - name: local-pg
    driver: postgres
    dsn: postgres://app@localhost:5432/app
    tags: [prod]   # trailing comment

  - name: cache
    driver: redis
    dsn: redis://localhost:6379/0

ai:
  provider: googleai   # comment
tui:
  theme: tobira-dark
`

var webConn = Connection{
	Name: "web", Driver: "postgres", DSN: "postgres://u@h/db", Password: "env:PW",
	Color: "red", ReadOnly: true, Tags: []string{"a", "b"},
}

const webEntry = `  - name: web
    driver: postgres
    dsn: postgres://u@h/db
    password: env:PW
    color: red
    read_only: true
    tags: [a, b]
`

func loaded(t *testing.T, path string) (*Config, Stat) {
	t.Helper()
	cfg, st, err := LoadWithStat(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, st
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddPreservesComments(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	if _, err := AddConnection(p, webConn, st); err != nil {
		t.Fatal(err)
	}
	// The entry goes right after the last connection, before the blank line.
	cut := strings.Index(seed, "redis://localhost:6379/0\n") + len("redis://localhost:6379/0\n")
	want := seed[:cut] + webEntry + seed[cut:]
	got := readFile(t, p)
	if got != want {
		t.Fatalf("file differs from seed + entry:\n--- got\n%s\n--- want\n%s", got, want)
	}
	if without := strings.Replace(got, webEntry, "", 1); without != seed {
		t.Fatal("removing the new entry does not restore the original bytes")
	}
	for _, c := range []string{"# tobira config", "# keep me", "# the local one", "# trailing comment", "# comment"} {
		if !strings.Contains(got, c) {
			t.Errorf("comment %q lost", c)
		}
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := cfg.Find("web"); !ok || c.DSN != webConn.DSN || !c.ReadOnly || len(c.Tags) != 2 {
		t.Fatalf("web = %+v", c)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestAddLayouts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no connections key", "tui:\n  theme: tobira-dark\n", "tui:\n  theme: tobira-dark\nconnections:\n" + webEntry},
		{"no trailing newline", "connections:\n  - {name: a, driver: redis}", "connections:\n  - {name: a, driver: redis}\n" + webEntry},
		{"empty flow list", "connections: []   # none yet\n", "connections:   # none yet\n" + webEntry},
		{"null list", "connections:\ntui:\n  theme: auto\n", "connections:\n" + webEntry + "tui:\n  theme: auto\n"},
		{"dash at key column", "connections:\n- name: a\n  driver: redis\nai: {}\n", "connections:\n- name: a\n  driver: redis\n- name: web\n  driver: postgres\n  dsn: postgres://u@h/db\n  password: env:PW\n  color: red\n  read_only: true\n  tags: [a, b]\nai: {}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeTemp(t, tt.in)
			_, st := loaded(t, p)
			if _, err := AddConnection(p, webConn, st); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, p); got != tt.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tt.want)
			}
			if _, err := Load(p); err != nil {
				t.Fatalf("result does not load: %v", err)
			}
		})
	}
}

func TestUpdateRenames(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	c := Connection{Name: "pg2", Driver: "postgres", DSN: "postgres://app@db/app", ReadOnly: true}
	if _, err := UpdateConnection(p, "local-pg", c, st); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	want := strings.Replace(seed, `  - name: local-pg
    driver: postgres
    dsn: postgres://app@localhost:5432/app
    tags: [prod]   # trailing comment
`, `  - name: pg2
    driver: postgres
    dsn: postgres://app@db/app
    read_only: true
`, 1)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	cfg, _ := Load(p)
	if _, ok := cfg.Find("local-pg"); ok {
		t.Fatal("old name still present")
	}
	if len(cfg.Connections) != 2 || cfg.Connections[0].Name != "pg2" {
		t.Fatalf("order not kept: %+v", cfg.Connections)
	}
}

func TestUpdateKeepsNameAndLast(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	c := Connection{Name: "cache", Driver: "redis", DSN: "redis://other:6379/1"}
	if _, err := UpdateConnection(p, "cache", c, st); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(seed, "redis://localhost:6379/0", "redis://other:6379/1", 1)
	if got := readFile(t, p); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestRemove(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	if _, err := RemoveConnection(p, "cache", st); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(seed, `
  - name: cache
    driver: redis
    dsn: redis://localhost:6379/0
`, "", 1)
	got := readFile(t, p)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	cfg, err := Load(p)
	if err != nil || len(cfg.Connections) != 1 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	// Removing the first entry keeps the comment above the list intact.
	_, st = loaded(t, p)
	if _, err := RemoveConnection(p, "local-pg", st); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(p)
	if err != nil || len(cfg.Connections) != 0 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if !strings.Contains(readFile(t, p), "# keep me") || !strings.Contains(readFile(t, p), "tui:") {
		t.Fatal("unrelated content lost")
	}
}

func TestSentinelErrors(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	if _, err := AddConnection(p, Connection{Name: "cache", Driver: "redis", DSN: "redis://x"}, st); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("add dup: %v", err)
	}
	if _, err := UpdateConnection(p, "cache", Connection{Name: "local-pg", Driver: "redis", DSN: "redis://x"}, st); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("rename onto existing: %v", err)
	}
	if _, err := UpdateConnection(p, "nope", webConn, st); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if _, err := RemoveConnection(p, "nope", st); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing: %v", err)
	}
	if readFile(t, p) != seed {
		t.Fatal("failed operations modified the file")
	}
}

func TestDuplicateName(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	_, err := AddConnection(p, Connection{Name: "local-pg", Driver: "postgres", DSN: "postgres://x"}, st)
	if !errors.Is(err, ErrDuplicate) || !strings.Contains(err.Error(), "local-pg") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateMissingFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "tobira")
	p := filepath.Join(dir, "config.yaml")
	cfg, st, err := LoadWithStat(p)
	if err != nil || len(cfg.Connections) != 0 || st.Exists {
		t.Fatalf("cfg=%+v st=%+v err=%v", cfg, st, err)
	}
	if _, err := AddConnection(p, Connection{Name: "a", Driver: "redis", DSN: "redis://h"}, st); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, p), "connections:\n  - name: a\n    driver: redis\n    dsn: redis://h\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
	// only connections: for an empty file, created by a remove-free path
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestConcurrentModification(t *testing.T) {
	for _, tt := range []struct {
		name  string
		touch func(t *testing.T, p string)
	}{
		{"content", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte(seed+"# hand edit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"mtime only", func(t *testing.T, p string) {
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(p, future, future); err != nil {
				t.Fatal(err)
			}
		}},
		{"deleted", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := writeTemp(t, seed)
			_, st := loaded(t, p)
			tt.touch(t, p)
			before, _ := os.ReadFile(p)
			for name, op := range map[string]func() error{
				"add":    func() error { _, err := AddConnection(p, webConn, st); return err },
				"update": func() error { _, err := UpdateConnection(p, "cache", webConn, st); return err },
				"remove": func() error { _, err := RemoveConnection(p, "cache", st); return err },
			} {
				if err := op(); !errors.Is(err, ErrConflict) {
					t.Fatalf("%s: err = %v, want ErrConflict", name, err)
				}
			}
			after, _ := os.ReadFile(p)
			if string(before) != string(after) {
				t.Fatal("file modified despite conflict")
			}
		})
	}
}

func TestSuccessiveWritesUseReturnedStat(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	st, err := AddConnection(p, webConn, st)
	if err != nil {
		t.Fatal(err)
	}
	c2 := Connection{Name: "web2", Driver: "redis", DSN: "redis://h"}
	if _, err := AddConnection(p, c2, st); err != nil {
		t.Fatalf("second write with returned stat: %v", err)
	}
}

func TestAtomicWrite(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	boom := errors.New("rename failed")
	w := writer{rename: func(string, string) error { return boom }}
	if _, err := w.add(p, webConn, st); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, p) != seed {
		t.Fatal("original file changed after failed rename")
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestWriteRejectsInvalid(t *testing.T) {
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	for name, c := range map[string]Connection{
		"bad name":   {Name: "a b", Driver: "redis", DSN: "redis://h"},
		"pw in dsn":  {Name: "a", Driver: "postgres", DSN: "postgres://u:pw@h/db"},
		"bad ref":    {Name: "a", Driver: "redis", DSN: "redis://h", Password: "op//x"},
		"no driver":  {Name: "a", DSN: "./x.db"},
		"empty name": {Driver: "redis", DSN: "redis://h"},
	} {
		if _, err := AddConnection(p, c, st); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if readFile(t, p) != seed {
		t.Fatal("file modified")
	}
}

func TestValidateRejectsPasswordInDSN(t *testing.T) {
	err := ValidateConnection(Connection{Name: "a", Driver: "postgres", DSN: "postgres://u:pw@h/db"}, testDrivers)
	if err == nil || !strings.Contains(err.Error(), "DSN contains a password; put it in a password ref (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME) instead") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "pw@") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestValidateConnection(t *testing.T) {
	tests := []struct {
		name    string
		c       Connection
		wantErr string
	}{
		{"ok", Connection{Name: "a.b_c-1", Driver: "postgres", DSN: "postgres://h/db", Password: "env:X", Color: "red", Tags: []string{"x"}}, ""},
		{"ok scheme only", Connection{Name: "a", DSN: "redis://h"}, ""},
		{"ok path dsn", Connection{Name: "a", Driver: "sqlite", DSN: "./x.db"}, ""},
		{"ok dotenv", Connection{Name: "a", Driver: "redis", DSN: "redis://h", Password: "dotenv://.env#KEY"}, ""},
		{"empty name", Connection{Driver: "redis", DSN: "redis://h"}, "name"},
		{"bad name", Connection{Name: "a/b", Driver: "redis", DSN: "redis://h"}, "name"},
		{"unknown driver", Connection{Name: "a", Driver: "nope", DSN: "x://h"}, "unknown driver"},
		{"unknown scheme", Connection{Name: "a", DSN: "nope://h"}, "no driver"},
		{"no driver no scheme", Connection{Name: "a", DSN: "./x.db"}, "driver"},
		{"scheme mismatch", Connection{Name: "a", Driver: "redis", DSN: "postgres://h/db"}, "does not match"},
		{"empty dsn", Connection{Name: "a", Driver: "redis"}, "DSN is required"},
		{"bad ref", Connection{Name: "a", Driver: "redis", DSN: "redis://h", Password: "op//x"}, "not a secret reference"},
		{"bad color", Connection{Name: "a", Driver: "redis", DSN: "redis://h", Color: "chartreuse"}, "color"},
		{"empty tag", Connection{Name: "a", Driver: "redis", DSN: "redis://h", Tags: []string{"ok", " "}}, "tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConnection(tt.c, testDrivers)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePasswordRefRequiresKnownScheme(t *testing.T) {
	for _, bad := range []string{"hunter:2", "My:Secret", "p@ss:w0rd", "env:OK,hunter:2", "plain", "op//x", "env:X\n"} {
		err := ValidatePasswordRef(bad)
		if err == nil || !strings.Contains(err.Error(), "not a secret reference: use env:NAME, file:///path, dotenv://path#KEY or keyring:NAME") {
			t.Errorf("%q: err = %v", bad, err)
		}
		if err != nil && strings.Contains(err.Error(), "hunter") {
			t.Errorf("%q: error echoes the value", bad)
		}
	}
	for _, ok := range []string{"", "env:PW", "file:///run/secret", "dotenv://.env#KEY", "env:A,file:///p"} {
		if err := ValidatePasswordRef(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	p := writeTemp(t, seed)
	_, st := loaded(t, p)
	c := Connection{Name: "a", Driver: "redis", DSN: "redis://h", Password: "hunter:2"}
	if _, err := AddConnection(p, c, st); err == nil || readFile(t, p) != seed {
		t.Fatalf("literal password written: %v", err)
	}
}

func TestValidateRejectsSecretOptionsAndControls(t *testing.T) {
	base := Connection{Name: "a", Driver: "redis", DSN: "redis://h"}
	mut := map[string]func(c *Connection){
		"option password": func(c *Connection) { c.Options = map[string]string{"sslPassword": "x"} },
		"option token":    func(c *Connection) { c.Options = map[string]string{"Auth_Token": "x"} },
		"option secret":   func(c *Connection) { c.Options = map[string]string{"client-secret": "x"} },
		"option pwd":      func(c *Connection) { c.Options = map[string]string{"pwd": "x"} },
		"option control":  func(c *Connection) { c.Options = map[string]string{"k": "a\x00b"} },
		"dsn newline":     func(c *Connection) { c.DSN = "redis://h\nx: y" },
		"dsn tab":         func(c *Connection) { c.DSN = "redis://h\t" },
		"dsn del":         func(c *Connection) { c.DSN = "redis://h\x7f" },
		"dsn c1":          func(c *Connection) { c.DSN = "redis://h\u0085" },
		"tag newline":     func(c *Connection) { c.Tags = []string{"a\nb"} },
		"name newline":    func(c *Connection) { c.Name = "a\n" },
		"ref nul":         func(c *Connection) { c.Password = "env:A\x00" },
	}
	for name, f := range mut {
		c := base
		f(&c)
		if err := ValidateConnection(c, testDrivers); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	c := base
	c.Options = map[string]string{"sslmode": "require", "busy_timeout": "5000"}
	if err := ValidateConnection(c, testDrivers); err != nil {
		t.Fatal(err)
	}
}

func TestWriteThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "link.yaml")
	if err := os.WriteFile(target, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, st := loaded(t, link)
	if _, err := AddConnection(link, webConn, st); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if !strings.Contains(readFile(t, target), "name: web") {
		t.Fatal("target not updated")
	}
}

func TestCRLFPreserved(t *testing.T) {
	crlf := strings.ReplaceAll(seed, "\n", "\r\n")
	p := writeTemp(t, crlf)
	_, st := loaded(t, p)
	if _, err := AddConnection(p, webConn, st); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("bare LF in CRLF file: %q", got)
	}
	if without := strings.Replace(got, strings.ReplaceAll(webEntry, "\n", "\r\n"), "", 1); without != crlf {
		t.Fatal("original bytes not preserved around the new entry")
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsPwdKeyInDSN(t *testing.T) {
	for _, dsn := range []string{"host=h pwd=hunter2 dbname=d", "host=h passwd=x", "host=h pass=x"} {
		err := ValidateConnection(Connection{Name: "a", Driver: "postgres", DSN: dsn}, testDrivers)
		if err == nil || !strings.Contains(err.Error(), "DSN contains a password") {
			t.Errorf("%q: err = %v", dsn, err)
		}
	}
}

func TestUpdateKeepsUnchangedHandWrittenValues(t *testing.T) {
	hand := "connections:\n  - name: a\n    driver: redis\n    dsn: redis://h:6379/0\n    password: op://vault/item\n    color: teal\n"
	p := writeTemp(t, hand)
	_, st := loaded(t, p)
	c := Connection{Name: "a", Driver: "redis", DSN: "redis://h:6379/1", Password: "op://vault/item", Color: "teal"}
	if _, err := UpdateConnection(p, "a", c, st); err != nil {
		t.Fatalf("unchanged URL-form ref and colour refused: %v", err)
	}
	_, st = loaded(t, p)
	c.Password = "aws-sm://other"
	if _, err := UpdateConnection(p, "a", c, st); err == nil {
		t.Fatal("changed unregistered ref accepted")
	}
	c.Password, c.Color = "op://vault/item", "chartreuse"
	if _, err := UpdateConnection(p, "a", c, st); err == nil {
		t.Fatal("changed unknown colour accepted")
	}
}

func TestValidateDSNRejectsPlaceholderPassword(t *testing.T) {
	err := ValidateDSN("postgres://u:xxxxx@h/db", "postgres", testDrivers)
	if err == nil || !strings.Contains(err.Error(), "DSN contains a password") {
		t.Fatalf("err = %v", err)
	}
}
