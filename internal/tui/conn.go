package tui

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets/keyring"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/connform"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// connOp is a config write.
type connOp int

const (
	opAdd connOp = iota
	opEdit
	opRemove
	opReload
)

const conflictText = "config file changed on disk; reload with :reload"

// connSavedMsg is the result of a config write plus the session reload that
// follows it. The write and the reload run in a command, off the Update
// goroutine; the sidebar refresh happens when this arrives.
type connSavedMsg struct {
	op   connOp
	name string
	// from is the old name of a connection the edit renamed ("" for none).
	from string
	err  error
	// stat is the version of the file the reload read.
	stat config.Stat
	// removed and changed are the connections the reload closed or dropped.
	removed, changed []string
	// warn is a non-fatal problem after a successful write.
	warn string
}

// confirmState is the delete confirmation: the user must type the name.
type confirmState struct {
	name string
	line int // where the entry starts in the config file
	// label is whether name is a label the sidebar made up, not a config name.
	label bool
	// ref is the entry's password ref, and cfg the config it came from, to
	// clean up a keyring entry.
	ref string
	cfg *config.Config
	in  textinput.Model
	err string
}

func newConfirm(name string, line int, ref string, cfg *config.Config) *confirmState {
	in := textinput.New()
	in.Prompt = "> "
	// A steady cursor: blinking would keep a timer alive while it is open.
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	in.Focus()
	return &confirmState{name: name, line: line, ref: ref, cfg: cfg, in: in}
}

// connOverlayOpen reports whether the form or the confirmation owns the
// keyboard.
func (a *Model) connOverlayOpen() bool { return a.form != nil || a.confirm != nil }

// busyText is flashed when an edit, delete or reload is asked for while a
// config write or reload is still running.
const busyText = "a config change is still running"

// openForm opens the add or edit form. The config file is read here, so the
// form's conflict token is the file as the user started editing it.
func (a *Model) openForm(mode connform.Mode, name string) tea.Cmd {
	if mode == connform.Edit && a.cur.set && name == a.cur.conn {
		if cmd, held := a.guard("editing connection "+name+" drops them", resend(palette.ConnEditCmd{Name: name})); held {
			return cmd
		}
	}
	if mode == connform.Edit && a.tx != nil && name == a.tx.conn {
		if cmd, held := a.txGuard("editing connection "+name+" closes it", resend(palette.ConnEditCmd{Name: name}), false); held {
			return cmd
		}
	}
	if a.saving {
		return a.flash(busyText, false)
	}
	if mode == connform.Edit && name == "" {
		return a.flash("no connection selected", false)
	}
	cfg, fail := a.loadForEdit()
	if fail != nil {
		return fail
	}
	var existing *config.Connection
	line := 0
	if mode == connform.Edit {
		l, c, fail := a.entryAt(cfg, name)
		if fail != nil {
			return fail
		}
		line, existing = l, &c
	}
	taken := make([]string, 0, len(cfg.Connections))
	for _, c := range cfg.Connections {
		taken = append(taken, c.Name)
	}
	f := connform.New(mode, existing, a.mgr.Drivers(), a.th, a.mgr.Test).WithTaken(taken...)
	a.form, a.formLine = &f, line
	a.help = false
	a.dropDetail()
	return f.Init()
}

// loadForEdit reads the config for a form or confirmation. When that is not
// possible, or the file is no longer the version the sidebar shows, it
// returns the command that flashes why, and cfg is nil.
func (a *Model) loadForEdit() (cfg *config.Config, fail tea.Cmd) {
	if a.cfgPath == "" {
		return nil, a.flash("no config file path: connections cannot be saved", true)
	}
	cfg, st, err := config.LoadWithStat(a.cfgPath)
	if err != nil {
		return nil, a.flash(a.mgr.Redact("", err.Error()), true)
	}
	if !st.Equal(a.cfgStat) {
		return nil, a.flash(conflictText, true)
	}
	return cfg, nil
}

