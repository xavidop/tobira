package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui"
)

var errNeedsTerminal = errors.New("tobira connect needs an interactive terminal")

// interactive reports whether both stdin and stdout are terminals.
func (a *app) interactive() bool {
	return a.deps.IsInputTTY != nil && a.deps.IsInputTTY() &&
		a.deps.IsTTY != nil && a.deps.IsTTY(a.streams.Out)
}

func (a *app) newConnectCmd() *cobra.Command {
	var readOnly bool
	cmd := &cobra.Command{
		Use:   "connect <connection|dsn>",
		Short: "Open the TUI on one connection",
		Long: "Open the interactive browser with a connection expanded.\n\n" +
			"The argument is a connection name from the config file, or a DSN\n" +
			"(anything containing \"://\" or starting with \"sqlite:\" or \"file:\"), which is\n" +
			"added for this session only. --read-only opens every connection read-only.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError(fmt.Errorf("accepts 1 argument, received %d", len(args)))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !a.interactive() {
				return usageError(errNeedsTerminal)
			}
			return a.launchTUI(cmd, args[0], readOnly)
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "open every connection read-only")
	return cmd
}

// launchTUI runs the TUI. focus is a connection name or DSN to expand at
// startup, "" for none. The config is loaded from the same path that is given
// to the TUI for connection management.
func (a *app) launchTUI(cmd *cobra.Command, focus string, readOnly bool) error {
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

	var notice string // shown inside the TUI too, where stderr is hidden
	cfg, path, st, err := a.loadConfigStat(cmd)
	if err != nil {
		// A DSN does not need the config file: warn and go on with no
		// configured connections, and with connection management off, since
		// there is no file version to write against.
		if !isDSN(focus) {
			return err
		}
		msg := query.EscapeControls(driver.RedactMessage(err.Error(), driver.ConnConfig{})) + "; continuing without the config file"
		_, _ = fmt.Fprintf(a.streams.Err, "warning: %s\n", msg)
		notice = msg
		cfg, path, st = &config.Config{}, "", config.Stat{}
	}
	mgr, embedErr := a.newManager(ctx, cfg, readOnly, log)
	if focus != "" {
		if isDSN(focus) {
			name, err := mgr.AddDSN(focus)
			if err != nil {
				return usageError(scrubDSN(err, focus))
			}
			focus = name
		} else if _, ok := cfg.Find(focus); !ok {
			return unknownConnection(cfg, focus)
		}
	}
	// The editor records its runs; history is best effort, so a store that
	// cannot be opened just leaves History nil.
	hist := a.openHistory(ctx, log, cfg)
	defer func() { _ = hist.Close() }()
	opts := tui.Options{
		Manager:    mgr,
		Theme:      cfg.TUI.Theme,
		Keymap:     cfg.TUI.Keymap,
		Focus:      focus,
		Logger:     log,
		ConfigPath: path,
		ConfigStat: st,
		Notice:     notice,
		Keyring:    a.deps.Keyring,
		Stderr:     a.streams.Err,
		// EmbedderError is shown with the "text similarity needs
		// ai.embedder" refusal.
		EmbedderError: embedErr,
	}
	if hist != nil {
		opts.History = hist // never a typed nil in the interface
	}
	opts.Assistant, opts.AIError = a.buildAssistant(ctx, cfg, mgr, hist, log)
	opts.ResolveDriver = a.pluginResolver(mgr, log)
	run := a.deps.RunTUI
	if run == nil {
		run = func(ctx context.Context, o tui.Options) error {
			return tui.Run(ctx, o, a.streams.In, a.streams.Out)
		}
	}
	log.Info("tui start", "focus", focus, "read_only", readOnly)
	if err := run(ctx, opts); err != nil {
		return err
	}
	return nil
}

// scrubDSN masks the credentials of dsn in err's message.
func scrubDSN(err error, dsn string) error {
	return scrub(err, driver.ConnConfig{DSN: dsn})
}
