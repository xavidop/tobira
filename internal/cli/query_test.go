package cli_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/secrets/keyring"
	"github.com/xavidop/tobira/internal/tui"
	"github.com/xavidop/tobira/internal/tui/connform"
)

const testConfig = `connections:
  - name: local-pg
    driver: fake
    dsn: fake://x
  - name: ro
    driver: fake
    dsn: fake://x
    read_only: true
  - name: nodrv
    driver: nope
    dsn: nope://x
`

type harness struct {
	fake *drivertest.Fake
	// extra drivers registered next to the fake.
	extra []driver.Driver
	// wrapDriver, when set, wraps the fake before it is registered.
	wrapDriver func(driver.Driver) driver.Driver
	cfg        string
	in         string
	tty        bool
	env        map[string]string
	// environ is what Deps.Environ returns: the environment driver plugins
	// get (through the allow-list).
	environ []string
	// opened records the ConnConfig of every Open the driver received,
	// under openedMu (doctor opens connections in parallel).
	opened   []driver.ConnConfig
	openedMu sync.Mutex
	// runTUI and runForm replace the interactive programs.
	runTUI  func(context.Context, tui.Options) error
	runForm func(context.Context, connform.Model) (config.Connection, error)
	// store is the fake keyring and pws the answers the no-echo prompt
	// returns, one per call. No test touches the real OS keychain.
	store *fakeStore
	pws   []string
	// onRead runs at the start of every prompt read; an error ends the read.
	onRead func(context.Context) error
	// keyringOverride replaces the fake store when set.
	keyringOverride keyring.Store
	// newAssistant replaces the AI assistant factory cmd/tobira wires.
	newAssistant func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error)
	// serveMCP replaces the MCP server factory cmd/tobira wires.
	serveMCP func(context.Context, cli.MCPOptions) error
	// newEmbedder replaces the embedder factory cmd/tobira wires.
	newEmbedder func(context.Context, config.AI, cli.EmbedderDeps) (driver.Embedder, error)
	// checkAI replaces the online AI provider check cmd/tobira wires.
	checkAI func(context.Context, config.AI, cli.AICheckDeps) error
}

// recordingDriver wraps the Fake's driver to capture the ConnConfig the
// command hands to Open.
type recordingDriver struct {
	driver.Driver
	h *harness
}

func (d recordingDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d.h.openedMu.Lock()
	d.h.opened = append(d.h.opened, cfg)
	d.h.openedMu.Unlock()
	return d.Driver.Open(ctx, cfg)
}

// registerKeyring registers the keyring: scheme once per test binary, as
// cmd/tobira does at start, over a store no test reads.
var registerKeyring = sync.OnceFunc(func() { mamori.Register(keyring.NewProvider(newFakeStore())) })

func newHarness(t *testing.T) *harness {
	t.Helper()
	registerKeyring()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return &harness{fake: drivertest.NewFake(drivertest.WithPeople()), cfg: cfg}
}

func (h *harness) keyring() keyring.Store {
	if h.keyringOverride != nil {
		return h.keyringOverride
	}
	if h.store == nil {
		h.store = newFakeStore()
	}
	return h.store
}