// entryAt finds the config entry behind sidebar row key in the freshly read
// cfg and returns the line it starts on, always the way a write addresses it,
// so a name shared by several entries cannot hit the wrong one. For a broken
// entry the fields are those that parsed safely. When the file changed under
// the sidebar and the entry is no longer there, it returns the command that
// flashes why.
func (a *Model) entryAt(cfg *config.Config, key string) (int, config.Connection, tea.Cmd) {
	if be, ok := a.mgr.BrokenEntry(key); ok {
		b, there := cfg.FindBrokenAt(be.Line)
		if !there || b.Name != be.Name {
			return 0, config.Connection{}, a.flash(conflictText, true)
		}
		return b.Line, b.Partial, nil
	}
	for _, l := range cfg.Occurrences(key) {
		if c, ok := cfg.FindAt(l); ok {
			return l, c, nil
		}
	}
	return 0, config.Connection{}, a.flash("connection "+strconv.Quote(query.EscapeControls(key))+" is not in the config file", true)
}

// openConfirm asks for the name of the connection to delete.
func (a *Model) openConfirm(name string) tea.Cmd {
	if a.cur.set && name == a.cur.conn {
		if cmd, held := a.guard("deleting connection "+name+" drops them", resend(palette.ConnRmCmd{Name: name})); held {
			return cmd
		}
	}
	if a.tx != nil && name == a.tx.conn {
		if cmd, held := a.txGuard("deleting connection "+name+" closes it", resend(palette.ConnRmCmd{Name: name}), false); held {
			return cmd
		}
	}
	if a.saving {
		return a.flash(busyText, false)
	}
	if name == "" {
		return a.flash("no connection selected", false)
	}
	cfg, fail := a.loadForEdit()
	if fail != nil {
		return fail
	}
	line, entry, fail := a.entryAt(cfg, name)
	if fail != nil {
		return fail
	}
	a.confirm = newConfirm(name, line, entry.Password, cfg)
	if be, ok := a.mgr.BrokenEntry(name); ok && be.Name != name {
		a.confirm.label = true // the row key is a label, not a config name
	}
	a.help = false
	a.dropDetail()
	return nil
}

// selectedConn is the connection under the sidebar cursor.
func (a *Model) selectedConn() string {
	conn, _ := a.sb.Selected()
	return conn
}

// connKey handles the connection management keys of a focused sidebar. It
// reports whether k was one.
func (a *Model) connKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case a.km.Matches(k, keymap.ConnAdd):
		return a.openForm(connform.Add, ""), true
	case a.km.Matches(k, keymap.ConnEdit):
		return a.openForm(connform.Edit, a.selectedConn()), true
	case a.km.Matches(k, keymap.ConnRm):
		return a.openConfirm(a.selectedConn()), true
	}
	return nil, false
}

// formKey sends every key to the open form: no global binding may fire.
func (a *Model) formKey(k tea.KeyPressMsg) tea.Cmd {
	f, cmd := a.form.Update(k)
	a.form = &f
	return cmd
}

func (a *Model) confirmKey(k tea.KeyPressMsg) tea.Cmd {
	c := a.confirm
	switch {
	case a.km.Matches(k, keymap.Back), a.km.Matches(k, keymap.Cancel):
		a.confirm = nil
		return nil
	case a.km.Matches(k, keymap.Open):
		if c.in.Value() != c.name {
			c.err = "the name does not match"
			return nil
		}
		a.saving = true
		record := a.pauseGridOf(c.name)
		store, cfg, name, ref := a.keyring, c.cfg, c.name, c.ref
		return tea.Batch(record, a.writeCmd(opRemove, config.Connection{Name: c.name}, c.line, func() string {
			return keyring.CleanupOnRemove(store, cfg, name, ref)
		}, "", ""))
	}
	c.err = ""
	var cmd tea.Cmd
	c.in, cmd = c.in.Update(k)
	return cmd
}

// saveForm persists a submitted form.
func (a *Model) saveForm(msg connform.SavedMsg) tea.Cmd {
	if a.saving {
		return nil
	}
	a.saving = true
	op := opAdd
	var record tea.Cmd
	carry, from := "", ""
	if a.formLine > 0 {
		op = opEdit
		record = a.pauseGridOf(msg.OldName)
		if msg.OldName != msg.Conn.Name {
			from = msg.OldName
			if a.mgr.ReadOnlyOverride(msg.OldName) {
				carry = msg.Conn.Name // a rename keeps :readonly on
			}
		}
	}
	return tea.Batch(record, a.writeCmd(op, msg.Conn, a.formLine, nil, from, carry))
}

