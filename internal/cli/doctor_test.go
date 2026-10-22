package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
)

// doctorSecret is a known secret value: no doctor output may contain it.
const doctorSecret = "s3cr3t-hunter2-doctor-value"

// cleanConfig is a config every check passes with --no-ping.
const cleanConfig = `connections:
  - name: local-pg
    driver: fake
    dsn: fake://x
`

type doctorCheck struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
}

type doctorOutput struct {
	Checks []doctorCheck `json:"checks"`
}

// doctorEnv is a harness for doctor with known state and plugin
// directories, a sqlite stand-in for history, and cfgText as the config.
type doctorEnv struct {
	*harness
	state, xdg string
}

func newDoctorEnv(t *testing.T, cfgText string) *doctorEnv {
	t.Helper()
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &doctorEnv{harness: h, state: t.TempDir(), xdg: t.TempDir()}
	h.env = map[string]string{"XDG_STATE_HOME": d.state, "XDG_CONFIG_HOME": d.xdg}
	h.extra = []driver.Driver{stubSQLite{}}
	return d
}

func (d *doctorEnv) pluginsDir() string { return filepath.Join(d.xdg, "tobira", "plugins") }

// run runs doctor and decodes its JSON output.
func (d *doctorEnv) run(t *testing.T, args ...string) (doctorOutput, string, string, int) {
	t.Helper()
	out, errOut, code := d.exec(t, context.Background(), append([]string{"doctor"}, args...)...)
	var rep doctorOutput
	if !d.tty {
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("doctor output is not JSON: %v\n%s\nstderr=%s", err, out, errOut)
		}
	}
	return rep, out, errOut, code
}

func (r doctorOutput) find(section, name string) (doctorCheck, bool) {
	for _, c := range r.Checks {
		if c.Section == section && c.Name == name {
			return c, true
		}
	}
	return doctorCheck{}, false
}

func (r doctorOutput) section(section string) []doctorCheck {
	var out []doctorCheck
	for _, c := range r.Checks {
		if c.Section == section {
			out = append(out, c)
		}
	}
	return out
}

func mustFind(t *testing.T, r doctorOutput, section, name string) doctorCheck {
	t.Helper()
	c, ok := r.find(section, name)
	if !ok {
		t.Fatalf("no %s/%s check in %+v", section, name, r.Checks)
	}
	return c
}

func wantStatus(t *testing.T, c doctorCheck, status, detailPart string) {
	t.Helper()
	if c.Status != status || !strings.Contains(c.Detail, detailPart) {
		t.Errorf("%s/%s = %s %q, want %s containing %q", c.Section, c.Name, c.Status, c.Detail, status, detailPart)
	}
}

// normalize replaces the test's temporary directories with placeholders.
func (d *doctorEnv) normalize(s string) string {
	for dir, name := range map[string]string{d.cfg: "$CONFIG", d.state: "$STATE", d.xdg: "$XDG"} {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			s = strings.ReplaceAll(s, real, name)
		}
		s = strings.ReplaceAll(s, dir, name)
	}
	return s
}

func TestDoctorAllOKExitsZero(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	rep, _, errOut, code := d.run(t, "--no-ping")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q checks=%+v", code, errOut, rep.Checks)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing", errOut)
	}
	for _, c := range rep.Checks {
		if c.Status != "ok" {
			t.Errorf("%+v is not ok", c)
		}
	}
	for _, s := range []string{"config", "connections", "refs", "drivers", "plugins", "ai", "mcp", "history", "permissions"} {
		if len(rep.section(s)) == 0 {
			t.Errorf("no %s section", s)
		}
	}
}

func TestDoctorExitCodeAggregation(t *testing.T) {
	t.Run("warn exits 1", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no unix modes")
		}
		d := newDoctorEnv(t, cleanConfig)
		if err := os.Chmod(d.cfg, 0o644); err != nil {
			t.Fatal(err)
		}
		rep, _, errOut, code := d.run(t, "--no-ping")
		if code != 1 || errOut != "" {
			t.Errorf("code=%d stderr=%q, want 1 and no error line; checks=%+v", code, errOut, rep.Checks)
		}
	})
	t.Run("fail exits 2", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"  - name: other\n    driver: fake\n    dsn: fake://y\n    password: env:TOBIRA_TEST_UNSET_PW\n")
		rep, _, errOut, code := d.run(t, "--no-ping")
		if code != 2 || errOut != "" {
			t.Errorf("code=%d stderr=%q, want 2 and no error line; checks=%+v", code, errOut, rep.Checks)
		}
	})
	t.Run("fail beats warn", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no unix modes")
		}
		d := newDoctorEnv(t, testConfig) // nodrv uses an unknown driver
		if err := os.Chmod(d.cfg, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, code := d.run(t, "--no-ping"); code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
	})
}

