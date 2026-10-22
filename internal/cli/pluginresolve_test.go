package cli_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/assistant/assistanttest"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/tui"
)

// The host's test plugin (plugin/host/testdata/plugin), built once: it
// serves the drivertest Fake with every capability, CapReadOnly and
// CapVectorSearcher included.
var (
	testPluginOnce sync.Once
	testPluginBin  string
	testPluginErr  error
)

// realPlugin copies the host's test plugin into dir as tobira-driver-<name>.
func realPlugin(t *testing.T, dir, name string) {
	t.Helper()
	testPluginOnce.Do(func() {
		out, err := os.MkdirTemp("", "tobira-cli-plugin")
		if err != nil {
			testPluginErr = err
			return
		}
		testPluginBin = filepath.Join(out, "plugin")
		cmd := exec.Command("go", "build", "-o", testPluginBin, ".")
		cmd.Dir = filepath.Join("..", "..", "plugin", "host", "testdata", "plugin")
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if b, err := cmd.CombinedOutput(); err != nil {
			testPluginErr = errors.New(err.Error() + "\n" + string(b))
		}
	})
	if testPluginErr != nil {
		t.Fatalf("build the test plugin: %v", testPluginErr)
	}
	b, err := os.ReadFile(testPluginBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tobira-driver-"+name), b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// recordingPlugin writes a plugin into dir that appends a line to the file
// it returns each time it starts, then exits (so it never answers).
func recordingPlugin(t *testing.T, dir, name string) string {
	t.Helper()
	skipOnWindows(t)
	starts := filepath.Join(t.TempDir(), "starts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho start >> '" + starts + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "tobira-driver-"+name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return starts
}

func assertNotStarted(t *testing.T, starts string) {
	t.Helper()
	if _, err := os.Stat(starts); !errors.Is(err, os.ErrNotExist) {
		b, _ := os.ReadFile(starts)
		t.Errorf("the plugin was started (%d times)", strings.Count(string(b), "\n"))
	}
}

// pluginHarness is a harness whose config holds testConfig, a plugins dir
// and extra connections.
func pluginHarness(t *testing.T, conns, extra string) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig + conns + "plugins:\n  dirs: [" + dir + "]\n" + extra
	if err := os.WriteFile(h.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.environ = []string{"TOBIRA_PLUGIN_TEST_KIND=vector"}
	if runtime.GOOS == "windows" {
		h.environ = append(h.environ, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	return h, dir
}

const pluginConns = "  - name: plug\n    driver: real\n    dsn: real://x\n  - name: gone\n    driver: gone\n    dsn: gone://x\n"

// tobira mcp resolves the real Info of the exposed plugin connections
// before it builds the tools, so their capabilities and kind are the
// plugin's, and a plugin whose Info fails makes its connection unavailable.
func TestMCPResolvesPluginInfo(t *testing.T) {
	h, dir := pluginHarness(t, pluginConns, "")
	realPlugin(t, dir, "real")
	starts := recordingPlugin(t, dir, "idle") // not exposed: never started
	recordingPlugin(t, dir, "gone")
	var got []cli.MCPOptions
	h.serveMCP = func(_ context.Context, o cli.MCPOptions) error {
		st := o.Manager.State("plug")
		if !driver.HasCapability(st.Info, driver.CapReadOnly) || !driver.HasCapability(st.Info, driver.CapVectorSearcher) ||
			st.Info.Kind != driver.KindVector || st.Info.Dialect != "fake" {
			t.Errorf("plug's Info when the server is built = %+v, want the plugin's", st.Info)
		}
		got = append(got, o)
		return nil
	}
	_, errOut, code := h.exec(t, context.Background(), "mcp", "--expose", "plug,gone,local-pg")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if len(got) != 1 {
		t.Fatalf("ServeMCP called %d times", len(got))
	}
	un := got[0].Unavailable
	if _, ok := un["plug"]; ok {
		t.Errorf("plug is unavailable: %v", un)
	}
	if _, ok := un["local-pg"]; ok {
		t.Errorf("a built-in driver's connection is unavailable: %v", un)
	}
	if why := un["gone"]; !strings.Contains(why, `driver plugin "gone"`) || strings.Contains(why, "\n") {
		t.Errorf("gone's reason = %q, want one line naming the plugin", why)
	}
	assertNotStarted(t, starts)
}

// tobira ask resolves the plugin of its connection first: the assistant's
// tools then see the real capabilities on their first call, and a plugin
// whose Info fails is a clear connection error before any model call.
func TestAskResolvesPluginInfo(t *testing.T) {
	h, dir := pluginHarness(t, pluginConns, "ai:\n  provider: ollama\n")
	realPlugin(t, dir, "real")
	recordingPlugin(t, dir, "gone")
	var seen driver.Info
	h.newAssistant = func(_ context.Context, _ config.AI, d cli.AssistantDeps) (assistant.Assistant, error) {
		return &assistanttest.Fake{ModelName: "fake/m", AskFn: func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
			seen = d.Manager.State("plug").Info
			return assistant.AskResult{Query: "select 1"}, nil
		}}, nil
	}
	if _, errOut, code := h.exec(t, context.Background(), "ask", "plug", "anything"); code != 0 {
		t.Fatalf("ask plug: code=%d stderr=%q", code, errOut)
	}
	if !driver.HasCapability(seen, driver.CapReadOnly) || !driver.HasCapability(seen, driver.CapVectorSearcher) || seen.Kind != driver.KindVector {
		t.Errorf("plug's Info during the turn = %+v, want the plugin's", seen)
	}

	asked := false
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
		return &assistanttest.Fake{ModelName: "fake/m", AskFn: func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
			asked = true
			return assistant.AskResult{}, nil
		}}, nil
	}
	_, errOut, code := h.exec(t, context.Background(), "ask", "gone", "anything")
	if code != 2 || !strings.Contains(errOut, `connection "gone" is unavailable: driver plugin "gone"`) {
		t.Errorf("ask gone: code=%d stderr=%q, want exit 2 saying the connection is unavailable", code, errOut)
	}
	if asked {
		t.Error("the assistant ran on an unavailable connection")
	}
}

// Listing drivers (connection validation, the connection form), a
// session's State and `connections list` start no plugin.
func TestValidationAndStateStartNoPlugin(t *testing.T) {
	h, dir := pluginHarness(t, "  - name: ext-conn\n    dsn: ext://x\n", "")
	starts := recordingPlugin(t, dir, "ext")
	if _, errOut, code := h.exec(t, context.Background(), "connections", "add", "--name", "new", "--dsn", "ext://y"); code != 0 {
		t.Fatalf("connections add: code=%d stderr=%q", code, errOut)
	}
	if _, errOut, code := h.exec(t, context.Background(), "connections", "add", "--name", "new2", "--driver", "ext", "--dsn", "ext://z"); code != 0 {
		t.Fatalf("connections add --driver: code=%d stderr=%q", code, errOut)
	}
	if _, errOut, code := h.exec(t, context.Background(), "connections", "list"); code != 0 {
		t.Fatalf("connections list: code=%d stderr=%q", code, errOut)
	}
	if _, errOut, code := h.exec(t, context.Background(), "driver", "list", "--no-start"); code != 0 {
		t.Fatalf("driver list --no-start: code=%d stderr=%q", code, errOut)
	}
	assertNotStarted(t, starts)
	b, err := os.ReadFile(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ext://y") || !strings.Contains(string(b), "ext://z") {
		t.Errorf("the plugin connections were not added:\n%s", b)
	}
}

// The TUI gets a resolver for its AI turns: it starts a plugin connection's
// plugin (in the turn's goroutine, not here), and says why one that does not
// answer is unavailable. Starting the TUI starts no plugin.
func TestConnectWiresThePluginResolver(t *testing.T) {
	h, dir := pluginHarness(t, pluginConns, "")
	realPlugin(t, dir, "real")
	starts := recordingPlugin(t, dir, "gone")
	h.tty = true
	ran := false
	h.runTUI = func(ctx context.Context, o tui.Options) error {
		ran = true
		assertNotStarted(t, starts)
		if o.ResolveDriver == nil {
			t.Fatal("no ResolveDriver")
		}
		if err := o.ResolveDriver(ctx, "plug"); err != nil {
			t.Errorf("resolve plug: %v", err)
		}
		if info := o.Manager.State("plug").Info; !driver.HasCapability(info, driver.CapVectorSearcher) {
			t.Errorf("plug's Info after resolving = %+v", info)
		}
		if err := o.ResolveDriver(ctx, "local-pg"); err != nil {
			t.Errorf("resolve a built-in driver's connection: %v", err)
		}
		err := o.ResolveDriver(ctx, "gone")
		if err == nil || !strings.Contains(err.Error(), `connection "gone" is unavailable: driver plugin "gone" did not answer: `) {
			t.Errorf("resolve gone = %v", err)
		}
		return nil
	}
	if _, errOut, code := h.exec(t, context.Background(), "connect", "local-pg"); code != 0 || !ran {
		t.Fatalf("connect: code=%d stderr=%q", code, errOut)
	}
}

// When the caller's context ends during a resolve, the resolver returns
// that cancellation, not an "unavailable" reason: the TUI then shows its
// usual "cancelled", and a plugin is not blamed for the user's interrupt.
func TestResolverReturnsTheCallersCancellation(t *testing.T) {
	h, dir := pluginHarness(t, pluginConns, "")
	recordingPlugin(t, dir, "gone")
	h.tty = true
	ran := false
	h.runTUI = func(ctx context.Context, o tui.Options) error {
		ran = true
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := o.ResolveDriver(cctx, "gone"); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "unavailable") {
			t.Errorf("resolve with a cancelled context = %v, want context.Canceled", err)
		}
		dctx, dcancel := context.WithTimeout(ctx, 0)
		defer dcancel()
		if err := o.ResolveDriver(dctx, "gone"); !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "unavailable") {
			t.Errorf("resolve past the caller's deadline = %v, want context.DeadlineExceeded", err)
		}
		// With the caller's context alive, the failure is the plugin's.
		if err := o.ResolveDriver(ctx, "gone"); err == nil || !strings.Contains(err.Error(), "is unavailable") {
			t.Errorf("resolve gone = %v, want unavailable", err)
		}
		return nil
	}
	if _, errOut, code := h.exec(t, context.Background(), "connect", "local-pg"); code != 0 || !ran {
		t.Fatalf("connect: code=%d stderr=%q", code, errOut)
	}
}

