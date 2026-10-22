package config

import (
	"errors"
	"strings"
	"testing"
)

const dupSeed = `connections:
  - name: x
    driver: redis
    dsn: redis://h/0
  - name: x
    driver: redis
    dsn: redis://h/1
  - driver: redis
    dsn: redis://h/2
  - name: ok
    driver: redis
    read_only: abc
    dsn: redis://h/3
`

func TestOccurrencesAndFindAt(t *testing.T) {
	cfg, _ := loaded(t, writeTemp(t, dupSeed))
	if got := cfg.Occurrences("x"); len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Fatalf("Occurrences(x) = %v", got)
	}
	if c, ok := cfg.FindAt(2); !ok || c.DSN != "redis://h/0" {
		t.Fatalf("FindAt(2) = %+v %v", c, ok)
	}
	if b, ok := cfg.FindBrokenAt(5); !ok || b.Err != "duplicate name" {
		t.Fatalf("FindBrokenAt(5) = %+v %v", b, ok)
	}
}

func TestRemoveAndUpdateAtTargetTheLine(t *testing.T) {
	p := writeTemp(t, dupSeed)
	_, st := loaded(t, p)
	st, err := RemoveConnectionAt(p, 5, st)
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if !strings.Contains(got, "redis://h/0") || strings.Contains(got, "redis://h/1") {
		t.Fatalf("removed the wrong entry:\n%s", got)
	}
	// Unnamed entry now at line 5: give it a name.
	if _, err := UpdateConnectionAt(p, 5, Connection{Name: "named", Driver: "redis", DSN: "redis://h/2"}, st); err != nil {
		t.Fatal(err)
	}
	cfg, _ := loaded(t, p)
	if c, ok := cfg.Find("named"); !ok || c.DSN != "redis://h/2" {
		t.Fatalf("named = %+v %v", c, ok)
	}
}

func TestUpdateAtRefusesToStayDuplicate(t *testing.T) {
	p := writeTemp(t, dupSeed)
	_, st := loaded(t, p)
	_, err := UpdateConnectionAt(p, 5, Connection{Name: "x", Driver: "redis", DSN: "redis://h/1"}, st)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestWriteAtUnknownLine(t *testing.T) {
	p := writeTemp(t, dupSeed)
	_, st := loaded(t, p)
	if _, err := RemoveConnectionAt(p, 3, st); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestByNameRefusesAmbiguity(t *testing.T) {
	p := writeTemp(t, dupSeed)
	_, st := loaded(t, p)
	before := readFile(t, p)
	_, err := RemoveConnection(p, "x", st)
	if !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "lines 2, 5") {
		t.Fatalf("remove err = %v", err)
	}
	_, err = UpdateConnection(p, "x", Connection{Name: "x", Driver: "redis", DSN: "redis://h/9"}, st)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("update err = %v", err)
	}
	if readFile(t, p) != before {
		t.Fatal("file changed")
	}
}

func TestWrongTypedFieldKeepsOtherFields(t *testing.T) {
	cfg, _ := loaded(t, writeTemp(t, dupSeed))
	b, ok := cfg.FindBroken("ok")
	if !ok {
		t.Fatal("ok not broken")
	}
	if b.Err != `field "read_only" has the wrong type` {
		t.Errorf("Err = %q", b.Err)
	}
	want := Connection{Name: "ok", Driver: "redis", DSN: "redis://h/3"}
	if b.Partial.Name != want.Name || b.Partial.Driver != want.Driver || b.Partial.DSN != want.DSN || b.Partial.ReadOnly {
		t.Errorf("Partial = %+v", b.Partial)
	}
}