func TestDoctorConfig(t *testing.T) {
	t.Run("loaded", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "config", "file"), "ok", d.cfg)
	})
	t.Run("missing file", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		_ = os.Remove(d.cfg)
		rep, _, _, code := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "config", "file"), "ok", "no config file")
		if code != 0 {
			t.Errorf("code=%d, want 0 on a fresh install; %+v", code, rep.Checks)
		}
	})
	t.Run("does not parse", func(t *testing.T) {
		d := newDoctorEnv(t, "connections: [\n")
		rep, _, _, code := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "config", "file"), "fail", "")
		if code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
		// The checks that do not need the config still run.
		if len(rep.section("drivers")) == 0 || len(rep.section("history")) == 0 {
			t.Errorf("drivers or history missing after a config error: %+v", rep.Checks)
		}
	})
	t.Run("broken entry", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"  - name: bad\n    driver: fake\n    dsn: fake://h/db\n    password: "+doctorSecret+"\n")
		rep, out, errOut, code := d.run(t, "--no-ping")
		c := mustFind(t, rep, "config", "bad")
		wantStatus(t, c, "fail", "line ")
		if code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
		if strings.Contains(out+errOut, doctorSecret) {
			t.Error("a broken entry's literal password reached the output")
		}
	})
	t.Run("password in a DSN warns", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"  - name: inline\n    driver: fake\n    dsn: fake://u:"+doctorSecret+"@h/db\n")
		rep, out, errOut, code := d.run(t, "--no-ping")
		c := mustFind(t, rep, "config", "inline")
		wantStatus(t, c, "warn", "password ref")
		if code != 1 {
			t.Errorf("code=%d, want 1 (a warning, not a failure); %+v", code, rep.Checks)
		}
		// The connection itself still loads and is checked.
		wantStatus(t, mustFind(t, rep, "connections", "inline"), "ok", "")
		if strings.Contains(out+errOut, doctorSecret) {
			t.Error("the DSN password reached the output")
		}
	})
	t.Run("DSN without a password does not warn", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		rep, _, _, _ := d.run(t, "--no-ping")
		if c, ok := rep.find("config", "local-pg"); ok {
			t.Errorf("unexpected config check for a DSN without a password: %+v", c)
		}
	})
}

func TestDoctorRefs(t *testing.T) {
	cfg := cleanConfig + `  - name: with-ref
    driver: fake
    dsn: fake://y
    password: env:DB_PW
  - name: missing-ref
    driver: fake
    dsn: fake://z
    password: env:TOBIRA_TEST_UNSET_PW
  - name: overridden
    driver: fake
    dsn: fake://w
    password: env:TOBIRA_TEST_UNSET_PW
`
	d := newDoctorEnv(t, cfg)
	t.Setenv("DB_PW", doctorSecret) // env: refs resolve through mamori, from the process
	d.env["TOBIRA_CONN_OVERRIDDEN_PASSWORD"] = doctorSecret
	rep, out, errOut, _ := d.run(t, "--no-ping")
	wantStatus(t, mustFind(t, rep, "refs", "connection with-ref"), "ok", "env ref")
	missing := mustFind(t, rep, "refs", "connection missing-ref")
	wantStatus(t, missing, "fail", "env ref")
	if strings.Contains(missing.Detail, "TOBIRA_TEST_UNSET_PW") {
		t.Errorf("detail %q names the ref's variable; only the scheme may be shown", missing.Detail)
	}
	wantStatus(t, mustFind(t, rep, "refs", "connection overridden"), "ok", "TOBIRA_CONN_OVERRIDDEN_PASSWORD")
	if _, ok := rep.find("refs", "connection local-pg"); ok {
		t.Error("a connection without a password ref has a refs row")
	}
	if strings.Contains(out+errOut, doctorSecret) {
		t.Fatal("a resolved secret reached the output")
	}
}

func TestDoctorRefsNoneConfigured(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	rep, _, _, _ := d.run(t, "--no-ping")
	refs := rep.section("refs")
	if len(refs) != 1 || refs[0].Status != "ok" || !strings.Contains(refs[0].Detail, "no secret refs") {
		t.Errorf("refs = %+v, want one ok row saying there are none", refs)
	}
}

// TestDoctorNeverPrintsSecrets runs every check that touches a secret, in
// both output modes and with --online, and looks for the value anywhere.
func TestDoctorNeverPrintsSecrets(t *testing.T) {
	cfg := cleanConfig + `  - name: with-ref
    driver: fake
    dsn: fake://y
    password: env:DB_PW
ai:
  provider: anthropic
  api_key: env:AI_KEY
mcp:
  expose: [local-pg]
  token: env:MCP_TOKEN
`
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			d := newDoctorEnv(t, cfg)
			d.tty = tty
			for _, k := range []string{"DB_PW", "AI_KEY", "MCP_TOKEN", "ANTHROPIC_API_KEY"} {
				d.env[k] = doctorSecret
				t.Setenv(k, doctorSecret)
			}
			d.newAssistant = stubAssistantFactory
			d.checkAI = func(context.Context, config.AI, cli.AICheckDeps) error {
				return fmt.Errorf("provider said: bad key %s\x1b[31m", doctorSecret)
			}
			d.wrapDriver = func(drv driver.Driver) driver.Driver {
				return openFunc{drv, func(ctx context.Context, c driver.ConnConfig) (driver.Conn, error) {
					return nil, fmt.Errorf("auth failed for password %s", c.Password.Reveal())
				}}
			}
			_, out, errOut, _ := d.run(t, "--online")
			if strings.Contains(out+errOut, doctorSecret) {
				t.Fatalf("a secret reached the output:\n%s\n%s", out, errOut)
			}
			if strings.Contains(out, "\x1b") {
				t.Errorf("an escape character reached the output:\n%q", out)
			}
		})
	}
}

