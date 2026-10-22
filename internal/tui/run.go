package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/tui/async"
)

const (
	// defaultLimit is the row limit of an editor run until :set limit.
	defaultLimit = 1000
	// recordTimeout bounds recording one history entry.
	recordTimeout = 2 * time.Second
)

// HistoryStore is the query history: the editor records every run in it and
// reads it back for ↑/↓ and ctrl+r. *history.Store satisfies it; nil
// records nothing and leaves both off.
type HistoryStore interface {
	Record(ctx context.Context, e history.Entry, cc driver.ConnConfig) error
	List(ctx context.Context, f history.Filter) ([]history.Entry, error)
}

// runner runs editor statements: the connection, or the open transaction
// (:tx) on it.
type runner interface {
	Query(ctx context.Context, q driver.Query) (driver.ResultSet, error)
}

var (
	// errNoExplain is returned by an explain on a connection without Explainer.
	errNoExplain = errors.New("explain is not supported")
	// errNoExplainTx is returned by an explain inside a transaction that has
	// no Explainer of its own.
	errNoExplainTx = errors.New("explain inside a transaction is not supported")
)

// runOutcome is what a run collected: a result, or a plan for an explain.
type runOutcome struct {
	cols  []driver.Column
	rows  []driver.Row
	stats driver.Stats
	plan  *driver.Plan
	// reached is set once the statement was handed to the driver; an error
	// before that (opening, waiting) cannot have aborted a transaction.
	reached bool
}

// runDoneMsg is the result of an editor run. Its ID is checked against the
// run tracker, so a cancelled or superseded run never lands in a tab.
type runDoneMsg = async.Result[runOutcome]

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// statementAt returns the span of the statement under cursor: the span
// containing it, else the nearest one before it, else the first.
func statementAt(spans []sqlcomplete.Span, cursor int) sqlcomplete.Span {
	best := spans[0]
	for _, s := range spans {
		if s.Start <= cursor {
			best = s
		}
		if s.Start <= cursor && cursor <= s.End {
			return s
		}
	}
	return best
}

// editorStatement is what ctrl+enter runs: the selection, or the statement
// under the cursor. refusal is set when there is nothing to run.
func (a *Model) editorStatement() (stmt, refusal string) {
	text := a.ed.Value()
	dialect := a.ed.Dialect()
	if start, end, ok := a.ed.Selection(); ok {
		sel := text[start:end]
		spans := splitStatements(dialect, sel)
		switch n := len(spans); {
		case n == 0:
			return "", "the selection holds no statement"
		case n > 1:
			return "", fmt.Sprintf("the selection holds %d statements; select one", n)
		}
		// Exactly the statement Split found: no trailing ";" or comment.
		return sel[spans[0].Start:spans[0].End], ""
	}
	spans := splitStatements(dialect, text)
	if len(spans) == 0 {
		return "", "nothing to run: type a statement first"
	}
	s := statementAt(spans, a.ed.Cursor())
	return text[s.Start:s.End], ""
}

// runStatement runs the selection or the statement under the cursor.
func (a *Model) runStatement() tea.Cmd {
	stmt, refusal := a.editorStatement()
	if refusal != "" {
		return a.flash(refusal, false)
	}
	return a.startRun(stmt, false)
}

// runBuffer runs the whole buffer, which must hold one statement.
func (a *Model) runBuffer() tea.Cmd {
	text := a.ed.Value()
	spans := splitStatements(a.ed.Dialect(), text)
	switch n := len(spans); {
	case n == 0:
		return a.flash("nothing to run: type a statement first", false)
	case n > 1:
		return a.flash(fmt.Sprintf("the buffer holds %d statements; put the cursor in one and press ctrl+enter", n), false)
	}
	return a.startRun(text[spans[0].Start:spans[0].End], false)
}

// explain explains the selection or the statement under the cursor.
func (a *Model) explain() tea.Cmd {
	stmt, refusal := a.editorStatement()
	if refusal != "" {
		return a.flash(strings.Replace(refusal, "nothing to run", "nothing to explain", 1), false)
	}
	return a.startRun(stmt, true)
}

