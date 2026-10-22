package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/mcphttp"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/session"
)

// MCPOptions are what the MCP server factory gets.
type MCPOptions struct {
	Manager *session.Manager
	// History records every tool call; nil records nothing.
	History *history.Store
	// Policy is the mcp: section with the flags applied and checked against
	// the connections: Expose is never empty and AllowWrites is a subset of
	// it.
	Policy config.MCP
	// Version is the binary's version, reported to clients.
	Version string
	// Stdin and Stdout carry the protocol. Nothing else may write to Stdout.
	Stdin  io.Reader
	Stdout io.Writer
	// Logger is the --debug log; it discards without --debug.
	Logger *slog.Logger
	// HTTP, when set (--http), serves Streamable HTTP instead of stdio.
	HTTP *MCPHTTP
	// Stderr gets the HTTP address served and a generated token.
	Stderr io.Writer
	// Unavailable maps exposed connections whose driver plugin could not
	// report its Info to a redacted one-line reason. The plugins of the
	// others have answered, so the Manager's State carries their real
	// capabilities and kind.
	Unavailable map[string]string
}

// MCPHTTP configures the Streamable HTTP transport.
type MCPHTTP struct {
	// Addr is the checked --http address as host:port (an empty host made
	// 127.0.0.1); it is loopback unless InsecureBind.
	Addr string
	// InsecureBind is --insecure-bind: a non-loopback Addr is allowed.
	InsecureBind bool
	// AllowedHosts are the --allowed-host values, checked and lowercased.
	AllowedHosts []string
	// Token is mcp.token resolved; zero means the server generates one.
	Token driver.Secret
}

// errNothingExposed is the refusal to serve no connection at all.
var errNothingExposed = errors.New("nothing is exposed: pass --expose or set mcp.expose")

func (a *app) newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the exposed connections to an MCP client over stdio or HTTP",
		Long: "Serve tobira's database tools to an MCP client (Claude Code, Claude Desktop,\n" +
			"Cursor, VS Code) over stdin and stdout, or over Streamable HTTP with --http.\n\n" +
			"Only the connections in mcp.expose (or --expose) are visible; any other name\n" +
			"is answered as unknown. Tools are read-only, except execute, which exists only\n" +
			"when some exposed connection is in mcp.allow_writes (or --allow-writes) and\n" +
			"runs only there, never on a read-only connection. Every call is recorded in\n" +
			"history with source mcp and the client's name (tobira history --mcp).\n\n" +
			"Each flag replaces its config value. Stdout carries only the protocol; logs\n" +
			"go to the --debug log.\n\n" +
			"--http serves at http://<addr>/mcp, on loopback unless --insecure-bind. Every\n" +
			"request needs Authorization: Bearer <token>: mcp.token, or a token generated\n" +
			"at start and printed once to stderr. Requests for another Host (see\n" +
			"--allowed-host) or from a non-loopback Origin are refused.\n\n" +
			"  claude mcp add tobira -- tobira mcp --expose local-pg",
		Args: cobra.NoArgs,
		RunE: a.runMCP,
	}
	f := cmd.Flags()
	f.StringSlice("expose", nil, "connections to serve, comma-separated (replaces mcp.expose)")
	f.StringSlice("allow-writes", nil, "exposed connections execute may write to (replaces mcp.allow_writes)")
	f.Int("max-rows", 0, fmt.Sprintf("most rows in one tool result, 1 to %d (replaces mcp.max_rows; default 200)", config.MCPMaxRowsCap))
	f.String("send-rows", "", "whether results carry row data: always or never (replaces mcp.send_rows)")
	f.String("http", "", "serve Streamable HTTP on host:port instead of stdio (an empty host is 127.0.0.1)")
	f.Bool("insecure-bind", false, "allow --http on a non-loopback address")
	f.StringArray("allowed-host", nil, "another Host header value to accept over --http, such as a name for this machine (repeatable)")
	return cmd
}