// openFunc replaces a driver's Open.
type openFunc struct {
	driver.Driver
	open func(context.Context, driver.ConnConfig) (driver.Conn, error)
}

func (o openFunc) Open(ctx context.Context, c driver.ConnConfig) (driver.Conn, error) {
	return o.open(ctx, c)
}

func TestDoctorConnections(t *testing.T) {
	cfg := cleanConfig + `  - name: down
    driver: fake
    dsn: fake://down
  - name: nodrv
    driver: nope
    dsn: nope://x
`
	d := newDoctorEnv(t, cfg)
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(ctx context.Context, c driver.ConnConfig) (driver.Conn, error) {
			if c.DSN == "fake://down" {
				return nil, driver.Wrap(driver.ErrConnection, "fake", "open", errors.New("connection refused"))
			}
			return drv.Open(ctx, c)
		}}
	}
	rep, _, _, code := d.run(t)
	wantStatus(t, mustFind(t, rep, "connections", "local-pg"), "ok", "reachable")
	wantStatus(t, mustFind(t, rep, "connections", "down"), "fail", "connection refused")
	wantStatus(t, mustFind(t, rep, "connections", "nodrv"), "fail", "unknown driver")
	if code != 2 {
		t.Errorf("code=%d, want 2", code)
	}
}

func TestDoctorPingsOpenReadOnly(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	var mu sync.Mutex
	var got []driver.ConnConfig
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(ctx context.Context, c driver.ConnConfig) (driver.Conn, error) {
			mu.Lock()
			got = append(got, c)
			mu.Unlock()
			return drv.Open(ctx, c)
		}}
	}
	if _, _, errOut, code := d.run(t); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(got) != 1 || !got[0].ReadOnly {
		t.Errorf("opens = %+v, want one read-only open", got)
	}
}

func TestDoctorNoPing(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	var opens atomic.Int32
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(ctx context.Context, c driver.ConnConfig) (driver.Conn, error) {
			opens.Add(1)
			return drv.Open(ctx, c)
		}}
	}
	rep, _, _, code := d.run(t, "--no-ping")
	if opens.Load() != 0 {
		t.Errorf("--no-ping opened %d connections", opens.Load())
	}
	wantStatus(t, mustFind(t, rep, "connections", "local-pg"), "ok", "not pinged")
	if code != 0 {
		t.Errorf("code=%d", code)
	}
}

func TestDoctorPingTimeout(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(ctx context.Context, _ driver.ConnConfig) (driver.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	}
	start := time.Now()
	rep, _, _, code := d.run(t, "--timeout", "100ms")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("doctor took %s with a 100ms ping timeout", took)
	}
	wantStatus(t, mustFind(t, rep, "connections", "local-pg"), "fail", "timed out after 100ms")
	if code != 2 {
		t.Errorf("code=%d, want 2", code)
	}
}

func TestDoctorUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{{"--timeout", "0s"}, {"--bogus"}, {"extra-arg"}} {
		d := newDoctorEnv(t, cleanConfig)
		_, errOut, code := d.exec(t, context.Background(), append([]string{"doctor"}, args...)...)
		if code != 2 || !strings.HasPrefix(errOut, "error: ") {
			t.Errorf("%v: code=%d stderr=%q, want a usage error with exit 2 (1 means warnings)", args, code, errOut)
		}
	}
}

func TestDoctorPingConcurrencyBound(t *testing.T) {
	var b strings.Builder
	b.WriteString("connections:\n")
	const n = 20
	for i := range n {
		fmt.Fprintf(&b, "  - name: c%02d\n    driver: fake\n    dsn: fake://h%d\n", i, i)
	}
	d := newDoctorEnv(t, b.String())
	var inFlight, peak, total atomic.Int32
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(ctx context.Context, c driver.ConnConfig) (driver.Conn, error) {
			cur := inFlight.Add(1)
			defer inFlight.Add(-1)
			total.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			return drv.Open(ctx, c)
		}}
	}
	rep, _, _, code := d.run(t)
	if code != 0 {
		t.Fatalf("code=%d checks=%+v", code, rep.Checks)
	}
	if total.Load() != n {
		t.Errorf("opened %d connections, want %d", total.Load(), n)
	}
	if p := peak.Load(); p > 8 || p < 2 {
		t.Errorf("peak concurrent pings = %d, want between 2 and 8", p)
	}
	if got := len(rep.section("connections")); got != n {
		t.Errorf("connections rows = %d, want %d", got, n)
	}
}

func TestDoctorDrivers(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	rep, _, _, _ := d.run(t, "--no-ping")
	wantStatus(t, mustFind(t, rep, "drivers", "fake"), "ok", "builtin")
	wantStatus(t, mustFind(t, rep, "drivers", "sqlite"), "ok", "builtin")
}