func (h *harness) exec(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	reg := driver.NewRegistry()
	var main driver.Driver = recordingDriver{Driver: h.fake.Driver(), h: h}
	if h.wrapDriver != nil {
		main = h.wrapDriver(main)
	}
	if err := reg.Add(main); err != nil {
		t.Fatal(err)
	}
	for _, d := range h.extra {
		if err := reg.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	getenv := func(k string) string { return h.env[k] }
	deps := cli.Deps{
		Registry:     reg,
		Getenv:       getenv,
		Environ:      func() []string { return h.environ },
		Home:         t.TempDir(),
		IsTTY:        func(io.Writer) bool { return h.tty },
		IsInputTTY:   func() bool { return h.tty },
		RunTUI:       h.runTUI,
		RunForm:      h.runForm,
		Resolver:     secrets.Resolver{Getenv: getenv},
		Keyring:      h.keyring(),
		NewAssistant: h.newAssistant,
		ServeMCP:     h.serveMCP,
		NewEmbedder:  h.newEmbedder,
		CheckAI:      h.checkAI,
		ReadPassword: func(ctx context.Context) (string, error) {
			if h.onRead != nil {
				if err := h.onRead(ctx); err != nil {
					return "", err
				}
			}
			if len(h.pws) == 0 {
				return "", io.EOF
			}
			pw := h.pws[0]
			h.pws = h.pws[1:]
			return pw, nil
		},
	}
	root := cli.NewRoot(cli.BuildInfo{}, cli.Streams{In: strings.NewReader(h.in), Out: &out, Err: &errOut}, deps)
	code = cli.Execute(ctx, root, append([]string{"--config", h.cfg}, args...))
	return out.String(), errOut.String(), code
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch:\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

const selectPeople = "select * from tobira_people limit 3"

func TestQueryByName(t *testing.T) {
	h := newHarness(t)
	out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--format", "ndjson")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	golden(t, "query_by_name.golden", out)
}

func TestQueryByDSN(t *testing.T) {
	h := newHarness(t)
	out, errOut, code := h.exec(t, context.Background(), "query", "fake://x", selectPeople, "--format", "ndjson")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	golden(t, "query_by_name.golden", out)
}

func TestQueryFromFile(t *testing.T) {
	h := newHarness(t)
	f := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(f, []byte(selectPeople+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", "-f", f, "--format", "ndjson")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	golden(t, "query_by_name.golden", out)
}

func TestQueryStdin(t *testing.T) {
	h := newHarness(t)
	h.in = selectPeople + "\n"
	out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", "-f", "-", "--format", "ndjson")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	golden(t, "query_by_name.golden", out)
}

func TestQueryFormatAutoTTY(t *testing.T) {
	h := newHarness(t)
	h.tty = true
	table, _, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	// The footer carries a timing, so only the grid is compared.
	grid, _, _ := strings.Cut(table, "\n\n")
	golden(t, "query_tty_table.golden", grid+"\n")
	if strings.HasPrefix(table, "{") {
		t.Errorf("TTY output looks like JSON:\n%s", table)
	}
	h.tty = false
	js, _, _ := h.exec(t, context.Background(), "query", "local-pg", selectPeople)
	if !strings.HasPrefix(js, "{") {
		t.Errorf("non-TTY output is not JSON:\n%s", js)
	}
	csv, _, _ := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--format", "csv")
	if strings.HasPrefix(csv, "{") || !strings.Contains(csv, ",") {
		t.Errorf("--format csv ignored:\n%s", csv)
	}
}

func TestQueryReadOnlyForced(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "query", "ro", "delete from tobira_people")
	if code != 4 || !strings.Contains(errOut, "read-only") {
		t.Fatalf("code=%d stderr=%q, want 4 and read-only", code, errOut)
	}
	_, errOut, code = h.exec(t, context.Background(), "query", "local-pg", "delete from tobira_people", "--read-only")
	if code != 4 || !strings.Contains(errOut, "read-only") {
		t.Fatalf("flag: code=%d stderr=%q, want 4 and read-only", code, errOut)
	}
}

func TestQueryReadOnlyFlagReachesDriver(t *testing.T) {
	for _, target := range []string{"local-pg", "fake://x"} {
		t.Run(target, func(t *testing.T) {
			h := newHarness(t)
			if _, errOut, code := h.exec(t, context.Background(), "query", target, selectPeople, "--read-only"); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if len(h.opened) != 1 || !h.opened[0].ReadOnly {
				t.Fatalf("driver got %+v, want one Open with ReadOnly", h.opened)
			}
			h.opened = nil
			if _, errOut, code := h.exec(t, context.Background(), "query", target, selectPeople); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errOut)
			}
			if len(h.opened) != 1 || h.opened[0].ReadOnly {
				t.Fatalf("driver got %+v, want one Open without ReadOnly", h.opened)
			}
		})
	}
	h := newHarness(t)
	if _, _, code := h.exec(t, context.Background(), "query", "ro", selectPeople); code != 0 || !h.opened[0].ReadOnly {
		t.Fatalf("config read_only: code=%d opened=%+v", code, h.opened)
	}
}

func TestExitCodes(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		kind error
		args []string
		want int
	}{
		{"auth", driver.ErrAuth, nil, 2},
		{"connection", driver.ErrConnection, nil, 2},
		{"syntax", driver.ErrSyntax, nil, 3},
		{"not found", driver.ErrNotFound, nil, 3},
		{"read only", driver.ErrReadOnly, nil, 4},
		{"permission", driver.ErrPermission, nil, 4},
		{"unclassified", nil, nil, 3},
		{"unknown driver", nil, []string{"query", "nodrv", "select 1"}, 2},
		{"bad flag", nil, []string{"query", "local-pg", "x", "--bogus"}, 1},
		{"no args", nil, []string{"query"}, 1},
		{"both query and file", nil, []string{"query", "local-pg", "x", "-f", "y"}, 1},
		{"neither query nor file", nil, []string{"query", "local-pg"}, 1},
		{"bad format", nil, []string{"query", "local-pg", "x", "--format", "xml"}, 1},
		{"unknown command", nil, []string{"bogus"}, 1},
		{"negative limit", nil, []string{"query", "local-pg", "x", "--limit", "-1"}, 1},
		{"negative timeout", nil, []string{"query", "local-pg", "x", "--timeout", "-1s"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			args := tt.args
			if args == nil {
				args = []string{"query", "local-pg", selectPeople}
				h.fake.FailNext("query", driver.Wrap(tt.kind, "fake", "query", boom))
			}
			_, errOut, code := h.exec(t, context.Background(), args...)
			if code != tt.want {
				t.Fatalf("code=%d, want %d (stderr %q)", code, tt.want, errOut)
			}
			if !strings.HasPrefix(errOut, "error: ") || strings.Count(errOut, "\n") != 1 {
				t.Errorf("stderr = %q, want a single 'error: ' line", errOut)
			}
		})
	}
}

