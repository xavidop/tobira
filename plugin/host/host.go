package host

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// defaultStartTimeout bounds a plugin's start and handshake.
const defaultStartTimeout = 10 * time.Second

// Options configures Register.
type Options struct {
	// Logger receives discovery and lifecycle diagnostics and, at debug
	// level, every line a plugin writes to stderr (redacted, escaped and
	// capped). nil discards them.
	Logger *slog.Logger
	// Environ is the environment (KEY=VALUE entries) the plugin env
	// allow-list is applied to; nil means os.Environ(). Only PATH, HOME,
	// TMPDIR, LANG, LC_*, TZ and TOBIRA_PLUGIN_* (minus names ending in
	// _PASSWORD, _TOKEN, _KEY or _SECRET) reach a plugin.
	Environ []string
	// StartTimeout bounds a plugin's start and handshake; 0 means 10s.
	StartTimeout time.Duration
}

// Conflict is a discovered plugin that was not registered.
type Conflict struct {
	Plugin Plugin
	// Reason says why, e.g. a built-in driver of the same name wins.
	Reason string
}

// Host owns the plugins Register added to a registry: their processes and
// what could not be registered. Close it on exit to stop the processes.
type Host struct {
	log          *slog.Logger
	environ      []string
	startTimeout time.Duration

	lifetime context.Context
	cancel   context.CancelFunc

	adapters  []*adapter
	conflicts []Conflict

	closeOnce sync.Once
}

// Register adds a driver.Driver adapter for each plugin to reg, under the
// plugin's name, without starting any plugin. A plugin starts only on an
// Open of its driver or a Host.Info call naming it; the process is then
// reused until it dies, when the next Open starts a new one.
//
// The adapter's Info never blocks and never starts a plugin, so
// Registry.Drivers, a session's State and connection validation stay
// prompt and launch nothing: until the plugin has answered (an Open loads
// its Info), it returns a stub (the plugin's name, Kind "other", and as its
// only scheme the name itself when no other driver claims it; no
// capabilities). Host.Info waits for the real one; InfoIfStarted reports
// which one Info would give.
//
// A name already in reg is never replaced: a built-in driver wins and the
// plugin is recorded in Conflicts. Re-registering a plugin that a Host
// already added is a no-op. The registry routes DSNs with the scheme
// <name>:// to the plugin; for the plugin's other schemes, name the driver
// explicitly.
func Register(reg *driver.Registry, plugins []Plugin, opts Options) *Host {
	h := &Host{log: opts.Logger, environ: opts.Environ, startTimeout: opts.StartTimeout}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.environ == nil {
		h.environ = os.Environ()
	}
	if h.startTimeout <= 0 {
		h.startTimeout = defaultStartTimeout
	}
	h.lifetime, h.cancel = context.WithCancel(context.Background())
	if reg == nil {
		return h
	}
	for _, p := range plugins {
		if existing, ok := reg.Lookup(p.Name); ok {
			if other, isPlugin := PluginOf(existing); isPlugin && other == p {
				continue // already registered by an earlier Register
			}
			reason := fmt.Sprintf("a built-in driver named %q wins", p.Name)
			if other, isPlugin := PluginOf(existing); isPlugin {
				reason = fmt.Sprintf("the plugin at %s is already registered as %q", other.Path, p.Name)
			}
			h.conflict(p, reason)
			continue
		}
		a := &adapter{h: h, plugin: p, stat: p.stat}
		if a.stat == nil {
			a.stat, _ = checkFile(p.Path)
		}
		if _, taken := reg.ForScheme(p.Name); !taken {
			a.routed = true
		}
		a.stub = driver.Info{Name: p.Name, Kind: driver.KindOther}
		if a.routed {
			a.stub.Schemes = []string{p.Name}
		}
		if err := reg.Add(a); err != nil {
			h.conflict(p, err.Error())
			continue
		}
		// From here on, Info asks the plugin (Add above only read the stub).
		a.registered.Store(true)
		h.adapters = append(h.adapters, a)
	}
	return h
}

func (h *Host) conflict(p Plugin, reason string) {
	h.log.Debug("plugin not registered", "plugin", p.Name, "path", p.Path, "reason", reason)
	h.conflicts = append(h.conflicts, Conflict{Plugin: p, Reason: reason})
}

// Plugins lists the plugins this Host registered, in discovery order.
func (h *Host) Plugins() []Plugin {
	out := make([]Plugin, len(h.adapters))
	for i, a := range h.adapters {
		out[i] = a.plugin
	}
	return out
}

// Conflicts lists the discovered plugins that were not registered, such as
// one shadowed by a built-in driver of the same name.
func (h *Host) Conflicts() []Conflict { return slices.Clone(h.conflicts) }

// Info starts the named plugin if needed and returns its Info, or the error
// that kept it from answering (a failed start, a refused handshake, an
// oversized answer). It blocks up to ctx; call it with no lock held. A
// failure is not cached: the next call tries again. The
// driver's own Info method returns the stub instead; `tobira driver info`
// uses this to say why, and a conformance test calls it before running the
// suite so Info is the real one from the start.
func (h *Host) Info(ctx context.Context, name string) (driver.Info, error) {
	for _, a := range h.adapters {
		if a.plugin.Name == name {
			return a.loadInfo(ctx)
		}
	}
	return driver.Info{}, driver.Wrap(driver.ErrUnknownDriver, "", "info", fmt.Errorf("no plugin named %q", name))
}

// Close stops every plugin process (a graceful shutdown, then a kill) and
// makes later Opens fail with driver.ErrConnection. Conns already open
// fail the same way. It is idempotent and always returns nil.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		var wg sync.WaitGroup
		for _, a := range h.adapters {
			wg.Go(a.close)
		}
		wg.Wait()
		h.cancel()
		for _, a := range h.adapters {
			a.starts.Wait()
		}
	})
	return nil
}

// InfoIfStarted returns d's real Info, without blocking or starting
// anything, and true when d is a plugin adapter whose plugin has answered
// Info (after an Open or a Host.Info). Otherwise it returns d.Info() for a built-in driver, or the stub,
// with false.
func InfoIfStarted(d driver.Driver) (driver.Info, bool) {
	a, ok := d.(*adapter)
	if !ok {
		return d.Info(), false
	}
	return a.cachedInfo()
}

// PluginOf reports whether d is a plugin adapter registered by a Host, and
// which plugin. `tobira driver list` uses it to tell plugins from
// built-in drivers.
func PluginOf(d driver.Driver) (Plugin, bool) {
	if a, ok := d.(*adapter); ok {
		return a.plugin, true
	}
	return Plugin{}, false
}
