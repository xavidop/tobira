package tui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/editor"
	"github.com/xavidop/tobira/internal/tui/grid"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

// Inline editing of the browse tab: e edits a cell, i inserts a row, d marks
// a row for deletion, :apply writes the pending changes and :discard drops
// them. Every driver call (Describe, the writes) runs in a command.

const (
	noEditText = "editing needs a primary key and a driver that supports edits"
	// describeTimeout bounds the schema read that enables editing.
	describeTimeout = 30 * time.Second
	// rollbackTimeout bounds the rollback of a failed or cancelled apply,
	// which runs after its context is cancelled.
	rollbackTimeout = 5 * time.Second
	applyingText    = "changes are being applied; ctrl+c cancels"
	readingText     = "reading the collection's schema; try again in a moment"
)

// roEditText refuses an edit on a read-only connection, worded like the
// editor's refusal.
func roEditText(conn string) string { return "🔒 " + conn + " is read-only; the edit was refused" }

// describedMsg carries a collection's schema to the Source it was read for.
type describedMsg struct {
	id  uint64
	src *grid.Source
	sch *driver.CollectionSchema
	err error
}

// describe reads the schema of src's collection once, off the Update
// goroutine; editing is enabled when it is back. It is cancelled when
// another collection opens, before a connection is closed and at shutdown.
func (a *Model) describe(src *grid.Source) tea.Cmd {
	a.stopDescribe()
	conn, ref := src.Conn(), src.Ref()
	ctx, cancel := context.WithTimeout(context.Background(), describeTimeout)
	a.descSeq++
	id := a.descSeq
	a.descCancel, a.descID, a.descErr = cancel, id, ""
	return func() tea.Msg {
		defer cancel()
		sch, err := conn.Describe(ctx, ref)
		return describedMsg{id: id, src: src, sch: sch, err: err}
	}
}

func (a *Model) stopDescribe() {
	if a.descCancel != nil {
		a.descCancel()
		a.descCancel = nil
	}
}

// ---- refusals ------------------------------------------------------------------

// mutator is where :apply writes: the open transaction (:tx) when there is
// one, otherwise the connection.
func (a *Model) mutator() (driver.Mutator, bool) {
	if tx, ok := a.txOn(a.cur.conn); ok {
		return driver.As[driver.Mutator](tx)
	}
	if a.src == nil || a.src.Conn() == nil {
		return nil, false
	}
	return driver.As[driver.Mutator](a.src.Conn())
}

// editKey is the key :apply addresses rows by: the primary key, or _id
// when there is none.
func editKey(sch *driver.CollectionSchema) []string {
	if len(sch.PrimaryKey) > 0 {
		return sch.PrimaryKey
	}
	for _, f := range sch.Fields {
		if f.Name == "_id" {
			return []string{"_id"}
		}
	}
	return nil
}

// editable checks that the open collection can be edited now and returns
// its schema and key, or why not. It is asked again at :apply, because the
// connection may have turned read-only in between.
func (a *Model) editable() (*driver.CollectionSchema, []string, string) {
	if !a.cur.set || a.src == nil {
		return nil, nil, "open a collection to edit it"
	}
	if a.saving {
		// A config write or reload may close the connection and drop the
		// collection: nothing may be edited or written until it is done.
		return nil, nil, busyText
	}
	if a.apply != nil {
		return nil, nil, applyingText
	}
	if a.mgr.State(a.cur.conn).ReadOnly {
		return nil, nil, roEditText(a.cur.conn)
	}
	if t := a.tx; t != nil && t.conn == a.cur.conn {
		switch {
		case t.busy != "":
			return nil, nil, a.txBusyText()
		case t.stuck:
			return nil, nil, stuckText
		case t.ro:
			return nil, nil, "the open transaction is read-only; :tx rollback to end it"
		}
		if _, ok := a.mutator(); !ok {
			return nil, nil, "the open transaction does not support edits; :tx commit or rollback first"
		}
	}
	if _, ok := a.mutator(); !ok {
		return nil, nil, noEditText
	}
	sch, ok := a.src.Schema()
	switch {
	case !ok && a.descErr != "":
		return nil, nil, "could not read the collection's schema: " + a.descErr + "; trying again"
	case !ok:
		return nil, nil, readingText
	}
	keys := editKey(sch)
	if len(keys) == 0 {
		return nil, nil, noEditText
	}
	cols := a.gr.Columns()
	for _, k := range keys {
		if !slices.ContainsFunc(cols, func(c driver.Column) bool { return c.Name == k }) {
			return nil, nil, noEditText
		}
	}
	if a.pend == nil {
		a.pend = newPendingSet(keys)
	}
	return sch, keys, ""
}

