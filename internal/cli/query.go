package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/query"
)

type queryFlags struct {
	file     string
	format   string
	limit    int
	timeout  time.Duration
	explain  bool
	readOnly bool
	output   string
}

func (a *app) newQueryCmd() *cobra.Command {
	var f queryFlags
	cmd := &cobra.Command{
		Use:   "query <connection|dsn> [query]",
		Short: "Run a query and print the result",
		Long: "Run one query against a configured connection or a DSN and print the result.\n\n" +
			"The first argument is a connection name from the config file, or a DSN\n" +
			"(anything containing \"://\" or starting with \"sqlite:\" or \"file:\").\n" +
			"The query is the second argument, or is read from a file with -f (\"-f -\" reads stdin).",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageError(fmt.Errorf("accepts 1 or 2 arguments, received %d", len(args)))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runQuery(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.file, "file", "f", "", `read the query from a file ("-" for stdin)`)
	fl.StringVar(&f.format, "format", "", "output format: table, json, ndjson or csv (default table on a terminal, else json)")
	fl.IntVar(&f.limit, "limit", 1000, "maximum rows to return (0 for no limit)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "timeout for the whole query (0 for none)")
	fl.BoolVar(&f.explain, "explain", false, "print the query plan instead of running the query")
	fl.BoolVar(&f.readOnly, "read-only", false, "refuse anything that would write")
	fl.StringVar(&f.output, "output", "", "write the result to a file (csv, json or ndjson) instead of stdout; exports every row unless --limit is set")
	return cmd
}

func isDSN(arg string) bool {
	return strings.Contains(arg, "://") || strings.HasPrefix(arg, "sqlite:") || strings.HasPrefix(arg, "file:")
}

func (a *app) runQuery(cmd *cobra.Command, args []string, f queryFlags) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if f.limit < 0 {
		return usageError(fmt.Errorf("--limit must not be negative, got %d", f.limit))
	}
	if f.timeout < 0 {
		return usageError(fmt.Errorf("--timeout must not be negative, got %s", f.timeout))
	}
	text, err := a.queryText(args, f.file)
	if err != nil {
		return err
	}
	var format query.Format
	if f.output != "" {
		if format, err = a.outputFormat(cmd, &f); err != nil {
			return err
		}
	} else if format, err = a.format(f.format); err != nil {
		return err
	}
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		return usageError(err)
	}
	defer closeLog()

	cfg, err := a.resolve(ctx, cmd, args[0])
	if err != nil {
		return err
	}
	// --read-only also opens the connection read-only at the protocol level.
	cfg.ReadOnly = cfg.ReadOnly || f.readOnly
	drvName := a.driverName(cfg)
	log.Info("query start", "driver", drvName, "connection", cfg.Name, "format", string(format),
		"read_only", cfg.ReadOnly, "explain", f.explain)

	if f.timeout > 0 {
		// The timeout also bounds connecting, not just the query.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}
	cfg.Logger = log // driver warnings go to the --debug log
	conn, err := a.deps.Registry.Open(ctx, cfg)
	if err != nil {
		log.Error("open failed", "kind", driver.KindName(err))
		return scrub(connectError(err), cfg)
	}
	defer func() { _ = conn.Close() }()

	hist := a.openHistory(ctx, log, a.historyConfig(cmd))
	defer func() { _ = hist.Close() }()

	if f.output != "" {
		return a.runExport(ctx, cmd, conn, cfg, text, format, f, hist, log)
	}

	start := time.Now()
	res, err := query.Run(ctx, conn, text, query.Options{
		Limit:    f.limit,
		Timeout:  f.timeout,
		ReadOnly: cfg.ReadOnly,
		Explain:  f.explain,
		Driver:   drvName,
	})
	entry := history.Entry{
		At: start, Conn: historyConn(cfg), Source: history.SourceCLI, Query: text, Took: time.Since(start),
	}
	if err != nil {
		log.Error("query failed", "kind", driver.KindName(err), "duration", time.Since(start).String())
		a.recordHistory(ctx, log, hist, entry, cfg)
		return scrub(err, cfg)
	}
	entry.OK = true
	entry.Rows = int64(len(res.Rows))
	if len(res.Columns) == 0 && res.Stats.RowsAffected > 0 {
		entry.Rows = res.Stats.RowsAffected
	}
	a.recordHistory(ctx, log, hist, entry, cfg)
	log.Info("query done", "rows", len(res.Rows), "duration", time.Since(start).String())
	return query.Write(cmd.OutOrStdout(), format, res)
}

// outputFormat settles the format, limit and timeout of `query --output`
// and checks the target. An explicit --format wins, else the file extension
// decides. Without an explicit --limit every row is exported, not the default
// 1000, and without an explicit --timeout there is none, not the default 30s.
func (a *app) outputFormat(cmd *cobra.Command, f *queryFlags) (query.Format, error) {
	if f.explain {
		return "", usageError(errors.New("--output cannot be combined with --explain"))
	}
	if !cmd.Flags().Changed("limit") {
		f.limit = 0
	}
	if !cmd.Flags().Changed("timeout") {
		f.timeout = 0
	}
	// Refuse a bad target before connecting: it is a usage error.
	if _, err := export.ResolveTarget(f.output); err != nil {
		return "", usageError(err)
	}
	if cmd.Flags().Changed("format") {
		format, err := a.format(f.format)
		if err != nil {
			return "", err
		}
		if format == query.FormatTable {
			return "", usageError(errors.New("--format table cannot be written to a file: choose --format csv|json|ndjson"))
		}
		return format, nil
	}
	if format, ok := export.FormatForPath(f.output); ok {
		return format, nil
	}
	return "", usageError(fmt.Errorf("cannot tell the format of %s from its extension: choose --format csv|json|ndjson",
		query.EscapeControls(f.output)))
}

// runExport streams the query result to f.output and reports one line on
// stderr. History records the exported rows, or -1 on the native path, where
// the count is unknown.
func (a *app) runExport(ctx context.Context, cmd *cobra.Command, conn driver.Conn, cfg driver.ConnConfig, text string,
	format query.Format, f queryFlags, hist *history.Store, log *slog.Logger) error {
	start := time.Now()
	res, err := export.ToFile(ctx, export.QuerySource{
		Conn:   conn,
		Query:  driver.Query{Text: text, Limit: f.limit, ReadOnly: cfg.ReadOnly},
		Driver: a.driverName(cfg),
	}, export.Options{Format: format, Path: f.output})
	entry := history.Entry{
		At: start, Conn: historyConn(cfg), Source: history.SourceCLI, Query: text, Took: time.Since(start),
	}
	if err != nil {
		log.Error("export failed", "kind", driver.KindName(err), "duration", time.Since(start).String())
		a.recordHistory(ctx, log, hist, entry, cfg)
		return scrub(err, cfg)
	}
	entry.OK, entry.Rows = true, res.Rows
	if res.NoColumns {
		// As without --output: a statement with no result set records the
		// rows it affected.
		entry.Rows = max(res.RowsAffected, 0)
	}
	a.recordHistory(ctx, log, hist, entry, cfg)
	if res.NoColumns {
		msg := "no result set"
		if res.RowsAffected >= 0 {
			msg += ": " + plural(res.RowsAffected, "row", "rows") + " affected"
		}
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "%s; nothing written to %s\n", msg, query.EscapeControls(res.Path))
		return err
	}
	log.Info("export done", "rows", res.Rows, "bytes", res.Bytes, "native", res.Native, "duration", time.Since(start).String())
	what := plural(res.Rows, "row", "rows")
	if res.Native {
		what = plural(res.Bytes, "byte", "bytes")
	}
	if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s to %s\n", what, query.EscapeControls(res.Path)); err != nil {
		return err
	}
	if w := export.PreviewWarning(res.Previews); w != "" {
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
	return err
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatInt(n, 10) + " " + many
}

