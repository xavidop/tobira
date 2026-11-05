package federate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Opener opens connections by name. *session.Manager is one.
type Opener interface {
	Open(ctx context.Context, name string) (driver.Conn, error)
}

// redactor masks a connection's secrets in text. *session.Manager is one;
// an Opener that is not one gets its errors unredacted.
type redactor interface {
	Redact(name, text string) string
}

// The limits of a run.
const (
	// DefaultMaxSourceRows is how many rows a source stages when Options
	// says nothing.
	DefaultMaxSourceRows = 10_000
	// MaxSourceRowsCap is the most rows a source may stage, whatever Options
	// says.
	MaxSourceRowsCap = 1_000_000
	// stageConcurrency is how many sources are read at once.
	stageConcurrency = 4
)

// Options configures Run.
type Options struct {
	// MaxSourceRows bounds the rows staged per source; not positive means
	// DefaultMaxSourceRows, and it is capped at MaxSourceRowsCap.
	MaxSourceRows int
	// Limit bounds the rows of the final result; 0 = no limit.
	Limit int
	// Registry holds the sqlite driver that runs the final statement; nil
	// means driver.Default.
	Registry *driver.Registry
	// Allowed is the set of connections a directive may name; nil means any
	// connection the Opener knows. The assistant and the MCP server narrow
	// it to the turn's connections; any other name is refused as unknown.
	Allowed []string
}

// SourceRun is the outcome of staging one source, for the caller's history.
type SourceRun struct {
	Source
	Rows      int
	Truncated bool
	Took      time.Duration
	// Err is why the source failed; nil when it staged.
	Err error
}

// note is the warning the result carries for the source.
func (r SourceRun) note() string {
	s := fmt.Sprintf("source %s (%s): %d rows", r.Alias, r.Conn, r.Rows)
	if r.Truncated {
		s += ", more existed"
	}
	return s
}

// Result is a finished run.
type Result struct {
	Columns []driver.Column
	Rows    []driver.Row
	// Stats covers the whole run; its warnings say how many rows each
	// source staged and whether more existed.
	Stats driver.Stats
	// Sources is the outcome of each source, in directive order. It is set
	// even when Run fails after staging, so a caller can record what ran.
	Sources []SourceRun
}

// SourceError is a source that failed to stage. Its message is redacted for
// the connection; errors.Is still sees the driver's classification.
type SourceError struct {
	Alias, Conn string
	Err         error
	msg         string
}

func (e *SourceError) Error() string { return "source " + e.Alias + " on " + e.Conn + ": " + e.msg }
func (e *SourceError) Unwrap() error { return e.Err }

// HistoryConn is the connection name a run's final statement is recorded
// under in the history, and the label of its result tab; the staged queries
// are recorded on their own connections.
const HistoryConn = "federated"

// Dialect is the dialect of the final statement, for highlighting: the
// scratch engine's.
const Dialect = "sqlite"

// errFinalWrite refuses a final statement the scratch database's read-only
// guard rejected. It carries driver.ErrReadOnly, so the surfaces treat it as
// any other read-only refusal (the CLI exits with the read-only code).
var errFinalWrite = fmt.Errorf("the final statement of a cross-source query must be a read: %w", driver.ErrReadOnly)

// Run stages every source of st read-only, loads the rows into a scratch
// SQLite database and runs the final statement there, read-only too. The
// returned Result carries the sources that ran even when the error is not
// nil, and no columns or rows then.
func Run(ctx context.Context, open Opener, st *Statement, opts Options) (*Result, error) {
	start := time.Now()
	if st.Sink != nil {
		return nil, &ParseError{st.Sink.Line, "@sink is not supported yet"}
	}
	if err := checkAllowed(st, opts.Allowed); err != nil {
		return nil, err
	}
	maxRows := opts.MaxSourceRows
	if maxRows <= 0 {
		maxRows = DefaultMaxSourceRows
	}
	maxRows = min(maxRows, MaxSourceRowsCap)

	staged, runs, err := stage(ctx, open, st.Sources, maxRows)
	res := &Result{Sources: runs}
	if err != nil {
		return res, err
	}
	sc, err := openScratch(ctx, opts.Registry)
	if err != nil {
		return res, err
	}
	defer sc.close()
	for i, src := range st.Sources {
		if err := sc.load(ctx, src.Alias, staged[i].cols, staged[i].rows); err != nil {
			return res, fmt.Errorf("stage %s: %w", src.Alias, err)
		}
	}
	rs, err := sc.conn.Query(ctx, driver.Query{Text: st.Final, Limit: opts.Limit, ReadOnly: true})
	if err != nil {
		return res, finalError(err)
	}
	cols := slices.Clone(rs.Columns())
	rows, stats, err := driver.Collect(ctx, rs, opts.Limit)
	if err != nil {
		return res, finalError(err)
	}
	// The columns come from the scratch tables: nothing to edit inline.
	for i := range cols {
		cols[i].Source, cols[i].Key = nil, false
	}
	stats.Duration = time.Since(start)
	stats.RowsAffected = -1
	for _, r := range runs {
		stats.Warnings = append(stats.Warnings, r.note())
	}
	res.Columns, res.Rows, res.Stats = cols, rows, stats
	return res, nil
}

