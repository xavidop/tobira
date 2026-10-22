// Package grid is the virtualised data grid: a table of rows held in pages
// of 200, drawn with cost proportional to the visible cells only. This file
// holds the state and movement; render.go draws it and data.go pages rows in
// from a driver.Browser.
package grid

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/click"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

const (
	pageSize     = 200 // rows per page
	minColWidth  = 4
	autoMaxWidth = 40 // automatic widths are clamped to minColWidth..autoMaxWidth
	maxColWidth  = 80 // manual widths (< and >) are clamped to minColWidth..maxColWidth
	resizeStep   = 2
	wheelStep    = 3
	separator    = "  " // between columns; cheaper than a rule and matches the CLI's table output
)

// RowActivatedMsg is emitted on a double-click: a second click on the same
// cell within click.Window. The parent opens row detail for CurrentRow.
type RowActivatedMsg struct{}

// Model is the grid. Update has a value receiver like the other views, but
// the page cache is a map shared between copies, so a Model must only ever
// live on the Update goroutine.
type Model struct {
	th theme.Theme
	km keymap.Map

	cols    []driver.Column
	widths  []int
	manual  map[string]int // widths set with < and >, by column name
	pages   map[int][]driver.Row
	known   int // rows known to exist: highest loaded row + 1 (exact when exact)
	total   int // total rows when exact, else an estimate (0 = unknown)
	exact   bool
	maxPage int // highest page index loaded so far, -1 when none

	row, col int                   // cursor
	seq      keymap.Seq            // first key of a pending two-key sequence (gg)
	clicks   click.Tracker[[2]int] // the last click, for double-click detection
	now      func() time.Time
	rowOff   int // first visible row
	colOff   int // first visible column
	w, h     int // size given to Resize; View takes its own

	// styles derived once from the theme
	hdr, cell, cursor, null, dim, errSt lipgloss.Style
	rowCell, rowNull                    lipgloss.Style // the cursor's row

	data dataState

	// Inline editing (marks.go): decorations of the loaded rows and the
	// inserted rows pinned above them. Grid rows count the pinned ones
	// first; pages, NeedRows and Status count loaded rows only.
	marks  Marks
	pinned []Pinned

	// Kind views (kinds.go): the parent's cell format, the columns each
	// page holding other columns than the union had (by page index), and
	// the widest cell seen of a column first met past page 0.
	format  Format
	hidden  func(driver.Column) bool
	mark    func(driver.Column) string // header suffix per column (SetHeaderMark)
	present map[int][]bool
	seenW   map[string]int
}

// New returns an empty grid.
func New(th theme.Theme, km keymap.Map) Model {
	m := Model{
		th: th, km: km,
		manual:  map[string]int{},
		pages:   map[int][]driver.Row{},
		present: map[int][]bool{},
		seenW:   map[string]int{},
		maxPage: -1,
		w:       80, h: 24,
	}
	m.applyTheme()
	m.data = newDataState()
	return m
}

// applyTheme derives the cached styles from the theme.
func (m *Model) applyTheme() {
	s := m.th.Styles
	m.hdr, m.cell, m.cursor, m.null, m.dim, m.errSt = s.Header, s.Cell, s.CellCursor, s.Null, s.Dim, s.Error
	// The cursor row reuses the header's background so no new colour is
	// introduced.
	bg := s.Header.GetBackground()
	m.rowCell = s.Cell.Background(bg)
	m.rowNull = s.Null.Background(bg)
}

// SetTheme restyles the grid; rows, cursor and paging state are kept.
func (m Model) SetTheme(th theme.Theme) Model {
	m.th = th
	m.applyTheme()
	return m
}

// SetKeymap swaps the key bindings.
func (m Model) SetKeymap(km keymap.Map) Model { m.km = km; m.seq = keymap.Seq{}; return m }

// WithClock sets the time source used for double-clicks and key sequences
// (time.Now by default); tests inject their own.
func (m Model) WithClock(now func() time.Time) Model { m.now = now; return m }

// ClearSeq forgets a pending first key of a sequence. The parent calls it
// when it handles a key itself, so a g followed by another command and then
// another g is not mistaken for gg.
func (m Model) ClearSeq() Model { m.seq = keymap.Seq{}; return m }

