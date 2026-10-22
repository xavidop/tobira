package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets/keyring"
)

// importSecrets are the passwords the synthetic import fixtures hold (raw
// and unescaped) plus the ones the export tests plant. None may appear in
// any output or in the config.
var importSecrets = []string{"Fx-S3cret-Pw-42", `Fx:S3c\ret`, `Fx\:S3c\\ret`, "Ex-S3cret-Pw-77"}

func assertNoImportSecret(t *testing.T, texts map[string]string) {
	t.Helper()
	for what, s := range texts {
		for _, sec := range importSecrets {
			if strings.Contains(s, sec) {
				t.Errorf("a fixture password leaked into %s", what)
			}
		}
	}
}

// importFixture copies a fixture from internal/connio/testdata into a temp
// dir (mode 0600, as pgpass needs) and returns its path.
func importFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "connio", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// untouchable is a keyring.Store that fails the test on any use.
type untouchable struct{ t *testing.T }

func (u untouchable) Get(string, string) (string, error) {
	u.t.Error("keyring Get called")
	return "", keyring.ErrNotFound
}
func (u untouchable) Set(string, string, string) error { u.t.Error("keyring Set called"); return nil }
func (u untouchable) Delete(string, string) error      { u.t.Error("keyring Delete called"); return nil }

// hookStore runs onSet before each Set of the wrapped store.
type hookStore struct {
	*fakeStore
	onSet func()
}

func (h hookStore) Set(service, account, secret string) error {
	h.onSet()
	return h.fakeStore.Set(service, account, secret)
}

const exportConfig = `connections:
  - name: app
    driver: postgres
    dsn: postgres://app:Ex-S3cret-Pw-77@db.example.test:5432/app?sslmode=require
    password: env:APP_PW
    color: green
    tags: [prod, eu]
  - name: kv
    driver: postgres
    dsn: host=db.example.test user=kv password=Ex-S3cret-Pw-77 dbname=kv
  - name: my
    driver: mysql
    dsn: my:Ex-S3cret-Pw-77@tcp(mysql.example.test:3306)/shop
    read_only: true
    options:
      tls: "true"
      api_token: Ex-S3cret-Pw-77
  - name: vault
    driver: postgres
    dsn: postgres://v@h/db
    password: op://Fixtures/vault/password
  - name: lit
    driver: postgres
    dsn: postgres://u@h/db
    password: Ex-S3cret-Pw-77
`

const exportYAML = `connections:
  - name: app
    driver: postgres
    dsn: postgres://app@db.example.test:5432/app?sslmode=require
    password: env:APP_PW
    color: green
    tags: [prod, eu]
  - name: kv
    driver: postgres
    dsn: host=db.example.test user=kv dbname=kv
  - name: my
    driver: mysql
    dsn: my@tcp(mysql.example.test:3306)/shop
    read_only: true
    options:
      tls: "true"
  - name: vault
    driver: postgres
    dsn: postgres://v@h/db
    password: op://Fixtures/vault/password
`

// namedStub is a registry entry with a real driver name, so imports see
// these drivers as available. It never opens anything.
type namedStub struct{ name string }

func (n namedStub) Info() driver.Info {
	return driver.Info{Name: n.name, Kind: driver.KindSQL, Schemes: []string{n.name}}
}

func (n namedStub) Open(context.Context, driver.ConnConfig) (driver.Conn, error) {
	return nil, errors.New("stub")
}

// importHarness is newHarness with stub postgres, mysql, mariadb, sqlite, redis and
// mongodb drivers registered, and no clickhouse: a build without it.
func importHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	for _, n := range []string{"postgres", "mysql", "mariadb", "sqlite", "redis", "mongodb"} {
		h.extra = append(h.extra, namedStub{n})
	}
	return h
}

func exportHarness(t *testing.T) *harness {
	t.Helper()
	h := importHarness(t)
	if err := os.WriteFile(h.cfg, []byte(exportConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestExportYAML(t *testing.T) {
	h := exportHarness(t)
	out, errOut, code := h.exec(t, context.Background(), "connections", "export")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if out != exportYAML {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, exportYAML)
	}
	for _, note := range []string{
		"note: app: removed the password from the DSN",
		"note: kv: removed the password from the DSN",
		"note: my: removed the password from the DSN",
		`note: my: dropped option "api_token", which looks like a secret`,
		"note: 1 broken entry left out (line 22)",
	} {
		if !strings.Contains(errOut, note) {
			t.Errorf("stderr lacks %q:\n%s", note, errOut)
		}
	}
	assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut})
}