// fieldOf describes column col for the cell editor, from the schema when it
// has the field.
func fieldOf(sch *driver.CollectionSchema, col driver.Column) fieldInfo {
	for _, f := range sch.Fields {
		if f.Name == col.Name {
			typ := f.Type
			if typ == driver.TypeNull {
				typ = col.Type
			}
			return fieldInfo{Name: f.Name, Type: typ, Native: f.NativeType, Nullable: f.Nullable, Generated: f.Generated}
		}
	}
	return fieldInfo{Name: col.Name, Type: col.Type, Native: col.NativeType, Nullable: true}
}

// editKeyPress handles e, i and d on the grid. It reports whether k was one.
func (a *Model) editKeyPress(k tea.KeyPressMsg) (tea.Cmd, bool) {
	var act func(*driver.CollectionSchema, []string) tea.Cmd
	switch {
	case a.km.Matches(k, keymap.Edit):
		act = a.editCell
	case a.km.Matches(k, keymap.InsertRow):
		act = a.insertRow
	case a.km.Matches(k, keymap.DeleteRow):
		act = a.deleteRow
	default:
		return nil, false
	}
	if a.activeTab() != nil {
		return a.flash("edit rows in the browse tab", false), true
	}
	sch, keys, why := a.editable()
	if why != "" {
		return tea.Batch(a.flash(why, why != applyingText && why != readingText && why != busyText), a.redescribe()), true
	}
	return act(sch, keys), true
}

// redescribe reads the schema again when an edit found none and no read is
// running: the last one failed, or was cancelled before a connection
// change.
func (a *Model) redescribe() tea.Cmd {
	if a.src == nil || a.src.Described() || a.descCancel != nil || a.saving {
		return nil
	}
	if _, ok := driver.As[driver.Mutator](a.src.Conn()); !ok {
		return nil
	}
	return a.describe(a.src)
}

// described records a schema read. Only the latest read of the open
// collection counts; a failed or cancelled one records nothing, so the next
// edit key reads again (a failure is shown then).
func (a *Model) described(msg describedMsg) {
	if msg.id != a.descID || msg.src != a.src {
		return
	}
	a.stopDescribe()
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) {
			a.descErr = a.mgr.Redact(a.cur.conn, msg.err.Error())
		}
		return
	}
	msg.src.SetSchema(msg.sch, nil)
	if marksTruncation(msg.sch) {
		a.gr.SetHidden(hiddenColumn)
	}
	if badges := keyBadges(msg.sch); badges != nil {
		a.gr.SetHeaderMark(badges)
	}
}

// ---- e, i and d --------------------------------------------------------------------

// cellEdit is the open cell editor.
type cellEdit struct {
	field     fieldInfo
	ins       *editRow      // the inserted row edited; nil for a loaded row
	key       driver.Object // the loaded row's key
	orig      driver.Value  // the loaded row's value of the field
	startText string
	start     driver.Value // the value the editor opened with
	unset     bool         // an inserted row's field with no value yet
	touched   bool         // the text was edited
	line      lineInput
	ed        *editor.Model // JSON, objects and lists
	err       string
}

func (c *cellEdit) multi() bool { return c.ed != nil }