// scrubbedError is an error whose message has had credentials masked. The
// original error stays reachable through errors.Is/As (for the exit code),
// but only the masked message is ever printed.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrub masks the DSN, the resolved password and any embedded credentials of
// cfg in err's message. It is the single host-side scrub of driver errors.
func scrub(err error, cfg driver.ConnConfig) error {
	return &scrubbedError{msg: driver.RedactMessage(err.Error(), cfg), err: err}
}

// connectError classifies an error from opening a connection: exit code 2
// ("could not open the connection") whatever its kind, except read-only and
// permission errors (4) and cancellation or timeout (3).
func connectError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch driver.KindOf(err) {
	case driver.ErrReadOnly, driver.ErrPermission:
		return err
	}
	return &exitError{code: exitConnection, err: err}
}

// queryText returns the query from the positional argument or the -f file.
func (a *app) queryText(args []string, file string) (string, error) {
	switch {
	case len(args) == 2 && file != "":
		return "", usageError(errors.New("give the query either as an argument or with -f, not both"))
	case len(args) == 1 && file == "":
		return "", usageError(errors.New("a query is required: pass it as an argument or with -f"))
	case len(args) == 2:
		return args[1], nil
	}
	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(a.streams.In)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return "", usageError(fmt.Errorf("read query: %w", err))
	}
	s := strings.TrimSuffix(string(data), "\n")
	return strings.TrimSuffix(s, "\r"), nil
}

func (a *app) format(name string) (query.Format, error) {
	if name == "" {
		if a.deps.IsTTY != nil && a.deps.IsTTY(a.streams.Out) {
			return query.FormatTable, nil
		}
		return query.FormatJSON, nil
	}
	f := query.Format(name)
	if !slices.Contains(query.Formats(), f) {
		names := make([]string, 0, 4)
		for _, v := range query.Formats() {
			names = append(names, string(v))
		}
		return "", usageError(fmt.Errorf("unknown format %q (valid formats: %s)", name, strings.Join(names, ", ")))
	}
	return f, nil
}

