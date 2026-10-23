package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/tui"
	"github.com/xavidop/tobira/internal/tui/connform"
)

const brokenConfig = `connections:
  - name: good
    driver: fake
    dsn: fake://x
  - name: leaky
    driver: fake
    dsn: fake://leaky
    password: hunter2-SUPERSECRET
  - driver: fake
    dsn: fake://noname
`

func (h *harness) writeBroken(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(h.cfg, []byte(brokenConfig), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenEntryWarnsAndGoodEntriesRun(t *testing.T) {
	h := newHarness(t)
	h.writeBroken(t)
	out, errOut, code := h.exec(t, context.Background(), "query", "good", selectPeople)
	if code != 0 || out == "" {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errOut)
	}
	for _, want := range []string{
		`warning: config ` + h.cfg + `: line 5: connection "leaky": password must be a secret reference`,
		`warning: config ` + h.cfg + `: line 9: name is required`,
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut+out, "SUPERSECRET") {
		t.Fatal("output leaks the password")
	}
}

func TestBrokenEntryFailsOnlyWhenNeeded(t *testing.T) {
	h := newHarness(t)
	h.writeBroken(t)
	for _, args := range [][]string{
		{"query", "leaky", selectPeople},
		{"connections", "test", "leaky"},
	} {
		_, errOut, code := h.exec(t, context.Background(), args...)
		if code != 1 || !strings.Contains(errOut, `error: connection "leaky": password must be a secret reference`) {
			t.Errorf("%v: code=%d stderr=%q, want exit 1 with the entry's error", args, code, errOut)
		}
		if strings.Contains(errOut, "unknown connection") || strings.Contains(errOut, "SUPERSECRET") {
			t.Errorf("%v: stderr = %q", args, errOut)
		}
	}
	h.tty = true
	h.runTUI = func(context.Context, tui.Options) error { t.Error("TUI started"); return nil }
	_, errOut, code := h.exec(t, context.Background(), "connect", "leaky")
	if code != 1 || !strings.Contains(errOut, `connection "leaky": password`) {
		t.Errorf("connect: code=%d stderr=%q", code, errOut)
	}
}

func TestConnectionsListIncludesBroken(t *testing.T) {
	h := newHarness(t)
	h.writeBroken(t)
	out, _, code := h.exec(t, context.Background(), "connections", "list")
	if code != 0 || strings.Contains(out, "SUPERSECRET") || strings.Contains(out, "noname") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 3 {
		t.Fatalf("rows = %v, err = %v\n%s", rows, err, out)
	}
	for i, r := range rows {
		for _, k := range []string{"name", "driver", "dsn", "ssh", "read_only", "tags", "color", "password_ref", "line", "broken"} {
			if _, ok := r[k]; !ok {
				t.Errorf("row %d lacks %q: %v", i, k, r)
			}
		}
		if len(r) != 10 {
			t.Errorf("row %d has %d keys, want 10: %v", i, len(r), r)
		}
	}
	if rows[0]["broken"] != "" || rows[0]["line"] != float64(0) {
		t.Errorf("good row = %v, want empty broken and line 0", rows[0])
	}
	b := rows[1]
	if b["name"] != "leaky" || b["line"] != float64(5) || !strings.Contains(b["broken"].(string), "password must be a secret reference") ||
		b["dsn"] != "" || b["driver"] != "" {
		t.Errorf("broken row = %v", b)
	}
	h.tty = true
	table, _, _ := h.exec(t, context.Background(), "connections", "list")
	if !strings.Contains(table, "BROKEN") || !strings.Contains(table, "line 5: password must be a secret reference") ||
		!strings.Contains(table, "(entry at line 9)") || strings.Contains(table, "SUPERSECRET") {
		t.Errorf("table:\n%s", table)
	}
}

func TestConnectionsEditBrokenPrefillsSafeFields(t *testing.T) {
	h := newHarness(t)
	h.writeBroken(t)
	h.tty = true
	var prefill string
	h.runForm = func(_ context.Context, m connform.Model) (config.Connection, error) {
		prefill = m.View()
		return config.Connection{Name: "leaky", Driver: "fake", DSN: "fake://leaky", Password: "env:LEAKY_PW"}, nil
	}
	_, errOut, code := h.exec(t, context.Background(), "connections", "edit", "leaky")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if prefill == "" || strings.Contains(prefill, "SUPERSECRET") {
		t.Errorf("form prefill:\n%s", prefill)
	}
	got, _ := os.ReadFile(h.cfg)
	if strings.Contains(string(got), "SUPERSECRET") || !strings.Contains(string(got), "password: env:LEAKY_PW") {
		t.Errorf("config:\n%s", got)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("leaky"); !ok || len(cfg.Broken) != 1 {
		t.Errorf("after repair: %+v", cfg)
	}
}

func TestConnectionsRmBroken(t *testing.T) {
	h := newHarness(t)
	h.writeBroken(t)
	out, errOut, code := h.exec(t, context.Background(), "connections", "rm", "--yes", "leaky")
	if code != 0 || !strings.Contains(out, "removed leaky") {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errOut)
	}
	got, _ := os.ReadFile(h.cfg)
	if strings.Contains(string(got), "leaky") || !strings.Contains(string(got), "name: good") {
		t.Errorf("config:\n%s", got)
	}
}

const dupConfig = `connections:
  - name: x
    driver: fake
    dsn: fake://first
  - name: x
    driver: fake
    dsn: fake://second
  - driver: fake
    dsn: fake://noname
`

func (h *harness) writeDup(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(h.cfg, []byte(dupConfig), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestByNameRefusesWhenAmbiguous(t *testing.T) {
	h := newHarness(t)
	h.writeDup(t)
	h.tty = true
	h.runForm = func(context.Context, connform.Model) (config.Connection, error) {
		t.Error("form opened for an ambiguous name")
		return config.Connection{}, connform.ErrCancelled
	}
	for _, args := range [][]string{{"connections", "rm", "--yes", "x"}, {"connections", "edit", "x"}} {
		_, errOut, code := h.exec(t, context.Background(), args...)
		if code != 1 || !strings.Contains(errOut, `connection "x" appears 2 times (lines 2, 5); use --line`) {
			t.Errorf("%v: code=%d stderr=%q", args, code, errOut)
		}
	}
	if got, _ := os.ReadFile(h.cfg); string(got) != dupConfig {
		t.Fatalf("config changed:\n%s", got)
	}
}

func TestRmAndEditByLine(t *testing.T) {
	h := newHarness(t)
	h.writeDup(t)
	out, errOut, code := h.exec(t, context.Background(), "connections", "rm", "--yes", "--line", "5")
	if code != 0 || !strings.Contains(out, "removed x") {
		t.Fatalf("rm: code=%d out=%q stderr=%q", code, out, errOut)
	}
	if got, _ := os.ReadFile(h.cfg); !strings.Contains(string(got), "fake://first") || strings.Contains(string(got), "fake://second") {
		t.Fatalf("wrong entry removed:\n%s", got)
	}
	// The unnamed entry now starts on line 5.
	h.tty = true
	h.runForm = func(context.Context, connform.Model) (config.Connection, error) {
		return config.Connection{Name: "named", Driver: "fake", DSN: "fake://noname"}, nil
	}
	_, errOut, code = h.exec(t, context.Background(), "connections", "edit", "--line", "5")
	if code != 0 {
		t.Fatalf("edit: code=%d stderr=%q", code, errOut)
	}
	if cfg, _ := config.Load(h.cfg); len(cfg.Broken) != 0 || len(cfg.Connections) != 2 {
		t.Fatalf("after edit: %+v", cfg)
	}
	_, errOut, code = h.exec(t, context.Background(), "connections", "rm", "--yes", "--line", "99")
	if code != 1 || !strings.Contains(errOut, "line 99") {
		t.Errorf("unknown line: code=%d stderr=%q", code, errOut)
	}
	_, _, code = h.exec(t, context.Background(), "connections", "rm", "--yes", "--line", "2", "x")
	if code != 1 {
		t.Errorf("name plus --line: code=%d, want usage error", code)
	}
}

func TestUnknownKeyNeverLeaks(t *testing.T) {
	h := newHarness(t)
	cfg := "connections:\n  - {name: good, driver: fake, dsn: 'fake://x'}\n  - {name: leak, driver: fake, password:hunter2SECRET}\n  - {driver: fake, hunter3SECRET: 1}\n"
	if err := os.WriteFile(h.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tty := range []bool{false, true} {
		h.tty = tty
		out, errOut, code := h.exec(t, context.Background(), "connections", "list")
		if code != 0 || strings.Contains(out+errOut, "SECRET") || !strings.Contains(errOut, `did you mean "password"`) {
			t.Errorf("tty=%v: code=%d out=%q stderr=%q", tty, code, out, errOut)
		}
	}
	_, errOut, code := h.exec(t, context.Background(), "query", "leak", selectPeople)
	if code != 1 || strings.Contains(errOut, "SECRET") {
		t.Errorf("query: code=%d stderr=%q", code, errOut)
	}
}

func TestRmPromptForUnnamedEntry(t *testing.T) {
	h := newHarness(t)
	h.writeDup(t)
	h.tty = true
	h.in = "line 8\n"
	out, _, code := h.exec(t, context.Background(), "connections", "rm", "--line", "8")
	if code != 0 || !strings.Contains(out, "type the label shown (line 8)") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

const dsnNamedConfig = `connections:
  - name: "postgres://u:supersecret@h/db"
  - name: ok
    driver: fake
    dsn: fake://x
`

func TestBrokenNameWithPasswordIsRedactedEverywhere(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, []byte(dsnNamedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, errOut, code := h.exec(t, ctx, "query", "ok", selectPeople)
	if code != 0 || !strings.Contains(errOut, "warning: config") || strings.Contains(out+errOut, "supersecret") {
		t.Errorf("query: code=%d out=%q stderr=%q", code, out, errOut)
	}
	js, errOut, code := h.exec(t, ctx, "connections", "list")
	if code != 0 || strings.Contains(js+errOut, "supersecret") || !strings.Contains(js, "xxxxx") {
		t.Errorf("list json: code=%d out=%q stderr=%q", code, js, errOut)
	}
	h.tty = true
	table, errOut, code := h.exec(t, ctx, "connections", "list")
	if code != 0 || strings.Contains(table+errOut, "supersecret") {
		t.Errorf("list table: code=%d out=%q stderr=%q", code, table, errOut)
	}
}

func TestConnectNoticeIsRedacted(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	// A whole-file failure whose message echoes a DSN with a password.
	if err := os.WriteFile(h.cfg, []byte("tui:\n  \"postgres://u:supersecret@h/db\": 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got tui.Options
	h.runTUI = func(_ context.Context, o tui.Options) error { got = o; return nil }
	_, errOut, code := h.exec(t, context.Background(), "connect", "sqlite:./a.db")
	if code != 0 || got.Notice == "" {
		t.Fatalf("code=%d notice=%q stderr=%q", code, got.Notice, errOut)
	}
	if strings.Contains(errOut+got.Notice, "supersecret") {
		t.Errorf("leak: stderr=%q notice=%q", errOut, got.Notice)
	}
}

func TestConnectionsEditDSNNamedBrokenEntryHidesPassword(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, []byte(dsnNamedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tty = true
	var prefill string
	h.runForm = func(_ context.Context, m connform.Model) (config.Connection, error) {
		prefill = renderForm(m)
		return config.Connection{Name: "fixed", Driver: "fake", DSN: "fake://fixed"}, nil
	}
	out, errOut, code := h.exec(t, context.Background(), "connections", "edit", "--line", "2")
	if code != 0 || prefill == "" || !strings.Contains(prefill, "Name") || strings.Contains(prefill+out+errOut, "supersecret") {
		t.Fatalf("code=%d out=%q stderr=%q prefill:\n%s", code, out, errOut, prefill)
	}
	got, _ := os.ReadFile(h.cfg)
	if strings.Contains(string(got), "supersecret") || !strings.Contains(string(got), "name: fixed") {
		t.Errorf("config:\n%s", got)
	}
}

// renderForm sizes m and feeds it its Init messages so the fields draw.
func renderForm(m connform.Model) string {
	m = m.Resize(100, 40)
	cmd := m.Init()
	for i := 0; i < 5 && cmd != nil; i++ {
		var next tea.Cmd
		m2, c := m.Update(cmd())
		m, next = m2, c
		cmd = next
	}
	return m.View()
}