// writeCmd writes the config against the version the Manager was loaded
// from, then reloads the session manager from what was written, in a command
// so file I/O and closing connections stay off the Update goroutine.
// from is the old name of a connection the edit renames ("" for none), and
// carryRO names a renamed connection whose :readonly override the reload
// must keep ("" for none).
func (a *Model) writeCmd(op connOp, c config.Connection, line int, after func() string, from, carryRO string) tea.Cmd {
	path, mgr, st := a.cfgPath, a.mgr, a.cfgStat
	stopping := a.takeStopping()
	return func() tea.Msg {
		waitStopped(stopping)
		res := connSavedMsg{op: op, name: c.Name, from: from}
		var err error
		switch op {
		case opAdd:
			_, err = config.AddConnection(path, c, st)
		case opEdit:
			_, err = config.UpdateConnectionAt(path, line, c, st)
		case opRemove:
			_, err = config.RemoveConnectionAt(path, line, st)
			if err == nil && after != nil {
				res.warn = after()
			}
		}
		if err != nil {
			res.err = err
			return res
		}
		res = reloadFrom(path, mgr, res)
		if res.err == nil && carryRO != "" {
			_ = mgr.SetReadOnly(carryRO, true) // turning it on never fails for a known name
		}
		return res
	}
}

// reloadConfigCmd re-reads the config file and reloads the session manager.
func (a *Model) reloadConfigCmd() tea.Cmd {
	path, mgr := a.cfgPath, a.mgr
	stopping := a.takeStopping()
	return func() tea.Msg {
		waitStopped(stopping)
		return reloadFrom(path, mgr, connSavedMsg{op: opReload})
	}
}

// reloadFrom loads the config (taking its Stat first) into mgr.
func reloadFrom(path string, mgr *session.Manager, res connSavedMsg) connSavedMsg {
	cfg, st, err := config.LoadWithStat(path)
	if err != nil {
		res.err = err
		return res
	}
	res.stat = st
	_, res.changed, res.removed = mgr.Reload(cfg)
	return res
}

// saved applies the result of a write, then resumes a chart read the write
// paused when the chart's connection is still there.
func (a *Model) saved(msg connSavedMsg) tea.Cmd {
	cmd := a.applySaved(msg)
	return tea.Batch(cmd, a.resumeChart())
}

// applySaved applies the result of a write: errors go inline, success
// refreshes the sidebar and says so in the status bar.
func (a *Model) applySaved(msg connSavedMsg) tea.Cmd {
	a.saving = false
	resume := a.resumeGrid
	a.resumeGrid = false
	if msg.err != nil {
		text := a.mgr.Redact(msg.name, msg.err.Error())
		if errors.Is(msg.err, config.ErrConflict) {
			text = conflictText
		}
		// The write failed, so the connection was not touched: bring back the
		// load that was cancelled before the write, with the sort, filter and
		// scroll the user had. A failed reload changed nothing either (the
		// file could not be read): a grid showing a failed page tries it
		// again, a paused load resumes, and anything else stays as it is.
		var again tea.Cmd
		switch {
		case resume && a.cur.set:
			again = a.gr.Resume()
		case msg.op == opReload && a.cur.set:
			if a.gr.Failed() {
				again = a.gr.Retry()
			} else {
				again = a.gr.Resume()
			}
		}
		switch {
		case a.form != nil:
			f := a.form.SetError(text)
			a.form = &f
			return tea.Batch(f.Init(), again)
		case a.confirm != nil:
			a.confirm.err = text
			return again
		}
		return tea.Batch(a.flash(text, true), again)
	}
	a.form, a.confirm = nil, nil
	a.cfgStat = msg.stat
	a.aiReloaded(msg.op == opReload)
	if msg.op == opEdit && msg.from != "" {
		a.renameEditor(msg.from, msg.name)
	}
	a.sb = a.sb.Refresh()
	if msg.op == opAdd || msg.op == opEdit {
		a.sb = a.sb.Select(msg.name)
	}
	if a.cur.set && (slices.Contains(msg.removed, a.cur.conn) || slices.Contains(msg.changed, a.cur.conn)) {
		a.clearCurrent()
	}
	if msg.op == opReload {
		flash := a.flash("reloaded the config file", false)
		if a.cur.set && a.src != nil {
			a.filterText = ""
			a.dropDetail()
			return tea.Batch(flash, a.gr.Open(a.src))
		}
		return flash
	}
	verb := "saved "
	if msg.op == opRemove {
		verb = "removed "
	}
	// The open connection survived the write (it was not changed or removed),
	// so the load cancelled for it carries on.
	var again tea.Cmd
	if resume && a.cur.set {
		again = a.gr.Resume()
	}
	text := verb + msg.name
	if msg.warn != "" {
		text += "; " + msg.warn
	}
	return tea.Batch(a.flash(text, msg.warn != ""), again)
}

