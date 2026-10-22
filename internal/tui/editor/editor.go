// Package editor is the query editor: a multi-line text buffer with a
// grapheme-aware cursor, a selection and chroma highlighting. It is a custom
// component rather than bubbles' textarea, which can neither draw styled
// text nor hold a selection. This file holds the buffer and editing; view.go
// draws it and highlight.go colours it.
package editor

import (
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// MaxBytes caps the buffer: typing or pasting past it is refused.
const MaxBytes = 1 << 20

// indent is what tab inserts, and what a pasted tab becomes.
const indent = "  "

// wheelStep is how many lines one wheel notch moves the cursor.
const wheelStep = 3

// RefusedMsg reports an edit the editor refused, such as a paste that would
// grow the buffer past MaxBytes. The parent flashes Text.
type RefusedMsg struct{ Text string }

// pos is a place in the buffer: a line and a byte offset into it, always on
// a grapheme cluster boundary.
type pos struct{ row, col int }

func (p pos) before(q pos) bool { return p.row < q.row || p.row == q.row && p.col < q.col }

// Model is the editor. Update has a value receiver like the other views, but
// the highlight cache is a pointer shared between copies, so a Model must
// only ever live on the Update goroutine.
type Model struct {
	th theme.Theme
	km keymap.Map

	lines  []string
	cur    pos
	anchor pos  // the other end of the selection
	sel    bool // anchor is set
	goal   int  // display column kept by vertical moves; -1 when none
	size   int  // bytes in Value()

	rowOff, colOff int // first visible line and display column
	w, h           int // size given to Resize

	version uint64 // bumped by every change, keys the highlight cache
	dialect string
	hl      *highlighter
}

// New returns an empty editor.
func New(th theme.Theme, km keymap.Map) Model {
	return Model{th: th, km: km, lines: []string{""}, goal: -1, w: 80, h: 10, hl: newHighlighter(th, "")}
}

// SetTheme restyles the editor.
func (m Model) SetTheme(th theme.Theme) Model {
	m.th = th
	m.hl = newHighlighter(th, m.dialect)
	return m
}

// SetKeymap swaps the key bindings.
func (m Model) SetKeymap(km keymap.Map) Model { m.km = km; return m }

// SetDialect chooses the highlighting lexer from a driver dialect
// (driver.Info.Dialect).
func (m *Model) SetDialect(d string) {
	if d == m.dialect {
		return
	}
	m.dialect = d
	m.hl = newHighlighter(m.th, d)
}

// Dialect returns the dialect set with SetDialect.
func (m Model) Dialect() string { return m.dialect }

// Resize sets the size used by keyboard and mouse handling.
func (m Model) Resize(w, h int) Model {
	m.w, m.h = w, h
	m.ensureVisible()
	return m
}

// Value returns the buffer text.
func (m Model) Value() string { return strings.Join(m.lines, "\n") }

// SetValue replaces the buffer, puts the cursor at its end and clears the
// selection. "\r\n" and "\r" become "\n", and a tab two spaces.
func (m *Model) SetValue(s string) {
	m.lines = strings.Split(normalize(s), "\n")
	m.size = 0
	for _, l := range m.lines {
		m.size += len(l)
	}
	m.size += len(m.lines) - 1
	m.cur = pos{len(m.lines) - 1, len(m.lines[len(m.lines)-1])}
	m.sel, m.goal = false, -1
	m.rowOff, m.colOff = 0, 0
	m.changed()
}

// Cursor returns the cursor as a byte offset into Value().
func (m Model) Cursor() int { return m.offset(m.cur) }

// SetCursor moves the cursor to byte offset (clamped to the buffer and
// snapped back to a grapheme cluster boundary) and clears the selection.
func (m *Model) SetCursor(offset int) {
	m.cur = m.posAt(offset)
	m.sel, m.goal = false, -1
	m.ensureVisible()
}

// Selection returns the selected byte range of Value(), start < end, when
// there is a non-empty selection.
func (m Model) Selection() (start, end int, ok bool) {
	a, b, ok := m.selRange()
	if !ok {
		return 0, 0, false
	}
	return m.offset(a), m.offset(b), true
}

// InsertAt inserts s at byte offset and leaves the cursor at the end of the
// insert, with no selection. An insert that would pass MaxBytes is dropped.
func (m *Model) InsertAt(offset int, s string) {
	p := m.posAt(offset)
	m.sel = false
	m.replace(p, p, s)
}

// ReplaceRange replaces the bytes [start, end) of Value() with s and leaves
// the cursor after the insert, with no selection. The ends are clamped,
// snapped back to cluster boundaries and put in order. It reports false,
// changing nothing, when the result would pass MaxBytes.
func (m *Model) ReplaceRange(start, end int, s string) bool {
	a, b := m.posAt(start), m.posAt(end)
	if b.before(a) {
		a, b = b, a
	}
	m.sel = false
	ok := m.replace(a, b, s)
	m.ensureVisible()
	return ok
}

// Version identifies the buffer text: every edit changes it, cursor moves do
// not. Asynchronous work computed for one version is stale at another.
func (m Model) Version() uint64 { return m.version }

// CursorPos is where the cursor is drawn in a w x h View: the column (gutter
// included) and the line, both from 0.
func (m Model) CursorPos(w, h int) (x, y int) {
	rowOff, colOff := m.scroll(w, h)
	return m.gutterWidth(w) + m.displayCol(m.cur.row, m.cur.col) - colOff, m.cur.row - rowOff
}

// CursorLine returns the cursor's line, the cursor's byte offset into it and
// the line's byte offset into Value(), without joining the buffer.
func (m Model) CursorLine() (line string, col, start int) {
	return m.lines[m.cur.row], m.cur.col, m.offset(pos{m.cur.row, 0})
}

// FirstLine reports whether the cursor is on the first line.
func (m Model) FirstLine() bool { return m.cur.row == 0 }

// LastLine reports whether the cursor is on the last line.
func (m Model) LastLine() bool { return m.cur.row == len(m.lines)-1 }

// Update handles editing keys, paste, clicks (coordinates relative to the
// editor's top-left corner) and the wheel, which moves the cursor and the
// view with it.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		cmd := m.key(msg)
		m.ensureVisible()
		return m, cmd
	case tea.PasteMsg:
		cmd := m.insert(msg.Content)
		m.ensureVisible()
		return m, cmd
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			m.click(msg.X, msg.Y)
		}
		return m, nil
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.vertical(false, -wheelStep)
		case tea.MouseWheelDown:
			m.vertical(false, wheelStep)
		}
		m.ensureVisible()
		return m, nil
	}
	return m, nil
}

