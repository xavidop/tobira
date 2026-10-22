package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets/keyring"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/connform"
	"github.com/xavidop/tobira/internal/tui/theme"
)

func (a *app) newConnectionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connections",
		Short: "List, add, edit, remove and test configured connections",
		Long: "Manage the connections in the config file. Passwords are never stored in it: give a\n" +
			"reference (env:NAME, file:///path, dotenv://path#KEY or keyring:NAME) with\n" +
			"--password-ref, or run set-password to keep one in the OS keyring.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError(fmt.Errorf("unknown command %q for \"tobira connections\"", args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newConnListCmd(), a.newConnAddCmd(), a.newConnEditCmd(), a.newConnRmCmd(), a.newConnTestCmd(), a.newConnSetPasswordCmd(), a.newConnExportCmd(), a.newConnImportCmd())
	return cmd
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return usageError(fmt.Errorf("accepts %d argument(s), received %d", n, len(args)))
		}
		return nil
	}
}

// connRow is one element of `connections list --format json`. Loaded and
// broken entries share this shape: a field that does not apply is its zero
// value (empty string, 0, false, empty list).
type connRow struct {
	Name     string   `json:"name"`
	Driver   string   `json:"driver"`
	DSN      string   `json:"dsn"`
	Password string   `json:"password_ref"`
	ReadOnly bool     `json:"read_only"`
	Color    string   `json:"color"`
	Tags     []string `json:"tags"`
	Line     int      `json:"line"`
	Broken   string   `json:"broken"`
}

func (a *app) newConnListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured connections (table on a terminal, JSON otherwise)",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			rows := make([]connRow, 0, len(cfg.Connections))
			for _, c := range cfg.Connections {
				drv := c.Driver
				if drv == "" {
					drv = a.driverName(driver.ConnConfig{DSN: c.DSN})
				}
				ref := c.Password
				if config.ValidateLoadedPasswordRef(ref) != nil {
					ref = "" // never print a value that is not a reference
				}
				tags := c.Tags
				if tags == nil {
					tags = []string{}
				}
				rows = append(rows, connRow{
					Name: c.Name, Driver: drv, DSN: driver.RedactDSN(c.DSN), Password: ref,
					ReadOnly: c.ReadOnly, Color: c.Color, Tags: tags,
				})
			}
			out := cmd.OutOrStdout()
			if a.deps.IsTTY != nil && a.deps.IsTTY(a.streams.Out) {
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "NAME\tDRIVER\tDSN\tREAD-ONLY\tTAGS\tBROKEN")
				for _, r := range rows {
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t\n", query.EscapeControls(r.Name),
						query.EscapeControls(r.Driver), query.EscapeControls(r.DSN), r.ReadOnly,
						query.EscapeControls(strings.Join(r.Tags, ",")))
				}
				for _, b := range cfg.Broken {
					name := driver.RedactMessage(b.Name, driver.ConnConfig{})
					if name == "" {
						name = fmt.Sprintf("(entry at line %d)", b.Line)
					}
					_, _ = fmt.Fprintf(tw, "%s\t\t\t\t\tline %d: %s\n", query.EscapeControls(name), b.Line,
						query.EscapeControls(driver.RedactMessage(b.Err, driver.ConnConfig{})))
				}
				return tw.Flush()
			}
			all := rows
			for _, b := range cfg.Broken {
				all = append(all, connRow{
					Name: driver.RedactMessage(b.Name, driver.ConnConfig{}), Tags: []string{},
					Line: b.Line, Broken: driver.RedactMessage(b.Err, driver.ConnConfig{}),
				})
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(all)
		},
	}
}

type addFlags struct {
	name, drv, dsn, passwordRef, color string
	readOnly                           bool
	tags                               []string
}

func (f addFlags) any() bool {
	return f.name != "" || f.drv != "" || f.dsn != "" || f.passwordRef != "" || f.color != "" ||
		f.readOnly || len(f.tags) > 0
}

func (a *app) newConnAddCmd() *cobra.Command {
	var f addFlags
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a connection (interactive form, or flags for scripts)",
		Long: "Add a connection to the config file. With no flags on a terminal this opens the\n" +
			"same form as the TUI. With --name, --driver and --dsn it is non-interactive.\n\n" +
			"The DSN must not contain a password. Use --password-ref env:NAME,\n" +
			"file:///path, dotenv://path#KEY or keyring:NAME.",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runAdd(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.name, "name", "", "connection name (letters, digits, '.', '_', '-')")
	fl.StringVar(&f.drv, "driver", "", "driver name")
	fl.StringVar(&f.dsn, "dsn", "", "DSN without a password")
	fl.StringVar(&f.passwordRef, "password-ref", "", "password reference: env:NAME, file:///path, dotenv://path#KEY or keyring:NAME")
	fl.BoolVar(&f.readOnly, "read-only", false, "open this connection read-only")
	fl.StringVar(&f.color, "color", "", "sidebar colour: "+strings.Join(config.ColorNames(), ", "))
	fl.StringArrayVar(&f.tags, "tag", nil, "tag (repeatable)")
	return cmd
}