func TestDoctorPlugins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script plugins")
	}
	d := newDoctorEnv(t, cleanConfig)
	fakePlugin(t, d.pluginsDir(), "extra")
	fakePlugin(t, d.pluginsDir(), "fake") // a built-in has the name
	rep, _, _, code := d.run(t, "--no-ping")
	extra := mustFind(t, rep, "plugins", "extra")
	wantStatus(t, extra, "ok", "tobira-driver-extra")
	if !strings.Contains(extra.Detail, "not started") {
		t.Errorf("extra detail = %q, want it to say the plugin was not started", extra.Detail)
	}
	wantStatus(t, mustFind(t, rep, "plugins", "fake"), "warn", "built-in")
	if code != 1 {
		t.Errorf("code=%d, want 1 for the conflict", code)
	}
	if _, ok := rep.find("drivers", "extra"); ok {
		t.Error("a plugin is listed as a built-in driver")
	}
}

func TestDoctorPluginsOnlineResolvesInfo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script plugins")
	}
	d := newDoctorEnv(t, cleanConfig)
	fakePlugin(t, d.pluginsDir(), "extra") // exits at once: it cannot answer
	rep, _, _, code := d.run(t, "--no-ping", "--online")
	wantStatus(t, mustFind(t, rep, "plugins", "extra"), "fail", "tobira-driver-extra")
	if code != 2 {
		t.Errorf("code=%d, want 2", code)
	}
}

func TestDoctorPluginsNone(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	rep, _, _, _ := d.run(t, "--no-ping")
	p := rep.section("plugins")
	if len(p) != 1 || p[0].Status != "ok" || !strings.Contains(p[0].Detail, "none found") {
		t.Errorf("plugins = %+v, want one ok row saying none were found", p)
	}
}

func TestDoctorPluginsDisabled(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig+"plugins:\n  enabled: false\n")
	rep, _, _, _ := d.run(t, "--no-ping")
	wantStatus(t, mustFind(t, rep, "plugins", "plugins"), "ok", "disabled")
}

func stubAssistantFactory(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
	return nil, errors.New("not used by doctor")
}

func TestDoctorAIOffline(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "ai"), "ok", "not configured")
	})
	t.Run("key from a ref", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"ai:\n  provider: anthropic\n  model: claude-x\n  api_key: env:AI_KEY\n")
		t.Setenv("AI_KEY", doctorSecret)
		d.newAssistant = stubAssistantFactory
		called := false
		d.checkAI = func(context.Context, config.AI, cli.AICheckDeps) error { called = true; return nil }
		rep, out, _, code := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "provider"), "ok", "anthropic")
		wantStatus(t, mustFind(t, rep, "ai", "model"), "ok", "claude-x")
		wantStatus(t, mustFind(t, rep, "ai", "api_key"), "ok", "env ref")
		if _, ok := rep.find("ai", "online"); ok || called {
			t.Error("the online check ran without --online")
		}
		if code != 0 || strings.Contains(out, doctorSecret) {
			t.Errorf("code=%d (want 0), or the key reached the output", code)
		}
	})
	t.Run("key from the environment", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"ai:\n  provider: anthropic\n")
		d.env["ANTHROPIC_API_KEY"] = doctorSecret
		d.newAssistant = stubAssistantFactory
		rep, out, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "api_key"), "ok", "ANTHROPIC_API_KEY")
		wantStatus(t, mustFind(t, rep, "ai", "model"), "ok", "provider default")
		if strings.Contains(out, doctorSecret) {
			t.Error("the key reached the output")
		}
	})
	t.Run("key missing", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"ai:\n  provider: openai\n")
		d.newAssistant = stubAssistantFactory
		rep, _, _, code := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "api_key"), "fail", "OPENAI_API_KEY")
		if code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
	})
	t.Run("provider without a key", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"ai:\n  provider: ollama\n")
		d.newAssistant = stubAssistantFactory
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "api_key"), "ok", "no key")
	})
	t.Run("binary without AI", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"ai:\n  provider: ollama\n")
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "provider"), "warn", "no AI")
	})
	t.Run("embedder", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"ai:\n  provider: ollama\n  embedder: anthropic/x\n")
		d.newAssistant = stubAssistantFactory
		d.newEmbedder = func(context.Context, config.AI, cli.EmbedderDeps) (driver.Embedder, error) {
			return nil, errors.New("ai.embedder: anthropic has no embedders")
		}
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "embedder"), "fail", "no embedders")

		d.newEmbedder = func(context.Context, config.AI, cli.EmbedderDeps) (driver.Embedder, error) { return nil, nil }
		rep, _, _, _ = d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "embedder"), "ok", "anthropic/x")
	})
}