func (a *Model) editCell(sch *driver.CollectionSchema, keys []string) tea.Cmd {
	row, ok := a.gr.CurrentRow()
	_, col := a.gr.Cursor()
	cols := a.gr.Columns()
	if !ok || col >= len(cols) || col >= len(row) {
		return a.flash("no row to edit", false)
	}
	if previewCut(sch, cols, row, col) {
		return a.flash(truncatedEditText, false)
	}
	f := fieldOf(sch, cols[col])
	if a.kv {
		var why string
		if f, why = kvEditField(cols, row, col, f); why != "" {
			return a.flash(why, false)
		}
	}
	if f.Generated {
		return a.flash(f.Name+" is generated", false)
	}
	ce := &cellEdit{field: f}
	start := row[col]
	if i, ok := a.gr.PinnedAt(); ok {
		ce.ins = a.pend.inserts[i]
		var set bool
		start, set = ce.ins.values.Get(f.Name)
		ce.unset = !set
	} else {
		k, ok := keyFrom(keys, cols, row)
		if !ok {
			return a.flash(noEditText, true)
		}
		ce.key, ce.orig = k, row[col]
		if r := a.pend.row(k); r != nil {
			if v, ok := r.values.Get(f.Name); ok {
				start = v
			}
		}
	}
	ce.start, ce.startText = start, editText(start)
	if f.multiline() {
		ed := editor.New(a.th, a.km)
		ed.SetDialect("json")
		ed.SetValue(ce.startText)
		ce.ed = &ed
	} else {
		ce.line.set(ce.startText)
	}
	a.cell = ce
	return nil
}

func (a *Model) insertRow(sch *driver.CollectionSchema, _ []string) tea.Cmd {
	r := a.pend.addInsert()
	a.syncPending()
	col := 0
	for i, c := range a.gr.Columns() {
		if !fieldOf(sch, c).Generated {
			col = i
			break
		}
	}
	a.gr.SetCursor(a.pend.insertIndex(r), col)
	return nil
}

func (a *Model) deleteRow(_ *driver.CollectionSchema, keys []string) tea.Cmd {
	if i, ok := a.gr.PinnedAt(); ok {
		a.pend.removeInsert(a.pend.inserts[i])
		a.syncPending()
		return nil
	}
	row, ok := a.gr.CurrentRow()
	if !ok {
		return a.flash("no row to delete", false)
	}
	k, ok := keyFrom(keys, a.gr.Columns(), row)
	if !ok {
		return a.flash(noEditText, true)
	}
	a.pend.toggleDelete(k)
	a.syncPending()
	return nil
}

// syncPending shows the pending changes in the browse grid.
func (a *Model) syncPending() {
	if a.pend.empty() {
		a.gr.SetMarks(nil)
		a.gr.SetPinned(nil)
		return
	}
	a.gr.SetMarks(pendingMarks{a.pend})
	a.gr.SetPinned(a.pend.pinned())
}

// discardPending drops every pending change.
func (a *Model) discardPending() {
	a.pend, a.cell = nil, nil
	a.syncPending()
}

// ---- the cell editor ----------------------------------------------------------------

// cellKey handles a key while the cell editor is open: it owns the keyboard.
func (a *Model) cellKey(k tea.KeyPressMsg) tea.Cmd {
	c := a.cell
	switch {
	case a.km.Matches(k, keymap.Back), a.km.Matches(k, keymap.Cancel):
		a.cell = nil
		return nil
	case a.km.Matches(k, keymap.SetNull):
		if !c.field.Nullable {
			return a.flash(c.field.Name+" can't be NULL", true)
		}
		a.commitCell(driver.Null())
		return nil
	}
	if c.multi() {
		if a.km.Matches(k, keymap.SaveEdit) || k.String() == "alt+enter" {
			return a.commitText(c.ed.Value())
		}
		ver := c.ed.Version()
		ed, cmd := c.ed.Update(k)
		c.ed, c.err = &ed, ""
		c.touched = c.touched || ed.Version() != ver
		return cmd
	}
	if a.km.Matches(k, keymap.Open) {
		return a.commitText(c.line.text)
	}
	before := c.line.text
	if k.String() == "ctrl+u" {
		c.line.text, c.line.cur = c.line.text[c.line.cur:], 0
	} else {
		c.line.key(k)
	}
	c.touched = c.touched || c.line.text != before
	c.err = ""
	return nil
}

func (a *Model) cellPaste(msg tea.PasteMsg) tea.Cmd {
	c := a.cell
	c.err, c.touched = "", true
	if c.multi() {
		ed, cmd := c.ed.Update(msg)
		c.ed = &ed
		return cmd
	}
	c.line.paste(msg.Content)
	return nil
}