func TestOpenExitCodes(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found", driver.Wrap(driver.ErrNotFound, "fake", "open", boom), 2},
		{"syntax", driver.Wrap(driver.ErrSyntax, "fake", "open", boom), 2},
		{"unsupported", driver.Wrap(driver.ErrUnsupported, "fake", "open", boom), 2},
		{"unclassified", driver.Wrap(nil, "fake", "open", boom), 2},
		{"auth", driver.Wrap(driver.ErrAuth, "fake", "open", boom), 2},
		{"read only", driver.Wrap(driver.ErrReadOnly, "fake", "open", boom), 4},
		{"permission", driver.Wrap(driver.ErrPermission, "fake", "open", boom), 4},
		{"canceled", driver.Wrap(nil, "fake", "open", context.Canceled), 3},
		{"deadline", driver.Wrap(nil, "fake", "open", context.DeadlineExceeded), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.FailNext("open", tt.err)
			_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople)
			if code != tt.want {
				t.Fatalf("code=%d, want %d (stderr %q)", code, tt.want, errOut)
			}
		})
	}
}

func TestExitCodeFunc(t *testing.T) {
	if got := cli.ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d", got)
	}
	if got := cli.ExitCode(context.DeadlineExceeded); got != 3 {
		t.Errorf("deadline = %d", got)
	}
}

func TestQueryErrorRedactsDSN(t *testing.T) {
	msgs := []string{
		"error at postgres://u:s3cret@h/db",
		`cannot parse "postgres://h/db?sslmode=disable&password=s3cret"`,
		"password=s3cret host=h",
		"auth failed: s3cret rejected", // the literal resolved password
	}
	for _, op := range []string{"open", "query", "explain"} {
		for _, target := range []string{"local-pg", "fake://u:s3cret@h/db"} {
			for _, msg := range msgs {
				t.Run(op+"/"+target+"/"+msg, func(t *testing.T) {
					h := newHarness(t)
					h.env = map[string]string{secrets.EnvName("local-pg"): "s3cret"}
					h.fake.FailNext(op, driver.Wrap(driver.ErrSyntax, "fake", op, errors.New(msg)))
					args := []string{"query", target, selectPeople}
					if op == "explain" {
						args = append(args, "--explain")
					}
					_, errOut, code := h.exec(t, context.Background(), args...)
					want := 3 // the error kind survives redaction
					if op == "open" {
						want = 2
					}
					if code != want {
						t.Errorf("code=%d, want %d", code, want)
					}
					if strings.Contains(errOut, "s3cret") || !strings.Contains(errOut, "xxxxx") {
						t.Errorf("stderr = %q, want redacted password", errOut)
					}
				})
			}
		}
	}
}

func TestQueryUnknownConnectionRedactsArg(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "query", "app:s3cret@tcp(h)/db", selectPeople)
	if code != 1 || strings.Contains(errOut, "s3cret") || !strings.Contains(errOut, "app:xxxxx@tcp(h)/db") {
		t.Fatalf("code=%d stderr=%q, want 1 and a redacted argument", code, errOut)
	}
}

func TestQueryErrorEscapesControls(t *testing.T) {
	h := newHarness(t)
	h.fake.FailNext("query", driver.Wrap(driver.ErrSyntax, "fake", "query",
		errors.New("near \x1b[31mRED\x07 \u009b2J\x7f\nline2")))
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople)
	if code != 3 {
		t.Fatalf("code=%d", code)
	}
	if strings.ContainsAny(errOut[:len(errOut)-1], "\x1b\x07\x7f\u009b\n") {
		t.Fatalf("stderr has raw control characters: %q", errOut)
	}
	for _, want := range []string{`\x1b[31mRED`, `\x07`, `\u009b2J`, `\x7f`, " line2"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, want %q", errOut, want)
		}
	}
}

func TestQueryInterrupt(t *testing.T) {
	h := newHarness(t)
	h.fake.SetLatency(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, errOut, code := h.exec(t, ctx, "query", "local-pg", selectPeople)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v, want < 2s", d)
	}
	if code != 3 || !strings.Contains(errOut, "canceled") {
		t.Fatalf("code=%d stderr=%q, want 3 and canceled", code, errOut)
	}
}

