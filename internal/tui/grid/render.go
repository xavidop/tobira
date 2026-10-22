package grid

import (
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
)

// span is one visible column: its index, left edge and drawn width.
type span struct{ col, x, w int }

// spans lays out the columns that fit in width, starting at first. The last
// one is clipped to the space left.
func (m Model) spans(width, first int) []span {
	var out []span
	x := 0
	for i := first; i < len(m.widths) && x < width; i++ {
		if m.hiddenCol(i) {
			continue
		}
		out = append(out, span{col: i, x: x, w: min(m.widths[i], width-x)})
		x += m.widths[i] + len(separator)
	}
	return out
}

// cellWidth is the display width of cell col of row, capped well above the
// widest column so a huge value is never scanned in full.
func (m Model) cellWidth(row driver.Row, col int) int {
	t, _ := m.cellText(row, col, maxColWidth)
	return query.DisplayWidth(t)
}

func runeStart(b byte) bool { return b&0xC0 != 0x80 }

// View draws the grid into a width x height box: a header line, then up to
// height-1 rows. Only the visible rows and columns are looked at.
func (m Model) View(width, height int, focused bool) string {
	if width < 1 || height < 1 {
		return ""
	}
	g := min(m.gutter(), width-1)
	inner := width - g
	rowOff, colOff := scroll(m.row, m.col, m.rowOff, m.colOff, m.widths, m.hiddenCol, inner, max(1, height-1))
	spans := m.spans(inner, colOff)

	lines := make([]string, 0, height)
	lines = append(lines, m.hdr.Render(strings.Repeat(" ", g))+m.headerLine(spans, inner))
	known := m.rowCount()
	for i := 0; i < height-1; i++ {
		r := rowOff + i
		if r >= known {
			lines = append(lines, strings.Repeat(" ", width))
			continue
		}
		lines = append(lines, m.rowLine(r, spans, width, g, focused))
	}
	m.overlayStatus(lines, width, m.known)
	return strings.Join(lines, "\n")
}

func (m Model) headerLine(spans []span, width int) string {
	var sb strings.Builder
	for i, sp := range spans {
		if i > 0 {
			sb.WriteString(separator)
		}
		sb.WriteString(padTo(query.Truncate(m.headerText(sp.col), sp.w), sp.w))
	}
	return m.hdr.Render(padTo(sb.String(), width))
}

func (m Model) headerText(col int) string {
	return query.EscapeControls(m.cols[col].Name) + m.headerMark(col) + m.sortMark(col)
}

// rowLine draws row r, after a gutter g wide. Runs of ordinary cells share
// one styled string; only NULLs, edited cells and the cursor cell are styled
// on their own.
func (m Model) rowLine(r int, spans []span, width, g int, focused bool) string {
	row, loaded := m.rowAt(r)
	base, nullSt := m.cell, m.null
	if r == m.row {
		base, nullSt = m.rowCell, m.rowNull
	}
	var mark RowMark
	var set []bool
	pinned := r < len(m.pinned)
	switch {
	case pinned:
		row, set = m.pinned[r].row(m.cols)
		mark.Changed = set
	case loaded && m.marks != nil:
		mark = m.marks.Row(m.cols, row)
	}
	if mark.Values != nil {
		row = mark.Values
	}
	if mark.Deleted {
		// Struck through, and dim for terminals without strikethrough.
		base, nullSt = base.Strikethrough(true).Foreground(m.th.Muted), nullSt.Strikethrough(true)
	}
	changed := m.cell.Background(m.th.Warn).Foreground(m.th.Bg)
	var out, run strings.Builder
	if g > 0 {
		sign := " "
		switch {
		case pinned:
			sign = "+"
		case mark.Deleted:
			sign = "-"
		case slices.Contains(mark.Changed, true):
			sign = "~"
		}
		out.WriteString(m.dim.Render(sign + strings.Repeat(" ", g-1)))
		width -= g
	}
	flush := func() {
		if run.Len() > 0 {
			out.WriteString(base.Render(run.String()))
			run.Reset()
		}
	}
	if !loaded {
		out.WriteString(m.dim.Render("…"))
		out.WriteString(base.Render(strings.Repeat(" ", width-1)))
		return out.String()
	}
	for i, sp := range spans {
		if i > 0 {
			run.WriteString(separator)
		}
		txt, kind := m.cellAt(r, row, sp.col, sp.w)
		unset := pinned && (sp.col >= len(set) || !set[sp.col])
		if unset {
			txt, kind = query.Truncate("default", sp.w), cellPlain
		}
		pad := strings.Repeat(" ", max(0, sp.w-query.DisplayWidth(txt)))
		switch {
		case focused && r == m.row && sp.col == m.col:
			flush()
			out.WriteString(m.cursor.Render(txt + pad))
		case sp.col < len(mark.Changed) && mark.Changed[sp.col] && !mark.Deleted:
			flush()
			out.WriteString(changed.Render(txt + pad))
		case unset, kind == cellAbsent:
			flush()
			out.WriteString(m.dim.Render(txt))
			run.WriteString(pad)
		case kind == cellNull:
			flush()
			out.WriteString(nullSt.Render(txt))
			run.WriteString(pad)
		default:
			run.WriteString(txt + pad)
		}
	}
	if used := spanEnd(spans); used < width {
		run.WriteString(strings.Repeat(" ", width-used))
	}
	flush()
	return out.String()
}

func spanEnd(spans []span) int {
	if len(spans) == 0 {
		return 0
	}
	last := spans[len(spans)-1]
	return last.x + last.w
}

// padTo pads s with spaces to w display columns.
func padTo(s string, w int) string {
	if n := w - query.DisplayWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