func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	// Movement keys extend the selection with shift held, in any
	// combination (shift+ctrl+home); the name is matched without it.
	shift := k.Mod&tea.ModShift != 0
	name := tea.Key{Code: k.Code, Mod: k.Mod &^ tea.ModShift}.Keystroke()
	switch name {
	case "left":
		m.move(shift, m.left(m.cur))
	case "right":
		m.move(shift, m.right(m.cur))
	case "up":
		m.vertical(shift, -1)
	case "down":
		m.vertical(shift, 1)
	case "pgup":
		m.vertical(shift, -max(1, m.h-1))
	case "pgdown":
		m.vertical(shift, max(1, m.h-1))
	case "home":
		m.move(shift, pos{m.cur.row, 0})
	case "end":
		m.move(shift, pos{m.cur.row, len(m.lines[m.cur.row])})
	case "ctrl+home":
		m.move(shift, pos{})
	case "ctrl+end":
		last := len(m.lines) - 1
		m.move(shift, pos{last, len(m.lines[last])})
	case "alt+left":
		m.move(shift, m.wordLeft(m.cur))
	case "alt+right":
		m.move(shift, m.wordRight(m.cur))
	case "enter":
		line := m.lines[m.cur.row]
		ws := len(line) - len(strings.TrimLeft(line, " \t"))
		return m.insert("\n" + line[:min(ws, m.cur.col)])
	case "tab":
		if !shift {
			return m.insert(indent)
		}
	case "backspace":
		if !m.deleteSelection() && m.cur != (pos{}) {
			m.replace(m.left(m.cur), m.cur, "")
		}
	case "delete":
		if !m.deleteSelection() {
			m.replace(m.cur, m.right(m.cur), "")
		}
	default:
		if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModSuper|tea.ModMeta|tea.ModHyper) == 0 {
			return m.insert(k.Text)
		}
	}
	return nil
}

// move puts the cursor at p, extending the selection when shift is held and
// clearing it otherwise.
func (m *Model) move(shift bool, p pos) {
	m.extend(shift)
	m.cur = p
	m.goal = -1
}

