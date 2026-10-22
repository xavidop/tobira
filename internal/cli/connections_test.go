package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/tui/connform"
)

func TestConnectionsAddFlags(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "connections", "add",
		"--name", "newone", "--driver", "fake", "--dsn", "fake://h/db",
		"--password-ref", "env:NEWONE_PW", "--read-only", "--color", "red", "--tag", "a", "--tag", "b")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	cfg, err := config.Load(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := cfg.Find("newone")
	if !ok || c.Driver != "fake" || c.DSN != "fake://h/db" || c.Password != "env:NEWONE_PW" ||
		!c.ReadOnly || c.Color != "red" || len(c.Tags) != 2 {
		t.Errorf("saved = %+v ok=%v", c, ok)
	}
	if len(cfg.Connections) != 4 {
		t.Errorf("connections = %d, want the 3 original plus 1", len(cfg.Connections))
	}
}

func TestConnectionsAddRejections(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"duplicate", []string{"--name", "ro", "--driver", "fake", "--dsn", "fake://x"}, "already exists"},
		{"password in dsn", []string{"--name", "n", "--driver", "fake", "--dsn", "fake://u:hunter2pw@h/db"}, "password"},
		{"bad driver", []string{"--name", "n", "--driver", "zzz", "--dsn", "zzz://h"}, "driver"},
		{"bad name", []string{"--name", "a b", "--driver", "fake", "--dsn", "fake://h"}, "name"},
		{"literal password", []string{"--name", "n", "--driver", "fake", "--dsn", "fake://h", "--password-ref", "hunter2pw"}, "secret reference"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			before, _ := os.ReadFile(h.cfg)
			_, errOut, code := h.exec(t, context.Background(), append([]string{"connections", "add"}, tc.args...)...)
			if code != 1 || !strings.Contains(errOut, tc.want) || strings.Contains(errOut, "hunter2pw") {
				t.Errorf("code=%d stderr=%q, want exit 1 mentioning %q and no secret", code, errOut, tc.want)
			}
			if after, _ := os.ReadFile(h.cfg); string(after) != string(before) {
				t.Error("config changed on a rejected add")
			}
		})
	}
}

func TestConnectionsAddNoTTYWithoutFlags(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "connections", "add")
	if code != 1 || !strings.Contains(errOut, "--name") || !strings.Contains(errOut, "--dsn") {
		t.Errorf("code=%d stderr=%q, want usage error pointing at the flags", code, errOut)
	}
}

func TestConnectionsAddInteractive(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.runForm = func(_ context.Context, m connform.Model) (config.Connection, error) {
		return config.Connection{Name: "viaform", Driver: "fake", DSN: "fake://f"}, nil
	}
	_, errOut, code := h.exec(t, context.Background(), "connections", "add")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("viaform"); !ok {
		t.Error("interactive add did not save")
	}
}

func TestConnectionsAddInteractiveCancelled(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.runForm = func(context.Context, connform.Model) (config.Connection, error) {
		return config.Connection{}, connform.ErrCancelled
	}
	before, _ := os.ReadFile(h.cfg)
	_, errOut, code := h.exec(t, context.Background(), "connections", "add")
	after, _ := os.ReadFile(h.cfg)
	if code != 0 || string(before) != string(after) || errOut != "" {
		t.Errorf("code=%d, config changed=%v", code, string(before) != string(after))
	}
}

func TestConnectionsEdit(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	var seen string
	h.runForm = func(_ context.Context, m connform.Model) (config.Connection, error) {
		seen = m.Title()
		return config.Connection{Name: "renamed", Driver: "fake", DSN: "fake://x", ReadOnly: true}, nil
	}
	_, errOut, code := h.exec(t, context.Background(), "connections", "edit", "local-pg")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("local-pg"); ok {
		t.Error("old name still present")
	}
	if c, ok := cfg.Find("renamed"); !ok || !c.ReadOnly {
		t.Errorf("renamed = %+v ok=%v", c, ok)
	}
	if !strings.Contains(strings.ToLower(seen), "edit") {
		t.Errorf("form title = %q", seen)
	}
}

func TestConnectionsEditNeedsTTYAndKnownName(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "connections", "edit", "ro")
	if code != 1 || !strings.Contains(errOut, "interactive terminal") {
		t.Errorf("no tty: code=%d stderr=%q", code, errOut)
	}
	h.tty = true
	_, errOut, code = h.exec(t, context.Background(), "connections", "edit", "r")
	if code != 1 || !strings.Contains(errOut, "unknown connection") {
		t.Errorf("unknown: code=%d stderr=%q", code, errOut)
	}
}

