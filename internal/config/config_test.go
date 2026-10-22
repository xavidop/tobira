package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPathPrecedence(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name string
		flag string
		env  map[string]string
		want string
	}{
		{"flag wins", "/f.yaml", map[string]string{"TOBIRA_CONFIG": "/e.yaml", "XDG_CONFIG_HOME": "/x"}, "/f.yaml"},
		{"env beats xdg", "", map[string]string{"TOBIRA_CONFIG": "/e.yaml", "XDG_CONFIG_HOME": "/x"}, "/e.yaml"},
		{"xdg beats home", "", map[string]string{"XDG_CONFIG_HOME": "/x"}, "/x/tobira/config.yaml"},
		{"home default", "", nil, "/h/.config/tobira/config.yaml"},
		{"tilde in flag", "~/c.yaml", nil, "/h/c.yaml"},
		{"tilde in env", "", map[string]string{"TOBIRA_CONFIG": "~/e.yaml"}, "/h/e.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Path(tt.flag, env(tt.env), "/h"); got != tt.want {
				t.Fatalf("Path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadFullBriefExample(t *testing.T) {
	c, err := Load(filepath.Join("testdata", "brief.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Connection{
		{Name: "local-pg", Driver: "postgres", DSN: "postgres://app@localhost:5432/app",
			Password: "op://dev/postgres/password", Color: "blue", ReadOnly: true, Tags: []string{"prod"}},
		{Name: "cache", Driver: "redis", DSN: "redis://localhost:6379/0"},
	}
	if !reflect.DeepEqual(c.Connections, want) {
		t.Fatalf("connections = %+v, want %+v", c.Connections, want)
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring of the broken entry's Err; "" means no broken entry
	}{
		{"ok driver only", "connections:\n  - name: a\n    driver: sqlite\n    dsn: ./x.db\n", ""},
		{"ok scheme only", "connections:\n  - name: a\n    dsn: postgres://h/db\n", ""},
		{"ok password ref", "connections:\n  - name: a\n    driver: x\n    password: env:PW\n", ""},
		{"duplicate name", "connections:\n  - {name: a, driver: x}\n  - {name: a, driver: y}\n", "duplicate"},
		{"missing name", "connections:\n  - driver: x\n", "name"},
		{"no driver no scheme", "connections:\n  - name: a\n    dsn: ./x.db\n", "driver"},
		{"bad password ref", "connections:\n  - name: a\n    driver: x\n    password: op//x\n", "password"},
		{"control in name", "connections:\n  - name: \"a\\u0085b\"\n    driver: x\n", "control characters"},
		{"control in tag", "connections:\n  - name: a\n    driver: x\n    tags: [\"t\\u009bx\"]\n", "control characters"},
		{"control in dsn", "connections:\n  - name: a\n    driver: x\n    dsn: \"x\\u0085y\"\n", "control characters"},
		{"control in driver", "connections:\n  - name: a\n    driver: \"x\\u0085\"\n", "control characters"},
		{"control in color", "connections:\n  - name: a\n    driver: x\n    color: \"r\\u009bed\"\n", "control characters"},
		{"typo key", "connections:\n  - name: a\n    driver: x\n    pasword: s3cret\n", "did you mean"},
		{"not a mapping", "connections:\n  - just-a-string\n", "mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeTemp(t, tt.yaml))
			if err != nil {
				t.Fatalf("a bad entry must not fail the load: %v", err)
			}
			if tt.wantErr == "" {
				if len(cfg.Broken) != 0 {
					t.Fatalf("unexpected broken entries: %+v", cfg.Broken)
				}
				return
			}
			if len(cfg.Broken) != 1 || !strings.Contains(cfg.Broken[0].Err, tt.wantErr) {
				t.Fatalf("Broken = %+v, want one entry containing %q", cfg.Broken, tt.wantErr)
			}
		})
	}
	if _, err := Load(writeTemp(t, "connections: [\n")); err == nil {
		t.Fatal("bad yaml must stay fatal")
	}
}

func TestBrokenErrNeverEchoesPassword(t *testing.T) {
	cfg, err := Load(writeTemp(t, "connections:\n  - name: a\n    driver: x\n    password: \"op//SUPERSECRET\"\n"))
	if err != nil || len(cfg.Broken) != 1 || strings.Contains(cfg.Broken[0].Err, "SUPERSECRET") {
		t.Fatalf("cfg = %+v, err = %v; want one broken entry that does not echo the password", cfg, err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || c == nil || len(c.Connections) != 0 {
		t.Fatalf("Load missing = %+v, %v; want empty config, nil", c, err)
	}
}

func TestLoadUnreadable(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected error reading a directory")
	}
}

func TestFind(t *testing.T) {
	c := &Config{Connections: []Connection{{Name: "a"}, {Name: "b", Driver: "x"}}}
	if got, ok := c.Find("b"); !ok || got.Driver != "x" {
		t.Fatalf("Find(b) = %+v, %v", got, ok)
	}
	if _, ok := c.Find("zz"); ok {
		t.Fatal("Find(zz) found")
	}
}

func TestSuggest(t *testing.T) {
	c := &Config{Connections: []Connection{{Name: "local-pg"}, {Name: "cache"}}}
	tests := []struct{ in, want string }{
		{"local-pgg", "local-pg"},
		{"zzz", ""},
		{"cach", "cache"},
	}
	for _, tt := range tests {
		if got := c.Suggest(tt.in); got != tt.want {
			t.Errorf("Suggest(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := (&Config{}).Suggest("a"); got != "" {
		t.Errorf("empty config Suggest = %q", got)
	}
	tie := &Config{Connections: []Connection{{Name: "ab"}, {Name: "ac"}}}
	if got := tie.Suggest("ad"); got != "ab" {
		t.Errorf("tie Suggest = %q, want ab", got)
	}
}

func TestBrokenLines(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
		line                int
	}{
		{"typo key", "connections:\n  - name: a\n    driver: x\n    pasword: s3cret\n", `unknown connection key (did you mean "password"?)`, 2},
		{"no driver", "connections:\n  - name: a\n    driver: x\n  - name: b\n    dsn: ./x.db\n", "set driver or use a DSN with a scheme", 4},
		{"missing name", "connections:\n  - driver: x\n", "name is required", 2},
		{"duplicate", "connections:\n  - {name: a, driver: x}\n  - {name: a, driver: y}\n", "duplicate name", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeTemp(t, tt.yaml))
			if err != nil || len(cfg.Broken) != 1 {
				t.Fatalf("cfg = %+v, err = %v", cfg, err)
			}
			if b := cfg.Broken[0]; b.Line != tt.line || b.Err != tt.wantErr {
				t.Fatalf("Broken = %+v, want line %d, %q", b, tt.line, tt.wantErr)
			}
		})
	}
}

func TestLoadTUI(t *testing.T) {
	c, err := Load(filepath.Join("testdata", "tui.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (TUI{Theme: "tobira-light", Keymap: "vim"}); c.TUI != want {
		t.Fatalf("TUI = %+v, want %+v", c.TUI, want)
	}
}

func TestTUIDefaults(t *testing.T) {
	want := TUI{Theme: "auto", Keymap: "default"}
	for name, body := range map[string]string{
		"missing section": "connections: []\n",
		"partial":         "tui:\n  keymap: default\n",
		"null section":    "tui:\n",
		"empty":           "",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(writeTemp(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if c.TUI != want {
				t.Fatalf("TUI = %+v, want %+v", c.TUI, want)
			}
		})
	}
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || c.TUI != want {
		t.Fatalf("missing file: TUI = %+v, err = %v", c.TUI, err)
	}
}

func TestTUIInvalid(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
		line                string
	}{
		{"bad theme", "connections: []\ntui:\n  theme: neon\n", "neon", "line 3"},
		{"bad keymap", "tui:\n  keymap: emacs\n", "emacs", "line 2"},
		{"unknown key", "tui:\n  theme: auto\n  colour: x\n", "colour", "line 3"},
		{"not a mapping", "tui: vim\n", "mapping", "line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeTemp(t, tt.yaml)
			_, err := Load(p)
			if err == nil {
				t.Fatal("want error")
			}
			msg := err.Error()
			for _, s := range []string{p, tt.line, tt.wantErr} {
				if !strings.Contains(msg, s) {
					t.Errorf("error %q missing %q", msg, s)
				}
			}
			if strings.Count(msg, p) != 1 || strings.Count(msg, "line ") != 1 {
				t.Errorf("error %q should name path and line once", msg)
			}
		})
	}
}

func TestLoadRejectsLiteralPassword(t *testing.T) {
	// "hunter:2" parses as a ref shape ("x:y") but no provider owns "hunter".
	p := writeTemp(t, "connections:\n  - name: x\n    driver: sqlite\n    dsn: sqlite:a.db\n    password: hunter:2\n")
	cfg, err := Load(p)
	if err != nil || len(cfg.Broken) != 1 || cfg.Broken[0].Name != "x" ||
		strings.Contains(cfg.Broken[0].Err, "hunter") || !strings.Contains(cfg.Broken[0].Err, "password must be a secret reference") {
		t.Fatalf("cfg = %+v, err = %v; want one broken entry that does not echo the value", cfg, err)
	}
}

func TestLoadPasswordRefForms(t *testing.T) {
	tests := []struct {
		pw string
		ok bool
	}{
		{"op://dev/pg/pw", true},
		{"env:A,op://x/y", true},
		{"hunter:2", false},
		{"env:A,hunter:2", false},
	}
	for _, tt := range tests {
		t.Run(tt.pw, func(t *testing.T) {
			p := writeTemp(t, "connections:\n  - name: x\n    driver: sqlite\n    dsn: sqlite:a.db\n    password: \""+tt.pw+"\"\n")
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.ok && len(cfg.Broken) != 0 {
				t.Fatalf("Broken = %+v", cfg.Broken)
			}
			if !tt.ok && (len(cfg.Broken) != 1 || strings.Contains(cfg.Broken[0].Err, "hunter:2")) {
				t.Fatalf("Broken = %+v, want one entry without the value", cfg.Broken)
			}
		})
	}
}