func (a *app) runMCP(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		return usageError(err)
	}
	defer closeLog()
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		_, _ = fmt.Fprintf(a.streams.Err, "warning: %s\n", query.EscapeControls(w))
	}
	pol, err := mcpPolicy(cmd, cfg)
	if err != nil {
		return usageError(err)
	}
	httpOpts, err := mcpHTTP(cmd)
	if err != nil {
		return err
	}
	if httpOpts != nil && pol.Token != "" {
		tok, err := a.deps.Resolver.Secret(ctx, "mcp.token", pol.Token)
		if err != nil {
			return usageError(err)
		}
		// Trimmed, as a client's bearer value is; blank is refused rather
		// than silently replaced by a generated token.
		v := strings.TrimSpace(tok.Reveal())
		if v == "" {
			return usageError(errors.New("mcp.token resolved to an empty value"))
		}
		httpOpts.Token = driver.NewSecret(v)
	}
	if a.deps.ServeMCP == nil {
		return usageError(errors.New("this binary has no MCP server"))
	}
	mgr, _ := a.newManager(ctx, cfg, false, log)
	defer func() { _ = mgr.CloseAll() }()
	hist := a.openHistory(ctx, log, cfg)
	defer func() { _ = hist.Close() }()
	// The exposed plugin connections' real Info, before the tools are
	// built: similar and the read-only gate read it from the start.
	unavailable, err := a.resolvePlugins(ctx, mgr, pol.Expose, log)
	if err != nil {
		return nil // a signal while starting: a clean shutdown, as below
	}
	log.Debug("mcp serving", "expose", pol.Expose, "allow_writes", pol.AllowWrites, "max_rows", pol.MaxRows, "send_rows", pol.SendRows)
	err = a.deps.ServeMCP(ctx, MCPOptions{
		Manager: mgr, History: hist, Policy: pol, Version: a.info.Version,
		Stdin: a.streams.In, Stdout: a.streams.Out, Logger: log,
		HTTP: httpOpts, Stderr: a.streams.Err, Unavailable: unavailable,
	})
	if ctx.Err() != nil {
		return nil // a signal: a clean shutdown
	}
	if errors.Is(err, mcphttp.ErrInsecureBind) {
		return &exitError{code: exitDenied, err: err}
	}
	if err != nil {
		msg := driver.RedactMessage(err.Error(), driver.ConnConfig{})
		log.Debug("mcp server failed", "error", msg)
		return &exitError{code: exitConnection, err: errors.New("mcp: " + msg)}
	}
	return nil
}

// mcpPolicy is the mcp: section with the flags applied, checked: something
// is exposed, every exposed name is a connection, allow_writes is a subset
// of expose, and the limits are in range. Errors name list positions, never
// the entries.
func mcpPolicy(cmd *cobra.Command, cfg *config.Config) (config.MCP, error) {
	pol := cfg.MCPSettings()
	flags := cmd.Flags()
	exposeFlag, writesFlag := flags.Changed("expose"), flags.Changed("allow-writes")
	if exposeFlag {
		v, _ := flags.GetStringSlice("expose")
		pol.SetExpose(names(v))
	}
	if writesFlag {
		v, _ := flags.GetStringSlice("allow-writes")
		pol.SetAllowWrites(names(v))
	}
	if flags.Changed("max-rows") {
		n, _ := flags.GetInt("max-rows")
		if n < 1 || n > config.MCPMaxRowsCap {
			return pol, fmt.Errorf("--max-rows must be between 1 and %d", config.MCPMaxRowsCap)
		}
		pol.MaxRows = n
	}
	if flags.Changed("send-rows") {
		s, _ := flags.GetString("send-rows")
		if !slices.Contains(config.MCPSendRowsModes(), s) {
			return pol, fmt.Errorf("--send-rows must be %s", strings.Join(config.MCPSendRowsModes(), " or "))
		}
		pol.SendRows = s
	}
	if len(pol.Expose) == 0 {
		return pol, errNothingExposed
	}
	if err := pol.Validate(cfg.Connections); err != nil {
		msg := err.Error()
		if exposeFlag {
			msg = strings.ReplaceAll(msg, "mcp.expose", "--expose")
		}
		if writesFlag {
			msg = strings.ReplaceAll(msg, "mcp.allow_writes", "--allow-writes")
		}
		return pol, errors.New(msg)
	}
	pol.Expose, pol.AllowWrites = dedupe(pol.Expose), dedupe(pol.AllowWrites)
	return pol, nil
}

// mcpHTTP is the HTTP transport the flags ask for, checked; nil without
// --http. A non-loopback address without --insecure-bind is exit 4, before
// anything listens; other mistakes are usage errors.
func mcpHTTP(cmd *cobra.Command) (*MCPHTTP, error) {
	flags := cmd.Flags()
	if !flags.Changed("http") {
		for _, f := range []string{"insecure-bind", "allowed-host"} {
			if flags.Changed(f) {
				return nil, usageError(fmt.Errorf("--%s needs --http", f))
			}
		}
		return nil, nil
	}
	addr, _ := flags.GetString("http")
	b, err := mcphttp.ParseBind(addr)
	if err != nil {
		return nil, usageError(err)
	}
	insecure, _ := flags.GetBool("insecure-bind")
	if err := mcphttp.CheckBind(b, insecure); err != nil {
		return nil, &exitError{code: exitDenied, err: err}
	}
	o := &MCPHTTP{Addr: b.String(), InsecureBind: insecure}
	// GetStringArray would drop a lone empty value; the raw slice keeps it,
	// so it is refused rather than ignored.
	var hosts []string
	if sv, ok := flags.Lookup("allowed-host").Value.(pflag.SliceValue); ok {
		hosts = sv.GetSlice()
	}
	for _, h := range hosts {
		v, err := mcphttp.ParseAllowedHost(h)
		if err != nil {
			return nil, usageError(err)
		}
		if !slices.Contains(o.AllowedHosts, v) {
			o.AllowedHosts = append(o.AllowedHosts, v)
		}
	}
	return o, nil
}

// names trims the entries of a list flag and drops empty ones. Repeats stay
// until the policy is checked, so errors name positions in the flag.
func names(v []string) []string {
	var out []string
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// dedupe drops repeated names, keeping the first.
func dedupe(v []string) []string {
	var out []string
	for _, s := range v {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