func (m *Model) extend(shift bool) {
	switch {
	case shift && !m.sel:
		m.anchor, m.sel = m.cur, true
	case !shift:
		m.sel = false
	}
}

// vertical moves the cursor by dy lines, keeping its display column.
func (m *Model) vertical(shift bool, dy int) {
	m.extend(shift)
	if m.goal < 0 {
		m.goal = m.displayCol(m.cur.row, m.cur.col)
	}
	row := clamp(m.cur.row+dy, 0, len(m.lines)-1)
	switch {
	case row == m.cur.row && dy < 0:
		m.cur.col = 0
	case row == m.cur.row && dy > 0:
		m.cur.col = len(m.lines[row])
	default:
		m.cur = pos{row, m.colAt(row, m.goal)}
	}
}

// insert replaces the selection (or inserts at the cursor) with text.
func (m *Model) insert(text string) tea.Cmd {
	start, end := m.cur, m.cur
	if a, b, ok := m.selRange(); ok {
		start, end = a, b
	}
	if !m.replace(start, end, text) {
		return func() tea.Msg {
			return RefusedMsg{Text: "refused: the editor holds at most 1 MiB"}
		}
	}
	return nil
}

func (m *Model) deleteSelection() bool {
	a, b, ok := m.selRange()
	if !ok {
		return false
	}
	m.replace(a, b, "")
	return true
}

// replace swaps the text between a and b (a not after b) for text, leaves
// the cursor after it and clears the selection. It reports false, changing
// nothing, when the result would pass MaxBytes.
func (m *Model) replace(a, b pos, text string) bool {
	text = normalize(text)
	removed := m.offset(b) - m.offset(a)
	if m.size-removed+len(text) > MaxBytes {
		return false
	}
	parts := strings.Split(text, "\n")
	prefix, suffix := m.lines[a.row][:a.col], m.lines[b.row][b.col:]
	last := len(parts) - 1
	cur := pos{a.row + last, len(parts[last])}
	if last == 0 {
		cur.col += len(prefix)
	}
	parts[0] = prefix + parts[0]
	parts[last] += suffix
	lines := make([]string, 0, len(m.lines)-(b.row-a.row)+last)
	lines = append(lines, m.lines[:a.row]...)
	lines = append(lines, parts...)
	lines = append(lines, m.lines[b.row+1:]...)
	m.lines = lines
	m.size += len(text) - removed
	m.cur, m.sel, m.goal = cur, false, -1
	m.changed()
	return true
}

func (m *Model) changed() { m.version++ }

// selRange returns the selection's ends in order, when it is not empty.
func (m Model) selRange() (pos, pos, bool) {
	if !m.sel || m.anchor == m.cur {
		return pos{}, pos{}, false
	}
	if m.anchor.before(m.cur) {
		return m.anchor, m.cur, true
	}
	return m.cur, m.anchor, true
}

// offset converts p to a byte offset into Value().
func (m Model) offset(p pos) int {
	n := 0
	for i := 0; i < p.row; i++ {
		n += len(m.lines[i]) + 1
	}
	return n + p.col
}

// posAt converts a byte offset into Value() to a position, clamped and
// snapped back to a cluster boundary.
func (m Model) posAt(offset int) pos {
	offset = clamp(offset, 0, m.size)
	for i, l := range m.lines {
		if offset <= len(l) {
			return pos{i, snap(l, offset)}
		}
		offset -= len(l) + 1
	}
	last := len(m.lines) - 1
	return pos{last, len(m.lines[last])}
}

// snap returns the start of the grapheme cluster of line containing byte col.
func snap(line string, col int) int {
	at := 0
	eachCluster(line, func(start int, c string) bool {
		if start+len(c) > col {
			return false
		}
		at = start + len(c)
		return true
	})
	return at
}

// left is the position one cluster before p (the end of the previous line at
// a line start).
func (m Model) left(p pos) pos {
	if p.col == 0 {
		if p.row == 0 {
			return p
		}
		return pos{p.row - 1, len(m.lines[p.row-1])}
	}
	prev := 0
	eachCluster(m.lines[p.row], func(start int, c string) bool {
		if start >= p.col {
			return false
		}
		prev = start
		return true
	})
	return pos{p.row, prev}
}