func TestDoctorAIOnline(t *testing.T) {
	cfg := cleanConfig + "ai:\n  provider: anthropic\n  api_key: env:AI_KEY\n"
	t.Run("reachable", func(t *testing.T) {
		d := newDoctorEnv(t, cfg)
		t.Setenv("AI_KEY", doctorSecret)
		d.newAssistant = stubAssistantFactory
		var got config.AI
		d.checkAI = func(_ context.Context, c config.AI, _ cli.AICheckDeps) error { got = c; return nil }
		rep, _, _, code := d.run(t, "--no-ping", "--online")
		wantStatus(t, mustFind(t, rep, "ai", "online"), "ok", "")
		if got.Provider != "anthropic" || code != 0 {
			t.Errorf("check got %+v, code=%d", got, code)
		}
	})
	t.Run("refused", func(t *testing.T) {
		d := newDoctorEnv(t, cfg)
		t.Setenv("AI_KEY", doctorSecret)
		d.newAssistant = stubAssistantFactory
		d.checkAI = func(context.Context, config.AI, cli.AICheckDeps) error {
			return fmt.Errorf("%w: HTTP 401", assistant.ErrAuth)
		}
		rep, _, _, code := d.run(t, "--no-ping", "--online")
		wantStatus(t, mustFind(t, rep, "ai", "online"), "fail", "HTTP 401")
		if code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
	})
	t.Run("binary without a check", func(t *testing.T) {
		d := newDoctorEnv(t, cfg)
		t.Setenv("AI_KEY", doctorSecret)
		d.newAssistant = stubAssistantFactory
		rep, _, _, _ := d.run(t, "--no-ping", "--online")
		wantStatus(t, mustFind(t, rep, "ai", "online"), "warn", "no online")
	})
}

func TestDoctorMCP(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "mcp", "mcp"), "ok", "nothing is exposed")
	})
	t.Run("names and token", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"mcp:\n  expose: [local-pg, gone]\n  allow_writes: [other]\n  token: env:MCP_TOKEN\n")
		t.Setenv("MCP_TOKEN", doctorSecret)
		rep, out, _, code := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "mcp", "expose local-pg"), "ok", "")
		wantStatus(t, mustFind(t, rep, "mcp", "expose gone"), "fail", "no connection")
		wantStatus(t, mustFind(t, rep, "mcp", "allow_writes other"), "warn", "not in mcp.expose")
		wantStatus(t, mustFind(t, rep, "mcp", "token"), "ok", "env ref")
		if code != 2 || strings.Contains(out, doctorSecret) {
			t.Errorf("code=%d (want 2), or the token reached the output", code)
		}
	})
	t.Run("token does not resolve", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"mcp:\n  expose: [local-pg]\n  token: env:MCP_TOKEN\n")
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "mcp", "token"), "fail", "env ref")
	})
	t.Run("no token", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"mcp:\n  expose: [local-pg]\n")
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "mcp", "token"), "ok", "generates")
	})
}

func TestDoctorHistory(t *testing.T) {
	t.Run("not created yet", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "history", "db"), "ok", "not created yet")
		if _, err := os.Stat(filepath.Join(d.state, "tobira", "history.db")); !os.IsNotExist(err) {
			t.Errorf("doctor created the history file (stat err=%v)", err)
		}
	})
	t.Run("opens", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		path := filepath.Join(d.state, "tobira", "history.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 2048), 0o600); err != nil {
			t.Fatal(err)
		}
		rep, _, _, _ := d.run(t, "--no-ping")
		c := mustFind(t, rep, "history", "db")
		wantStatus(t, c, "ok", "schema v2")
		if !strings.Contains(c.Detail, "2.0 KiB") {
			t.Errorf("detail %q does not give the size", c.Detail)
		}
	})
	t.Run("no sqlite driver", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		d.extra = nil
		path := filepath.Join(d.state, "tobira", "history.db")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, nil, 0o600)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "history", "db"), "warn", "sqlite")
	})
}

func TestDoctorPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	t.Run("private files", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "permissions", "config"), "ok", "0600")
		wantStatus(t, mustFind(t, rep, "permissions", "history"), "ok", "does not exist")
		wantStatus(t, mustFind(t, rep, "permissions", "plugins dir"), "ok", "does not exist")
	})
	t.Run("readable config and history", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		_ = os.Chmod(d.cfg, 0o644)
		path := filepath.Join(d.state, "tobira", "history.db")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, nil, 0o640)
		_ = os.Chmod(path, 0o640)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "permissions", "config"), "warn", "readable")
		wantStatus(t, mustFind(t, rep, "permissions", "history"), "warn", "group")
	})
	t.Run("world-writable plugins dir", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		if err := os.MkdirAll(d.pluginsDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.pluginsDir(), 0o777); err != nil {
			t.Fatal(err)
		}
		rep, _, _, code := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "permissions", "plugins dir"), "fail", "world-writable")
		if code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
	})
	t.Run("group-writable plugins dir", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig)
		_ = os.MkdirAll(d.pluginsDir(), 0o700)
		_ = os.Chmod(d.pluginsDir(), 0o770)
		rep, _, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "permissions", "plugins dir"), "warn", "group-writable")
	})
}

// goldenConfig shows every section with a mix of statuses.
const goldenConfig = `connections:
  - name: local-pg
    driver: fake
    dsn: fake://x
    password: env:DB_PW
  - name: nodrv
    driver: nope
    dsn: nope://x
ai:
  provider: anthropic
  api_key: env:TOBIRA_TEST_UNSET_AI_KEY
mcp:
  expose: [local-pg, gone]
`