// commitText parses the edited text; a parse error stays in the editor.
// Untouched text changes nothing, so a NULL or an unset field stays so;
// text edited down to nothing is the empty string (NULL is ctrl+n).
func (a *Model) commitText(text string) tea.Cmd {
	c := a.cell
	if !c.touched {
		a.cell = nil
		return nil
	}
	v, err := parseCell(text, c.field)
	if err != nil {
		c.err = err.Error()
		return nil
	}
	a.commitCell(v)
	return nil
}

func (a *Model) commitCell(v driver.Value) {
	c := a.cell
	a.cell = nil
	if c.ins != nil {
		a.pend.setInsertCell(c.ins, c.field.Name, v)
	} else {
		a.pend.setCell(c.key, c.field.Name, c.orig, v)
	}
	a.syncPending()
}

// placeholder is shown in an empty input: NULL over a NULL value, default
// over an inserted row's unset field.
func (c *cellEdit) placeholder() string {
	switch {
	case c.line.text != "":
		return ""
	case c.unset:
		return "default"
	case c.start.IsNull():
		return "NULL"
	}
	return ""
}

func (c *cellEdit) typeName() string {
	if c.field.isUUID() {
		return "uuid"
	}
	return c.field.Type.String()
}

// cellView draws the one-line cell editor: the input over a hint or the
// parse error.
func (a *Model) cellView(w int) string {
	c := a.cell
	st := a.th.Styles
	prompt := query.Truncate("edit "+query.EscapeControls(c.field.Name)+" ("+c.typeName()+")> ", max(w-2, 0))
	pw := lipgloss.Width(prompt)
	input := c.line.view(max(w-pw, 0), st.CellCursor)
	if ph := c.placeholder(); ph != "" && w-pw > 1 {
		// An empty input over NULL or an unset field says so, dimmed.
		rest := query.Truncate(ph, w-pw-1)
		input = st.CellCursor.Render(" ") + st.Dim.Render(rest) + strings.Repeat(" ", max(0, w-pw-1-lipgloss.Width(rest)))
	}
	line := st.Title.Render(prompt) + input
	return line + "\n" + a.cellFooter(c, w, "enter save · esc cancel · ctrl+n NULL")
}

func (a *Model) cellFooter(c *cellEdit, w int, hint string) string {
	if c.err != "" {
		return a.th.Styles.Error.Render(query.Truncate(query.EscapeControls(c.err), w))
	}
	return a.th.Styles.Dim.Render(query.Truncate(hint, w))
}

// cellPane draws the multi-line JSON editor in the grid area.
func (a *Model) cellPane(w, h int) (title, content string) {
	c := a.cell
	title = "edit " + query.EscapeControls(c.field.Name) + " (" + c.typeName() + ")"
	edH := max(h-1, 0)
	body := strings.Join(fit(c.ed.View(w, edH, true), w, edH), "\n")
	if edH == 0 {
		body = ""
	} else {
		body += "\n"
	}
	return title, body + a.cellFooter(c, w, "ctrl+s / alt+enter save · enter newline · esc cancel · ctrl+n NULL")
}

// ---- :apply ----------------------------------------------------------------------------

type applyMode int

const (
	applyTx     applyMode = iota // Begin, every change, Commit
	applySeq                     // one at a time on the connection
	applyOpenTx                  // one at a time in the open :tx, no commit
)

// applyRun is the :apply in flight. Its result is never dropped, even when
// cancelled, because it says which changes were written.
type applyRun struct {
	id        uint64
	cancel    context.CancelFunc
	done      chan struct{} // closed when the command has returned
	cancelled bool
	cancelAt  time.Time
	changes   []change
	mode      applyMode
	conn      string
	src       *grid.Source
}

// applyOutcome is what an apply wrote: the first applied changes, then the
// failure (at index failed, or at stage begin/commit), if any.
type applyOutcome struct {
	applied   int
	failed    int
	stage     string
	err       error
	rbErr     error // the rollback that followed a failure, when it failed too
	cancelled bool
}

type applyDoneMsg struct {
	id  uint64
	out applyOutcome
}

