package editor

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// minTextWidth is the narrowest text area for which a line-number gutter is
// drawn.
const minTextWidth = 8

// gutterWidth is the width of the line-number column in a view w wide: the
// digits of the last line number plus a space, or 0 when too narrow.
func (m Model) gutterWidth(w int) int {
	g := len(strconv.Itoa(len(m.lines))) + 1
	g = max(g, 3)
	if w-g < minTextWidth {
		return 0
	}
	return g
}

// segment kinds, which decide how a run of clusters is styled on top of its
// highlight.
const (
	segText = iota
	segSel
	segCursor
)

// View draws the editor into a width x height box: height lines, each
// exactly width columns. Only the visible lines are looked at. Every piece of
// buffer text goes through query.EscapeControls, so control characters show
// as escapes and are never sent to the terminal.
func (m Model) View(width, height int, focused bool) string {
	if width < 1 || height < 1 {
		return ""
	}
	rowOff, colOff := m.scroll(width, height)
	gw := m.gutterWidth(width)
	tw := width - gw
	selA, selB, hasSel := m.selRange()
	dim := m.th.Styles.Dim
	selSt := lipgloss.NewStyle().Foreground(m.th.Bg).Background(m.th.Muted)
	curSt := m.th.Styles.CellCursor
	bottom := min(len(m.lines), rowOff+height)

	out := make([]string, 0, height)
	for i := rowOff; i < rowOff+height; i++ {
		if i >= len(m.lines) {
			out = append(out, strings.Repeat(" ", width))
			continue
		}
		var sb strings.Builder
		if gw > 0 {
			num := strconv.Itoa(i + 1)
			sb.WriteString(dim.Render(strings.Repeat(" ", gw-1-len(num)) + num + " "))
		}
		runs := m.hl.lineRuns(&m, rowOff, bottom, i)
		line := m.lines[i]
		// The buffer cell state of byte b of this line.
		kind := func(b int) int {
			p := pos{i, b}
			switch {
			case focused && p == m.cur:
				return segCursor
			case hasSel && !p.before(selA) && p.before(selB):
				return segSel
			}
			return segText
		}
		var seg strings.Builder
		segKind, segSt := -1, -1
		flush := func() {
			if seg.Len() == 0 {
				return
			}
			var st lipgloss.Style
			switch segKind {
			case segCursor:
				st = curSt
			case segSel:
				st = selSt
			default:
				st = m.hl.styles[0]
				if segSt > 0 {
					st = m.hl.styles[segSt]
				}
			}
			sb.WriteString(st.Render(seg.String()))
			seg.Reset()
		}
		add := func(text string, k, st int) {
			if k != segKind || (k == segText && st != segSt) {
				flush()
				segKind, segSt = k, st
			}
			seg.WriteString(text)
		}
		x, used, ri := 0, 0, 0
		eachCluster(line, func(start int, c string) bool {
			shown, w := clusterWidth(c)
			if x+w <= colOff {
				x += w
				return true
			}
			for ri < len(runs) && runs[ri].end <= start {
				ri++
			}
			st := 0
			if ri < len(runs) && runs[ri].start <= start {
				st = runs[ri].st
			}
			if x < colOff {
				// A wide cluster cut by the left edge: blank its visible part.
				shown, w = strings.Repeat(" ", x+w-colOff), x+w-colOff
			}
			if used+w > tw {
				return false
			}
			add(shown, kind(start), st)
			x += w
			used += w
			return true
		})
		// The cursor or the selected newline after the line's last character.
		if used < tw && x >= colOff {
			if k := kind(len(line)); k != segText || (hasSel && i < selB.row && !(pos{i, len(line)}).before(selA)) {
				if k == segText {
					k = segSel
				}
				add(" ", k, 0)
				used++
			}
		}
		flush()
		if used < tw {
			sb.WriteString(strings.Repeat(" ", tw-used))
		}
		out = append(out, sb.String())
	}
	return strings.Join(out, "\n")
}