func TestConnectionsList(t *testing.T) {
	h := newHarness(t)
	cfg := `connections:
  - name: pw
    driver: fake
    dsn: fake://user:hunter2pw@host/db
    tags: [prod]
  - name: plain
    driver: fake
    dsn: fake://host
    read_only: true
`
	if err := os.WriteFile(h.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code := h.exec(t, context.Background(), "connections", "list")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(out, "hunter2pw") {
		t.Errorf("list leaked the password: %s", out)
	}
	var rows []struct {
		Name     string   `json:"name"`
		Driver   string   `json:"driver"`
		DSN      string   `json:"dsn"`
		ReadOnly bool     `json:"read_only"`
		Tags     []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 || rows[0].Name != "pw" || rows[0].Tags[0] != "prod" || !rows[1].ReadOnly || rows[1].Tags == nil {
		t.Errorf("rows = %+v", rows)
	}

	h.tty = true
	table, _, _ := h.exec(t, context.Background(), "connections", "list")
	if !strings.Contains(table, "NAME") || !strings.Contains(table, "plain") || strings.Contains(table, "hunter2pw") {
		t.Errorf("table = %q", table)
	}
}

func TestConnectionsListEmpty(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code := h.exec(t, context.Background(), "connections", "list")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestConnectionsRm(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "connections", "rm", "ro")
	if code != 1 || !strings.Contains(errOut, "--yes") {
		t.Errorf("no tty, no --yes: code=%d stderr=%q", code, errOut)
	}
	_, errOut, code = h.exec(t, context.Background(), "connections", "rm", "ro", "--yes")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("ro"); ok || len(cfg.Connections) != 2 {
		t.Errorf("connections = %+v", cfg.Connections)
	}
	_, errOut, code = h.exec(t, context.Background(), "connections", "rm", "ghost", "--yes")
	if code != 1 || !strings.Contains(errOut, "unknown connection") {
		t.Errorf("unknown: code=%d stderr=%q", code, errOut)
	}
}

func TestConnectionsRmTypedConfirmation(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	h.in = "nope\n"
	out, _, code := h.exec(t, context.Background(), "connections", "rm", "ro")
	if code != 1 || !strings.Contains(out, "type the connection name to confirm") {
		t.Errorf("wrong confirmation: code=%d out=%q", code, out)
	}
	if cfg, _ := config.Load(h.cfg); len(cfg.Connections) != 3 {
		t.Error("removed despite the wrong confirmation")
	}
	h.in = "ro\n"
	if _, errOut, code := h.exec(t, context.Background(), "connections", "rm", "ro"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if cfg, _ := config.Load(h.cfg); len(cfg.Connections) != 2 {
		t.Error("not removed after typing the name")
	}
}

func TestConnectionsTest(t *testing.T) {
	h := newHarness(t)
	out, errOut, code := h.exec(t, context.Background(), "connections", "test", "local-pg")
	if code != 0 || !strings.HasPrefix(out, "ok (") {
		t.Errorf("success: code=%d out=%q stderr=%q", code, out, errOut)
	}
	h.fake.FailNext("open", driver.Wrap(driver.ErrConnection, "fake", "open", errors.New("dial fake://bob:hunter2pw@h failed\x1b[31m")))
	_, errOut, code = h.exec(t, context.Background(), "connections", "test", "local-pg")
	if code != 2 || strings.Contains(errOut, "hunter2pw") || strings.Contains(errOut, "\x1b") {
		t.Errorf("failure: code=%d stderr=%q", code, errOut)
	}
	_, errOut, code = h.exec(t, context.Background(), "connections", "test", "ghost")
	if code != 1 || !strings.Contains(errOut, "unknown connection") {
		t.Errorf("unknown: code=%d stderr=%q", code, errOut)
	}
}

func TestConnectionsConflict(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	// A concurrent writer is simulated by the form callback changing the file.
	h.runForm = func(context.Context, connform.Model) (config.Connection, error) {
		f, _ := os.OpenFile(h.cfg, os.O_APPEND|os.O_WRONLY, 0)
		_, _ = f.WriteString("# changed\n")
		_ = f.Close()
		return config.Connection{Name: "late", Driver: "fake", DSN: "fake://x"}, nil
	}
	_, errOut, code := h.exec(t, context.Background(), "connections", "add")
	if code != 1 || !strings.Contains(errOut, "config changed on disk; re-run") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestConnectionsListRejectsLiteralPassword(t *testing.T) {
	h := newHarness(t)
	cfg := "connections:\n  - name: x\n    driver: fake\n    dsn: fake://h\n    password: hunter:2\n"
	if err := os.WriteFile(h.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"connections", "test", "x"}, {"query", "x", "select 1"}} {
		out, errOut, code := h.exec(t, context.Background(), args...)
		if code != 1 || strings.Contains(out+errOut, "hunter") || !strings.Contains(errOut, "password must be a secret reference") {
			t.Errorf("%v: code=%d out=%q stderr=%q", args, code, out, errOut)
		}
	}
	// Listing still works and marks the entry instead.
	out, errOut, code := h.exec(t, context.Background(), "connections", "list")
	if code != 0 || strings.Contains(out+errOut, "hunter") || !strings.Contains(out, `"broken": "password must be a secret reference`) {
		t.Errorf("list: code=%d out=%q stderr=%q", code, out, errOut)
	}
}

func TestConnectionsListRejectsControlCharacters(t *testing.T) {
	h := newHarness(t)
	cfg := "connections:\n  - name: \"a\\u0085b\"\n    driver: fake\n    dsn: fake://host\n"
	if err := os.WriteFile(h.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.exec(t, context.Background(), "connections", "list")
	if code != 0 || !strings.Contains(errOut, "control characters") || !strings.Contains(out, `"broken"`) {
		t.Errorf("code=%d out=%q stderr=%q", code, out, errOut)
	}
	if strings.ContainsRune(errOut+out, '\u0085') {
		t.Errorf("output echoes the control character: %q %q", out, errOut)
	}
}