func (a *Model) applyPending() tea.Cmd {
	if a.apply != nil {
		return a.flash("changes are already being applied", false)
	}
	if a.pend.empty() {
		return a.flash("nothing to apply", false)
	}
	sch, _, why := a.editable()
	if why != "" {
		return tea.Batch(a.flash(why, why != readingText && why != busyText), a.redescribe())
	}
	if msg := a.pend.missing(sch); msg != "" {
		return a.flash(msg, true)
	}
	if _, inTx := a.txOn(a.cur.conn); inTx && a.running != nil && a.running.inTx {
		// A driver transaction is not safe for two calls at once.
		return a.flash("a statement is still running in the transaction; ctrl+c cancels it", false)
	}
	a.cell = nil
	mode := applySeq
	conn := a.src.Conn()
	open, inTx := a.txOn(a.cur.conn)
	switch _, isTx := driver.As[driver.Transactor](conn); {
	case inTx:
		mode = applyOpenTx
	case isTx:
		mode = applyTx
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.applySeq++
	done := make(chan struct{})
	run := &applyRun{id: a.applySeq, cancel: cancel, done: done, changes: a.pend.changes(), mode: mode, conn: a.cur.conn, src: a.src}
	a.apply = run
	ref, changes := a.cur.ref, run.changes
	var users []<-chan struct{}
	if inTx {
		users = a.txUsers(done) // wait for a cancelled run still inside it
	}
	wait, hook := a.txWait(), a.txWaitHook
	return func() tea.Msg {
		defer close(done)
		defer cancel()
		switch err := waitUsers(ctx, users, wait, hook); {
		case errors.Is(err, errTxStuck):
			return applyDoneMsg{id: run.id, out: applyOutcome{failed: -1, stage: "wait", err: errTxStuck}}
		case err != nil: // cancelled while waiting: nothing reached the Tx
			return applyDoneMsg{id: run.id, out: applyOutcome{failed: -1, cancelled: true}}
		}
		return applyDoneMsg{id: run.id, out: runApply(ctx, mode, conn, open, ref, changes)}
	}
}

// runApply writes changes, off the Update goroutine.
func runApply(ctx context.Context, mode applyMode, conn driver.Conn, open driver.Tx, ref driver.CollectionRef, changes []change) applyOutcome {
	switch mode {
	case applyTx:
		tr, _ := driver.As[driver.Transactor](conn)
		tx, err := tr.Begin(ctx, driver.TxOptions{})
		if err != nil {
			return applyOutcome{failed: -1, stage: "begin", err: err, cancelled: ctx.Err() != nil}
		}
		mu, ok := driver.As[driver.Mutator](tx)
		if !ok {
			return applyOutcome{failed: -1, stage: "begin", err: errors.New("the transaction does not support edits"), rbErr: rollback(tx)}
		}
		for i, c := range changes {
			if err := applyOne(ctx, mu, ref, c); err != nil {
				return applyOutcome{failed: i, err: err, cancelled: ctx.Err() != nil, rbErr: rollback(tx)}
			}
		}
		if ctx.Err() != nil {
			return applyOutcome{failed: -1, cancelled: true, rbErr: rollback(tx)}
		}
		if err := tx.Commit(ctx); err != nil {
			return applyOutcome{failed: -1, stage: "commit", err: err, cancelled: ctx.Err() != nil, rbErr: rollback(tx)}
		}
		return applyOutcome{applied: len(changes), failed: -1}
	}
	var mu driver.Mutator
	if mode == applyOpenTx {
		mu, _ = driver.As[driver.Mutator](open)
	} else {
		mu, _ = driver.As[driver.Mutator](conn)
	}
	for i, c := range changes {
		if ctx.Err() != nil {
			return applyOutcome{applied: i, failed: -1, cancelled: true}
		}
		if err := applyOne(ctx, mu, ref, c); err != nil {
			return applyOutcome{applied: i, failed: i, err: err, cancelled: ctx.Err() != nil}
		}
	}
	return applyOutcome{applied: len(changes), failed: -1}
}

func applyOne(ctx context.Context, mu driver.Mutator, ref driver.CollectionRef, c change) error {
	switch c.kind {
	case kindInsert:
		_, err := mu.Insert(ctx, ref, c.values)
		return err
	case kindDelete:
		return mu.Delete(ctx, ref, c.key)
	}
	return mu.Update(ctx, ref, c.key, c.values)
}

// rollback ends a failed or cancelled transaction; its context is gone, so
// the rollback gets its own. Its error is reported with the failure.
func rollback(tx driver.Tx) error {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	return tx.Rollback(ctx)
}

// applyDone reports an apply. In a transaction it is all or nothing; one at
// a time, the changes written leave pending and the rest stay.
func (a *Model) applyDone(msg applyDoneMsg) tea.Cmd {
	run := a.apply
	if run == nil || msg.id != run.id {
		return nil
	}
	a.apply = nil
	o, n := msg.out, len(run.changes)
	if errors.Is(o.err, errTxStuck) {
		if a.tx != nil {
			a.tx.stuck = true
		}
		return a.flash(stuckText+"; nothing was applied", true)
	}
	errText := func() string {
		if errors.Is(o.err, driver.ErrReadOnly) {
			// The server refused behind tobira's back (a standby, a role
			// read-only by default): say so like the editor does.
			return roEditText(run.conn)
		}
		if errors.Is(o.err, driver.ErrUnsupported) {
			// A value the driver cannot write (a key-value type it does
			// not replace, say): say so plainly.
			return "the driver does not support this change: " + a.mgr.Redact(run.conn, o.err.Error())
		}
		return a.mgr.Redact(run.conn, o.err.Error())
	}
	var text string
	written := 0
	if run.mode == applyTx {
		switch {
		case o.cancelled:
			text = "apply cancelled; nothing was applied"
		case o.err == nil:
			written = n
			text = "applied " + strconv.Itoa(n) + plural(n, " change", " changes")
		case o.stage != "":
			text = o.stage + ": " + errText() + "; nothing was applied"
		default:
			c := run.changes[o.failed]
			text = c.kind + " row " + c.label + ": " + errText()
		}
	} else {
		written = o.applied
		of := "applied " + strconv.Itoa(o.applied) + " of " + strconv.Itoa(n)
		switch {
		case o.cancelled:
			text = "apply cancelled; " + of
		case o.err != nil:
			c := run.changes[o.failed]
			text = of + " — failed at " + c.kind + " " + c.label + ": " + errText()
		default:
			text = "applied " + strconv.Itoa(n) + plural(n, " change", " changes")
		}
		if run.mode == applyOpenTx {
			// Nothing is committed: the open transaction holds what was written.
			switch {
			case o.err == nil && !o.cancelled:
				text += " inside the open transaction; :tx commit keeps " + plural(n, "it", "them")
			case written > 0:
				text += "; " + strconv.Itoa(written) + plural(written, " change", " changes") +
					" written inside the open transaction; :tx rollback to undo"
			case o.err != nil:
				text += "; " + abortNote
			}
		}
	}
	if a.pend != nil { // nil only if the collection was closed meanwhile
		if o.rbErr != nil {
			text += "; rollback failed: " + a.mgr.Redact(run.conn, o.rbErr.Error())
		}
		for _, c := range run.changes[:written] {
			a.pend.drop(c.row)
		}
	}
	a.syncPending()
	cmds := []tea.Cmd{a.flash(text, o.err != nil && !o.cancelled)}
	if written > 0 {
		cmds = append(cmds, a.invalidateCmd(run.conn))
		if a.src == run.src {
			cmds = append(cmds, a.gr.Refresh())
		}
	}
	return tea.Batch(cmds...)
}

// cancelApply cancels the apply in flight; its result still arrives and says
// what was written.
func (a *Model) cancelApply() bool {
	if a.apply == nil {
		return false
	}
	a.apply.cancel()
	if !a.apply.cancelled {
		a.apply.cancelled, a.apply.cancelAt = true, a.clock()
	}
	return true
}

// stopApplyOn cancels an apply writing on conn ("" for any) before a config
// write or reload may close it, and hands its done channel to that write to
// wait for, like a cancelled editor run.
func (a *Model) stopApplyOn(conn string) {
	if a.apply == nil || conn != "" && a.apply.conn != conn {
		return
	}
	a.cancelApply()
	a.stopping = append(stillRunning(a.stopping), a.apply.done)
}

// stuckAfter is how long a cancelled apply may take to return before quit
// stops waiting for it.
const stuckAfter = 5 * time.Second

func (a *Model) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// stuck reports a cancelled apply that has not returned within stuckAfter:
// its driver ignores cancellation.
func (a *Model) stuck() bool {
	return a.apply != nil && a.apply.cancelled && a.clock().Sub(a.apply.cancelAt) >= stuckAfter
}

// quitStuck quits with an apply that will not return. In a transaction,
// closing the connections at shutdown ends it uncommitted, so the database
// rolls it back; one at a time, the changes already written stay. The
// notice is flashed and, since the screen is gone after quitting, printed
// on stderr by Run.
func (a *Model) quitStuck() tea.Cmd {
	text := "quitting with a stuck apply; changes already applied stay applied"
	if a.apply.mode != applySeq { // a transaction ends, uncommitted, when its connection closes
		text = "quitting with a stuck apply; the transaction is rolled back when the connection closes"
	}
	a.exitNotice = text
	flash := a.flash(text, true)
	a.discardPending()
	return tea.Batch(flash, a.leave())
}

// ---- never lose edits silently ------------------------------------------------------

// guardState is the y/N confirmation before an action that would drop the
// pending changes.
type guardState struct {
	n       int
	reason  string
	proceed tea.Cmd
}

// guardDoneMsg is flashed after a confirmed :discard.
type guardDoneMsg struct{ text string }

// resend is a command that delivers msg again, to redo an action once its
// guard is passed.
func resend(msg tea.Msg) tea.Cmd { return func() tea.Msg { return msg } }

// guard holds an action that would drop the pending changes: it asks
// "discard N pending changes? y/N" and runs proceed (after discarding) on y.
// It reports false, holding nothing, when nothing is pending. While an apply
// runs nothing may be discarded, so the action is refused.
func (a *Model) guard(reason string, proceed tea.Cmd) (tea.Cmd, bool) {
	n := a.pend.count()
	if n == 0 {
		return nil, false
	}
	if a.apply != nil {
		return a.flash(applyingText, false), true
	}
	a.guardSt = &guardState{n: n, reason: reason, proceed: proceed}
	a.cell = nil
	a.help = false
	a.dropDetail()
	return nil, true
}

func (a *Model) guardKey(k tea.KeyPressMsg) tea.Cmd {
	g := a.guardSt
	a.guardSt = nil
	if s := k.String(); s == "y" || s == "Y" {
		a.discardPending()
		return g.proceed
	}
	return a.flash("kept "+strconv.Itoa(g.n)+" pending "+plural(g.n, "change", "changes"), false)
}

func (a *Model) guardView(w, h int) string {
	g := a.guardSt
	st := a.th.Styles
	lines := []string{
		st.Title.Render(query.Truncate("discard "+strconv.Itoa(g.n)+" pending "+plural(g.n, "change", "changes")+"? y/N", w)),
		st.Dim.Render(ansi.Wrap(query.EscapeControls(g.reason), w, "")),
		"",
		st.Dim.Render(ansi.Wrap("y discards them · any other key keeps them (:apply writes them)", w, "")),
	}
	return lipgloss.NewStyle().MaxWidth(w).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// discard is :discard: after a confirmation, drop every pending change.
func (a *Model) discard() tea.Cmd {
	n := a.pend.count()
	if n == 0 {
		return a.flash("nothing to discard", false)
	}
	cmd, _ := a.guard("the pending changes are dropped, not written",
		resend(guardDoneMsg{"discarded " + strconv.Itoa(n) + plural(n, " change", " changes")}))
	return cmd
}

// pendingFor reports whether leaving the open collection for conn and ref
// would drop pending changes.
func (a *Model) pendingFor(conn string, ref driver.CollectionRef) bool {
	return a.cur.set && !a.pend.empty() && (conn != a.cur.conn || ref != a.cur.ref)
}

func collName(ref driver.CollectionRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "." + ref.Name
}
