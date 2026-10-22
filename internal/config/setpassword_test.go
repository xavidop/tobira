package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pwSeed = `# top comment
connections:
  # first
  - name: good
    driver: postgres
    dsn: postgres://u@h/db   # trailing
    # inside comment
    password: hunter2
    mystery_key: kept

  # second, broken: no driver and a DSN without a scheme
  - name: broken
    dsn: no-scheme-here
    # inside broken
    tags: [a]
    unknown_field: 42
  - name: last
    driver: redis
    dsn: redis://h/0
`

func writeSeed(t *testing.T, text string) (string, Stat) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := StatOf(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, st
}

func TestSetPasswordRefReplacesOnlyThePasswordLine(t *testing.T) {
	p, st := writeSeed(t, pwSeed)
	if _, err := SetPasswordRefAt(p, 4, "env:PW", st); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := strings.Replace(pwSeed, "password: hunter2", "password: env:PW", 1)
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetPasswordRefOnBrokenEntryInsertsALine(t *testing.T) {
	p, st := writeSeed(t, pwSeed)
	if _, err := SetPasswordRefAt(p, 12, "env:PW", st); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := strings.Replace(pwSeed, "    unknown_field: 42\n", "    unknown_field: 42\n    password: env:PW\n", 1)
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	cfg, _ := loaded(t, p)
	if b, ok := cfg.FindBroken("broken"); !ok || b.Partial.Password != "env:PW" {
		t.Errorf("broken = %+v ok=%v", b, ok)
	}
}

func TestSetPasswordRefInsertKeepsFirstKeyOnDashLine(t *testing.T) {
	p, st := writeSeed(t, "connections:\n  - driver: redis\n    dsn: redis://h\n\n# tail\n")
	if _, err := SetPasswordRefAt(p, 2, "env:PW", st); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if want := "connections:\n  - driver: redis\n    dsn: redis://h\n    password: env:PW\n\n# tail\n"; string(got) != want {
		t.Errorf("got:\n%s", got)
	}
}

func TestSetPasswordRefValidatesOnlyTheRef(t *testing.T) {
	p, st := writeSeed(t, pwSeed)
	for _, ref := range []string{"", "hunter2", "nope:thing", "env:"} {
		if _, err := SetPasswordRefAt(p, 12, ref, st); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
	}
	if got, _ := os.ReadFile(p); string(got) != pwSeed {
		t.Error("file changed on a rejected ref")
	}
}

func TestSetPasswordRefConflictAndNotFound(t *testing.T) {
	p, st := writeSeed(t, pwSeed)
	if _, err := SetPasswordRefAt(p, 99, "env:PW", st); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if err := os.WriteFile(p, []byte(pwSeed+"# more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPasswordRefAt(p, 4, "env:PW", st); !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v", err)
	}
}

func setPW(t *testing.T, text string, line int) (string, error) {
	t.Helper()
	p, st := writeSeed(t, text)
	_, err := SetPasswordRefAt(p, line, "env:PW", st)
	got, _ := os.ReadFile(p)
	return string(got), err
}

func TestSetPasswordRefQuotedKeys(t *testing.T) {
	for _, key := range []string{`"password"`, `'password'`} {
		for _, val := range []string{`'foo'`, `x`, `"y z"`} {
			in := "connections:\n  - name: a\n    driver: redis\n    " + key + ": " + val + "\n    dsn: redis://h\n"
			got, err := setPW(t, in, 2)
			want := "connections:\n  - name: a\n    driver: redis\n    password: env:PW\n    dsn: redis://h\n"
			if err != nil || got != want {
				t.Errorf("%s: %s: err=%v got:\n%s", key, val, err, got)
			}
		}
	}
}

func TestSetPasswordRefQuotedKeyOnDashLine(t *testing.T) {
	got, err := setPW(t, "connections:\n  - \"password\": x\n    driver: redis\n    dsn: redis://h\n", 2)
	if err != nil || got != "connections:\n  - password: env:PW\n    driver: redis\n    dsn: redis://h\n" {
		t.Errorf("err=%v got:\n%s", err, got)
	}
}

func TestSetPasswordRefKeepsTrailingComment(t *testing.T) {
	for in, want := range map[string]string{
		"password: foo   # note":       "password: env:PW   # note",
		"password: 'a # b'  # note":    "password: env:PW  # note",
		"password: \"a\\\" #\" # note": "password: env:PW # note",
		"password: 'a # b'":            "password: env:PW",
		"password: foo#notcomment":     "password: env:PW",
	} {
		text := "connections:\n  - name: a\n    driver: redis\n    dsn: redis://h\n    " + in + "\n"
		got, err := setPW(t, text, 2)
		if w := "connections:\n  - name: a\n    driver: redis\n    dsn: redis://h\n    " + want + "\n"; err != nil || got != w {
			t.Errorf("%q: err=%v got:\n%s", in, err, got)
		}
	}
}

func TestSetPasswordRefInsertsBeforeTrailingComments(t *testing.T) {
	in := "connections:\n  - name: a\n    driver: redis\n    dsn: redis://h\n    # about the next one\n  - name: b\n    driver: redis\n    dsn: redis://i\n"
	got, err := setPW(t, in, 2)
	want := "connections:\n  - name: a\n    driver: redis\n    dsn: redis://h\n    password: env:PW\n    # about the next one\n  - name: b\n    driver: redis\n    dsn: redis://i\n"
	if err != nil || got != want {
		t.Errorf("err=%v got:\n%s", err, got)
	}
}

func TestSetPasswordRefBareDashEntryIsUnsupported(t *testing.T) {
	in := "connections:\n  -\n    name: a\n    driver: redis\n    dsn: redis://h\n"
	for _, line := range []int{2, 3} {
		got, err := setPW(t, in, line)
		if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not supported") || got != in {
			t.Errorf("line %d: err=%v changed=%v", line, err, got != in)
		}
	}
}

func TestSetPasswordRefFlowEntryIsRefused(t *testing.T) {
	in := "connections:\n  - {name: a, driver: redis, dsn: 'redis://h'}\n"
	got, err := setPW(t, in, 2)
	if err == nil || !strings.Contains(err.Error(), "not supported") || got != in {
		t.Errorf("err=%v got:\n%s", err, got)
	}
}

func TestSetPasswordRefPreservesCRLF(t *testing.T) {
	in := "connections:\r\n  - name: a\r\n    driver: redis\r\n    dsn: redis://h\r\n    password: old # n\r\n  - name: b\r\n    driver: redis\r\n    dsn: redis://i\r\n"
	got, err := setPW(t, in, 2)
	want := strings.Replace(in, "password: old # n", "password: env:PW # n", 1)
	if err != nil || got != want {
		t.Errorf("err=%v got:\n%q", err, got)
	}
	got, err = setPW(t, strings.Replace(in, "    password: old # n\r\n", "", 1), 2)
	if err != nil || !strings.Contains(got, "    dsn: redis://h\r\n    password: env:PW\r\n  - name: b") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("insert: err=%v got:\n%q", err, got)
	}
}

func TestSetPasswordRefRefusesDuplicateKeys(t *testing.T) {
	// A second password key the line scan cannot reach (an explicit key).
	in := "connections:\n  - name: a\n    driver: redis\n    dsn: redis://h\n    ? password\n    : x\n"
	got, err := setPW(t, in, 2)
	if err == nil && strings.Count(got, "password") > 1 && !strings.Contains(got, "password: env:PW\n") {
		t.Errorf("duplicate written:\n%s", got)
	}
	if err == nil {
		t.Errorf("accepted an entry with an unreachable password key:\n%s", got)
	}
}
