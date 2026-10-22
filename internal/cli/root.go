package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/secrets/keyring"
	"github.com/xavidop/tobira/internal/tui"
	"github.com/xavidop/tobira/internal/tui/connform"
	"github.com/xavidop/tobira/plugin/host"
)

// BuildInfo describes the running binary. It is injected at link time by
// the main package.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// Streams are the standard I/O streams used by commands.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Deps are the injected collaborators of the commands.
type Deps struct {
	// Registry holds the available drivers; nil means driver.Default.
	Registry *driver.Registry
	// Getenv reads environment variables; nil means no variables are set.
	Getenv func(string) string
	// Environ lists the environment as KEY=VALUE entries, which the plugin
	// env allow-list filters for driver plugins; nil means an empty
	// environment.
	Environ func() []string
	// Home is the user's home directory; empty when unknown.
	Home string
	// IsTTY reports whether w is a terminal; nil means never.
	IsTTY func(io.Writer) bool
	// Resolver resolves connection passwords.
	Resolver secrets.Resolver
	// IsInputTTY reports whether stdin is a terminal; nil means never.
	IsInputTTY func() bool
	// Keyring is the OS keyring behind keyring:<name> passwords; nil means
	// none, so set-password refuses and removals leave no entry to clean up.
	Keyring keyring.Store
	// ReadPassword reads one line from the terminal with no echo and returns
	// early, with the terminal restored, when ctx is cancelled; nil means the
	// prompt is unavailable. cmd/tobira wires golang.org/x/term.
	ReadPassword func(ctx context.Context) (string, error)
	// RunTUI runs the interactive UI; nil means tui.Run on Streams.In and
	// Streams.Out. Tests inject a fake.
	RunTUI func(ctx context.Context, opts tui.Options) error
	// NewAssistant builds the AI assistant from the ai: config section. It
	// is called only when the config has one; nil means this binary has no
	// AI. cmd/tobira wires the Genkit implementation (module ai/).
	NewAssistant func(ctx context.Context, cfg config.AI, deps AssistantDeps) (assistant.Assistant, error)
	// RunForm shows the connection form; nil means connform.Run on
	// Streams.In and Streams.Out. Tests inject a fake.
	RunForm func(ctx context.Context, m connform.Model) (config.Connection, error)
	// ServeMCP runs the MCP server for `tobira mcp` until the client hangs
	// up or ctx is cancelled; nil means this binary has no MCP server.
	// cmd/tobira wires the Genkit implementation (module ai/).
	ServeMCP func(ctx context.Context, o MCPOptions) error
	// NewEmbedder builds the embedder for vector search from ai.embedder.
	// It is called only when the config sets one; nil means this binary
	// has no embedder. cmd/tobira wires the Genkit implementation (module
	// ai/).
	NewEmbedder func(ctx context.Context, cfg config.AI, deps EmbedderDeps) (driver.Embedder, error)
	// CheckAI asks the configured AI provider whether it answers, for
	// `tobira doctor --online`: it lists models, or the like, and returns
	// why it could not. nil means this binary has no online check.
	// cmd/tobira wires the implementation in module ai/.
	CheckAI func(ctx context.Context, cfg config.AI, deps AICheckDeps) error
}

// AICheckDeps are the collaborators of the online AI check.
type AICheckDeps struct {
	// Secrets resolves the ai.api_key ref.
	Secrets secrets.Resolver
	// Getenv reads the provider key variables (cli.Deps.Getenv).
	Getenv func(string) string
}

type app struct {
	streams Streams
	deps    Deps
	info    BuildInfo
	// pluginsLoaded is set once loadPlugins has run for this app.
	pluginsLoaded bool
	// plugins is the Host loadPlugins registered the discovered plugins
	// with (Plugins, Conflicts, Info); nil when plugins are disabled or
	// none were looked for yet.
	plugins *host.Host
	// shadowed are the plugins discovery skipped because an earlier
	// directory has one of the same name; `driver list` warns about them.
	shadowed []host.Plugin
}

func (a *app) getenv(k string) string { return a.deps.Getenv(k) }

// NewRoot builds the root tobira command wired to the given streams.
func NewRoot(info BuildInfo, streams Streams, deps Deps) *cobra.Command {
	if deps.Registry == nil {
		deps.Registry = driver.Default
	}
	if deps.Getenv == nil {
		deps.Getenv = func(string) string { return "" }
	}
	a := &app{streams: streams, deps: deps, info: info}
	root := &cobra.Command{
		Use:   "tobira",
		Short: "Every database, one terminal.",
		Long: "tobira: Every database, one terminal.\n\n" +
			"Run without arguments in a terminal to open the interactive browser.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Cobra runs only the nearest PersistentPreRun(E) (EnableTraverseRunHooks
		// is off): a subcommand that defines its own must call a.loadPlugins
		// too, or plugins will not be registered for it.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.loadPlugins(cmd)
		},
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError(fmt.Errorf("unknown command %q for \"tobira\"", args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !a.interactive() {
				return cmd.Help()
			}
			ro, _ := cmd.Flags().GetBool("read-only")
			return a.launchTUI(cmd, "", ro)
		},
	}
	root.SetIn(streams.In)
	root.SetOut(streams.Out)
	root.SetErr(streams.Err)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	root.PersistentFlags().String("config", "", "config file (default $TOBIRA_CONFIG or $XDG_CONFIG_HOME/tobira/config.yaml)")
	root.PersistentFlags().Bool("debug", false, "write JSON debug logs to $XDG_STATE_HOME/tobira/tobira.log")
	root.Flags().Bool("read-only", false, "open every connection read-only in the TUI")
	root.AddCommand(newVersionCmd(info), a.newQueryCmd(), a.newAskCmd(), a.newConnectCmd(), a.newConnectionsCmd(), a.newHistoryCmd(), a.newMCPCmd(), a.newAICmd(), a.newDriverCmd(), a.newDoctorCmd(), a.newWatchCmd())
	return root
}

// sizeLimitHint follows an error that hit a size limit (driver.ErrLimit).
const sizeLimitHint = "a size limit: narrow the query, for example with a LIMIT or fewer columns, or raise the driver's limit"

// Execute runs root with args and returns the process exit code (see
// ExitCode). Errors are printed to stderr as a single "error: <message>"
// line with control characters escaped (a size-limit error adds a hint on
// that line); causes are never printed. Commands
// mask credentials in their errors before returning them. Before it
// returns, it stops the driver plugin processes the command started.
func Execute(ctx context.Context, root *cobra.Command, args []string) int {
	if args == nil {
		args = []string{} // nil would make cobra fall back to os.Args
	}
	root.SetArgs(args)
	cl := &cleanups{}
	defer cl.run()
	if err := root.ExecuteContext(context.WithValue(ctx, cleanupsKey{}, cl)); err != nil {
		var ee *exitError
		if errors.As(err, &ee) && ee.silent {
			return ee.code
		}
		msg := query.EscapeControls(driver.RedactMessage(err.Error(), driver.ConnConfig{}))
		if errors.Is(err, driver.ErrLimit) {
			msg += " (" + sizeLimitHint + ")"
		}
		_, _ = fmt.Fprintf(root.ErrOrStderr(), "error: %s\n", msg)
		return ExitCode(err)
	}
	return 0
}