func TestExportJSONAndNames(t *testing.T) {
	h := exportHarness(t)
	out, errOut, code := h.exec(t, context.Background(), "connections", "export", "--format", "json", "vault", "app")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	want := `{
  "connections": [
    {
      "name": "vault",
      "driver": "postgres",
      "dsn": "postgres://v@h/db",
      "password": "op://Fixtures/vault/password"
    },
    {
      "name": "app",
      "driver": "postgres",
      "dsn": "postgres://app@db.example.test:5432/app?sslmode=require",
      "password": "env:APP_PW",
      "color": "green",
      "tags": [
        "prod",
        "eu"
      ]
    }
  ]
}
`
	if out != want {
		t.Errorf("stdout:\n%s", out)
	}
	assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut})
}

func TestExportErrors(t *testing.T) {
	h := exportHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "connections", "export", "ap")
	if code != 1 || !strings.Contains(errOut, `unknown connection "ap"`) {
		t.Errorf("unknown name: code=%d stderr=%q", code, errOut)
	}
	_, errOut, code = h.exec(t, context.Background(), "connections", "export", "--format", "toml")
	if code != 1 || !strings.Contains(errOut, "--format") {
		t.Errorf("bad format: code=%d stderr=%q", code, errOut)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			src := exportHarness(t)
			out, errOut, code := src.exec(t, context.Background(), "connections", "export", "--format", format)
			if code != 0 {
				t.Fatalf("export: code=%d stderr=%q", code, errOut)
			}
			file := filepath.Join(t.TempDir(), "conns."+format)
			if err := os.WriteFile(file, []byte(out), 0o600); err != nil {
				t.Fatal(err)
			}
			dst := importHarness(t)
			dst.cfg = filepath.Join(t.TempDir(), "fresh", "config.yaml")
			iout, ierr, code := dst.exec(t, context.Background(), "connections", "import", "--from", "file", file, "--apply")
			if code != 0 {
				t.Fatalf("import: code=%d stderr=%q stdout=%q", code, ierr, iout)
			}
			got, err := config.Load(dst.cfg)
			if err != nil {
				t.Fatal(err)
			}
			want, err := config.ParseConnectionList([]byte(exportYAML))
			if err != nil {
				t.Fatal(err)
			}
			var wantConns []config.Connection
			for _, e := range want {
				wantConns = append(wantConns, e.Conn)
			}
			if !reflect.DeepEqual(got.Connections, wantConns) {
				t.Errorf("round trip:\ngot  %+v\nwant %+v", got.Connections, wantConns)
			}
			assertNoImportSecret(t, map[string]string{"config": readFile(t, dst.cfg), "stdout": iout, "stderr": ierr})
		})
	}
}

func TestImportPlanGoldens(t *testing.T) {
	cases := []struct{ from, fixture string }{
		{"dbeaver", "data-sources.json"},
		{"pgpass", "pgpass"},
		{"tableplus", "Connections.plist"},
		{"file", "export.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			h := importHarness(t)
			h.keyringOverride = untouchable{t}
			before := readFile(t, h.cfg)
			st, _ := config.StatOf(h.cfg)
			out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", tc.from, importFixture(t, tc.fixture))
			if code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			golden(t, "import_"+tc.from+".golden", out)
			if after, _ := config.StatOf(h.cfg); readFile(t, h.cfg) != before || !after.Equal(st) {
				t.Error("a dry run changed the config")
			}
			assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut})
		})
	}
}

func TestImportDryRunWithTTYAndKeyringYesTouchesNothing(t *testing.T) {
	h := importHarness(t)
	h.tty = true
	h.in = "y\ny\ny\ny\n"
	h.keyringOverride = untouchable{t}
	before := readFile(t, h.cfg)
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "pgpass", importFixture(t, "pgpass"), "--keyring-yes")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if readFile(t, h.cfg) != before {
		t.Error("config changed")
	}
	if strings.Contains(errOut, "[y/N]") {
		t.Error("a dry run prompted")
	}
	if !strings.Contains(out, "password: will store it in the OS keyring") || strings.Contains(out, "will ask") {
		t.Errorf("--keyring-yes should win on a terminal:\n%s", out)
	}
}

func TestImportKeyringYesOnATerminalDoesNotAsk(t *testing.T) {
	h := importHarness(t)
	h.tty = true
	h.in = "n\nn\nn\nn\n"
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "pgpass", importFixture(t, "pgpass"), "--apply", "--keyring-yes")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if strings.Contains(errOut, "[y/N]") {
		t.Errorf("prompted despite --keyring-yes:\n%s", errOut)
	}
	if v, ok := h.stored("localhost-local-pg"); !ok || v != "Fx-S3cret-Pw-42" {
		t.Error("not stored")
	}
	assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut, "config": readFile(t, h.cfg)})
}

