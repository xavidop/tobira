package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/query"
)

// historyPath locates the history file: cfg's history.path (expanded), else
// the default location under the state directory. cfg may be nil.
func (a *app) historyPath(cfg *config.Config) (string, error) {
	return history.Resolve(cfg.HistoryPath(), a.getenv, a.deps.Home)
}

// historyConfig loads the config for its history.path, for the commands
// that work without a config (`tobira history`, ad-hoc `tobira query
// <dsn>`). A config that cannot load is not an error there: it prints one
// redacted warning and returns nil, so history uses the default path, as
// it did before history.path existed.
func (a *app) historyConfig(cmd *cobra.Command) *config.Config {
	path := a.configPath(cmd)
	if path == "" {
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		if a.streams.Err != nil {
			reason := query.EscapeControls(driver.RedactMessage(err.Error(), driver.ConnConfig{}))
			_, _ = fmt.Fprintf(a.streams.Err, "warning: config not loaded: %s; using the default history path\n", reason)
		}
		return nil
	}
	return cfg
}

// openHistory opens the history store at cfg's history.path, else the
// default location; cfg may be nil. History is best effort: any failure is
// logged (debug only) and yields a nil store, which does nothing.
func (a *app) openHistory(ctx context.Context, log *slog.Logger, cfg *config.Config) *history.Store {
	path, err := a.historyPath(cfg)
	if err != nil {
		log.Debug("history unavailable", "error", query.EscapeControls(err.Error()))
		return nil
	}
	var opts []history.OpenOption
	if cfg.HistoryPath() != "" {
		// A configured file may sit in a directory the user shares.
		opts = append(opts, history.KeepDirMode())
	}
	s, err := history.Open(ctx, a.deps.Registry, path, opts...)
	if err != nil {
		log.Debug("history unavailable", "error", driver.RedactMessage(err.Error(), driver.ConnConfig{}))
		return nil
	}
	s.SetLogger(log)
	return s
}

// recordHistory stores e; a failure is only logged.
func (a *app) recordHistory(ctx context.Context, log *slog.Logger, s *history.Store, e history.Entry, cfg driver.ConnConfig) {
	if err := s.Record(ctx, e, cfg); err != nil {
		log.Debug("history record failed", "error", driver.RedactMessage(err.Error(), cfg))
	}
}

// historyConn is the name a run is recorded under: the connection name, or
// the redacted DSN for an ad-hoc connection.
func historyConn(cfg driver.ConnConfig) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return driver.RedactDSN(cfg.DSN)
}

type historyFlags struct {
	conn   string
	source string
	mcp    bool
	search string
	limit  int
	format string
}

func (a *app) newHistoryCmd() *cobra.Command {
	var f historyFlags
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List the queries tobira ran, newest first",
		Long: "List the queries recorded by `tobira query`, the TUI, the AI assistant's tools and `tobira mcp`, newest first.\n\n" +
			"Use --source to see one front end, or --mcp for the calls MCP clients made, with the client's name.\n\n" +
			"History lives at the history.path config key, else in $XDG_STATE_HOME/tobira/history.db\n" +
			"(default ~/.local/state/tobira/history.db).\n" +
			"Query text is stored with credentials masked.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runHistory(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.conn, "conn", "", "only entries of this connection")
	fl.StringVar(&f.source, "source", "", "only entries from this front end: "+wantSources())
	fl.BoolVar(&f.mcp, "mcp", false, "only calls made by MCP clients (same as --source mcp)")
	fl.StringVar(&f.search, "search", "", "only entries whose query contains this text (case-insensitive)")
	fl.IntVar(&f.limit, "limit", 100, "maximum entries to list (0 for every retained entry)")
	fl.StringVar(&f.format, "format", "", "output format: table, json, ndjson or csv (default table on a terminal, else json)")
	return cmd
}