func TestUnknownKeyIsNeverQuoted(t *testing.T) {
	for name, body := range map[string]string{
		"flow secret key": "connections:\n  - {name: a, driver: x, password:hunter2SECRET}\n",
		"bare secret key": "connections:\n  - name: a\n    driver: x\n    hunter2SECRET: y\n",
		"typo":            "connections:\n  - name: a\n    driver: x\n    pasword: y\n",
	} {
		cfg, err := Load(writeTemp(t, body))
		if err != nil || len(cfg.Broken) != 1 {
			t.Fatalf("%s: %+v, %v", name, cfg, err)
		}
		if e := cfg.Broken[0].Err; strings.Contains(e, "SECRET") || strings.Contains(e, "hunter") || strings.Contains(e, "pasword") {
			t.Errorf("%s: Err quotes the key: %q", name, e)
		}
	}
	cfg, _ := Load(writeTemp(t, "connections:\n  - {name: a, driver: x, password:hunter2SECRET}\n  - {name: b, driver: x, hunter2SECRET: 1}\n"))
	if cfg.Broken[0].Err != `unknown connection key (did you mean "password"?)` || cfg.Broken[1].Err != "unknown connection key" {
		t.Errorf("Errs = %q, %q", cfg.Broken[0].Err, cfg.Broken[1].Err)
	}
}

func TestPasswordChainsCheckEveryElement(t *testing.T) {
	for _, ref := range []string{"env:A,SECRET", "env:A, SECRET", "SECRET", "env:A,hunter:2"} {
		for _, f := range []func(string) error{ValidatePasswordRef, ValidateLoadedPasswordRef} {
			err := f(ref)
			if err == nil {
				t.Errorf("%q accepted", ref)
			} else if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "hunter") {
				t.Errorf("error echoes the value: %v", err)
			}
		}
	}
	for _, ref := range []string{"env:A,env:B", "env:A,file:///p", "file:///tmp/a,b", "dotenv:///tmp/.env#K,L"} {
		if err := ValidatePasswordRef(ref); err != nil {
			t.Errorf("%q: %v", ref, err)
		}
	}
	for _, ref := range []string{"env:A,op://v/i/f", "op://v/i,x/f"} {
		if err := ValidateLoadedPasswordRef(ref); err != nil {
			t.Errorf("%q: %v", ref, err)
		}
	}
	cfg, err := Load(writeTemp(t, "connections:\n  - {name: a, driver: x, password: \"env:A,SECRET\"}\n"))
	if err != nil || len(cfg.Broken) != 1 || strings.Contains(cfg.Broken[0].Err, "SECRET") {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestBlockScalarHashLinesAreContent(t *testing.T) {
	body := "connections:\n  - name: a\n    driver: x\n    dsn: |\n      fake://h\n      # inside the scalar\n  - name: b\n    driver: y\n"
	p := writeTemp(t, body)
	_, st := loaded(t, p)
	if _, err := RemoveConnection(p, "a", st); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, p), "connections:\n  - name: b\n    driver: y\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestUpdateKeepsUnchangedValuesOfBrokenEntry(t *testing.T) {
	// The entry is broken by its name-less DSN rule, yet carries a colour
	// outside the list and a URL-form ref that only the loader accepts.
	body := "connections:\n  - name: a\n    dsn: ./x.db\n    color: teal\n    password: \"op://v/i/f\"\n"
	p := writeTemp(t, body)
	cfg, st := loaded(t, p)
	if len(cfg.Broken) != 1 {
		t.Fatalf("setup: %+v", cfg.Broken)
	}
	fixed := Connection{Name: "a", Driver: "sqlite", DSN: "./x.db", Color: "teal", Password: "op://v/i/f"}
	if _, err := UpdateConnectionAt(p, cfg.Broken[0].Line, fixed, st); err != nil {
		t.Fatalf("unchanged hand-written values refused: %v", err)
	}
}

func TestMultiLineScalarHashLinesAreContent(t *testing.T) {
	for name, scalar := range map[string]string{
		"folded":        "    dsn: >\n      fake://h\n      # tail\n",
		"literal":       "    dsn: |\n      fake://h\n      # tail\n",
		"double quoted": "    dsn: \"fake://h\\\"x\n      # tail\"\n",
		"single quoted": "    dsn: 'fake://h''x\n      # tail'\n",
	} {
		body := "connections:\n  - name: a\n    driver: x\n" + scalar + "  - name: b\n    driver: y\n"
		p := writeTemp(t, body)
		_, st := loaded(t, p)
		if _, err := RemoveConnection(p, "a", st); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, want := readFile(t, p), "connections:\n  - name: b\n    driver: y\n"; got != want {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
}