func (m Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// Columns returns the current columns (nil before the first page).
func (m Model) Columns() []driver.Column { return m.cols }

// Resize sets the size used by keyboard and mouse handling. View still draws
// at the size it is given.
func (m Model) Resize(w, h int) Model {
	m.w, m.h = w, h
	m.ensureVisible()
	return m
}

// SetColumns sets the columns and recomputes their widths.
func (m *Model) SetColumns(cols []driver.Column) {
	m.cols = cols
	m.widths = make([]int, len(cols))
	m.recomputeWidths()
	m.col = clamp(m.col, 0, max(0, len(cols)-1))
	m.skipHidden()
	m.ensureVisible()
}

// SetRows stores rows starting at absolute row index start. Rows land in the
// pages they belong to.
func (m *Model) SetRows(start int, rows []driver.Row) {
	for i, r := range rows {
		idx := start + i
		p, off := idx/pageSize, idx%pageSize
		pg := m.pages[p]
		for len(pg) <= off {
			pg = append(pg, nil)
		}
		pg[off] = r
		m.pages[p] = pg
		m.maxPage = max(m.maxPage, p)
	}
	m.known = max(m.known, start+len(rows))
	if m.exact {
		m.known = m.total
	}
	if start < pageSize {
		m.recomputeWidths()
	}
}

// SetTotal records the row count: exact when the last page has been seen,
// otherwise an estimate (0 = unknown).
func (m *Model) SetTotal(total int, exact bool) {
	m.total, m.exact = total, exact
	if exact {
		m.known = total
		m.row = clamp(m.row, 0, max(0, m.rowCount()-1))
		m.ensureVisible()
	}
}

// Cursor returns the cursor's absolute row (pinned rows first) and column.
func (m Model) Cursor() (row, col int) { return m.row, m.col }

// CurrentRow returns the row under the cursor, if it is loaded, as loaded
// (marks do not change it); on a pinned row it is that row's values.
func (m Model) CurrentRow() (driver.Row, bool) { return m.rowAt(m.row) }

func (m Model) rowAt(i int) (driver.Row, bool) {
	if i >= 0 && i < len(m.pinned) {
		r, _ := m.pinned[i].row(m.cols)
		return r, true
	}
	return m.loadedAt(i - len(m.pinned))
}

// loadedAt returns loaded row i (not counting pinned rows).
func (m Model) loadedAt(i int) (driver.Row, bool) {
	if i < 0 {
		return nil, false
	}
	pg := m.pages[i/pageSize]
	off := i % pageSize
	if off >= len(pg) || pg[off] == nil {
		return nil, false
	}
	return pg[off], true
}

// bodyRows is how many rows fit under the header.
func (m Model) bodyRows() int { return max(1, m.h-1) }

// NeedRows reports the first run of rows [from, to) that the viewport, plus
// 50 rows of prefetch below it, would show but that are not loaded. Only rows
// known to exist are reported.
func (m Model) NeedRows() (from, to int, ok bool) {
	off := m.dataRow(m.rowOff)
	end := min(m.known, m.rowOff-len(m.pinned)+m.bodyRows()+prefetchAhead)
	for i := off; i < end; i++ {
		if _, loaded := m.loadedAt(i); loaded {
			if ok {
				return from, i, true
			}
			continue
		}
		if !ok {
			from, ok = i, true
		}
	}
	if ok {
		return from, end, true
	}
	return 0, 0, false
}

// rowCount is how many rows the cursor may move over.
func (m Model) rowCount() int { return m.known + len(m.pinned) }

// Update handles movement, column resizing and the mouse.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.key(msg)
	case PageMsg:
		return m.applyPage(msg)
	case tea.MouseClickMsg:
		return m.click(tea.Mouse(msg))
	case tea.MouseWheelMsg:
		return m.wheel(tea.Mouse(msg))
	}
	return m, nil
}

func (m Model) key(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	n := m.rowCount()
	var seqAct keymap.Action
	var held bool
	m.seq, seqAct, held = m.km.Step(m.seq, msg, m.clock())
	if held && seqAct == "" {
		return m, nil // the first key of a sequence
	}
	switch {
	case seqAct == keymap.Top:
		m.row = 0
	case m.km.Matches(msg, keymap.Up):
		m.row--
	case m.km.Matches(msg, keymap.Down):
		m.row++
	case m.km.Matches(msg, keymap.PageUp):
		m.row -= m.bodyRows()
	case m.km.Matches(msg, keymap.PageDown):
		m.row += m.bodyRows()
	case m.km.Matches(msg, keymap.Top):
		m.row = 0
	case m.km.Matches(msg, keymap.Bottom):
		m.row = n - 1
	case m.km.Matches(msg, keymap.Left):
		m.col = m.visibleFrom(m.col-1, -1, m.col)
	case m.km.Matches(msg, keymap.Right):
		m.col = m.visibleFrom(m.col+1, 1, m.col)
	case m.km.Matches(msg, keymap.Home):
		m.col = m.visibleFrom(0, 1, m.col)
	case m.km.Matches(msg, keymap.End):
		m.col = m.visibleFrom(len(m.cols)-1, -1, m.col)
	case m.km.Matches(msg, keymap.WidenCol):
		m.resizeCol(resizeStep)
	case m.km.Matches(msg, keymap.NarrowCol):
		m.resizeCol(-resizeStep)
	default:
		return m.dataKey(msg)
	}
	m.settle()
	cmd := m.next()
	return m, cmd
}

