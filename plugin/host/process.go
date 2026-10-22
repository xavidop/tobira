package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-plugin/runner"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
	"github.com/xavidop/tobira/plugin/shared"
)

const (
	// maxStartErr caps the plugin-supplied text quoted in a start error.
	maxStartErr = 512
	// stderrTail is how much of a plugin's last stderr output is kept to
	// explain a failed start.
	stderrTail = 4 << 10
	// maxRedactConfigs bounds the configs a process redacts its stderr
	// with; the oldest are dropped first.
	maxRedactConfigs = 64
	// healthTimeout bounds the liveness check after a transport error.
	healthTimeout = 2 * time.Second
)

// proc is one running plugin process. A proc never restarts: when it dies
// the adapter starts a new one, and Conns of the old one stay dead.
type proc struct {
	name   string
	client *plugin.Client
	rpc    pb.DriverClient
	health grpc_health_v1.HealthClient
	stderr []*stderrLog
	redact *redactSet
	// broken is set when a call saw the transport fail, before go-plugin
	// notices the exit.
	broken atomic.Bool
}

// alive reports whether calls may still reach the process.
func (p *proc) alive() bool { return !p.broken.Load() && !p.client.Exited() }

// healthy asks go-plugin's health service whether the process still
// answers, to tell a dead process from a plugin that returned a bare
// Unavailable status. The service is not the driver, so a driver stuck in
// a call does not make it fail.
func (p *proc) healthy() bool {
	if !p.alive() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	resp, err := p.health.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: plugin.GRPCServiceName})
	return err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING
}

// kill ends the process: a graceful shutdown first, then a kill. It blocks
// until the process is gone and its stderr is drained.
func (p *proc) kill() {
	p.client.Kill()
	for _, w := range p.stderr {
		w.Flush()
	}
}

// redactSet holds the ConnConfigs a process has been given, to mask their
// credentials in its stderr.
type redactSet struct {
	mu   sync.Mutex
	cfgs []driver.ConnConfig
}

func (r *redactSet) add(cfg driver.ConnConfig) {
	if cfg.DSN == "" && cfg.Password.Reveal() == "" {
		return
	}
	cfg = driver.ConnConfig{DSN: cfg.DSN, Password: cfg.Password}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cfgs {
		if c.DSN == cfg.DSN && c.Password.Reveal() == cfg.Password.Reveal() {
			return
		}
	}
	if len(r.cfgs) == maxRedactConfigs {
		r.cfgs = r.cfgs[1:]
	}
	r.cfgs = append(r.cfgs, cfg)
}