// writeContext is what the commands that change the config file need.
type writeContext struct {
	cfg  *config.Config
	path string
	stat config.Stat
	mgr  *session.Manager
}

func (a *app) writeCtx(cmd *cobra.Command) (writeContext, error) {
	cfg, path, st, err := a.loadConfigStat(cmd)
	if err != nil {
		return writeContext{}, err
	}
	if path == "" {
		return writeContext{}, usageError(errors.New("cannot locate the config file; use --config or set TOBIRA_CONFIG"))
	}
	return writeContext{cfg: cfg, path: path, stat: st, mgr: session.New(cfg, a.deps.Registry, a.deps.Resolver, false)}, nil
}

func (a *app) runAdd(cmd *cobra.Command, f addFlags) error {
	wc, err := a.writeCtx(cmd)
	if err != nil {
		return err
	}
	var c config.Connection
	if f.any() {
		c = config.Connection{
			Name: f.name, Driver: f.drv, DSN: f.dsn, Password: f.passwordRef,
			Color: f.color, ReadOnly: f.readOnly, Tags: f.tags,
		}
		if err := config.ValidateConnection(c, a.deps.Registry.Drivers()); err != nil {
			return usageError(redactErr(err, c.DSN))
		}
	} else {
		if !a.interactive() {
			return usageError(errors.New("connections add needs an interactive terminal; " +
				"pass --name, --driver and --dsn (and optionally --password-ref, --read-only, --color, --tag)"))
		}
		m := a.newForm(wc, connform.Add, nil)
		c, err = a.runForm(cmd.Context(), m)
		if err != nil {
			return formError(err)
		}
	}
	if _, err := config.AddConnection(wc.path, c, wc.stat); err != nil {
		return writeError(err, c.Name)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added %s\n", query.EscapeControls(c.Name))
	return nil
}

// target is the config entry a write command acts on, always addressed by
// its line so that a name shared by several entries can never hit the wrong
// one.
type target struct {
	line int
	name string
	// conn holds the fields to pre-fill: all of a loaded entry, only the
	// fields that parsed safely for a broken one.
	conn config.Connection
}

func (t target) label() string {
	if t.name != "" {
		return t.name
	}
	return fmt.Sprintf("line %d", t.line)
}

// argsOrLine accepts one name argument, or none when --line is given.
func argsOrLine(cmd *cobra.Command, args []string) error {
	if line, _ := cmd.Flags().GetInt("line"); line > 0 {
		return exactArgs(0)(cmd, args)
	}
	return exactArgs(1)(cmd, args)
}

// findTarget resolves the name argument or --line to one entry.
func findTarget(cfg *config.Config, cmd *cobra.Command, args []string) (target, error) {
	if line, _ := cmd.Flags().GetInt("line"); line > 0 {
		if c, ok := cfg.FindAt(line); ok {
			return target{line, c.Name, c}, nil
		}
		if b, ok := cfg.FindBrokenAt(line); ok {
			return target{line, b.Name, b.Partial}, nil
		}
		return target{}, usageError(fmt.Errorf("no connection entry starts at line %d", line))
	}
	name := args[0]
	occ := cfg.Occurrences(name)
	switch len(occ) {
	case 0:
		return target{}, unknownConnection(cfg, name)
	case 1:
		if c, ok := cfg.Find(name); ok {
			return target{occ[0], name, c}, nil
		}
		b, _ := cfg.FindBroken(name)
		return target{occ[0], name, b.Partial}, nil
	}
	lines := make([]string, len(occ))
	for i, l := range occ {
		lines[i] = strconv.Itoa(l)
	}
	return target{}, usageError(fmt.Errorf("connection %q appears %d times (lines %s); use --line",
		name, len(occ), strings.Join(lines, ", ")))
}

func (a *app) newConnEditCmd() *cobra.Command {
	var line int
	cmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Edit a connection in the interactive form",
		Long: "Edit a connection in the interactive form. A broken entry is edited from the\n" +
			"fields that parsed safely. Use --line N for an entry with no usable name or a\n" +
			"name shared by several entries.",
		Args: argsOrLine,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !a.interactive() {
				return usageError(errors.New("connections edit needs an interactive terminal"))
			}
			wc, err := a.writeCtx(cmd)
			if err != nil {
				return err
			}
			t, err := findTarget(wc.cfg, cmd, args)
			if err != nil {
				return err
			}
			prefill := t.conn
			if config.ValidateLoadedPasswordRef(prefill.Password) != nil {
				prefill.Password = "" // never pre-fill a value that is not a reference
			}
			c, err := a.runForm(cmd.Context(), a.newForm(wc, connform.Edit, &prefill))
			if err != nil {
				return formError(err)
			}
			if _, err := config.UpdateConnectionAt(wc.path, t.line, c, wc.stat); err != nil {
				return writeError(err, c.Name)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "updated %s\n", query.EscapeControls(c.Name))
			return nil
		},
	}
	cmd.Flags().IntVar(&line, "line", 0, "target the entry that starts on this line of the config file")
	return cmd
}