// startRun runs stmt on the editor's connection in a command with its own
// cancellable context, into a new tab. The connection is opened there if it
// is not open yet; the rows are drained, up to the limit, there too.
func (a *Model) startRun(stmt string, explain bool) tea.Cmd {
	a.closePopup()
	if a.running != nil {
		return a.flash("a statement is still running; ctrl+c cancels it", false)
	}
	name := a.edConn
	if a.tx != nil && a.tx.conn == name {
		if a.tx.busy != "" {
			return a.flash(a.txBusyText(), false)
		}
		if a.tx.stuck {
			return a.flash(stuckText, false)
		}
		if a.apply != nil && a.apply.mode == applyOpenTx {
			return a.flash(applyingText, false)
		}
	}
	if name == "" || !hasConn(a.mgr, name) {
		return a.flash("select a connection in the sidebar first", false)
	}
	st := a.mgr.State(name)
	if st.Broken {
		return a.flash("connection "+name+" is misconfigured", true)
	}
	ns := ""
	if a.cur.set && a.cur.conn == name {
		ns = a.cur.ref.Namespace
	}
	t := a.addTab(name, stmt, explain)
	a.running = t
	a.runBuf[name] = a.ed.Value() // what quit compares the buffer with
	done := make(chan struct{})
	t.done = done
	var exec driver.Tx
	var users []<-chan struct{}
	if exec, t.inTx = a.txOn(name); t.inTx {
		users = a.txUsers(done) // wait for a cancelled run still inside it
	}
	wait, hook := a.txWait(), a.txWaitHook
	a.dropDetail()
	a.wantTick = true // the connection may be opening
	id, ctx := a.runs.Start(context.Background())
	mgr, limit := a.mgr, a.limit
	q := driver.Query{Text: stmt, Namespace: ns, Limit: limit, ReadOnly: st.ReadOnly}
	return async.Do(id, ctx, func(ctx context.Context) (runOutcome, error) {
		defer close(done)
		conn, err := mgr.Open(ctx, name)
		if err != nil {
			return runOutcome{}, err
		}
		if explain {
			ex, ok := driver.As[driver.Explainer](conn)
			if exec != nil {
				// Inside a transaction the plan must come from it too, once a
				// cancelled call still inside it has returned.
				if ex, ok = driver.As[driver.Explainer](exec); !ok {
					return runOutcome{}, errNoExplainTx
				}
				if err := waitUsers(ctx, users, wait, hook); err != nil {
					return runOutcome{}, err
				}
			}
			if !ok {
				return runOutcome{}, errNoExplain
			}
			plan, err := ex.Explain(ctx, q, driver.ExplainOptions{})
			return runOutcome{plan: plan}, err
		}
		var r runner = conn
		if exec != nil {
			if err := waitUsers(ctx, users, wait, hook); err != nil {
				return runOutcome{}, err
			}
			r = exec
		}
		rs, err := r.Query(ctx, q)
		if err != nil {
			return runOutcome{reached: true}, err
		}
		cols := rs.Columns()
		rows, stats, err := driver.Collect(ctx, rs, limit)
		return runOutcome{cols: cols, rows: rows, stats: stats, reached: true}, err
	})
}

// runDone puts a finished run into its tab and records it in the history.
func (a *Model) runDone(r runDoneMsg) tea.Cmd {
	if !a.runs.Finish(r.ID) {
		return nil
	}
	t := a.running
	a.running = nil
	if t == nil {
		return nil
	}
	t.took = r.Took
	entry := history.Entry{At: t.started, Conn: t.conn, Source: history.SourceTUI, Query: t.stmt, Took: r.Took}
	var flash, inv tea.Cmd
	v := r.Val
	switch {
	case r.Err != nil:
		t.state = tabFailed
		text := a.runErrText(t, r.Err)
		if errors.Is(r.Err, errTxStuck) && a.tx != nil {
			a.tx.stuck = true
			text = stuckText
		}
		// Only an error the database may have seen can abort the transaction:
		// not one raised before the call, nor a read-only refusal (the driver
		// guards refuse before running anything).
		if t.inTx && r.Val.reached && !errors.Is(r.Err, driver.ErrReadOnly) {
			text += "\n" + abortNote
		}
		t.setText(text)
		t.err, t.errText = r.Err, text
		flash = a.flash(strings.ReplaceAll(text, "\n", "; "), true)
	case t.explain:
		t.state = tabDone
		t.setText("explain · " + duration(r.Took) + "\n\n" + planText(v.plan))
		entry.OK = true
	case len(v.cols) == 0:
		t.state = tabDone
		n := max(v.stats.RowsAffected, 0)
		t.setText(fmt.Sprintf("%s %s affected · %s", commas(int(n)), plural(int(n), "row", "rows"), duration(r.Took)))
		entry.OK, entry.Rows = true, n
		// DDL or DML: a new or dropped name must show in completion now.
		inv = a.invalidateCmd(t.conn)
	default:
		t.state = tabDone
		t.isText = false
		t.gr.SetFormat(resultFormat(a.mgr.State(t.conn).Info))
		t.gr.OpenStatic(v.cols, v.rows)
		t.rows, t.truncated = len(v.rows), v.stats.Truncated
		t.cols, t.result, t.stats = v.cols, v.rows, v.stats // for :export
		entry.OK, entry.Rows = true, int64(len(v.rows))
	}
	if t.noHistory {
		return tea.Batch(flash, inv)
	}
	return tea.Batch(flash, inv, a.recordCmd(entry))
}