func goldenDoctor(t *testing.T, tty bool) (string, int) {
	t.Helper()
	d := newDoctorEnv(t, goldenConfig)
	d.tty = tty
	t.Setenv("DB_PW", doctorSecret)
	d.newAssistant = stubAssistantFactory
	_, out, errOut, code := d.run(t, "--no-ping")
	if errOut != "" {
		t.Errorf("stderr = %q", errOut)
	}
	return d.normalize(out), code
}

func TestDoctorJSONGolden(t *testing.T) {
	out, code := goldenDoctor(t, false)
	if code != 2 {
		t.Errorf("code=%d, want 2", code)
	}
	golden(t, "doctor.json.golden", out)
}

func TestDoctorTTYGolden(t *testing.T) {
	out, code := goldenDoctor(t, true)
	if code != 2 {
		t.Errorf("code=%d, want 2", code)
	}
	golden(t, "doctor_tty.golden", out)
}

// testProvider is a mamori provider for a scheme no real store has.
type testProvider struct {
	scheme  string
	resolve func(context.Context, mamori.Ref) (mamori.Value, error)
}

func (p testProvider) Scheme() string { return p.scheme }
func (p testProvider) Resolve(ctx context.Context, r mamori.Ref) (mamori.Value, error) {
	return p.resolve(ctx, r)
}

// remoteCalls counts resolutions through the remotex: scheme.
var remoteCalls atomic.Int32

// registerTestProviders registers, once per test binary, remotex: (a
// remote store that answers doctorSecret) and hangx: (a store that ignores
// ctx and answers only after two seconds).
var registerTestProviders = sync.OnceFunc(func() {
	mamori.Register(testProvider{"remotex", func(context.Context, mamori.Ref) (mamori.Value, error) {
		remoteCalls.Add(1)
		return mamori.Value{Bytes: []byte(doctorSecret)}, nil
	}})
	mamori.Register(testProvider{"hangx", func(context.Context, mamori.Ref) (mamori.Value, error) {
		time.Sleep(2 * time.Second)
		return mamori.Value{Bytes: []byte(doctorSecret)}, nil
	}})
})

func TestDoctorNoPingSkipsRemoteRefs(t *testing.T) {
	registerTestProviders()
	cfg := cleanConfig + `  - name: remote
    driver: fake
    dsn: fake://r
    password: remotex://vault/item
  - name: chained
    driver: fake
    dsn: fake://c
    password: env:TOBIRA_TEST_UNSET_PW,remotex://vault/other
  - name: local
    driver: fake
    dsn: fake://l
    password: env:DB_PW
mcp:
  expose: [local-pg]
  token: remotex://vault/token
`
	d := newDoctorEnv(t, cfg)
	t.Setenv("DB_PW", doctorSecret)
	before := remoteCalls.Load()
	rep, out, _, code := d.run(t, "--no-ping")
	if n := remoteCalls.Load() - before; n != 0 {
		t.Errorf("--no-ping resolved %d remote refs", n)
	}
	wantStatus(t, mustFind(t, rep, "refs", "connection remote"), "skip", "remote ref, not resolved with --no-ping")
	wantStatus(t, mustFind(t, rep, "refs", "connection chained"), "skip", "remote ref, not resolved with --no-ping")
	wantStatus(t, mustFind(t, rep, "refs", "connection local"), "ok", "env ref resolves")
	wantStatus(t, mustFind(t, rep, "mcp", "token"), "skip", "not resolved with --no-ping")
	if code != 0 {
		t.Errorf("code=%d, want 0: a skip is not a warning; %+v", code, rep.Checks)
	}
	if strings.Contains(out, doctorSecret) {
		t.Error("a secret reached the output")
	}

	// Without --no-ping the remote ref is resolved.
	before = remoteCalls.Load()
	rep, _, _, _ = d.run(t)
	wantStatus(t, mustFind(t, rep, "refs", "connection remote"), "ok", "resolves")
	if remoteCalls.Load() == before {
		t.Error("the remote ref was not resolved without --no-ping")
	}
}

// TestDoctorUnknownRefSchemeFails: a ref whose scheme no provider in this
// build handles is a fail that says so, with or without --no-ping, never a
// skipped "remote ref". The scheme name is not echoed.
func TestDoctorUnknownRefSchemeFails(t *testing.T) {
	registerTestProviders()
	cfg := cleanConfig + `  - name: nosuch
    driver: fake
    dsn: fake://n
    password: nosuchstore://vault/item
  - name: chained
    driver: fake
    dsn: fake://c
    password: env:TOBIRA_TEST_UNSET_PW,nosuchstore://vault/other
  - name: remote
    driver: fake
    dsn: fake://r
    password: remotex://vault/item
`
	d := newDoctorEnv(t, cfg)
	for _, args := range [][]string{{"--no-ping"}, nil} {
		rep, out, _, code := d.run(t, args...)
		for _, name := range []string{"connection nosuch", "connection chained"} {
			c := mustFind(t, rep, "refs", name)
			wantStatus(t, c, "fail", "unknown ref scheme")
			if strings.Contains(c.Detail, "remote ref") || strings.Contains(c.Detail, "nosuchstore") {
				t.Errorf("args %v: %s detail = %q", args, name, c.Detail)
			}
		}
		if args != nil {
			wantStatus(t, mustFind(t, rep, "refs", "connection remote"), "skip", "remote ref, not resolved with --no-ping")
		}
		if code != 2 {
			t.Errorf("args %v: code=%d, want 2", args, code)
		}
		if strings.Contains(out, "vault/item") {
			t.Errorf("args %v: the ref reached the output", args)
		}
	}
}

