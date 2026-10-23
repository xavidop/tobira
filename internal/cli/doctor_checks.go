package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/sshtunnel"
	"github.com/xavidop/tobira/plugin/host"
)

// dsnPasswordHint is the warning for a connection whose DSN carries a
// password: it still works, but the password sits in the config file.
const dsnPasswordHint = "the DSN carries a password; move it to password: as a password ref " +
	"(env:NAME, file:///path, dotenv://path#KEY or keyring:NAME), or run tobira connections set-password"

// checkConfig loads the config file into r.cfg and reports it, each broken
// entry and each warning (the mcp ones are the MCP section's).
func (r *doctorRun) checkConfig() []doctorCheck {
	row := func(name, status, detail string) doctorCheck {
		return doctorCheck{Section: "config", Name: name, Status: status, Detail: detail}
	}
	path := r.a.configPath(r.cmd)
	if path == "" {
		r.cfg = &config.Config{}
		return []doctorCheck{row("file", statusWarn, "cannot locate the config file; use --config or set TOBIRA_CONFIG")}
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		r.cfg = &config.Config{}
		return []doctorCheck{row("file", statusOK, "no config file at "+path+"; using the defaults")}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return []doctorCheck{row("file", statusFail, err.Error())}
	}
	r.cfg = cfg
	out := []doctorCheck{row("file", statusOK, path)}
	for _, b := range cfg.Broken {
		name := b.Name
		if name == "" {
			name = fmt.Sprintf("line %d", b.Line)
		}
		out = append(out, row(name, statusFail, fmt.Sprintf("line %d: %s", b.Line, b.Err)))
	}
	for _, c := range cfg.Connections {
		if driver.HasPassword(c.DSN) {
			out = append(out, row(c.Name, statusWarn, dsnPasswordHint))
		}
	}
	for _, w := range cfg.Warnings {
		if !strings.HasPrefix(w, "mcp.") {
			out = append(out, row("warning", statusWarn, w))
		}
	}
	return out
}

// refItem is one secret ref to resolve.
type refItem struct {
	name, what, ref string
	// override is the environment variable that replaces the ref, when set.
	override string
}

// checkRefs resolves every secret ref of the config (connection passwords,
// ai.api_key, mcp.token), at most doctorPingParallel at a time, each
// bounded by the timeout. A row says ok or fail and the ref's scheme,
// never the value or the rest of the ref. A ref with a scheme no provider in
// this build handles fails as an unknown ref scheme, with or without
// --no-ping. With --no-ping a remote ref (a
// scheme other than env, file, dotenv and keyring, anywhere in a chain) is
// not resolved and reports skip.
func (r *doctorRun) checkRefs() []doctorCheck {
	if r.cfg == nil {
		return nil
	}
	var items []refItem
	for _, c := range r.cfg.Connections {
		if c.Password != "" {
			items = append(items, refItem{name: "connection " + c.Name, what: fmt.Sprintf("connection %q password", c.Name),
				ref: c.Password, override: secrets.EnvName(c.Name)})
		}
		if c.SSH != nil {
			for field, ref := range map[string]string{"ssh.passphrase": c.SSH.Passphrase, "ssh.password": c.SSH.Password} {
				if ref != "" {
					items = append(items, refItem{name: "connection " + c.Name + " " + field,
						what: fmt.Sprintf("connection %q %s", c.Name, field), ref: ref})
				}
			}
		}
	}
	if r.cfg.AI != nil && r.cfg.AI.APIKey != "" {
		items = append(items, refItem{name: "ai.api_key", what: "ai.api_key", ref: r.cfg.AI.APIKey})
	}
	if r.cfg.MCP != nil && r.cfg.MCP.Token != "" {
		items = append(items, refItem{name: "mcp.token", what: "mcp.token", ref: r.cfg.MCP.Token})
	}
	if len(items) == 0 {
		return []doctorCheck{{Section: "refs", Name: "refs", Status: statusOK, Detail: "no secret refs configured"}}
	}
	out := make([]doctorCheck, len(items))
	forEachLimit(len(items), doctorPingParallel, func(i int) {
		out[i] = r.resolveRef(items[i])
	})
	for _, c := range out {
		r.refs[c.Name] = c
	}
	return out
}