// cancelRun cancels the run in flight, if any, marks its tab and returns the
// command that records it in the history.
func (a *Model) cancelRun() (tea.Cmd, bool) {
	t := a.running
	if t == nil {
		return nil, false
	}
	a.runs.Cancel()
	a.running = nil
	if t.done != nil {
		a.stopping = append(stillRunning(a.stopping), t.done)
	}
	t.state = tabCancelled
	t.took = time.Since(t.started)
	t.setText("cancelled after " + duration(t.took))
	return a.recordCmd(history.Entry{
		At: t.started, Conn: t.conn, Source: history.SourceTUI, Query: t.stmt, Took: t.took,
	}), true
}

// stopWait bounds how long a config write waits for cancelled runs to stop.
// A driver honours cancellation well before this; one that does not is not
// waited on forever.
const stopWait = 5 * time.Second

// takeStopping hands the cancelled runs still stopping to a command that must
// wait for them.
func (a *Model) takeStopping() []<-chan struct{} {
	out := stillRunning(a.stopping)
	a.stopping = nil
	return out
}

// stillRunning drops the runs that have already returned.
func stillRunning(stopping []<-chan struct{}) []<-chan struct{} {
	out := stopping[:0]
	for _, done := range stopping {
		select {
		case <-done:
		default:
			out = append(out, done)
		}
	}
	return out
}

// waitStopped waits, off the Update goroutine, until every run in stopping
// has returned (or stopWait passed), so no run still uses a connection the
// caller is about to close.
func waitStopped(stopping []<-chan struct{}) {
	if len(stopping) == 0 {
		return
	}
	deadline := time.NewTimer(stopWait)
	defer deadline.Stop()
	for _, done := range stopping {
		select {
		case <-done:
		case <-deadline.C:
			return
		}
	}
}

// sizeLimitHint follows a run that hit a size limit (driver.ErrLimit).
const sizeLimitHint = "the result hit a size limit: narrow the query (add a LIMIT, select fewer or smaller columns) or raise the driver's limit"

// runErrText is the message for a failed run: redacted with the run's
// connection (and escaped when drawn).
func (a *Model) runErrText(t *resultTab, err error) string {
	switch {
	case errors.Is(err, errNoExplain):
		return "explain is not supported by " + a.mgr.State(t.conn).Info.Name
	case errors.Is(err, errNoExplainTx):
		return "explain inside a transaction is not supported by " + a.mgr.State(t.conn).Info.Name
	case errors.Is(err, driver.ErrReadOnly):
		return "🔒 " + t.conn + " is read-only; the statement was refused"
	case errors.Is(err, driver.ErrLimit):
		return a.mgr.Redact(t.conn, err.Error()) + "\n" + sizeLimitHint
	}
	return a.embedderText(err, a.mgr.Redact(t.conn, err.Error()))
}

// planText renders a plan: its text, with a JSON plan pretty-printed, or the
// parsed tree as JSON when there is no text.
func planText(p *driver.Plan) string {
	if p == nil {
		return ""
	}
	if p.Text == "" && p.Root != nil {
		b, err := json.MarshalIndent(p.Root, "", "  ")
		if err == nil {
			return string(b)
		}
	}
	if strings.EqualFold(p.Format, "json") {
		var buf bytes.Buffer
		if json.Indent(&buf, []byte(p.Text), "", "  ") == nil {
			return buf.String()
		}
	}
	return p.Text
}

// recordCmd stores e in the history, off the Update goroutine and bounded.
// The query text is redacted with the connection's settings first; a
// failure is only logged.
func (a *Model) recordCmd(e history.Entry) tea.Cmd {
	if a.hist == nil {
		return nil
	}
	h, log := a.hist, a.log
	e.Query = a.mgr.Redact(e.Conn, e.Query)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
		defer cancel()
		if err := h.Record(ctx, e, driver.ConnConfig{}); err != nil {
			log.Debug("history record failed", "error", driver.RedactMessage(err.Error(), driver.ConnConfig{}))
		}
		return nil
	}
}

// duration formats a run time like the status bar.
func duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
