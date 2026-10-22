package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAddConnectionsWritesOnceKeepingComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, st := loaded(t, path)
	a := Connection{Name: "a", Driver: "postgres", DSN: "postgres://u@h/a", Password: "env:A_PW"}
	b := Connection{Name: "b", Driver: "sqlite", DSN: "sqlite:./b.db", Tags: []string{"x"}}
	if _, err := AddConnections(path, []Connection{a, b}, st); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	for _, keep := range []string{"# tobira config", "# the local one", "tags: [prod]   # trailing comment", "provider: googleai   # comment"} {
		if !strings.Contains(string(got), keep) {
			t.Errorf("lost %q:\n%s", keep, got)
		}
	}
	cfg, _ := loaded(t, path)
	if len(cfg.Connections) != 4 {
		t.Fatalf("connections = %d", len(cfg.Connections))
	}
	if g, _ := cfg.Find("a"); !reflect.DeepEqual(g, a) {
		t.Errorf("a = %+v", g)
	}
	if g, _ := cfg.Find("b"); !reflect.DeepEqual(g, b) {
		t.Errorf("b = %+v", g)
	}
}

func TestAddConnectionsRefusesConflictDuplicateAndBadEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, st := loaded(t, path)
	ok := Connection{Name: "n", Driver: "sqlite", DSN: "sqlite:x"}
	cases := map[string][]Connection{
		"existing name": {ok, {Name: "cache", Driver: "redis", DSN: "redis://h"}},
		"batch dup":     {ok, ok},
		"dsn password":  {{Name: "p", Driver: "postgres", DSN: "postgres://u:hunter22@h/db"}},
		"ado password":  {{Name: "p", Driver: "postgres", DSN: "Server=h;User Id=u;Password=hunter22;"}},
		"auth_pass":     {{Name: "p", Driver: "sqlite", DSN: "file:p.db?_auth_pass=hunter22"}},
		"literal pw":    {{Name: "p", Driver: "postgres", DSN: "postgres://u@h/db", Password: "hunter22"}},
		"bad name":      {{Name: "a b", Driver: "sqlite", DSN: "sqlite:x"}},
	}
	for name, conns := range cases {
		t.Run(name, func(t *testing.T) {
			before, _ := os.ReadFile(path)
			_, err := AddConnections(path, conns, st)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "hunter22") {
				t.Errorf("error leaks the password: %v", err)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Error("file changed")
			}
		})
	}
	// A file changed since the Stat was taken is a conflict.
	stale := st
	stale.Size--
	if _, err := AddConnections(path, []Connection{ok}, stale); !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestAddConnectionsKeepsLoadedRefsAndCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	st, _ := StatOf(path)
	// A URL-form ref of a provider that is not compiled in, as Load accepts.
	c := Connection{Name: "v", Driver: "postgres", DSN: "postgres://u@h/db", Password: "op://vault/item/pw"}
	if _, err := AddConnections(path, []Connection{c}, st); err != nil {
		t.Fatal(err)
	}
	cfg, _ := loaded(t, path)
	if g, _ := cfg.Find("v"); g.Password != c.Password {
		t.Errorf("password ref = %q", g.Password)
	}
}

func TestParseConnectionList(t *testing.T) {
	data := `connections:
  - name: a
    driver: sqlite
    dsn: sqlite:a.db
  - name: b
    driver: postgres
    dsn: postgres://u@h/db
    password: hunter22
`
	entries, err := ParseConnectionList([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Broken != nil || entries[0].Conn.Name != "a" {
		t.Fatalf("entries = %+v", entries)
	}
	if b := entries[1].Broken; b == nil || b.Name != "b" || b.Line != 5 || strings.Contains(b.Err, "hunter22") {
		t.Errorf("broken = %+v", b)
	}
	// A bare list and JSON parse too.
	for _, in := range []string{
		"- name: a\n  driver: sqlite\n  dsn: sqlite:a.db\n",
		`{"connections":[{"name":"a","driver":"sqlite","dsn":"sqlite:a.db"}]}`,
	} {
		entries, err := ParseConnectionList([]byte(in))
		if err != nil || len(entries) != 1 || entries[0].Broken != nil {
			t.Errorf("%q: entries=%+v err=%v", in, entries, err)
		}
	}
	for _, bad := range []string{"connections: [\n", "connections: 3\n", "just text\n"} {
		if _, err := ParseConnectionList([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestMarshalConnectionsRoundTrips(t *testing.T) {
	conns := []Connection{
		{Name: "a", Driver: "postgres", DSN: "postgres://u@h/db", Password: "env:PW", Color: "red",
			ReadOnly: true, Tags: []string{"x", "y"}, Options: map[string]string{"sslmode": "require"}},
		{Name: "b", DSN: "sqlite:b.db"},
	}
	out, err := MarshalConnections(conns)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "connections:\n  - name: a\n") {
		t.Errorf("out:\n%s", out)
	}
	entries, err := ParseConnectionList(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []Connection
	for _, e := range entries {
		if e.Broken != nil {
			t.Fatalf("broken: %+v", e.Broken)
		}
		got = append(got, e.Conn)
	}
	if !reflect.DeepEqual(got, conns) {
		t.Errorf("got %+v\nwant %+v", got, conns)
	}
	empty, _ := MarshalConnections(nil)
	if string(empty) != "connections: []\n" {
		t.Errorf("empty = %q", empty)
	}
}
