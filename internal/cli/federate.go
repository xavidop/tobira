package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/federate"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/session"
)

type federateFlags struct {
	file          string
	format        string
	limit         int
	timeout       time.Duration
	explain       bool
	output        string
	maxSourceRows int
	apply         bool
	dryRun        bool
}

func (a *app) newFederateCmd() *cobra.Command {
	var f federateFlags
	cmd := &cobra.Command{
		Use:   "federate [query]",
		Short: "Run one query over several connections",
		Long: "Run a cross-source query: stage rows from several configured connections, each with\n" +
			"its own native query, join them locally in SQLite and print the result.\n\n" +
			"The query names its connections itself, in comment directives before a final\n" +
			"SQLite statement over the alias tables:\n\n" +
			"  -- @source orders = local-pg/public: select id, user_id, total from orders\n" +
			"  -- @source issues = gh: {\"collection\":\"issues\",\"repo\":\"acme/app\"}\n" +
			"  select o.user_id, count(*) from orders o join issues i on i.author_id = o.user_id group by 1\n\n" +
			"Every source runs read-only, whatever its connection allows, and stages at most\n" +
			"federate.max_source_rows rows (default 10000); the final statement must be a read.\n" +
			"The query is the argument, or is read from a file with -f (\"-f -\" reads stdin).\n\n" +
			"A \"-- @sink <connection>[/<namespace>]: <collection>\" line writes the result into\n" +
			"that collection, one insert per row keyed by the result's column names, with\n" +
			"--apply; --dry-run prints the rows instead. Without either, a statement with a\n" +
			"sink is refused. The target must be writable and its driver must edit rows.",
		// A cross-source statement starts with "-- @source", which cobra would
		// read as a flag, so the command parses its own arguments.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := parseStatementArgs(cmd, args)
			if err != nil {
				return err
			}
			if help, _ := cmd.Flags().GetBool("help"); help {
				return cmd.Help()
			}
			if len(args) > 1 {
				return usageError(fmt.Errorf("accepts at most 1 argument, received %d", len(args)))
			}
			return a.runFederate(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.file, "file", "f", "", `read the query from a file ("-" for stdin)`)
	fl.StringVar(&f.format, "format", "", "output format: table, json, ndjson or csv (default table on a terminal, else json)")
	fl.IntVar(&f.limit, "limit", 1000, "maximum rows of the final result (0 for no limit)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "timeout for the whole run, connecting included (0 for none)")
	fl.BoolVar(&f.explain, "explain", false, "print each source and the final statement instead of running them; contacts no database")
	fl.StringVar(&f.output, "output", "", "write the result to a file (csv, json or ndjson) instead of stdout; keeps every row unless --limit is set")
	fl.IntVar(&f.maxSourceRows, "max-source-rows", 0, fmt.Sprintf("most rows staged from each source, 1 to %d (replaces federate.max_source_rows; default %d)", federate.MaxSourceRowsCap, federate.DefaultMaxSourceRows))
	fl.BoolVar(&f.apply, "apply", false, "write the result into the @sink collection, in one transaction where the driver has them")
	fl.BoolVar(&f.dryRun, "dry-run", false, "with a @sink: print the rows that --apply would write, and write nothing")
	return cmd
}

// parseStatementArgs separates the statement from the flags of a command
// whose statement starts with "--": an argument that starts with "-- " (a
// SQL comment, which no flag can be) is kept as a positional argument and
// the rest is parsed as flags. It returns the positional arguments.
func parseStatementArgs(cmd *cobra.Command, args []string) ([]string, error) {
	var stmts, rest []string
	for _, arg := range args {
		if isCommentArg(arg) {
			stmts = append(stmts, arg)
		} else {
			rest = append(rest, arg)
		}
	}
	if err := cmd.Flags().Parse(rest); err != nil {
		return nil, usageError(err)
	}
	return append(stmts, cmd.Flags().Args()...), nil
}

// isCommentArg reports whether a command-line argument is a SQL comment
// line rather than a flag: a flag never has whitespace after its dashes.
func isCommentArg(arg string) bool {
	return len(arg) > 2 && arg[0] == '-' && arg[1] == '-' && (arg[2] == ' ' || arg[2] == '\t' || arg[2] == '\n')
}

func (a *app) runFederate(cmd *cobra.Command, args []string, f federateFlags) error {
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
	if cmd.Flags().Changed("max-source-rows") && (f.maxSourceRows < 1 || f.maxSourceRows > federate.MaxSourceRowsCap) {
		return usageError(fmt.Errorf("--max-source-rows must be between 1 and %d", federate.MaxSourceRowsCap))
	}
	text, err := a.federateText(args, f.file)
	if err != nil {
		return err
	}
	st, err := federate.Parse(text)
	if err != nil {
		return usageError(fmt.Errorf("cross-source query: %w", err))
	}
	// The engine only reads; the sink is this command's business.
	sink := st.Sink
	switch {
	case f.apply && f.dryRun:
		return usageError(errors.New("--apply and --dry-run cannot be combined"))
	case sink == nil && (f.apply || f.dryRun):
		return usageError(errors.New("--apply and --dry-run need a -- @sink line in the statement"))
	case sink != nil && f.output != "":
		return usageError(errors.New("--output cannot be combined with a @sink: use --apply or --dry-run"))
	case sink != nil && !f.explain && !f.apply && !f.dryRun:
		return usageError(fmt.Errorf("line %d: the statement has a @sink; pass --apply to write the result into %s, or --dry-run to see what would be written", sink.Line, sinkTargetLabel(sink)))
	}
	st.Sink = nil
	// --output settles the format, limit and timeout as it does for tobira
	// query: an explicit --format wins, else the extension; every row and no
	// timeout unless the flags say otherwise.
	var format query.Format
	if f.output != "" {
		qf := queryFlags{format: f.format, limit: f.limit, timeout: f.timeout, explain: f.explain, output: f.output}
		if format, err = a.outputFormat(cmd, &qf); err != nil {
			return err
		}
		f.limit, f.timeout = qf.limit, qf.timeout
	} else if format, err = a.format(f.format); err != nil {
		return err
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
	// Every source (and the sink) names a configured connection; a DSN has
	// no place in a directive, since the statement is meant to be kept and
	// shared.
	var names []string
	for _, s := range st.Sources {
		if _, ok := cfg.Find(s.Conn); !ok {
			return unknownConnection(cfg, s.Conn)
		}
		if !slices.Contains(names, s.Conn) {
			names = append(names, s.Conn)
		}
	}
	if sink != nil {
		if _, ok := cfg.Find(sink.Conn); !ok {
			return unknownConnection(cfg, sink.Conn)
		}
		if !slices.Contains(names, sink.Conn) {
			names = append(names, sink.Conn)
		}
	}
	// Staging is read-only, so every connection is opened read-only at the
	// protocol level too, as tobira query --read-only opens it. Only a sink
	// needs a writable connection: --apply to write, --dry-run to check the
	// target the way --apply would. The sources still run read-only on it.
	mgr, _ := a.newManager(ctx, cfg, !f.apply && !f.dryRun, log)
	defer func() { _ = mgr.CloseAll() }()
	unavailable, err := a.resolvePlugins(ctx, mgr, names, log)
	if err != nil {
		return err // cancelled: the usual exit for an interrupt
	}
	for _, n := range names {
		if why, ok := unavailable[n]; ok {
			return &exitError{code: exitConnection, err: errUnavailableConn(n, why)}
		}
	}
	hist := a.openHistory(ctx, log, cfg)
	defer func() { _ = hist.Close() }()

	maxRows := f.maxSourceRows
	if maxRows == 0 {
		maxRows = cfg.FederateMaxSourceRows()
	}
	log.Info("federate start", "sources", len(st.Sources), "connections", names, "format", string(format),
		"explain", f.explain, "max_source_rows", maxRows)
	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}
	if f.explain {
		start := time.Now()
		entry := history.Entry{At: start, Conn: federate.HistoryConn, Source: history.SourceCLI, Query: federateHistoryText(mgr, st, text), OK: true}
		entry.Took = time.Since(start)
		a.recordHistory(ctx, log, hist, entry, driver.ConnConfig{})
		st.Sink = sink // the plan names it
		return query.Write(cmd.OutOrStdout(), format, &query.Result{Plan: federate.Explain(st), Stats: driver.Stats{RowsAffected: -1}})
	}
	// The sink is checked before anything is staged: a refusal costs nothing.
	var target *sinkTarget
	if sink != nil {
		if target, err = a.openSink(ctx, mgr, sink); err != nil {
			return err
		}
	}
	res, err := a.runFederated(ctx, mgr, st, text, federate.Options{MaxSourceRows: maxRows, Limit: f.limit, Registry: a.deps.Registry}, hist, log)
	if err != nil {
		return err
	}
	switch {
	case f.output != "":
		return a.writeFederateFile(ctx, cmd, res, format, f.output)
	case f.dryRun:
		if err := query.Write(cmd.OutOrStdout(), format, res); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "dry run: %s would be inserted into %s; nothing written\n", plural(int64(len(res.Rows)), "row", "rows"), target.label)
		return err
	case f.apply:
		return a.applySink(ctx, cmd, mgr, target, res, hist, log)
	}
	return query.Write(cmd.OutOrStdout(), format, res)
}

// sinkTarget is the collection a @sink writes to, opened and checked.
type sinkTarget struct {
	conn  driver.Conn
	name  string // the connection
	ref   driver.CollectionRef
	label string // "conn ▸ ns.coll", escaped
}

// sinkTargetLabel names a sink's collection as messages do.
func sinkTargetLabel(s *federate.Sink) string {
	coll := s.Collection
	if s.Namespace != "" {
		coll = s.Namespace + "." + coll
	}
	return query.EscapeControls(s.Conn + " ▸ " + coll)
}

// openSink opens the sink's connection and checks the target: the
// connection must not be read-only (exit 4), its driver must edit rows and
// the collection must exist. Errors are redacted and map to the usual exit
// codes.
func (a *app) openSink(ctx context.Context, mgr *session.Manager, s *federate.Sink) (*sinkTarget, error) {
	label := sinkTargetLabel(s)
	conn, err := mgr.Open(ctx, s.Conn)
	if err != nil {
		return nil, connectError(redactedWith(mgr, s.Conn, fmt.Errorf("sink %s: %w", label, err)))
	}
	if mgr.State(s.Conn).ReadOnly {
		return nil, fmt.Errorf("sink %s: the connection is read-only, so --apply cannot write to it: %w", label, driver.ErrReadOnly)
	}
	if _, ok := driver.As[driver.Mutator](conn); !ok {
		return nil, fmt.Errorf("sink %s: the %s driver does not edit rows: %w", label, mgr.State(s.Conn).Info.Name, driver.ErrUnsupported)
	}
	ref := driver.CollectionRef{Namespace: s.Namespace, Name: s.Collection}
	if _, err := conn.Describe(ctx, ref); err != nil {
		return nil, redactedWith(mgr, s.Conn, fmt.Errorf("sink %s: %w", label, err))
	}
	return &sinkTarget{conn: conn, name: s.Conn, ref: ref, label: label}, nil
}

// applySink inserts the result's rows into the sink, one insert per row
// keyed by the result's column names, in one transaction where the driver
// has them (nothing is written on a failure), else one at a time, stopping
// at the first error. The write is recorded in the history on the sink's
// connection and reported on stderr.
func (a *app) applySink(ctx context.Context, cmd *cobra.Command, mgr *session.Manager, t *sinkTarget, res *query.Result, hist *history.Store, log *slog.Logger) error {
	n := len(res.Rows)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "inserting %s into %s\n", plural(int64(n), "row", "rows"), t.label)
	start := time.Now()
	written, err := insertRows(ctx, t.conn, t.ref, res.Columns, res.Rows)
	what := fmt.Sprintf("insert into %s (%d rows from a cross-source query)", qualifiedRef(t.ref), n)
	entry := history.Entry{At: start, Conn: t.name, Source: history.SourceCLI, Query: what, OK: err == nil, Rows: int64(written), Took: time.Since(start)}
	a.recordHistory(ctx, log, hist, entry, driver.ConnConfig{})
	if err != nil {
		log.Error("sink failed", "kind", driver.KindName(err), "written", written, "duration", entry.Took.String())
		return redactedWith(mgr, t.name, fmt.Errorf("sink %s: inserted %d of %d rows: %w", t.label, written, n, err))
	}
	log.Info("sink done", "rows", written, "duration", entry.Took.String())
	_, err = fmt.Fprintf(cmd.ErrOrStderr(), "inserted %s into %s\n", plural(int64(written), "row", "rows"), t.label)
	return err
}