func (a *app) newForm(wc writeContext, mode connform.Mode, existing *config.Connection) connform.Model {
	th, err := theme.ByName(wc.cfg.TUI.Theme, true)
	if err != nil {
		th = theme.Dark()
	}
	m := connform.New(mode, existing, a.deps.Registry.Drivers(), th, wc.mgr.Test)
	var taken []string
	for _, c := range wc.cfg.Connections {
		taken = append(taken, c.Name)
	}
	return m.WithTaken(taken...)
}

func (a *app) runForm(ctx context.Context, m connform.Model) (config.Connection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a.deps.RunForm != nil {
		return a.deps.RunForm(ctx, m)
	}
	return connform.Run(ctx, a.streams.In, a.streams.Out, m)
}

func formError(err error) error {
	if errors.Is(err, connform.ErrCancelled) {
		return nil
	}
	return err
}

func writeError(err error, name string) error {
	switch {
	case errors.Is(err, config.ErrDuplicate):
		return usageError(fmt.Errorf("connection %q already exists", driver.RedactMessage(name, driver.ConnConfig{})))
	case errors.Is(err, config.ErrNotFound):
		return usageError(fmt.Errorf("unknown connection %q", name))
	case errors.Is(err, config.ErrConflict):
		return usageError(errors.New("config changed on disk; re-run"))
	}
	return usageError(err)
}

func redactErr(err error, dsn string) error {
	return scrub(err, driver.ConnConfig{DSN: dsn})
}

func (a *app) newConnRmCmd() *cobra.Command {
	var yes bool
	var line int
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a connection from the config file",
		Long: "Remove a connection from the config file. Use --line N for an entry with no\n" +
			"usable name or a name shared by several entries.",
		Args: argsOrLine,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes && !a.interactive() {
				return usageError(errors.New("connections rm needs --yes when there is no interactive terminal"))
			}
			wc, err := a.writeCtx(cmd)
			if err != nil {
				return err
			}
			t, err := findTarget(wc.cfg, cmd, args)
			if err != nil {
				return err
			}
			label := t.label()
			if !yes {
				out := cmd.OutOrStdout()
				if t.name != "" {
					_, _ = fmt.Fprintf(out, "type the connection name to confirm removing %s: ", query.EscapeControls(label))
				} else {
					_, _ = fmt.Fprintf(out, "type the label shown (%s) to confirm removing it: ", label)
				}
				in, _ := bufio.NewReader(a.streams.In).ReadString('\n')
				if strings.TrimSpace(in) != label {
					_, _ = fmt.Fprintln(out)
					return usageError(errors.New("confirmation did not match; nothing removed"))
				}
			}
			if _, err := config.RemoveConnectionAt(wc.path, t.line, wc.stat); err != nil {
				return writeError(err, label)
			}
			if w := keyring.CleanupOnRemove(a.deps.Keyring, wc.cfg, t.name, t.conn.Password); w != "" {
				_, _ = fmt.Fprintf(a.streams.Err, "warning: %s\n", query.EscapeControls(w))
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", query.EscapeControls(label))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().IntVar(&line, "line", 0, "target the entry that starts on this line of the config file")
	return cmd
}

func (a *app) newConnTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Check that a connection can be opened",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			c, ok := cfg.Find(args[0])
			if !ok {
				return unknownConnection(cfg, args[0])
			}
			debug, _ := cmd.Flags().GetBool("debug")
			log, closeLog, err := a.openLog(debug)
			if err != nil {
				return usageError(err)
			}
			defer closeLog()
			mgr := session.New(cfg, a.deps.Registry, a.deps.Resolver, false, session.WithLogger(log))
			start := time.Now()
			// Manager.Test already redacts its errors.
			if err := mgr.Test(ctx, c); err != nil {
				return connectError(err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "ok (%s)\n", time.Since(start).Round(time.Millisecond))
			return nil
		},
	}
}