func (r *redactSet) redact(s string) string {
	r.mu.Lock()
	cfgs := r.cfgs
	r.mu.Unlock()
	for _, c := range cfgs {
		s = driver.RedactMessage(s, c)
	}
	return driver.RedactMessage(s, driver.ConnConfig{})
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - stderrTail; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// launch starts the plugin binary and completes the go-plugin handshake.
// lifetime ends with the Host: cancelling it kills the process, even in the
// middle of a start. The start itself is bounded by the StartTimeout.
func (a *adapter) launch(lifetime context.Context) (*proc, error) {
	if err := a.verify(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(lifetime, a.plugin.Path)
	cmd.Env = pluginEnv(a.h.environ, runtime.GOOS)
	rs := &redactSet{}
	stderr := newStderrLog(a.h.log, a.plugin.Name, rs.redact, maxStderrLine)
	// What the plugin writes to os.Stderr once go-plugin serves arrives
	// over gRPC instead; it gets its own line buffer.
	synced := newStderrLog(a.h.log, a.plugin.Name, rs.redact, maxStderrLine)
	tail := &tailBuffer{}
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: shared.Handshake(),
		Plugins:         shared.PluginSet(nil),
		// Our runner, not go-plugin's: it gives the plugin a null stdin.
		RunnerFunc: func(_ hclog.Logger, gp *exec.Cmd, _ string) (runner.Runner, error) {
			return newProcRunner(cmd, gp)
		},
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		AutoMTLS:         true,
		GRPCDialOptions:  convert.GRPCDialOptions(),
		// The allow-listed cmd.Env is the whole environment: go-plugin only
		// appends its own variables (the cookie, PLUGIN_*) to it.
		SkipHostEnv:  true,
		StartTimeout: a.h.startTimeout,
		// Stderr gets the plugin's raw stderr, line by line; go-plugin's
		// own logger is silenced. Stdout belongs to the handshake and is
		// never shown, synced or not.
		Stderr:     io.MultiWriter(stderr, tail),
		SyncStdout: io.Discard,
		SyncStderr: synced,
		Logger:     hclog.NewNullLogger(),
	})
	p := &proc{name: a.plugin.Name, client: client, stderr: []*stderrLog{stderr, synced}, redact: rs}
	rpcClient, err := client.Client()
	if err != nil {
		// Kill waits for the stderr reader, so the tail is complete.
		p.kill()
		return nil, a.startError(err, tail.String(), rs)
	}
	raw, err := rpcClient.Dispense(shared.PluginName)
	if err != nil {
		p.kill()
		return nil, a.startError(err, tail.String(), rs)
	}
	dc, ok := raw.(pb.DriverClient)
	gc, isGRPC := rpcClient.(*plugin.GRPCClient)
	if !ok || !isGRPC {
		p.kill()
		return nil, a.startError(errors.New("the plugin does not serve the tobira driver protocol"), "", rs)
	}
	p.rpc, p.health = dc, grpc_health_v1.NewHealthClient(gc.Conn)
	a.h.log.Debug("plugin started", "plugin", a.plugin.Name, "path", a.plugin.Path)
	return p, nil
}

// verify re-checks the binary before it runs: it must still pass
// discovery's checks and be the very file discovery saw (same device,
// inode, mode and size), so a binary swapped or loosened in between is
// refused.
func (a *adapter) verify() error {
	st, why := checkFile(a.plugin.Path)
	if why == "" && a.stat != nil && !sameFile(a.stat, st) {
		why = "the binary changed since discovery; restart tobira to use the new one"
	}
	if why != "" {
		return driver.Wrap(driver.ErrConnection, a.plugin.Name, "start",
			fmt.Errorf("plugin %q at %s: %s", a.plugin.Name, a.plugin.Path, why))
	}
	return nil
}

// pluginVersionRE finds the version in go-plugin's incompatible-version error.
var pluginVersionRE = regexp.MustCompile(`Plugin version: (-?\d+)`)

// startError explains a failed start as a driver.ErrConnection naming the
// plugin. A refused cookie and a protocol version mismatch get their own
// messages; otherwise go-plugin's error and the plugin's last stderr line
// are quoted, redacted and capped.
func (a *adapter) startError(err error, tail string, rs *redactSet) error {
	msg := err.Error()
	var text string
	switch {
	case strings.Contains(tail, "This binary is a plugin"):
		text = fmt.Sprintf("plugin %q refused the handshake: magic cookie mismatch "+
			"(it is not a tobira driver plugin, or it was built for another host)", a.plugin.Name)
	case strings.Contains(msg, "incompatible API version"):
		v := "another"
		if m := pluginVersionRE.FindStringSubmatch(msg); m != nil {
			v = m[1]
		}
		text = fmt.Sprintf("plugin %q speaks protocol version %s, this tobira speaks protocol version %d: "+
			"install a build of the plugin for this tobira", a.plugin.Name, v, shared.ProtocolVersion)
	default:
		text = fmt.Sprintf("plugin %q failed to start: %s", a.plugin.Name, quote(rs.redact(firstLine(msg))))
		if last := lastLine(tail); last != "" {
			text += "; its last stderr line: " + quote(rs.redact(last))
		}
	}
	return driver.Wrap(driver.ErrConnection, a.plugin.Name, "start", errors.New(text))
}

// quote caps plugin-supplied text and makes its control characters visible.
func quote(s string) string {
	if len(s) > maxStartErr {
		s = cutUTF8(s, maxStartErr) + truncatedMark
	}
	return escapeControls(s)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(s)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