func TestQueryTimeout(t *testing.T) {
	h := newHarness(t)
	h.fake.SetLatency(10 * time.Second)
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--timeout", "50ms")
	if code != 3 || !strings.Contains(errOut, "deadline exceeded") {
		t.Fatalf("code=%d stderr=%q, want 3 and deadline exceeded", code, errOut)
	}
}

func TestQueryUnknownConnection(t *testing.T) {
	h := newHarness(t)
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pgg", selectPeople)
	want := "error: unknown connection \"local-pgg\" (did you mean \"local-pg\"?)\n"
	if code != 1 || errOut != want {
		t.Fatalf("code=%d stderr=%q, want 1 and %q", code, errOut, want)
	}
	_, errOut, code = h.exec(t, context.Background(), "query", "zzzzzzzz", selectPeople)
	if want := "error: unknown connection \"zzzzzzzz\"\n"; code != 1 || errOut != want {
		t.Fatalf("code=%d stderr=%q, want 1 and %q", code, errOut, want)
	}
}

func TestQueryMissingConfig(t *testing.T) {
	h := newHarness(t)
	h.cfg = filepath.Join(t.TempDir(), "absent.yaml")
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople)
	if want := "error: unknown connection \"local-pg\"\n"; code != 1 || errOut != want {
		t.Fatalf("code=%d stderr=%q, want 1 and %q", code, errOut, want)
	}
}

func TestQueryExplain(t *testing.T) {
	h := newHarness(t)
	out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--explain")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	golden(t, "query_explain.golden", out)
	if !strings.Contains(out, "FAKE SCAN") {
		t.Errorf("no plan in output:\n%s", out)
	}
}

func TestQueryDebugLog(t *testing.T) {
	h := newHarness(t)
	state := t.TempDir()
	reg := driver.NewRegistry()
	_ = reg.Add(h.fake.Driver())
	var out, errOut bytes.Buffer
	deps := cli.Deps{Registry: reg, Home: t.TempDir(), Getenv: func(k string) string {
		if k == "XDG_STATE_HOME" {
			return state
		}
		return ""
	}}
	root := cli.NewRoot(cli.BuildInfo{}, cli.Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, deps)
	code := cli.Execute(context.Background(), root, []string{"--config", h.cfg, "--debug", "query", "local-pg", selectPeople})
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	path := filepath.Join(state, "tobira", "tobira.log")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", st.Mode().Perm())
	}
	if ds, _ := os.Stat(filepath.Dir(path)); ds.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode = %v, want 0700", ds.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "query start") {
		t.Errorf("log missing entries: %s", data)
	}
}

func TestQueryDebugLogOmitsSecrets(t *testing.T) {
	h := newHarness(t)
	state := t.TempDir()
	h.env = map[string]string{"XDG_STATE_HOME": state, secrets.EnvName("local-pg"): "hunter2pw"}
	// A successful run by name (password from the environment), then a failed
	// run by DSN with the password embedded.
	if _, errOut, code := h.exec(t, context.Background(), "--debug", "query", "local-pg", selectPeople); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	h.fake.FailNext("query", driver.Wrap(driver.ErrSyntax, "fake", "query", errors.New("bad postgres://u:dsnpw@h/db")))
	if _, _, code := h.exec(t, context.Background(), "--debug", "query", "fake://u:dsnpw@h/db", selectPeople); code != 3 {
		t.Fatalf("code=%d, want 3", code)
	}
	data, err := os.ReadFile(filepath.Join(state, "tobira", "tobira.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Count(log, "query start") != 2 {
		t.Fatalf("log does not cover both runs:\n%s", log)
	}
	for _, leak := range []string{"hunter2pw", "dsnpw", "fake://"} {
		if strings.Contains(log, leak) {
			t.Errorf("debug log contains %q:\n%s", leak, log)
		}
	}
}

func TestUsageErrorsRedactDSN(t *testing.T) {
	const dsn = "postgres://u:s3cret@h/db"
	tests := []struct {
		name string
		args []string
	}{
		{"forgot query", []string{dsn, selectPeople}},
		{"bad format", []string{"query", "local-pg", selectPeople, "--format", dsn}},
		{"bad limit", []string{"query", "local-pg", selectPeople, "--limit", dsn}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			_, errOut, code := h.exec(t, context.Background(), tt.args...)
			if code == 0 || errOut == "" {
				t.Fatalf("code=%d stderr=%q, want a usage error", code, errOut)
			}
			if strings.Contains(errOut, "s3cret") {
				t.Errorf("stderr leaks the password: %q", errOut)
			}
		})
	}
}