func (a *app) runHistory(cmd *cobra.Command, f historyFlags) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if f.limit < 0 {
		return usageError(fmt.Errorf("--limit must not be negative, got %d", f.limit))
	}
	source, err := historySource(cmd, f)
	if err != nil {
		return err
	}
	format, err := a.format(f.format)
	if err != nil {
		return err
	}
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		return usageError(err)
	}
	defer closeLog()

	limit := f.limit
	if limit == 0 {
		limit = 10000 // like `tobira query`, 0 means no limit: every retained entry
	}
	cfg := a.historyConfig(cmd)
	path, err := a.historyPath(cfg)
	if err != nil && cfg.HistoryPath() != "" {
		return usageError(err) // a history.path that does not expand
	}
	var entries []history.Entry
	// Never create the file just to list it: a missing file is an empty history.
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			s := a.openHistory(ctx, log, cfg)
			defer func() { _ = s.Close() }()
			entries, err = s.List(ctx, history.Filter{Conn: f.conn, Source: source, Search: f.search, Limit: limit})
			if err != nil {
				return fmt.Errorf("read history: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Debug("history unavailable", "error", err.Error())
		}
	}
	return writeHistory(cmd.OutOrStdout(), format, entries, source == history.SourceMCP)
}

// historySources are the valid --source values.
var historySources = []history.Source{history.SourceCLI, history.SourceTUI, history.SourceAI, history.SourceMCP}

func wantSources() string {
	names := make([]string, len(historySources))
	for i, s := range historySources {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

// historySource resolves --source and --mcp to a filter value; "" means all.
func historySource(cmd *cobra.Command, f historyFlags) (history.Source, error) {
	var source history.Source
	if cmd.Flags().Changed("source") {
		source = history.Source(f.source)
		if !slices.Contains(historySources, source) {
			return "", usageError(fmt.Errorf("--source must be one of %s", wantSources()))
		}
	}
	if f.mcp {
		if source != "" && source != history.SourceMCP {
			return "", usageError(errors.New("--mcp conflicts with --source (it means --source mcp)"))
		}
		source = history.SourceMCP
	}
	return source, nil
}

// writeHistory prints entries. The client column shows in json and ndjson
// always (null when empty), and in the other formats for an MCP listing or
// any listing that has an MCP row.
func writeHistory(w io.Writer, format query.Format, entries []history.Entry, mcpListing bool) error {
	if len(entries) == 0 {
		if format == query.FormatJSON {
			_, err := io.WriteString(w, "[]\n")
			return err
		}
		return nil
	}
	withClient := format == query.FormatJSON || format == query.FormatNDJSON || mcpListing ||
		slices.ContainsFunc(entries, func(e history.Entry) bool { return e.Source == history.SourceMCP })
	type col struct {
		name string
		typ  driver.Type
	}
	cols := []col{{"at", driver.TypeString}, {"conn", driver.TypeString}, {"source", driver.TypeString}}
	if withClient {
		cols = append(cols, col{"client", driver.TypeString})
	}
	cols = append(cols, col{"ok", driver.TypeBool}, col{"rows", driver.TypeInt}, col{"ms", driver.TypeInt}, col{"query", driver.TypeString})
	columns := make([]driver.Column, len(cols))
	for i, c := range cols {
		columns[i] = driver.Column{Name: c.name, Type: c.typ}
	}
	rows := make([]driver.Row, len(entries))
	for i, e := range entries {
		row := driver.Row{driver.String(e.At.UTC().Format(time.RFC3339)), driver.String(e.Conn), driver.String(string(e.Source))}
		if withClient {
			client := driver.Null()
			if e.Client != "" {
				client = driver.String(e.Client)
			}
			row = append(row, client)
		}
		rows[i] = append(row, driver.Bool(e.OK), driver.Int(e.Rows), driver.Int(e.Took.Milliseconds()), driver.String(e.Query))
	}
	return query.Write(w, format, &query.Result{Columns: columns, Rows: rows, Stats: driver.Stats{RowsAffected: -1}})
}