// tobira ask interrupted while resolving takes the cancel exit path (3),
// not the unavailable one (2).
func TestAskCancelledWhileResolving(t *testing.T) {
	h, dir := pluginHarness(t, pluginConns, "ai:\n  provider: ollama\n")
	recordingPlugin(t, dir, "gone")
	h.newAssistant = func(context.Context, config.AI, cli.AssistantDeps) (assistant.Assistant, error) {
		return &assistanttest.Fake{ModelName: "fake/m"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errOut, code := h.exec(t, ctx, "ask", "gone", "anything")
	if code != 3 || strings.Contains(errOut, "unavailable") {
		t.Errorf("cancelled ask: code=%d stderr=%q, want 3 without an unavailable reason", code, errOut)
	}
}

// tobira mcp interrupted while resolving shuts down cleanly without
// serving: no connection is reported unavailable for the interrupt.
func TestMCPCancelledWhileResolving(t *testing.T) {
	h, dir := pluginHarness(t, pluginConns, "")
	recordingPlugin(t, dir, "gone")
	served := false
	h.serveMCP = func(context.Context, cli.MCPOptions) error { served = true; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errOut, code := h.exec(t, ctx, "mcp", "--expose", "gone"); code != 0 {
		t.Errorf("cancelled mcp: code=%d stderr=%q, want a clean exit", code, errOut)
	}
	if served {
		t.Error("the server was built after the interrupt")
	}
}
