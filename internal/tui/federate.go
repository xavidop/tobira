package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/federate"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/async"
)

// federatedConn is the connection name a cross-source run's tab and history
// entry carry: the run belongs to no single connection.
const federatedConn = federate.HistoryConn

// startFederated runs the whole buffer as a cross-source statement: every
// @source is staged read-only on its own connection and the final statement
// runs in a scratch SQLite database, in a command with its own cancellable
// context, into a new tab labelled federated. With explain the tab gets the
// plan (each source and the final statement) and nothing is contacted. The
// editor's connection is not involved; a directive may name any connection
// of the session, except one with an open transaction.
func (a *Model) startFederated(explain bool) tea.Cmd {
	a.closePopup()
	if a.running != nil {
		return a.flash("a statement is still running; ctrl+c cancels it", false)
	}
	text := a.ed.Value()
	st, err := federate.Parse(text)
	if err != nil {
		return a.flash("cross-source query: "+query.EscapeControls(err.Error()), true)
	}
	if st.Sink != nil && !explain {
		// The rows become pending inserts of the sink collection, which must
		// be open in the browse tab and editable, as for i.
		if why := a.sinkRefusal(st.Sink); why != "" {
			return a.flash(why, true)
		}
		if _, _, why := a.editable(); why != "" {
			return a.flash("sink: "+why, true)
		}
	}
	var conns []string
	for _, s := range st.Sources {
		name := query.EscapeControls(s.Conn)
		switch {
		case !hasConn(a.mgr, s.Conn):
			return a.flash(fmt.Sprintf("line %d: unknown connection %q", s.Line, name), true)
		case a.mgr.State(s.Conn).Broken:
			return a.flash("connection "+name+" is misconfigured", true)
		case a.tx != nil && a.tx.conn == s.Conn:
			return a.flash("a transaction is open on "+name+"; a cross-source query never runs inside :tx", false)
		}
		conns = append(conns, s.Conn)
	}
	t := a.addTab(federatedConn, text, explain)
	t.srcConns = conns
	if !explain {
		// The engine only reads; the sink is this tab's business (sinkPending).
		t.sink, st.Sink = st.Sink, nil
	}
	a.running = t
	if a.edConn != "" {
		a.runBuf[a.edConn] = text // what quit compares the buffer with
	}
	done := make(chan struct{})
	t.done = done
	a.dropDetail()
	a.wantTick = true // a connection may be opening
	id, ctx := a.runs.Start(context.Background())
	mgr, opts := a.mgr, federate.Options{MaxSourceRows: a.maxSourceRows, Limit: a.limit}
	return async.Do(id, ctx, func(ctx context.Context) (runOutcome, error) {
		defer close(done)
		if explain {
			return runOutcome{plan: federate.Explain(st)}, nil
		}
		res, err := federate.Run(ctx, mgr, st, opts)
		out := runOutcome{reached: true}
		if res != nil {
			out.sources = res.Sources
		}
		if err != nil {
			return out, err
		}
		out.cols, out.rows, out.stats = res.Columns, res.Rows, res.Stats
		return out, nil
	})
}

// sinkLabel names a sink's collection as messages do: "conn ▸ ns.coll".
func sinkLabel(s *federate.Sink) string {
	coll := s.Collection
	if s.Namespace != "" {
		coll = s.Namespace + "." + coll
	}
	return query.EscapeControls(s.Conn + " ▸ " + coll)
}

// sinkRefusal says why a sink cannot take pending inserts now, "" when it
// can: its collection must be the one open in the browse tab, where the
// pending changes live and :apply writes them.
func (a *Model) sinkRefusal(s *federate.Sink) string {
	if !a.cur.set || a.cur.conn != s.Conn || a.cur.ref.Name != s.Collection || (s.Namespace != "" && a.cur.ref.Namespace != s.Namespace) {
		return "open " + sinkLabel(s) + " in the browse tab first: the sink's rows become pending inserts there"
	}
	return ""
}

// sinkPending turns a finished cross-source result into pending inserts of
// the open collection, one per row with the result's column names as
// fields, for :apply to write (and :discard to drop). The collection is
// checked again: it may have changed while the run was in flight.
func (a *Model) sinkPending(t *resultTab, cols []driver.Column, rows []driver.Row) tea.Cmd {
	if why := a.sinkRefusal(t.sink); why != "" {
		return a.flash(why, true)
	}
	if _, _, why := a.editable(); why != "" {
		return a.flash("sink: "+why, true)
	}
	for _, row := range rows {
		r := a.pend.addInsert()
		for i, c := range cols {
			if i < len(row) {
				a.pend.setInsertCell(r, c.Name, row[i])
			}
		}
	}
	a.syncPending()
	n := len(rows)
	return a.flash(fmt.Sprintf("%d %s added as pending inserts of %s; :apply writes them, :discard drops them", n, plural(n, "row", "rows"), sinkLabel(t.sink)), false)
}
