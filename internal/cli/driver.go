package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/plugininstall"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/plugin/host"
)

const (
	// listStartTimeout bounds each plugin `driver list` starts.
	listStartTimeout = 5 * time.Second
	// listStartParallel is how many plugins `driver list` starts at once.
	listStartParallel = 4
	// infoStartTimeout bounds the plugin start of `driver info`: longer
	// than the host's own start timeout, so its clearer error wins.
	infoStartTimeout = 15 * time.Second
	// defaultInstallTimeout bounds `driver install` without --timeout.
	defaultInstallTimeout = 5 * time.Minute
)

// allCapabilities is the order of the `driver info` capability matrix.
var allCapabilities = []driver.Capability{
	driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor,
	driver.CapVectorSearcher, driver.CapTimeSeriesReader, driver.CapGraphReader,
	driver.CapCompleter, driver.CapExporter, driver.CapWatcher, driver.CapReadOnly,
}

func (a *app) newDriverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "driver",
		Short: "List, inspect and install database drivers, built in or plugins",
		Long: "Built-in drivers are compiled into tobira. Plugins are tobira-driver-<name>\n" +
			"binaries found in the plugins directory ($XDG_CONFIG_HOME/tobira/plugins, or\n" +
			"plugins.dirs in the config) and on PATH. A plugin is code that runs as you:\n" +
			"install only binaries you trust, and always with their sha256.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError(fmt.Errorf("unknown command %q for \"tobira driver\"", args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newDriverListCmd(), a.newDriverInfoCmd(), a.newDriverInstallCmd())
	return cmd
}

// driverRow is one element of `driver list` JSON output.
type driverRow struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Version      string   `json:"version"`
	Source       string   `json:"source"` // "builtin" or "plugin:<path>"
	Status       string   `json:"status"` // "ok", "not started", "error: ...", "shadowed: ..."
	Capabilities []string `json:"capabilities"`
}

func capNames(caps []driver.Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}

func rowOf(info driver.Info, source, status string) driverRow {
	return driverRow{
		Name: info.Name, Kind: string(info.Kind), Version: info.Version, Source: source,
		Status: status, Capabilities: capNames(info.Capabilities),
	}
}

// pluginStatus is a plugin's Info error as a status, redacted.
func pluginStatus(err error) string {
	return "error: " + driver.RedactMessage(err.Error(), driver.ConnConfig{})
}

// shadow is a discovered plugin that is not used, and why.
type shadow struct {
	plugin host.Plugin
	reason string
}

// shadows lists the plugins that are not used: those a built-in (or an
// earlier plugin) won over, then those an earlier directory shadowed.
func (a *app) shadows() []shadow {
	if a.plugins == nil {
		return nil
	}
	var out []shadow
	winners := map[string]string{}
	for _, p := range a.plugins.Plugins() {
		winners[p.Name] = p.Path
	}
	for _, c := range a.plugins.Conflicts() {
		out = append(out, shadow{c.Plugin, c.Reason})
		if _, ok := winners[c.Plugin.Name]; !ok {
			winners[c.Plugin.Name] = c.Plugin.Path
		}
	}
	for _, p := range a.shadowed {
		out = append(out, shadow{p, "shadowed by " + winners[p.Name]})
	}
	return out
}

func (s shadow) warn(w io.Writer) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "warning: plugin %q at %s is not used: %s\n", s.plugin.Name,
		query.EscapeControls(s.plugin.Path), query.EscapeControls(s.reason))
}

func (a *app) warnPluginsDisabled() {
	if a.plugins == nil && a.streams.Err != nil {
		_, _ = fmt.Fprintln(a.streams.Err, "note: plugins are disabled (plugins.enabled: false); only built-in drivers are listed")
	}
}

func (a *app) newDriverListCmd() *cobra.Command {
	var noStart bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List built-in and plugin drivers (table on a terminal, JSON otherwise)",
		Long: "List every driver: name, kind, version, source (builtin or plugin:<path>), status and\n" +
			"capabilities. Each plugin is started (4 at a time, 5s each) to ask for its real Info;\n" +
			"one that cannot answer shows status \"error: ...\". --no-start starts nothing and shows\n" +
			"only what is known without running a plugin. Plugins that are not used, because a\n" +
			"built-in driver or an earlier directory has the same name, are listed with status\n" +
			"\"shadowed: ...\" and warned about on stderr.",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			a.warnPluginsDisabled()
			rows := a.listRows(ctx, !noStart)
			out := cmd.OutOrStdout()
			if a.deps.IsTTY != nil && a.deps.IsTTY(a.streams.Out) {
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "NAME\tKIND\tVERSION\tSOURCE\tSTATUS\tCAPABILITIES")
				for _, r := range rows {
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", query.EscapeControls(r.Name),
						query.EscapeControls(r.Kind), query.EscapeControls(r.Version),
						query.EscapeControls(r.Source), query.EscapeControls(r.Status),
						query.EscapeControls(strings.Join(r.Capabilities, ",")))
				}
				return tw.Flush()
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		},
	}
	cmd.Flags().BoolVar(&noStart, "no-start", false, "do not start plugins; show only what is known without running them")
	return cmd
}