// settle clamps the cursor and scrolls it into view.
func (m *Model) settle() {
	m.row = clamp(m.row, 0, max(0, m.rowCount()-1))
	m.col = clamp(m.col, 0, max(0, len(m.cols)-1))
	m.skipHidden()
	m.ensureVisible()
	m.touchVisible()
}

func (m *Model) resizeCol(delta int) {
	if m.col >= len(m.cols) {
		return
	}
	w := clamp(m.widths[m.col]+delta, minColWidth, maxColWidth)
	m.widths[m.col] = w
	m.manual[m.cols[m.col].Name] = w
}

// ensureVisible scrolls so the cursor is on screen at the stored size.
func (m *Model) ensureVisible() {
	m.rowOff, m.colOff = scroll(m.row, m.col, m.rowOff, m.colOff, m.widths, m.hiddenCol, m.w-m.gutter(), m.bodyRows())
}

// scroll returns row and column offsets that keep the cursor in view.
// Hidden columns take no space.
func scroll(row, col, rowOff, colOff int, widths []int, hidden func(int) bool, width, body int) (int, int) {
	if row < rowOff {
		rowOff = row
	}
	if row >= rowOff+body {
		rowOff = row - body + 1
	}
	rowOff = max(0, rowOff)
	if col < colOff {
		colOff = col
	}
	for colOff < col && colEnd(widths, hidden, colOff, col) > width {
		colOff++
	}
	return rowOff, max(0, colOff)
}

// colEnd is the x just past column last when drawing from column first,
// skipping hidden columns.
func colEnd(widths []int, hidden func(int) bool, first, last int) int {
	x, drawn := 0, false
	for i := first; i <= last && i < len(widths); i++ {
		if hidden(i) {
			continue
		}
		if drawn {
			x += len(separator)
		}
		x += widths[i]
		drawn = true
	}
	return x
}

func (m Model) click(ms tea.Mouse) (Model, tea.Cmd) {
	if ms.Button != tea.MouseLeft || ms.Y < 1 {
		return m, nil
	}
	r := m.rowOff + ms.Y - 1
	if r >= m.rowCount() {
		return m, nil
	}
	c := -1
	x := ms.X - m.gutter()
	for _, sp := range m.spans(m.w-m.gutter(), m.colOff) {
		if x >= sp.x && x < sp.x+sp.w {
			c = sp.col
		}
	}
	if c < 0 {
		return m, nil
	}
	var dbl bool
	m.clicks, dbl = m.clicks.Hit([2]int{r, c}, m.clock())
	if dbl && r == m.row && c == m.col {
		return m, func() tea.Msg { return RowActivatedMsg{} }
	}
	m.row, m.col = r, c
	m.settle()
	cmd := m.next()
	return m, cmd
}

// wheel scrolls the viewport and keeps the cursor inside it.
func (m Model) wheel(ms tea.Mouse) (Model, tea.Cmd) {
	maxOff := max(0, m.rowCount()-m.bodyRows())
	switch ms.Button {
	case tea.MouseWheelUp:
		m.rowOff = max(0, m.rowOff-wheelStep)
	case tea.MouseWheelDown:
		m.rowOff = min(maxOff, m.rowOff+wheelStep)
	default:
		return m, nil
	}
	m.row = clamp(m.row, m.rowOff, m.rowOff+m.bodyRows()-1)
	m.row = clamp(m.row, 0, max(0, m.rowCount()-1))
	m.touchVisible()
	cmd := m.next()
	return m, cmd
}

// recomputeWidths sets every column that has no manual width from its header
// and the cells of the first page.
func (m *Model) recomputeWidths() {
	if len(m.widths) != len(m.cols) {
		m.widths = make([]int, len(m.cols))
	}
	first := m.pages[0]
	for i, c := range m.cols {
		if w, ok := m.manual[c.Name]; ok {
			m.widths[i] = w
			continue
		}
		w := max(query.DisplayWidth(m.headerText(i)), m.seenW[c.Name])
		for _, r := range first {
			if i < len(r) {
				w = max(w, m.cellWidth(r, i))
			}
		}
		m.widths[i] = clamp(w, minColWidth, autoMaxWidth)
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }
