package grid

import (
	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
)

// gutterW is the width of the change gutter drawn while marks are set: "+ "
// for an inserted row, "~ " for a row with edited cells, "- " for a row
// marked for deletion.
const gutterW = 2

// Marks decorates the loaded rows with pending edits. The parent sets it
// while it holds pending changes; the grid asks it about every row it draws,
// so it must be cheap.
type Marks interface {
	// Row says how a loaded row, with the grid's columns cols, is drawn.
	Row(cols []driver.Column, row driver.Row) RowMark
}

// RowMark is how one loaded row is drawn.
type RowMark struct {
	// Values is what to show instead of the row as loaded; nil shows the row.
	Values driver.Row
	// Changed flags the edited cells, by column; nil when none.
	Changed []bool
	// Deleted marks the row for deletion: it is struck through and dimmed.
	Deleted bool
}

// Pinned is an inserted row, drawn in a block above the loaded rows. Its
// values are matched to the columns by name when drawn, so a change of
// columns cannot put one under the wrong column. A column it has no value
// for shows "default", because the database fills it.
type Pinned struct {
	Values driver.Object
}

// row lays p out in the columns cols: the values, and which are set.
func (p Pinned) row(cols []driver.Column) (driver.Row, []bool) {
	vals, set := make(driver.Row, len(cols)), make([]bool, len(cols))
	for i, c := range cols {
		vals[i], set[i] = p.Values.Get(c.Name)
	}
	return vals, set
}

// SetMarks sets (or, with nil, clears) the decorations of the loaded rows.
// While marks are set, every row has the change gutter.
func (m *Model) SetMarks(mk Marks) {
	m.marks = mk
	m.ensureVisible()
}

// SetPinned replaces the inserted rows shown above the loaded ones. The
// cursor stays on the row it was on: a loaded row moves with the block, and
// a pinned row keeps its index when it still exists.
func (m *Model) SetPinned(rows []Pinned) {
	old := len(m.pinned)
	m.pinned = rows
	if m.row >= old {
		m.row += len(rows) - old
	}
	m.settle()
}

// PinnedAt reports the index of the pinned row under the cursor.
func (m Model) PinnedAt() (int, bool) {
	if m.row < len(m.pinned) {
		return m.row, true
	}
	return 0, false
}

// PinnedCount is how many inserted rows head the grid.
func (m Model) PinnedCount() int { return len(m.pinned) }

// SetCursor moves the cursor to row (pinned rows first) and column col,
// clamped, and scrolls it into view.
func (m *Model) SetCursor(row, col int) {
	m.row, m.col = row, col
	m.settle()
}

// Refresh fetches the rows again from the first page, keeping sort, filter,
// column widths and the cursor: once the pages up to the cursor's row are
// back it is where it was, or on the last row when there are fewer now.
func (m *Model) Refresh() tea.Cmd {
	if !m.ready() {
		return nil
	}
	want := m.row - len(m.pinned)
	m.clearRows()
	m.data.want = max(want, 0)
	m.data.placed = m.row
	return m.fetch(0)
}

// gutter is the width taken by the change gutter.
func (m Model) gutter() int {
	if m.marks == nil {
		return 0
	}
	return gutterW
}

// dataRow converts a grid row (pinned rows first) to a loaded row index.
func (m Model) dataRow(r int) int { return max(r-len(m.pinned), 0) }

// restoreCursor moves the cursor toward the row Refresh wants as pages come
// back, and stops once it is there, the data ends before it, or the user
// moved the cursor since it was last placed.
func (m *Model) restoreCursor() {
	if m.data.want < 0 {
		return
	}
	if m.row != m.data.placed {
		m.data.want = -1
		return
	}
	target := m.data.want + len(m.pinned)
	m.row = min(target, max(0, m.rowCount()-1))
	m.data.placed = m.row
	if m.row == target || m.exact {
		m.data.want = -1
	}
}