// listRows builds `driver list`: the registered drivers by name, each
// followed by the plugins of that name that are not used.
func (a *app) listRows(ctx context.Context, start bool) []driverRow {
	reg := a.deps.Registry
	infos := reg.Drivers() // never blocks: plugins answer with a stub
	rows := make([]driverRow, len(infos))
	var wg sync.WaitGroup
	sem := make(chan struct{}, listStartParallel)
	for i, info := range infos {
		d, ok := reg.Lookup(info.Name)
		p, isPlugin := host.PluginOf(d)
		if !ok || !isPlugin {
			rows[i] = rowOf(info, "builtin", "ok")
			continue
		}
		source := "plugin:" + p.Path
		if !start || a.plugins == nil {
			real, started := host.InfoIfStarted(d)
			status := "not started"
			if started {
				status = "ok"
			}
			rows[i] = rowOf(real, source, status)
			continue
		}
		sem <- struct{}{} // at most listStartParallel goroutines exist at once
		wg.Go(func() {
			defer func() { <-sem }()
			sctx, cancel := context.WithTimeout(ctx, listStartTimeout)
			defer cancel()
			real, err := a.plugins.Info(sctx, info.Name)
			if err != nil {
				rows[i] = rowOf(info, source, pluginStatus(err))
				return
			}
			rows[i] = rowOf(real, source, "ok")
		})
	}
	wg.Wait()
	for _, s := range a.shadows() {
		s.warn(a.streams.Err)
		rows = append(rows, driverRow{
			Name: s.plugin.Name, Source: "plugin:" + s.plugin.Path, Status: "shadowed: " + s.reason,
			Capabilities: []string{},
		})
	}
	slices.SortStableFunc(rows, func(x, y driverRow) int { return strings.Compare(x.Name, y.Name) })
	return rows
}

// capabilityRow is one entry of the `driver info` capability matrix.
type capabilityRow struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
}

// driverDetail is `driver info` JSON output.
type driverDetail struct {
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Version      string          `json:"version"`
	Source       string          `json:"source"`
	Status       string          `json:"status"`
	Schemes      []string        `json:"schemes"`
	Dialect      string          `json:"dialect"`
	Path         string          `json:"path"`
	SHA256       string          `json:"sha256"`
	Capabilities []capabilityRow `json:"capabilities"`
	Conflicts    []string        `json:"conflicts"`
}

func (a *app) newDriverInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show one driver: Info, capability matrix, and for a plugin its path and sha256",
		Long: "Show a driver's kind, version, source, schemes, dialect and capability matrix, and\n" +
			"the plugins of the same name that are not used. For a plugin it also shows the\n" +
			"binary's path and sha256, and starts it to ask for its Info (the status says why\n" +
			"when it cannot answer). Text on a terminal, JSON otherwise.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			det, err := a.driverDetail(ctx, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if a.deps.IsTTY != nil && a.deps.IsTTY(a.streams.Out) {
				return writeDetail(out, det)
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(det)
		},
	}
}

func (a *app) driverDetail(ctx context.Context, name string) (driverDetail, error) {
	d, ok := a.deps.Registry.Lookup(name)
	if !ok {
		return driverDetail{}, usageError(fmt.Errorf("unknown driver %q (tobira driver list shows them)", name))
	}
	info := d.Info()
	det := driverDetail{Source: "builtin", Status: "ok"}
	if p, isPlugin := host.PluginOf(d); isPlugin && a.plugins != nil {
		det.Source, det.Path = "plugin:"+p.Path, p.Path
		det.SHA256 = fileSHA256(p.Path)
		sctx, cancel := context.WithTimeout(ctx, infoStartTimeout)
		real, err := a.plugins.Info(sctx, name)
		cancel()
		if err != nil {
			det.Status = pluginStatus(err)
		} else {
			info = real
		}
	}
	det.Name, det.Kind, det.Version, det.Dialect = info.Name, string(info.Kind), info.Version, info.Dialect
	det.Schemes = append([]string{}, info.Schemes...)
	for _, c := range allCapabilities {
		det.Capabilities = append(det.Capabilities, capabilityRow{Name: string(c), Supported: driver.HasCapability(info, c)})
	}
	det.Conflicts = []string{}
	for _, s := range a.shadows() {
		if s.plugin.Name == name {
			det.Conflicts = append(det.Conflicts, s.plugin.Path+": "+s.reason)
		}
	}
	return det, nil
}