// right is the position one cluster after p.
func (m Model) right(p pos) pos {
	line := m.lines[p.row]
	if p.col >= len(line) {
		if p.row == len(m.lines)-1 {
			return p
		}
		return pos{p.row + 1, 0}
	}
	c, _ := ansi.FirstGraphemeCluster(line[p.col:], ansi.GraphemeWidth)
	if c == "" {
		c = line[p.col : p.col+1]
	}
	return pos{p.row, p.col + len(c)}
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// runeBefore and runeAt look at the rune on either side of p within its line.
func (m Model) runeBefore(p pos) rune {
	r, _ := utf8.DecodeLastRuneInString(m.lines[p.row][:p.col])
	return r
}

func (m Model) runeAt(p pos) rune {
	r, _ := utf8.DecodeRuneInString(m.lines[p.row][p.col:])
	return r
}

// wordLeft moves to the start of the word before p, crossing a line start.
func (m Model) wordLeft(p pos) pos {
	if p.col == 0 {
		return m.left(p)
	}
	for p.col > 0 && !isWordRune(m.runeBefore(p)) {
		p = m.left(p)
	}
	for p.col > 0 && isWordRune(m.runeBefore(p)) {
		p = m.left(p)
	}
	return p
}

// wordRight moves to the end of the word after p, crossing a line end.
func (m Model) wordRight(p pos) pos {
	n := len(m.lines[p.row])
	if p.col >= n {
		return m.right(p)
	}
	for p.col < n && !isWordRune(m.runeAt(p)) {
		p = m.right(p)
	}
	for p.col < n && isWordRune(m.runeAt(p)) {
		p = m.right(p)
	}
	return p
}

// eachCluster calls f with the start offset and text of every grapheme
// cluster of s until f returns false.
func eachCluster(s string, f func(start int, c string) bool) {
	for i := 0; i < len(s); {
		c, _ := ansi.FirstGraphemeCluster(s[i:], ansi.GraphemeWidth)
		if c == "" {
			c = s[i : i+1]
		}
		if !f(i, c) {
			return
		}
		i += len(c)
	}
}

// clusterWidth is a cluster as the view draws it, and its display width:
// query.EscapeControls shows controls, and bytes that are not UTF-8, as
// escapes.
func clusterWidth(c string) (shown string, w int) {
	shown = query.EscapeControls(c)
	return shown, ansi.StringWidth(shown)
}

// displayCol is the display column of byte col on line row.
func (m Model) displayCol(row, col int) int {
	x := 0
	eachCluster(m.lines[row][:col], func(_ int, c string) bool {
		_, w := clusterWidth(c)
		x += w
		return true
	})
	return x
}

// colAt is the byte offset of the last cluster boundary on line row whose
// display column is at most x.
func (m Model) colAt(row, x int) int {
	at, used := 0, 0
	eachCluster(m.lines[row], func(start int, c string) bool {
		_, w := clusterWidth(c)
		if used+w > x {
			return false
		}
		used += w
		at = start + len(c)
		return true
	})
	return at
}

// click places the cursor at (x, y) of the drawn view.
func (m *Model) click(x, y int) {
	row := clamp(m.rowOff+y, 0, len(m.lines)-1)
	tx := max(0, x-m.gutterWidth(m.w))
	m.cur = pos{row, m.colAt(row, m.colOff+tx)}
	m.sel, m.goal = false, -1
	m.ensureVisible()
}

// ensureVisible scrolls so the cursor is inside the stored size.
func (m *Model) ensureVisible() {
	m.rowOff, m.colOff = m.scroll(m.w, m.h)
}

// scroll returns the offsets that keep the cursor visible in a w x h view.
func (m Model) scroll(w, h int) (rowOff, colOff int) {
	rowOff, colOff = m.rowOff, m.colOff
	h = max(1, h)
	if m.cur.row < rowOff {
		rowOff = m.cur.row
	}
	if m.cur.row >= rowOff+h {
		rowOff = m.cur.row - h + 1
	}
	rowOff = clamp(rowOff, 0, max(0, len(m.lines)-1))
	tw := max(1, w-m.gutterWidth(w))
	cx := m.displayCol(m.cur.row, m.cur.col)
	if cx < colOff {
		colOff = cx
	}
	if cx >= colOff+tw {
		colOff = cx - tw + 1
	}
	return rowOff, max(0, colOff)
}

// normalize turns "\r\n" and "\r" into "\n" and every tab into the indent
// (two spaces), so the buffer never holds a tab.
func normalize(s string) string {
	if strings.ContainsRune(s, '\t') {
		s = strings.ReplaceAll(s, "\t", indent)
	}
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }
