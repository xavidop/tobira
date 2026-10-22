package tui

import (
	"context"
	"errors"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// :export csv|json|ndjson <path>. On the browse tab it exports the whole
// collection with the grid's filter and sort; on a result tab it exports the
// rows the tab already holds and never runs the statement again (a re-run
// could have side effects). One export at a time, in a command; its progress
// comes back through a channel as messages.

const cutNote = " (the result was cut at the limit; raise :set limit or use tobira query --output)"

// exportRun is the export in flight.
type exportRun struct {
	id        uint64
	cancel    context.CancelFunc
	done      chan struct{} // closed when the export has returned
	conn      string        // the connection it reads; "" for a result tab
	rows      int64         // progress
	truncated bool          // the result tab was cut at the limit
}

type exportProgressMsg struct {
	id   uint64
	rows int64
	ch   <-chan int64 // where the next count comes from
}

type exportDoneMsg struct {
	id  uint64
	res export.Result
	err error
}

// exportStartMsg redoes an export once its overwrite prompt is answered yes.
type exportStartMsg struct {
	cmd       palette.ExportCmd
	overwrite bool
}

// exportTargetMsg is the export's target, resolved and checked in a command
// (file I/O stays off the Update goroutine, which a hung mount would
// freeze), with the source captured when the export was asked for.
type exportTargetMsg struct {
	cmd       palette.ExportCmd
	overwrite bool
	src       export.Source
	conn      string
	from      driver.Conn // the open collection's connection; nil for a result tab
	truncated bool
	target    string
	exists    bool
	err       error
}

// exportingText is the status bar's progress.
func exportingText(rows int64) string {
	return "exporting… " + commas(int(rows)) + plural(int(rows), " row", " rows")
}

func (a *Model) startExport(cmd palette.ExportCmd, overwrite bool) tea.Cmd {
	if a.export != nil {
		return a.flash("an export is already running; ctrl+c cancels it", false)
	}
	if a.saving {
		// A config write or reload may close the connection under it.
		return a.flash(busyText, false)
	}
	var src export.Source
	var from driver.Conn
	conn, truncated := "", false
	if t := a.activeTab(); t != nil {
		switch {
		case t.state == tabRunning:
			return a.flash("the statement is still running", false)
		case t.isText:
			return a.flash("only a result with rows can be exported; this tab holds text", false)
		}
		src = export.RowsSource{Columns: t.cols, Rows: t.result, Stats: t.stats}
		truncated = t.truncated
	} else {
		if !a.cur.set || a.src == nil {
			return a.flash("open a collection or a result tab to export it", false)
		}
		if !a.src.Browser() {
			return a.flash("exporting needs a browsable collection", false)
		}
		st := a.mgr.State(a.cur.conn)
		conn, from = a.cur.conn, a.src.Conn()
		src = export.CollectionSource{Conn: from, Ref: a.cur.ref, Driver: st.Info.Name,
			Opts: driver.BrowseOptions{Filter: a.gr.Filter(), Sort: a.gr.Status().Sort, ReadOnly: st.ReadOnly}}
	}
	return func() tea.Msg {
		msg := exportTargetMsg{cmd: cmd, overwrite: overwrite, src: src, conn: conn, from: from, truncated: truncated}
		msg.target, msg.err = export.ResolveTarget(cmd.Path)
		if msg.err == nil {
			_, err := os.Stat(msg.target)
			msg.exists = err == nil
		}
		return msg
	}
}

// exportTarget asks before replacing an existing file, else starts the
// export. What may have changed since it was asked for is checked again.
func (a *Model) exportTarget(msg exportTargetMsg) tea.Cmd {
	switch {
	case msg.err != nil:
		return a.flash(msg.err.Error(), true)
	case a.export != nil:
		return a.flash("an export is already running; ctrl+c cancels it", false)
	case a.saving:
		return a.flash(busyText, false)
	case msg.from != nil && (a.src == nil || a.src.Conn() != msg.from):
		// A config write closed the connection meanwhile.
		return a.flash("the collection was closed; export again", false)
	case msg.exists && !msg.overwrite:
		a.askYN("overwrite "+query.EscapeControls(msg.target)+"? y/N", "",
			resend(exportStartMsg{cmd: msg.cmd, overwrite: true}), "kept "+msg.target)
		return nil
	}
	return a.runExport(msg)
}

// runExport runs the export in a command, its progress coming back through
// a channel.
func (a *Model) runExport(msg exportTargetMsg) tea.Cmd {
	src, conn, truncated, target, cmd := msg.src, msg.conn, msg.truncated, msg.target, msg.cmd
	ctx, cancel := context.WithCancel(context.Background())
	a.exportSeq++
	id := a.exportSeq
	done := make(chan struct{})
	a.export = &exportRun{id: id, cancel: cancel, done: done, conn: conn, truncated: truncated}
	progress := make(chan int64, 1)
	report := func(p export.Progress) {
		// Keep only the newest count: the reader may lag behind.
		select {
		case progress <- p.Rows:
		default:
			select {
			case <-progress:
			default:
			}
			select {
			case progress <- p.Rows:
			default:
			}
		}
	}
	run := func() tea.Msg {
		defer close(done)
		defer close(progress)
		defer cancel()
		res, err := export.ToFile(ctx, src, export.Options{Format: cmd.Format, Path: target, Progress: report})
		return exportDoneMsg{id: id, res: res, err: err}
	}
	return tea.Batch(run, listenExport(id, progress))
}

// listenExport delivers the next progress count; it stops when the export
// closes the channel.
func listenExport(id uint64, ch <-chan int64) tea.Cmd {
	return func() tea.Msg {
		n, ok := <-ch
		if !ok {
			return nil
		}
		return exportProgressMsg{id: id, rows: n, ch: ch}
	}
}

func (a *Model) exportProgress(msg exportProgressMsg) tea.Cmd {
	if a.export == nil || msg.id != a.export.id {
		return nil
	}
	a.export.rows = max(msg.rows, 0)
	return listenExport(msg.id, msg.ch)
}

func (a *Model) exportDone(msg exportDoneMsg) tea.Cmd {
	run := a.export
	if run == nil || msg.id != run.id {
		return nil
	}
	a.export = nil
	switch {
	case errors.Is(msg.err, context.Canceled):
		return a.flash("export cancelled", false)
	case msg.err != nil:
		return a.flash("export failed: "+a.mgr.Redact(run.conn, msg.err.Error()), true)
	}
	n := int(msg.res.Rows)
	text := "exported " + commas(n) + plural(n, " row", " rows")
	if w := export.PreviewWarning(msg.res.Previews); w != "" {
		// Before the path, which may be long enough to cut the flash.
		text += " (" + w + ")"
	}
	if run.truncated {
		text += cutNote
	} else {
		text += " to " + msg.res.Path
	}
	return a.flash(text, false)
}

// cancelExport cancels the export in flight; its result still arrives and
// says so.
func (a *Model) cancelExport() bool {
	if a.export == nil {
		return false
	}
	a.export.cancel()
	return true
}

// stopExportOn cancels an export reading conn ("" for any) before a config
// write or reload may close it, and hands its done channel to that write to
// wait for.
func (a *Model) stopExportOn(conn string) {
	if a.export == nil || a.export.conn == "" || conn != "" && a.export.conn != conn {
		return
	}
	a.cancelExport()
	a.stopping = append(stillRunning(a.stopping), a.export.done)
}

// exportWait is closed once the export, if any, has returned: its temp file
// is gone by then.
func (a *Model) exportWait() <-chan struct{} {
	if a.export == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return a.export.done
}

// exportStatus is the status bar's activity text while an export runs.
func (a *Model) exportStatus() string {
	if a.export == nil {
		return ""
	}
	return exportingText(a.export.rows)
}