// qualifiedRef is "ns.name", or the name alone without a namespace.
func qualifiedRef(ref driver.CollectionRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "." + ref.Name
}

// insertRows writes rows into ref through conn's Mutator: in one transaction
// when conn is a Transactor whose transactions edit rows, else one at a time.
// It returns how many rows were written; in a transaction that is all or
// none.
func insertRows(ctx context.Context, conn driver.Conn, ref driver.CollectionRef, cols []driver.Column, rows []driver.Row) (int, error) {
	objs := make([]driver.Object, len(rows))
	for i, row := range rows {
		o := make(driver.Object, 0, len(cols))
		for j, c := range cols {
			if j < len(row) {
				o = append(o, driver.Member{Key: c.Name, Value: row[j]})
			}
		}
		objs[i] = o
	}
	if tr, ok := driver.As[driver.Transactor](conn); ok {
		tx, err := tr.Begin(ctx, driver.TxOptions{})
		if err != nil {
			return 0, err
		}
		if mu, ok := driver.As[driver.Mutator](tx); ok {
			for i, o := range objs {
				if _, err := mu.Insert(ctx, ref, o); err != nil {
					rollbackSink(tx)
					return 0, fmt.Errorf("row %d: %w", i+1, err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				rollbackSink(tx)
				return 0, err
			}
			return len(objs), nil
		}
		rollbackSink(tx) // the transaction cannot edit: write through the connection
	}
	mu, _ := driver.As[driver.Mutator](conn)
	for i, o := range objs {
		if _, err := mu.Insert(ctx, ref, o); err != nil {
			return i, fmt.Errorf("row %d: %w", i+1, err)
		}
	}
	return len(objs), nil
}

// rollbackSink ends a failed sink transaction with its own bounded context:
// the run's may already be cancelled. Its error adds nothing to the failure.
func rollbackSink(tx driver.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// runFederated runs st through the engine with opts and records the run in
// the history: each staged query on its own connection, the whole statement
// under the connection name federated. The error is scrubbed; the result is
// ready for query.Write.
func (a *app) runFederated(ctx context.Context, mgr *session.Manager, st *federate.Statement, text string, opts federate.Options,
	hist *history.Store, log *slog.Logger) (*query.Result, error) {
	start := time.Now()
	entry := history.Entry{At: start, Conn: federate.HistoryConn, Source: history.SourceCLI, Query: federateHistoryText(mgr, st, text)}
	res, err := federate.Run(ctx, mgr, st, opts)
	entry.Took = time.Since(start)
	if res != nil {
		a.recordSources(ctx, log, hist, mgr, start, res.Sources)
	}
	if err != nil {
		log.Error("federate failed", "kind", driver.KindName(err), "duration", entry.Took.String())
		a.recordHistory(ctx, log, hist, entry, driver.ConnConfig{})
		// The engine redacts each source's message with its own connection;
		// the generic pass covers the rest.
		return nil, scrub(err, driver.ConnConfig{})
	}
	entry.OK, entry.Rows = true, int64(len(res.Rows))
	a.recordHistory(ctx, log, hist, entry, driver.ConnConfig{})
	log.Info("federate done", "rows", len(res.Rows), "duration", entry.Took.String())
	return &query.Result{Columns: res.Columns, Rows: res.Rows, Stats: res.Stats}, nil
}

// federateText returns the query from the positional argument or the -f file.
func (a *app) federateText(args []string, file string) (string, error) {
	switch {
	case len(args) == 1 && file != "":
		return "", usageError(errors.New("give the query either as an argument or with -f, not both"))
	case len(args) == 0 && file == "":
		return "", usageError(errors.New("a query is required: pass it as an argument or with -f"))
	case len(args) == 1:
		return args[0], nil
	}
	return a.queryText([]string{"federate"}, file)
}

// federateHistoryText is the statement as the history stores it: masked
// with the settings of every connection it names, then generically.
func federateHistoryText(mgr *session.Manager, st *federate.Statement, text string) string {
	for _, s := range st.Sources {
		text = mgr.Redact(s.Conn, text)
	}
	return text
}

// recordSources records each staged query on its own connection, so
// `tobira history --conn <name>` shows what ran there. A source that never
// started (cancelled because another failed first) is left out.
func (a *app) recordSources(ctx context.Context, log *slog.Logger, hist *history.Store, mgr *session.Manager, at time.Time, runs []federate.SourceRun) {
	for _, r := range runs {
		if r.Err != nil && r.Took == 0 {
			continue
		}
		a.recordHistory(ctx, log, hist, history.Entry{
			At: at, Conn: r.Conn, Source: history.SourceCLI, Query: mgr.Redact(r.Conn, r.Query),
			OK: r.Err == nil, Rows: int64(r.Rows), Took: r.Took,
		}, driver.ConnConfig{})
	}
}

// writeFederateFile writes the collected result to path and reports one line
// on stderr, as `tobira query --output` does.
func (a *app) writeFederateFile(ctx context.Context, cmd *cobra.Command, res *query.Result, format query.Format, path string) error {
	out, err := export.ToFile(ctx, export.RowsSource{Columns: res.Columns, Rows: res.Rows, Stats: res.Stats}, export.Options{Format: format, Path: path})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s to %s\n", plural(out.Rows, "row", "rows"), query.EscapeControls(out.Path))
	return err
}