// fileSHA256 is the hex sha256 of the file at path, or "" when it cannot
// be read.
func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeDetail(w io.Writer, det driverDetail) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	field := func(k, v string) { _, _ = fmt.Fprintf(tw, "%s:\t%s\n", k, query.EscapeControls(v)) }
	field("name", det.Name)
	field("kind", det.Kind)
	field("version", det.Version)
	field("source", det.Source)
	field("status", det.Status)
	field("schemes", strings.Join(det.Schemes, ", "))
	field("dialect", det.Dialect)
	if det.Path != "" {
		field("path", det.Path)
		field("sha256", det.SHA256)
	}
	_, _ = fmt.Fprintln(tw, "capabilities:\t")
	for _, c := range det.Capabilities {
		mark := "no"
		if c.Supported {
			mark = "yes"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", c.Name, mark)
	}
	for _, c := range det.Conflicts {
		_, _ = fmt.Fprintf(tw, "not used:\t%s\n", query.EscapeControls(c))
	}
	return tw.Flush()
}

type installFlags struct {
	sha, name string
	force     bool
	timeout   time.Duration
}

func (a *app) newDriverInstallCmd() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:   "install <url|path> --sha256 <hex>",
		Short: "Install a driver plugin binary from an http(s) URL or a local path, checked by sha256",
		Long: "Download (http or https, at most 5 redirects, never https to http) or copy a\n" +
			"tobira-driver-<name> binary into the plugins directory: the first plugins.dirs entry\n" +
			"of the config, else\n" +
			"$XDG_CONFIG_HOME/tobira/plugins (created 0700). --sha256 is required: the binary is\n" +
			"written only if its sha256 matches. It is capped at 256 MiB, written 0755 through a\n" +
			"temporary file and renamed into place. The name is --name, or the URL's or file's\n" +
			"base name. An existing plugin is replaced only with --force. The binary is never run\n" +
			"here: `tobira driver info <name>` starts it.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.runInstall(cmd, args[0], f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.sha, "sha256", "", "the binary's sha256, 64 hex characters (required)")
	fl.StringVar(&f.name, "name", "", "plugin name, tobira-driver-<name> or <name> (default the URL's or file's base name)")
	fl.BoolVar(&f.force, "force", false, "replace a plugin already installed under that name")
	fl.DurationVar(&f.timeout, "timeout", defaultInstallTimeout, "timeout for the download or copy (0 for none)")
	return cmd
}

// installDir is where `driver install` writes: the first plugins.dirs
// entry, else the default plugins directory.
func (a *app) installDir(cmd *cobra.Command) string {
	if dirs := config.ExpandPluginDirs(a.peekConfig(cmd).PluginSettings().Dirs, a.deps.Home); len(dirs) > 0 {
		return dirs[0]
	}
	return config.PluginsDir(a.getenv, a.deps.Home)
}

func (a *app) runInstall(cmd *cobra.Command, source string, f installFlags) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if f.timeout < 0 {
		return usageError(fmt.Errorf("--timeout must not be negative, got %s", f.timeout))
	}
	if _, err := plugininstall.ParseSHA256(f.sha); err != nil {
		return usageError(err)
	}
	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}
	res, err := plugininstall.Install(ctx, plugininstall.Options{
		Source: source, SHA256: f.sha, Name: f.name, Dir: a.installDir(cmd), Force: f.force,
		UserAgent: "tobira/" + a.info.Version,
	})
	if err != nil {
		return installError(err)
	}
	if res.Resolved != "" && a.streams.Err != nil {
		_, _ = fmt.Fprintf(a.streams.Err, "note: %s is a symlink; installed the file it points to, %s\n",
			query.EscapeControls(source), query.EscapeControls(res.Resolved))
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "installed plugin %q to %s\n", res.Name, query.EscapeControls(res.Path))
	_, _ = fmt.Fprintf(out, "it has not been run: `tobira driver info %s` starts it and shows what it supports\n", res.Name)
	if a.plugins == nil {
		_, _ = fmt.Fprintln(out, "note: plugins are disabled (plugins.enabled: false); enable them to use it")
	}
	return nil
}

// installError maps an install failure to its exit code: 1 for a bad
// request or an existing plugin, 2 for a failed download, 4 for an unsafe
// plugins directory (world- or group-writable, or another user's), and 3 otherwise (checksum, size, timeout).
func installError(err error) error {
	switch {
	case errors.Is(err, plugininstall.ErrInput), errors.Is(err, plugininstall.ErrExists):
		return usageError(err)
	case errors.Is(err, plugininstall.ErrDownload):
		return &exitError{code: exitConnection, err: err}
	case errors.Is(err, plugininstall.ErrUnsafeDir):
		return &exitError{code: exitDenied, err: err}
	}
	return &exitError{code: exitQuery, err: err}
}