// resolve turns the connection argument into a driver.ConnConfig.
func (a *app) resolve(ctx context.Context, cmd *cobra.Command, arg string) (driver.ConnConfig, error) {
	if isDSN(arg) {
		return driver.ConnConfig{DSN: arg}, nil
	}
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return driver.ConnConfig{}, err
	}
	conn, ok := cfg.Find(arg)
	if !ok {
		return driver.ConnConfig{}, unknownConnection(cfg, arg)
	}
	cc, err := a.deps.Resolver.ConnConfig(ctx, conn)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return driver.ConnConfig{}, err
		}
		return driver.ConnConfig{}, &exitError{code: exitConnection, err: err}
	}
	return cc, nil
}

// unknownConnection is the usage error for a name that is not configured. arg
// may be a DSN in a form isDSN does not recognise (user:pw@host), so it is
// redacted.
func unknownConnection(cfg *config.Config, arg string) error {
	if b, ok := cfg.FindBroken(arg); ok {
		// The entry exists but did not load; its error never holds a value.
		return usageError(fmt.Errorf("connection %q: %s", arg, b.Err))
	}
	msg := fmt.Sprintf("unknown connection %q", driver.RedactMessage(arg, driver.ConnConfig{DSN: arg}))
	if s := cfg.Suggest(arg); s != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", s)
	}
	return usageError(errors.New(msg))
}

// configPath is the config file path, or "" when there is no way to locate one.
func (a *app) configPath(cmd *cobra.Command) string {
	flagPath, _ := cmd.Flags().GetString("config")
	if a.deps.Home == "" && flagPath == "" && a.getenv("TOBIRA_CONFIG") == "" && a.getenv("XDG_CONFIG_HOME") == "" {
		return ""
	}
	return config.Path(flagPath, a.getenv, a.deps.Home)
}

func (a *app) loadConfig(cmd *cobra.Command) (*config.Config, error) {
	cfg, _, _, err := a.loadConfigStat(cmd)
	return cfg, err
}

// loadConfigStat loads the config and returns the path it came from ("" when
// the file cannot be located) with the Stat of the file as read.
func (a *app) loadConfigStat(cmd *cobra.Command) (*config.Config, string, config.Stat, error) {
	path := a.configPath(cmd)
	if path == "" {
		// No way to locate a config file: treat it as absent.
		return &config.Config{}, "", config.Stat{}, nil
	}
	cfg, st, err := config.LoadWithStat(path)
	if err != nil {
		return nil, "", config.Stat{}, usageError(err)
	}
	a.warnBroken(path, cfg)
	return cfg, path, st, nil
}

// warnBroken prints one stderr line per connection entry that did not load.
// Commands go on without them and fail only when they need one.
func (a *app) warnBroken(path string, cfg *config.Config) {
	if a.streams.Err == nil {
		return
	}
	for _, b := range cfg.Broken {
		who := ""
		if b.Name != "" {
			who = fmt.Sprintf("connection %q: ", driver.RedactMessage(b.Name, driver.ConnConfig{}))
		}
		_, _ = fmt.Fprintf(a.streams.Err, "warning: config %s: line %d: %s%s\n",
			query.EscapeControls(path), b.Line, query.EscapeControls(who),
			query.EscapeControls(driver.RedactMessage(b.Err, driver.ConnConfig{})))
	}
}

func (a *app) driverName(cfg driver.ConnConfig) string {
	var d driver.Driver
	var ok bool
	if cfg.Driver != "" {
		d, ok = a.deps.Registry.Lookup(cfg.Driver)
	} else {
		d, ok = a.deps.Registry.ForScheme(driver.Scheme(cfg.DSN))
	}
	if !ok {
		return ""
	}
	return d.Info().Name
}

// openLog returns the logger for this run. Without debug it discards
// everything; with debug it writes JSON lines to the state directory, never
// to stdout or stderr.
func (a *app) openLog(debug bool) (*slog.Logger, func(), error) {
	if !debug {
		return slog.New(slog.NewJSONHandler(io.Discard, nil)), func() {}, nil
	}
	dir := a.getenv("XDG_STATE_HOME")
	switch {
	case dir != "":
	case a.deps.Home != "":
		dir = filepath.Join(a.deps.Home, ".local", "state")
	default:
		return nil, nil, errors.New("--debug: cannot locate the state directory (set XDG_STATE_HOME)")
	}
	dir = filepath.Join(dir, "tobira")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("--debug: create log directory: %w", err)
	}
	fh, err := os.OpenFile(filepath.Join(dir, "tobira.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("--debug: open log file: %w", err)
	}
	h := slog.NewJSONHandler(fh, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), func() { _ = fh.Close() }, nil
}