func (r *doctorRun) resolveRef(it refItem) doctorCheck {
	c := doctorCheck{Section: "refs", Name: it.name}
	if it.override != "" {
		if v := r.a.getenv(it.override); v != "" {
			r.sawSecret(driver.NewSecret(v))
			c.Status, c.Detail = statusOK, it.override+" is set, so the ref is not used"
			return c
		}
	}
	if !secrets.KnownSchemes(it.ref) {
		// The scheme is not echoed: a malformed ref may not be a ref at all.
		c.Status, c.Detail = statusFail, "unknown ref scheme: no provider in this build handles it"
		return c
	}
	if r.f.noPing && !secrets.IsLocal(it.ref) {
		c.Status, c.Detail = statusSkip, "remote ref, not resolved with --no-ping"
		return c
	}
	scheme := secrets.Scheme(it.ref)
	ctx, cancel := context.WithTimeout(r.ctx, r.f.timeout)
	defer cancel()
	sec, err := within(ctx, func(ctx context.Context) (driver.Secret, error) {
		return r.res.Secret(ctx, it.what, it.ref)
	})
	switch {
	case err == nil:
	case r.ctx.Err() != nil:
		c.Status, c.Detail = statusFail, scheme+" ref: cancelled"
		return c
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		c.Status, c.Detail = statusFail, fmt.Sprintf("%s ref does not resolve: timed out after %s", scheme, r.f.timeout)
		return c
	default:
		c.Status, c.Detail = statusFail, fmt.Sprintf("%s ref does not resolve: %s", scheme, secrets.FailureClass(err))
		return c
	}
	r.sawSecret(sec)
	c.Status, c.Detail = statusOK, scheme+" ref resolves"
	return c
}

// checkConnections reports each loaded connection: an unknown driver
// fails; otherwise it is pinged through a read-only open, at most
// doctorPingParallel at a time, each bounded by --timeout, unless
// --no-ping. A connection whose driver is a plugin starts the plugin.
func (r *doctorRun) checkConnections() []doctorCheck {
	if r.cfg == nil {
		return nil
	}
	conns := r.cfg.Connections
	if len(conns) == 0 {
		return []doctorCheck{{Section: "connections", Name: "connections", Status: statusOK, Detail: "no connections configured"}}
	}
	out := make([]doctorCheck, len(conns))
	var ping []int
	for i, c := range conns {
		out[i] = doctorCheck{Section: "connections", Name: c.Name}
		drv := r.a.driverName(driver.ConnConfig{Driver: c.Driver, DSN: c.DSN})
		switch {
		case drv == "" && c.Driver != "":
			out[i].Status, out[i].Detail = statusFail, fmt.Sprintf("unknown driver %q (tobira driver list shows them)", c.Driver)
		case drv == "":
			out[i].Status, out[i].Detail = statusFail, fmt.Sprintf("unknown driver: no driver handles the scheme %q", driver.Scheme(c.DSN))
		case c.SSH != nil && r.sshProblem(c) != "":
			out[i].Status, out[i].Detail = statusFail, r.sshProblem(c)
		case r.f.noPing:
			out[i].Status, out[i].Detail = statusOK, fmt.Sprintf("not pinged (--no-ping); driver %s%s", drv, sshNote(c))
		default:
			out[i].Detail = drv + sshNote(c)
			ping = append(ping, i)
		}
	}
	mgr := session.New(r.cfg, r.a.deps.Registry, r.res, true, session.WithLogger(r.log))
	forEachLimit(len(ping), doctorPingParallel, func(j int) {
		i := ping[j]
		out[i].Status, out[i].Detail = r.ping(mgr, conns[i], out[i].Detail)
	})
	return out
}

// sshNote says which SSH server a connection goes through, or "".
func sshNote(c config.Connection) string {
	if c.SSH == nil {
		return ""
	}
	return " via ssh " + c.SSH.Host
}