// renameEditor moves what the editor keeps under a renamed connection's old
// name to the new one, the way a rename carries the :readonly override: its
// buffer, the text at its last run, the editor itself (with its history walk,
// which syncEditorConn then leaves alone) and the open transaction's owner.
func (a *Model) renameEditor(from, to string) {
	if b, ok := a.buffers[from]; ok {
		delete(a.buffers, from)
		a.buffers[to] = b
	}
	if r, ok := a.runBuf[from]; ok {
		delete(a.runBuf, from)
		a.runBuf[to] = r
	}
	if a.edConn == from {
		a.edConn = to
	}
	if a.tx != nil && a.tx.conn == from {
		a.tx.conn = to
	}
	if a.chart.conn == from {
		a.chart.conn = to
	}
	for _, t := range a.tabs {
		if t.watch != nil && t.watch.conn == from {
			t.watch.conn = to
		}
	}
}

// pauseGridOf cancels conn's grid load before a write that may close the
// connection, so nothing keeps using it. The grid stays as it is: it is
// cleared only once the write succeeded (saved), and a load that was running
// is restarted when the write fails. An editor run on conn is cancelled for
// good (its tab is marked cancelled), and the write waits for it to stop;
// the returned command records it in the history. A completion lookup on
// conn is cancelled and waited for the same way.
func (a *Model) pauseGridOf(conn string) tea.Cmd {
	a.stopLookups(conn)
	a.stopAIOn(conn)
	if a.cur.set && a.cur.conn == conn {
		a.stopDescribe()
	}
	a.stopApplyOn(conn)
	a.stopExportOn(conn)
	a.stopChartOn(conn)
	a.stopWatchesOn(conn, "", "the connection's config is being written")
	if a.cur.set && a.cur.conn == conn {
		a.resumeGrid = a.gr.Loading()
		a.gr.CancelLoad()
	}
	if a.running != nil && a.running.conn == conn {
		record, _ := a.cancelRun()
		return record
	}
	return nil
}

// clearCurrent forgets the open collection, whose connection was closed.
func (a *Model) clearCurrent() {
	a.gr.CancelLoad()
	a.gr.Open(nil)
	a.dropDetail()
	a.closeChart()
	a.discardPending() // asked for before the write that closed it
	a.cur, a.src, a.filterText = current{}, nil, ""
}

// confirmView draws the deletion prompt in a w x h area.
func (a *Model) confirmView(w, h int) string {
	c := a.confirm
	st := a.th.Styles
	name := query.EscapeControls(c.name)
	lines := []string{
		st.Title.Render(query.Truncate("Delete connection "+strconv.Quote(name)+"?", w)),
		st.Dim.Render(ansi.Wrap(confirmHint(c, name), w, "")),
		"",
		c.in.View(),
	}
	if c.err != "" {
		lines = append(lines, st.Error.Render(query.Truncate(query.EscapeControls(c.err), w)))
	}
	lines = append(lines, "", st.Dim.Render(query.Truncate("enter delete · esc cancel", w)))
	out := strings.Join(lines, "\n")
	return lipgloss.NewStyle().MaxWidth(w).MaxHeight(h).Render(out)
}

func confirmHint(c *confirmState, name string) string {
	if c.label {
		return "It is removed from the config file. Type the label shown (" + name + ") to confirm."
	}
	return "It is removed from the config file. Type its name to confirm."
}
