package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/async"
)

const (
	// similarK is how many neighbours "find similar" asks for.
	similarK = 20
	// similarLabelLen is how many characters a similar tab's label shows.
	similarLabelLen = 32
)

const (
	noVectorDriverText = "find similar needs a vector driver"
	noVectorCellText   = "this row has no vector to search with"
	similarWhereText   = "find similar works on the browse tab or a similar tab"
)

// similarTarget is the collection S searches from the active tab: the
// browse collection, or the one a similar tab searched.
func (a *Model) similarTarget() (conn string, ref driver.CollectionRef, why string) {
	if t := a.activeTab(); t != nil {
		if t.simRef == nil {
			return "", driver.CollectionRef{}, similarWhereText
		}
		return t.conn, *t.simRef, ""
	}
	if !a.cur.set {
		return "", driver.CollectionRef{}, "open a vector collection first"
	}
	return a.cur.conn, a.cur.ref, ""
}

// searcher returns the vector searcher of open connection conn, or why not.
func (a *Model) searcher(conn string) (string, bool) {
	c, ok := a.mgr.Conn(conn)
	if !ok {
		return "connection " + conn + " is not open", false
	}
	if _, ok := driver.As[driver.VectorSearcher](c); !ok {
		return noVectorDriverText, false
	}
	return "", true
}

// findSimilar is S: the rows nearest to the vector of the row under the
// cursor (the cursor's cell when it is a vector, else the row's first
// vector), in a new tab.
func (a *Model) findSimilar() tea.Cmd {
	conn, ref, why := a.similarTarget()
	if why != "" {
		return a.flash(why, false)
	}
	if why, ok := a.searcher(conn); !ok {
		return a.flash(why, false)
	}
	gr := a.activeGrid()
	row, ok := gr.CurrentRow()
	if !ok {
		return a.flash("no row to search from", false)
	}
	cols := gr.Columns()
	_, cur := gr.Cursor()
	vi := -1
	if cur < len(row) && cur < len(cols) && row[cur].Type() == driver.TypeVector {
		vi = cur
	}
	for i := 0; vi < 0 && i < len(row) && i < len(cols); i++ {
		if row[i].Type() == driver.TypeVector {
			vi = i
		}
	}
	if vi < 0 {
		return a.flash(noVectorCellText, false)
	}
	vec, _ := row[vi].AsVector()
	q := driver.VectorQuery{Vector: vec, Field: cols[vi].Name, K: similarK}
	return a.startSimilar(conn, ref, q, "similar to "+rowID(cols, row))
}

// rowID names a row: its key column, else an id or _id column, else its
// first column.
func rowID(cols []driver.Column, row driver.Row) string {
	idx := -1
	for i, c := range cols {
		if c.Key && i < len(row) {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = max(kvColumn(cols, "id", "_id", "_key"), 0)
	}
	if idx >= len(row) {
		return "row"
	}
	return row[idx].String()
}

// similarText is :similar <text>: the rows nearest to text, which the
// driver embeds with the host's embedder.
func (a *Model) similarText(text string) tea.Cmd {
	if !a.cur.set {
		return a.flash("open a vector collection first", false)
	}
	if why, ok := a.searcher(a.cur.conn); !ok {
		return a.flash(why, false)
	}
	q := driver.VectorQuery{Text: text, K: similarK}
	return a.startSimilar(a.cur.conn, a.cur.ref, q, "similar to "+strconv.Quote(text))
}

// startSimilar runs q on ref in a cancellable command, into a new tab
// named label. ctrl+c cancels it like an editor run; it is not recorded in
// the query history.
func (a *Model) startSimilar(conn string, ref driver.CollectionRef, q driver.VectorQuery, label string) tea.Cmd {
	if a.running != nil {
		return a.flash("a statement is still running; ctrl+c cancels it", false)
	}
	t := a.addTab(conn, label, false)
	t.label = cutLabel(label, similarLabelLen)
	t.simRef, t.noHistory = &ref, true
	a.running = t
	done := make(chan struct{})
	t.done = done
	a.dropDetail()
	id, ctx := a.runs.Start(context.Background())
	mgr := a.mgr
	return async.Do(id, ctx, func(ctx context.Context) (runOutcome, error) {
		defer close(done)
		c, err := mgr.Open(ctx, conn)
		if err != nil {
			return runOutcome{}, err
		}
		vs, ok := driver.As[driver.VectorSearcher](c)
		if !ok {
			return runOutcome{}, errors.New(noVectorDriverText)
		}
		rs, err := vs.Similar(ctx, ref, q)
		if err != nil {
			return runOutcome{reached: true}, err
		}
		cols := rs.Columns()
		rows, stats, err := driver.Collect(ctx, rs, max(q.K, 1))
		return runOutcome{cols: cols, rows: rows, stats: stats, reached: true}, err
	})
}

// embedderText adds the embedder's build error to a driver's "needs
// ai.embedder" refusal, so the user sees why there is none.
func (a *Model) embedderText(err error, text string) string {
	if a.embedErr == nil || !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		return text
	}
	return driver.NeedEmbedderHint + ": " + a.embedErr.Error()
}