func TestDoctorTTYSummaryCountsSkips(t *testing.T) {
	registerTestProviders()
	d := newDoctorEnv(t, cleanConfig+"    password: remotex://vault/item\n")
	d.tty = true
	_, out, _, _ := d.run(t, "--no-ping")
	if !strings.Contains(out, "1 skip") {
		t.Errorf("summary does not count the skip:\n%s", out)
	}
}

// TestDoctorTimeoutsHoldWhenCtxIsIgnored uses a driver and a secret store
// that ignore ctx: each probe must still end at --timeout.
func TestDoctorTimeoutsHoldWhenCtxIsIgnored(t *testing.T) {
	registerTestProviders()
	cfg := cleanConfig + `  - name: hung-store
    driver: fake
    dsn: fake://s
    password: hangx://vault/item
`
	d := newDoctorEnv(t, cfg)
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(context.Context, driver.ConnConfig) (driver.Conn, error) {
			time.Sleep(2 * time.Second) // ignores ctx
			return nil, errors.New("too late")
		}}
	}
	start := time.Now()
	rep, _, _, code := d.run(t, "--timeout", "100ms")
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("doctor took %s with a 100ms timeout and probes that ignore ctx", took)
	}
	wantStatus(t, mustFind(t, rep, "connections", "local-pg"), "fail", "timed out after 100ms")
	wantStatus(t, mustFind(t, rep, "refs", "connection hung-store"), "fail", "timed out after 100ms")
	if code != 2 {
		t.Errorf("code=%d, want 2", code)
	}
}

// TestDoctorMasksShortSecrets: a secret shorter than the redactor's
// minimum is still masked when it is a value doctor read.
func TestDoctorMasksShortSecrets(t *testing.T) {
	const short = "Q9z"
	d := newDoctorEnv(t, cleanConfig+"    password: env:DB_PW\n")
	t.Setenv("DB_PW", short)
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(_ context.Context, c driver.ConnConfig) (driver.Conn, error) {
			return nil, fmt.Errorf("auth failed: %s", c.Password.Reveal())
		}}
	}
	_, out, errOut, _ := d.run(t)
	if strings.Contains(out+errOut, short) {
		t.Errorf("a short secret reached the output:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("auth failed")) {
		t.Errorf("the ping error is missing:\n%s", out)
	}
}

func TestDoctorEmbedderProviderKey(t *testing.T) {
	cfg := cleanConfig + "ai:\n  provider: anthropic\n  embedder: googleai/text-embedding-005\n"
	t.Run("missing", func(t *testing.T) {
		d := newDoctorEnv(t, cfg)
		d.env["ANTHROPIC_API_KEY"] = doctorSecret
		d.newAssistant = stubAssistantFactory
		d.newEmbedder = func(context.Context, config.AI, cli.EmbedderDeps) (driver.Embedder, error) { return nil, nil }
		rep, _, _, code := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "embedder api_key"), "fail", "GEMINI_API_KEY")
		if code != 2 {
			t.Errorf("code=%d, want 2", code)
		}
	})
	t.Run("present", func(t *testing.T) {
		d := newDoctorEnv(t, cfg)
		d.env["ANTHROPIC_API_KEY"] = doctorSecret
		d.env["GOOGLE_API_KEY"] = doctorSecret + "-g"
		d.newAssistant = stubAssistantFactory
		d.newEmbedder = func(context.Context, config.AI, cli.EmbedderDeps) (driver.Embedder, error) { return nil, nil }
		rep, out, _, _ := d.run(t, "--no-ping")
		wantStatus(t, mustFind(t, rep, "ai", "embedder api_key"), "ok", "GOOGLE_API_KEY")
		if strings.Contains(out, doctorSecret) {
			t.Error("a key reached the output")
		}
	})
	t.Run("same provider has no extra row", func(t *testing.T) {
		d := newDoctorEnv(t, cleanConfig+"ai:\n  provider: googleai\n  embedder: text-embedding-005\n")
		d.env["GEMINI_API_KEY"] = doctorSecret
		d.newAssistant = stubAssistantFactory
		d.newEmbedder = func(context.Context, config.AI, cli.EmbedderDeps) (driver.Embedder, error) { return nil, nil }
		rep, _, _, _ := d.run(t, "--no-ping")
		if _, ok := rep.find("ai", "embedder api_key"); ok {
			t.Error("an embedder of ai.provider got its own key row")
		}
	})
}