// sshProblem checks what an SSH tunnel needs before any network: a DSN
// the tunnel can forward, a key file that exists when one is set, and a
// known_hosts file that exists. It is "" when all is well.
func (r *doctorRun) sshProblem(c config.Connection) string {
	if _, err := sshtunnel.Target(c.DSN); err != nil {
		return err.Error()
	}
	tc := sshtunnel.Config{KeyFile: c.SSH.Key, KnownHosts: c.SSH.KnownHosts, Home: r.a.deps.Home, Getenv: r.a.getenv}
	if p := tc.KeyPath(); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "ssh.key: " + errText(err)
		}
	}
	if _, err := os.Stat(tc.KnownHostsPath()); err != nil {
		return "ssh known_hosts: " + errText(err) + " (add the server with: ssh-keyscan -H <host> >> ~/.ssh/known_hosts)"
	}
	return ""
}

// ping opens c read-only and pings it, bounded by --timeout even when the
// driver ignores ctx (see within).
func (r *doctorRun) ping(mgr *session.Manager, c config.Connection, drv string) (status, detail string) {
	ctx, cancel := context.WithTimeout(r.ctx, r.f.timeout)
	defer cancel()
	start := time.Now()
	_, err := within(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, mgr.Test(ctx, c) // already redacted for c
	})
	switch {
	case err == nil:
		return statusOK, fmt.Sprintf("reachable in %s (read-only open, driver %s)", time.Since(start).Round(time.Millisecond), drv)
	case r.ctx.Err() != nil:
		return statusFail, "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return statusFail, fmt.Sprintf("timed out after %s (driver %s)", r.f.timeout, drv)
	}
	return statusFail, errLine(err)
}

// checkDrivers lists the built-in drivers; plugins are the next section.
func (r *doctorRun) checkDrivers() []doctorCheck {
	reg := r.a.deps.Registry
	var out []doctorCheck
	for _, info := range reg.Drivers() {
		if d, ok := reg.Lookup(info.Name); ok {
			if _, isPlugin := host.PluginOf(d); isPlugin {
				continue
			}
		}
		detail := "builtin, " + string(info.Kind)
		if info.Version != "" {
			detail += " " + info.Version
		}
		out = append(out, doctorCheck{Section: "drivers", Name: info.Name, Status: statusOK, Detail: detail})
	}
	if len(out) == 0 {
		out = append(out, doctorCheck{Section: "drivers", Name: "drivers", Status: statusWarn, Detail: "this binary has no built-in drivers"})
	}
	return out
}

// checkPlugins lists the discovered plugins with their paths, and warns
// about those not used (a built-in or an earlier directory has the name).
// It starts none, unless --online: then each is asked for its Info, at
// most resolveParallel at a time, each bounded by resolveTimeout.
func (r *doctorRun) checkPlugins() []doctorCheck {
	row := func(name, status, detail string) doctorCheck {
		return doctorCheck{Section: "plugins", Name: name, Status: status, Detail: detail}
	}
	h := r.a.plugins
	if h == nil {
		return []doctorCheck{row("plugins", statusOK, "plugins are disabled (plugins.enabled: false)")}
	}
	plugins, shadows := h.Plugins(), r.a.shadows()
	if len(plugins) == 0 && len(shadows) == 0 {
		return []doctorCheck{row("plugins", statusOK, "none found in the plugins directory or on PATH")}
	}
	out := make([]doctorCheck, len(plugins))
	for i, p := range plugins {
		out[i] = row(p.Name, statusOK, p.Path+" (not started; --online asks it for its Info)")
	}
	if r.f.online {
		forEachLimit(len(plugins), resolveParallel, func(i int) {
			p := plugins[i]
			ctx, cancel := context.WithTimeout(r.ctx, resolveTimeout)
			defer cancel()
			info, err := within(ctx, func(ctx context.Context) (driver.Info, error) { return h.Info(ctx, p.Name) })
			if err != nil {
				out[i] = row(p.Name, statusFail, p.Path+": "+errLine(err))
				return
			}
			out[i] = row(p.Name, statusOK, fmt.Sprintf("%s: %s %s", p.Path, info.Kind, info.Version))
		})
	}
	for _, s := range shadows {
		out = append(out, row(s.plugin.Name, statusWarn, s.plugin.Path+" is not used: "+s.reason))
	}
	return out
}
