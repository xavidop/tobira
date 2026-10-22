package cli

import (
	"context"
	"sync"

	"github.com/spf13/cobra"

	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/plugin/host"
)

// cleanups are the functions Execute runs once the command returns, such
// as stopping plugin processes. Execute puts them in the command's context.
type cleanups struct {
	mu  sync.Mutex
	fns []func()
}

type cleanupsKey struct{}

func (c *cleanups) add(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fns = append(c.fns, fn)
}

// run calls the functions in reverse order of registration.
func (c *cleanups) run() {
	c.mu.Lock()
	fns := c.fns
	c.fns = nil
	c.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// onExit runs fn when the command finishes: through Execute's cleanups, or
// when ctx ends for a root executed some other way. Run the root through
// Execute: with root.ExecuteContext on a context that never ends (such as
// context.Background()), fn never runs and plugin processes are left to
// notice their parent's exit themselves (SDK plugins do within a second;
// on Linux the host also sets a parent-death signal).
func onExit(ctx context.Context, fn func()) {
	if c, ok := ctx.Value(cleanupsKey{}).(*cleanups); ok {
		c.add(fn)
		return
	}
	context.AfterFunc(ctx, fn)
}

// loadPlugins discovers external driver plugins and registers them, once
// per app, unless the config says plugins.enabled: false. It runs before
// every command, starts no plugin (each starts on first use) and never
// fails the command: a config that does not load means the defaults here,
// and the command reports the config error itself. Built-in drivers win
// over plugins of the same name.
func (a *app) loadPlugins(cmd *cobra.Command) {
	if a.pluginsLoaded {
		return
	}
	a.pluginsLoaded = true
	settings := a.peekConfig(cmd).PluginSettings()
	if !settings.Enabled {
		return
	}
	dirs := a.pluginDirs(settings)
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		// The command opens the log itself and reports why it cannot.
		log, closeLog, _ = a.openLog(false)
	}
	plugins, shadowed := host.DiscoverAll(host.SearchDirs(dirs, a.getenv("PATH")), log)
	a.shadowed = shadowed
	var environ []string
	if a.deps.Environ != nil {
		environ = a.deps.Environ()
	}
	if environ == nil {
		environ = []string{} // nil would mean the process environment
	}
	h := host.Register(a.deps.Registry, plugins, host.Options{Logger: log, Environ: environ})
	a.plugins = h
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	onExit(ctx, func() {
		_ = h.Close()
		closeLog()
	})
}

// pluginDirs are the plugins directories searched before PATH:
// plugins.dirs, else the default plugins directory (none when it cannot be
// located).
func (a *app) pluginDirs(settings config.Plugins) []string {
	dirs := config.ExpandPluginDirs(settings.Dirs, a.deps.Home)
	if len(dirs) == 0 {
		if d := config.PluginsDir(a.getenv, a.deps.Home); d != "" {
			dirs = []string{d}
		}
	}
	return dirs
}
