package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

// buffer is a connection's editor text, kept while another connection's is
// shown.
type buffer struct {
	text   string
	cursor int
}

// editorConn is the connection the editor works on: the open transaction's
// while one is open (it stays pinned there), else the one of the open
// collection, else the one under the sidebar cursor.
func (a *Model) editorConn() string {
	if a.tx != nil {
		return a.tx.conn
	}
	if a.cur.set {
		return a.cur.conn
	}
	conn, _ := a.sb.Selected()
	return conn
}

// syncEditorConn swaps the editor buffer when its connection changed: the
// old one is kept in memory and the new one's restored.
func (a *Model) syncEditorConn() {
	conn := a.editorConn()
	if conn == a.edConn {
		// The same name may now be another driver (an edit or :reload):
		// keep the lexer and the statement splitter in step.
		if d := a.mgr.State(conn).Info.Dialect; d != a.ed.Dialect() {
			a.ed.SetDialect(d)
		}
		return
	}
	if a.edConn != "" {
		a.buffers[a.edConn] = buffer{a.ed.Value(), a.ed.Cursor()}
	}
	a.leaveEditor()
	b := a.buffers[conn]
	a.ed.SetValue(b.text)
	a.ed.SetCursor(b.cursor)
	a.ed.SetDialect(a.mgr.State(conn).Info.Dialect)
	a.edConn = conn
}

// openEditor opens the editor, focused.
func (a *Model) openEditor() {
	a.edOpen, a.focus = true, focusEditor
	a.help = false
	a.dropDetail()
}

// toggleEditor is ctrl+e: open and focus a closed editor, focus an open one,
// close a focused one.
func (a *Model) toggleEditor() {
	switch {
	case !a.edOpen || a.focus != focusEditor:
		a.openEditor()
	default:
		a.edOpen, a.focus = false, focusGrid
	}
}

// editorKey handles a key while the editor has the focus. The editor owns
// every key except these: no bare-letter binding may fire while typing.
//
// The history search prompt and the completion popup see a key first; the
// popup takes only its own keys, so enter and tab keep their meaning when
// it is closed.
func (a *Model) editorKey(k tea.KeyPressMsg) tea.Cmd {
	if a.hs.active {
		if cmd, ok := a.searchKey(k); ok {
			return cmd
		}
	}
	if a.popupShown() {
		if cmd, ok := a.popupKey(k); ok {
			return cmd
		}
	}
	switch {
	case a.km.Matches(k, keymap.Back):
		a.focus = focusGrid
		return nil
	case a.km.Matches(k, keymap.FocusNext):
		a.cycleFocus(1)
		return nil
	case a.km.Matches(k, keymap.FocusPrev):
		a.cycleFocus(-1)
		return nil
	case a.km.Matches(k, keymap.EditorToggle):
		a.toggleEditor()
		return nil
	case a.km.Matches(k, keymap.RunStatement):
		return a.runStatement()
	case a.km.Matches(k, keymap.RunBuffer):
		return a.runBuffer()
	case k.String() == "f1":
		a.openHelp()
		return nil
	case a.km.Matches(k, keymap.Complete):
		return a.manualComplete()
	case a.km.Matches(k, keymap.HistSearch):
		return a.startSearch()
	}
	if cmd, ok := a.walkKey(k); ok {
		a.closePopup()
		return cmd
	}
	ver, cur := a.ed.Version(), a.ed.Cursor()
	var cmd tea.Cmd
	a.ed, cmd = a.ed.Update(k)
	return tea.Batch(cmd, a.afterEditorKey(k.Text != "", ver, cur))
}

// leaveEditor drops the popup, the history search (keeping its match) and
// the history walk: the editor lost the focus or switched buffers.
func (a *Model) leaveEditor() {
	if a.cp.open || a.cp.manual {
		a.closePopup()
	}
	a.endSearch(true)
	a.endWalk()
}

// editorTitle is the line over the editor text: what it runs on and how.
func (a *Model) editorTitle(w int) string {
	parts := []string{"editor"}
	if a.edConn != "" {
		parts = append(parts, query.EscapeControls(a.edConn))
	}
	if d := a.ed.Dialect(); d != "" {
		parts = append(parts, query.EscapeControls(d))
	}
	parts = append(parts, "limit "+strconv.Itoa(a.limit))
	if a.focus == focusEditor {
		parts = append(parts, "f5 run")
	} else {
		parts = append(parts, "ctrl+e focus")
	}
	st := a.th.Styles.Dim
	if a.focus == focusEditor {
		st = a.th.Styles.Title
	}
	t := query.Truncate(strings.Join(parts, " · "), w)
	return st.Render(t) + strings.Repeat(" ", max(0, w-query.DisplayWidth(t)))
}