// Explain describes what Run would do, without contacting any database:
// each source with its connection and query, then the final statement.
func Explain(st *Statement) *driver.Plan {
	var sb strings.Builder
	root := &driver.PlanNode{Label: "cross-source query", Rows: -1}
	for _, s := range st.Sources {
		target := s.Conn
		if s.Namespace != "" {
			target += "/" + s.Namespace
		}
		fmt.Fprintf(&sb, "source %s: %s, read-only\n  %s\n", s.Alias, target, indent(s.Query))
		root.Children = append(root.Children, &driver.PlanNode{Label: "stage " + s.Alias, Detail: target + ": " + s.Query, Rows: -1})
	}
	fmt.Fprintf(&sb, "final statement (sqlite, read-only):\n  %s\n", indent(st.Final))
	root.Children = append(root.Children, &driver.PlanNode{Label: "run in sqlite", Detail: st.Final, Rows: -1})
	if s := st.Sink; s != nil {
		target := s.Conn
		if s.Namespace != "" {
			target += "/" + s.Namespace
		}
		fmt.Fprintf(&sb, "sink: insert the result into %s: %s\n", target, s.Collection)
		root.Children = append(root.Children, &driver.PlanNode{Label: "insert into " + s.Collection, Detail: target + ": " + s.Collection, Rows: -1})
	}
	return &driver.Plan{Format: "text", Text: sb.String(), Root: root}
}

func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n  ") }

func checkAllowed(st *Statement, allowed []string) error {
	if allowed == nil {
		return nil
	}
	for _, s := range st.Sources {
		if !slices.Contains(allowed, s.Conn) {
			return &ParseError{s.Line, fmt.Sprintf("unknown connection %q", s.Conn)}
		}
	}
	return nil
}

func finalError(err error) error {
	if errors.Is(err, driver.ErrReadOnly) {
		return errFinalWrite
	}
	return fmt.Errorf("final statement: %w", err)
}

// stagedRows is what one source produced.
type stagedRows struct {
	cols []driver.Column
	rows []driver.Row
}

// stage reads every source, stageConcurrency at a time. The first failure
// cancels the rest and is the error returned; a source cancelled because
// another failed reports context.Canceled in its run.
func stage(ctx context.Context, open Opener, sources []Source, maxRows int) ([]stagedRows, []SourceRun, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]stagedRows, len(sources))
	runs := make([]SourceRun, len(sources))
	sem := make(chan struct{}, stageConcurrency)
	var wg sync.WaitGroup
	for i, src := range sources {
		runs[i].Source = src
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				runs[i].Err = ctx.Err()
				return
			}
			defer func() { <-sem }()
			out[i], runs[i] = stageOne(ctx, open, src, maxRows)
			if runs[i].Err != nil {
				cancel()
			}
		}()
	}
	wg.Wait()
	// A real failure first, then whatever cancellation is left.
	for _, r := range runs {
		if r.Err != nil && !errors.Is(r.Err, context.Canceled) {
			return out, runs, r.Err
		}
	}
	for _, r := range runs {
		if r.Err != nil {
			return out, runs, r.Err
		}
	}
	return out, runs, nil
}

// stageOne runs one source read-only and collects up to maxRows rows.
func stageOne(ctx context.Context, open Opener, src Source, maxRows int) (stagedRows, SourceRun) {
	run := SourceRun{Source: src}
	start := time.Now()
	fail := func(err error) (stagedRows, SourceRun) {
		run.Took = time.Since(start)
		run.Err = sourceError(open, src, err)
		return stagedRows{}, run
	}
	conn, err := open.Open(ctx, src.Conn)
	if err != nil {
		return fail(err)
	}
	rs, err := conn.Query(ctx, driver.Query{Text: src.Query, Namespace: src.Namespace, Limit: maxRows, ReadOnly: true})
	if err != nil {
		return fail(err)
	}
	cols := slices.Clone(rs.Columns())
	rows, stats, err := driver.Collect(ctx, rs, maxRows)
	if err != nil {
		return fail(err)
	}
	if len(cols) == 0 {
		return fail(errors.New("the query returned no columns"))
	}
	run.Rows, run.Truncated, run.Took = len(rows), stats.Truncated, time.Since(start)
	return stagedRows{cols: cols, rows: rows}, run
}

// sourceError wraps a source failure with its alias and connection, the
// message redacted when the Opener can redact.
func sourceError(open Opener, src Source, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	msg := err.Error()
	if r, ok := open.(redactor); ok {
		msg = r.Redact(src.Conn, msg)
	}
	if errors.Is(err, driver.ErrReadOnly) {
		msg = "sources are always read-only; " + msg
	}
	return &SourceError{Alias: src.Alias, Conn: src.Conn, Err: err, msg: msg}
}