func TestImportSkipsDriversNotInThisBuild(t *testing.T) {
	h := importHarness(t)
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "dbeaver", importFixture(t, "data-sources.json"), "--apply")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "- skip   Events: driver not available in this build") {
		t.Errorf("stdout:\n%s", out)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("Events"); ok {
		t.Error("an unavailable driver's connection was written")
	}
}

func TestImportRefusesNonRegularAndHugeFiles(t *testing.T) {
	h := importHarness(t)
	dir := t.TempDir()
	huge := filepath.Join(dir, "huge.yaml")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16<<20 + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for _, from := range []string{"dbeaver", "pgpass", "tableplus", "file"} {
		_, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", from, dir)
		if code != 1 || !strings.Contains(errOut, "not a regular file") {
			t.Errorf("%s dir: code=%d stderr=%q", from, code, errOut)
		}
		if from == "pgpass" {
			_ = os.Chmod(huge, 0o600)
		}
		_, errOut, code = h.exec(t, context.Background(), "connections", "import", "--from", from, huge)
		if code != 1 || !strings.Contains(errOut, "larger than 16 MiB") {
			t.Errorf("%s huge: code=%d stderr=%q", from, code, errOut)
		}
	}
}

const commentedConfig = `# my tobira config
connections:
  # the local one
  - name: local-pg
    driver: fake
    dsn: fake://x   # trailing
tui:
  theme: tobira-dark # keep
`

func TestImportApplyStoresWithKeyringYesAndKeepsComments(t *testing.T) {
	h := importHarness(t)
	if err := os.WriteFile(h.cfg, []byte(commentedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "dbeaver",
		importFixture(t, "data-sources.json"), "--apply", "--keyring-yes")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	text := readFile(t, h.cfg)
	for _, keep := range []string{"# my tobira config", "# the local one", "dsn: fake://x   # trailing", "theme: tobira-dark # keep"} {
		if !strings.Contains(text, keep) {
			t.Errorf("lost %q:\n%s", keep, text)
		}
	}
	cfg, err := config.Load(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cfg.Connections {
		names = append(names, c.Name)
	}
	wantNames := []string{"local-pg", "Orders-DB", "local-pg-imported", "Maria", "Local-file", "URL-only"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("names = %q", names)
	}
	for name, ref := range map[string]string{"Orders-DB": "keyring:Orders-DB", "URL-only": "keyring:URL-only", "Maria": ""} {
		if c, _ := cfg.Find(name); c.Password != ref {
			t.Errorf("%s password = %q, want %q", name, c.Password, ref)
		}
		if ref != "" {
			if v, ok := h.stored(name); !ok || v != "Fx-S3cret-Pw-42" {
				t.Errorf("%s not stored in the keyring", name)
			}
		}
	}
	if !strings.Contains(out, "added 5 connections") || !strings.Contains(errOut, "stored password for Orders-DB in the OS keyring") {
		t.Errorf("stdout=%q stderr=%q", out, errOut)
	}
	assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut, "config": text})
}

func TestImportApplyAsksPerConnectionOnATerminal(t *testing.T) {
	h := importHarness(t)
	h.tty = true
	h.in = "y\nn\nyes\n\n"
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "pgpass", importFixture(t, "pgpass"), "--apply")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if strings.Count(errOut, "[y/N]") != 4 {
		t.Errorf("want 4 prompts:\n%s", errOut)
	}
	cfg, _ := config.Load(h.cfg)
	for name, ref := range map[string]string{
		"db.example.test-orders":          "keyring:db.example.test-orders",
		"db.example.test-orders-imported": "",
		"1-we-ird":                        "keyring:1-we-ird",
		"localhost-local-pg":              "",
		"replica.example.test-analytics":  "",
	} {
		c, ok := cfg.Find(name)
		if !ok || c.Password != ref {
			t.Errorf("%s: found=%v password=%q want %q", name, ok, c.Password, ref)
		}
	}
	if v, _ := h.stored("1-we-ird"); v != `Fx:S3c\ret` {
		t.Error("the unescaped pgpass password was not stored")
	}
	if _, ok := h.stored("localhost-local-pg"); ok {
		t.Error("stored without consent")
	}
	assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut, "config": readFile(t, h.cfg)})
}

func TestImportApplyWithoutTerminalOrConsentStoresNothing(t *testing.T) {
	for name, extra := range map[string][]string{
		"no terminal":  nil,
		"no-passwords": {"--no-passwords", "--keyring-yes"},
	} {
		t.Run(name, func(t *testing.T) {
			h := importHarness(t)
			h.keyringOverride = untouchable{t}
			args := append([]string{"connections", "import", "--from", "pgpass", importFixture(t, "pgpass"), "--apply"}, extra...)
			out, errOut, code := h.exec(t, context.Background(), args...)
			if code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			cfg, _ := config.Load(h.cfg)
			for _, c := range cfg.Connections {
				if c.Password != "" {
					t.Errorf("%s got password %q", c.Name, c.Password)
				}
			}
			if len(cfg.Connections) != 3+5 {
				t.Errorf("connections = %d", len(cfg.Connections))
			}
			assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut, "config": readFile(t, h.cfg)})
		})
	}
}