// TestDoctorNoPingNeverStartsAPluginDriver: a connection on a plugin
// driver is reported from the stub Info under --no-ping; the plugin binary
// never runs.
func TestDoctorNoPingNeverStartsAPluginDriver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script plugins")
	}
	d := newDoctorEnv(t, cleanConfig+"  - name: viaplugin\n    driver: extra\n    dsn: extra://h\n")
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.MkdirAll(d.pluginsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(d.pluginsDir(), "tobira-driver-extra"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, _, _, _ := d.run(t, "--no-ping")
	wantStatus(t, mustFind(t, rep, "connections", "viaplugin"), "ok", "not pinged")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("doctor --no-ping started the plugin")
	}
}

// pgxStyleErr is how pgx reports a refused connection: the cause is on the
// lines after the first.
const pgxStyleErr = "failed to connect to `user=app database=app`:\n\t127.0.0.1:1 (localhost): dial error: dial tcp 127.0.0.1:1: connect: connection refused\n\t[::1]:1 (localhost): dial error: dial tcp [::1]:1: connect: connection refused"

// TestDoctorMultiLineErrorKeepsTheCause: a driver error over several lines
// is one line in the report, with the cause after the first line kept.
func TestDoctorMultiLineErrorKeepsTheCause(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(context.Context, driver.ConnConfig) (driver.Conn, error) {
			return nil, driver.Wrap(driver.ErrConnection, "postgres", "open", errors.New(pgxStyleErr))
		}}
	}
	rep, _, _, _ := d.run(t)
	c := mustFind(t, rep, "connections", "local-pg")
	wantStatus(t, c, "fail", "dial tcp 127.0.0.1:1: connect: connection refused")
	if strings.ContainsAny(c.Detail, "\r\n\t") {
		t.Errorf("detail is not one line: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "failed to connect to") || !strings.Contains(c.Detail, "[::1]:1") {
		t.Errorf("detail lost a line: %q", c.Detail)
	}
}

// TestDoctorCapsLongDetails: a very long error is cut at about 300
// characters and ends with an ellipsis, after redaction.
func TestDoctorCapsLongDetails(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig)
	long := "postgres://app:" + doctorSecret + "@db/app refused\n" + strings.Repeat("x", 2000)
	d.wrapDriver = func(drv driver.Driver) driver.Driver {
		return openFunc{drv, func(context.Context, driver.ConnConfig) (driver.Conn, error) {
			return nil, errors.New(long)
		}}
	}
	rep, out, _, _ := d.run(t)
	c := mustFind(t, rep, "connections", "local-pg")
	if n := len([]rune(c.Detail)); n > 310 || !strings.HasSuffix(c.Detail, "…") {
		t.Errorf("detail has %d runes, want a cut near 300 ending in …: %q", n, c.Detail)
	}
	if strings.Contains(out, doctorSecret) {
		t.Error("a secret reached the output")
	}
}

// TestDoctorResolvesEachRefOnce: the refs phase and the ping share one
// resolution per ref, so a keyring ref prompts once, not twice.
func TestDoctorResolvesEachRefOnce(t *testing.T) {
	registerTestProviders()
	d := newDoctorEnv(t, `connections:
  - name: remote
    driver: fake
    dsn: fake://r
    password: remotex://vault/once
`)
	before := remoteCalls.Load()
	rep, _, _, _ := d.run(t)
	wantStatus(t, mustFind(t, rep, "refs", "connection remote"), "ok", "resolves")
	wantStatus(t, mustFind(t, rep, "connections", "remote"), "ok", "reachable")
	if n := remoteCalls.Load() - before; n != 1 {
		t.Errorf("the ref was resolved %d times in one run, want 1", n)
	}
}

// TestDoctorHistoryReadOnly: doctor opens the history file read-only and
// leaves a loose mode as it found it.
func TestDoctorHistoryReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	d := newDoctorEnv(t, cleanConfig)
	var mu sync.Mutex
	var opened []driver.ConnConfig
	d.extra = []driver.Driver{openFunc{stubSQLite{}, func(ctx context.Context, c driver.ConnConfig) (driver.Conn, error) {
		mu.Lock()
		opened = append(opened, c)
		mu.Unlock()
		return stubSQLite{}.Open(ctx, c)
	}}}
	path := filepath.Join(d.state, "tobira", "history.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o644)
	_ = os.Chmod(filepath.Dir(path), 0o755)
	rep, _, _, _ := d.run(t, "--no-ping")
	wantStatus(t, mustFind(t, rep, "history", "db"), "ok", "writable")
	wantStatus(t, mustFind(t, rep, "permissions", "history"), "warn", "world-readable")
	if len(opened) != 1 || !opened[0].ReadOnly {
		t.Errorf("history opens = %+v, want one read-only open", opened)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("doctor changed the history mode to %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o755 {
		t.Errorf("doctor changed the state dir mode to %v", fi.Mode().Perm())
	}
}

// TestDoctorHistoryPath: the history row follows history.path.
func TestDoctorHistoryPath(t *testing.T) {
	d := newDoctorEnv(t, cleanConfig+"history:\n  path: $XDG_STATE_HOME/elsewhere/h.db\n")
	rep, _, _, _ := d.run(t, "--no-ping")
	wantStatus(t, mustFind(t, rep, "history", "db"), "ok", filepath.Join(d.state, "elsewhere", "h.db")+" (history.path)")
}