func TestImportApplyFailsOnConcurrentModification(t *testing.T) {
	h := importHarness(t)
	changed := testConfig + "  - name: other\n    driver: fake\n    dsn: fake://y\n"
	h.keyringOverride = hookStore{fakeStore: newFakeStore(), onSet: func() {
		if err := os.WriteFile(h.cfg, []byte(changed), 0o600); err != nil {
			t.Error(err)
		}
	}}
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "pgpass", importFixture(t, "pgpass"), "--apply", "--keyring-yes")
	if code != 1 || !strings.Contains(errOut, "config changed on disk") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := readFile(t, h.cfg); got != changed {
		t.Errorf("the concurrent change was overwritten:\n%s", got)
	}
	if strings.Contains(out, "added") {
		t.Errorf("stdout claims success: %q", out)
	}
	assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut})
}

func TestImportRenameSuffix(t *testing.T) {
	h := importHarness(t)
	file := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(file, []byte("connections:\n  - name: local-pg\n    driver: fake\n    dsn: fake://z\n  - name: ro\n    driver: fake\n    dsn: fake://z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "file", file, "--apply", "--rename-suffix", "-new")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "~ rename local-pg -> local-pg-new") {
		t.Errorf("stdout:\n%s", out)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("ro-new"); !ok {
		t.Error("ro-new missing")
	}
	for _, bad := range []string{"", "has space", "x/y"} {
		_, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "file", file, "--rename-suffix", bad)
		if code != 1 || !strings.Contains(errOut, "--rename-suffix") {
			t.Errorf("%q: code=%d stderr=%q", bad, code, errOut)
		}
	}
}

func TestImportPgpassPermissionsRefused(t *testing.T) {
	h := importHarness(t)
	p := importFixture(t, "pgpass")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, h.cfg)
	_, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "pgpass", p, "--apply", "--keyring-yes")
	if code != 1 || !strings.Contains(errOut, "group or world access") || !strings.Contains(errOut, "chmod 0600") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
	if readFile(t, h.cfg) != before {
		t.Error("config changed")
	}
}

func TestImportMalformedInputs(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"dbeaver":   {`{"connections": {"a": {"configuration": {"password": "Fx-S3cret-Pw-42"`, "import from dbeaver: not valid JSON"},
		"pgpass":    {"h:1:d:u:Fx-S3cret-Pw-42\x00", "import from pgpass: not a text file"},
		"tableplus": {"<plist>Fx-S3cret-Pw-42", "import from tableplus: not a property list"},
		"file":      {"connections: {Fx-S3cret-Pw-42", "import from file: not valid YAML or JSON"},
	}
	for from, tc := range cases {
		t.Run(from, func(t *testing.T) {
			h := importHarness(t)
			p := filepath.Join(t.TempDir(), "in")
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", from, p, "--apply")
			if code != 1 || !strings.Contains(errOut, tc.want) {
				t.Errorf("code=%d stderr=%q", code, errOut)
			}
			assertNoImportSecret(t, map[string]string{"stdout": out, "stderr": errOut})
		})
	}
}

func TestImportUsageErrors(t *testing.T) {
	h := importHarness(t)
	for _, args := range [][]string{
		{"connections", "import"},
		{"connections", "import", "--from", "access", "x"},
		{"connections", "import", "--from", "file"},
		{"connections", "import", "--from", "file", "a", "b"},
		{"connections", "import", "--from", "file", filepath.Join(t.TempDir(), "missing")},
	} {
		_, errOut, code := h.exec(t, context.Background(), args...)
		if code != 1 || errOut == "" {
			t.Errorf("%q: code=%d stderr=%q", args, code, errOut)
		}
	}
}

func TestImportFileKeepsUnavailableDriversWithANote(t *testing.T) {
	h := importHarness(t)
	file := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(file, []byte("connections:\n  - name: events\n    driver: clickhouse\n    dsn: clickhouse://h/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.exec(t, context.Background(), "connections", "import", "--from", "file", file, "--apply")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "+ add    events (clickhouse)") ||
		!strings.Contains(out, "    note: driver clickhouse not available in this build; install the plugin") {
		t.Errorf("stdout:\n%s", out)
	}
	cfg, _ := config.Load(h.cfg)
	if _, ok := cfg.Find("events"); !ok {
		t.Error("the entry was not added")
	}
}
