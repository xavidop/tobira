package grid

import (
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
)

// Format renders cell col of row as text before the grid escapes and cuts
// it; ok == false leaves the cell to the default rendering. The parent sets
// one for a kind-specific view (a key-value collection's TTL column, for
// example). It is called for every visible cell, so it must be cheap and
// bounded on huge values.
type Format func(cols []driver.Column, row driver.Row, col int) (text string, ok bool)

// SetFormat sets (or, with nil, clears) the cell format and recomputes the
// column widths.
func (m *Model) SetFormat(f Format) {
	m.format = f
	m.recomputeWidths()
}

// SetHeaderMark sets (or, with nil, clears) a suffix drawn after a
// column's name in the header, such as a wide-column key badge; "" draws
// nothing. The mark is escaped like the name and counts in the column
// width.
func (m *Model) SetHeaderMark(f func(driver.Column) string) {
	m.mark = f
	if len(m.cols) > 0 {
		m.recomputeWidths()
	}
}

// headerMark is column col's header suffix with its leading space, or "".
func (m Model) headerMark(col int) string {
	if m.mark == nil || col < 0 || col >= len(m.cols) {
		return ""
	}
	if s := m.mark(m.cols[col]); s != "" {
		return " " + query.EscapeControls(s)
	}
	return ""
}

// SetHidden sets (or, with nil, clears) which columns the grid does not
// draw, such as a driver's marker columns. They stay in Columns and in the
// rows, so the row detail and copies still show them; the cursor skips
// them.
func (m *Model) SetHidden(f func(driver.Column) bool) {
	m.hidden = f
	m.skipHidden()
	m.ensureVisible()
}

// skipHidden moves the cursor off a hidden column: to the next drawn one,
// else the previous one.
func (m *Model) skipHidden() {
	m.col = m.visibleFrom(m.col, 1, m.visibleFrom(m.col, -1, m.col))
}

// hiddenCol reports whether column col is not drawn.
func (m Model) hiddenCol(col int) bool {
	return m.hidden != nil && col >= 0 && col < len(m.cols) && m.hidden(m.cols[col])
}

// visibleFrom returns the first drawn column from col, stepping by dir
// (1 or -1), or fallback when there is none in that direction.
func (m Model) visibleFrom(col, dir, fallback int) int {
	for ; col >= 0 && col < len(m.cols); col += dir {
		if !m.hiddenCol(col) {
			return col
		}
	}
	return fallback
}

// cellKind tells how a cell is drawn.
type cellKind int

const (
	cellPlain  cellKind = iota
	cellNull            // an explicit NULL: the italic NULL
	cellAbsent          // a field the row's page did not have: a dim dash
)

// absentText is what an absent field shows.
const absentText = "—"

// cellText is the one-line, escaped text of cell col of row, cut to at most
// w columns, and how it is drawn. A cell past the end of row is absent.
func (m Model) cellText(row driver.Row, col, w int) (string, cellKind) {
	if col >= len(row) {
		return query.Truncate(absentText, w), cellAbsent
	}
	v := row[col]
	if m.format != nil {
		if s, ok := m.format(m.cols, row, col); ok {
			return cutText(s, w), cellPlain
		}
	}
	if v.IsNull() {
		return query.Truncate("NULL", w), cellNull
	}
	switch v.Type() {
	case driver.TypeObject, driver.TypeList:
		return cutText(previewText(v, 4*w), w), cellPlain
	}
	return cutText(v.String(), w), cellPlain
}

// cellAt is cellText for grid row r (whose values are row), marking the
// fields its page did not have as absent.
func (m Model) cellAt(r int, row driver.Row, col, w int) (string, cellKind) {
	if m.absent(r, col) {
		return query.Truncate(absentText, w), cellAbsent
	}
	return m.cellText(row, col, w)
}

// CurrentAbsent flags, by column, the fields the row under the cursor did
// not have (nil when it had them all), for the row detail.
func (m Model) CurrentAbsent() []bool {
	row, ok := m.CurrentRow()
	if !ok {
		return nil
	}
	var out []bool
	for c := range m.cols {
		if c >= len(row) || m.absent(m.row, c) {
			if out == nil {
				out = make([]bool, len(m.cols))
			}
			out[c] = true
		}
	}
	return out
}

// absent reports whether column col of grid row r is a field its page did
// not have. Pinned rows never are.
func (m Model) absent(r, col int) bool {
	i := r - len(m.pinned)
	if i < 0 {
		return false
	}
	pr, ok := m.present[i/pageSize]
	return ok && (col >= len(pr) || !pr[col])
}

// cutText escapes s and cuts it to w columns. Only a prefix of 4w bytes is
// looked at: control bytes expand to at most four columns, so it always
// holds w columns; a longer s is marked as cut.
func cutText(s string, w int) string {
	cut := false
	if limit := 4 * w; len(s) > limit {
		for limit > 0 && !runeStart(s[limit]) {
			limit--
		}
		s, cut = s[:limit], true
	}
	s = query.EscapeControls(s)
	if cut {
		s += "…"
	}
	return query.Truncate(s, w)
}

// previewText is the compact one-line preview of an Object or List:
// {name: "Ada", tags: [2]}. Members show scalars as they are, strings
// quoted, nested lists by their length and nested objects as {…}. It stops
// once past budget bytes, so a huge value costs no more than a short one.
func previewText(v driver.Value, budget int) string {
	var b strings.Builder
	writePreview(&b, v, budget)
	return b.String()
}

func writePreview(b *strings.Builder, v driver.Value, budget int) {
	if obj, ok := v.AsObject(); ok {
		b.WriteByte('{')
		for i, m := range obj {
			if b.Len() > budget {
				b.WriteString("…")
				return
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(boundedPreview(m.Key, budget))
			b.WriteString(": ")
			writeMember(b, m.Value, budget)
		}
		b.WriteByte('}')
		return
	}
	list, _ := v.AsList()
	b.WriteByte('[')
	for i, e := range list {
		if b.Len() > budget {
			b.WriteString("…")
			return
		}
		if i > 0 {
			b.WriteString(", ")
		}
		writeMember(b, e, budget)
	}
	b.WriteByte(']')
}

// writeMember writes one member or element of a preview.
func writeMember(b *strings.Builder, v driver.Value, budget int) {
	switch v.Type() {
	case driver.TypeNull:
		b.WriteString("null")
	case driver.TypeString:
		s, _ := v.AsText()
		b.WriteString(`"` + boundedPreview(s, budget) + `"`)
	case driver.TypeObject:
		if obj, _ := v.AsObject(); len(obj) == 0 {
			b.WriteString("{}")
		} else {
			b.WriteString("{…}")
		}
	case driver.TypeList:
		if list, _ := v.AsList(); len(list) == 0 {
			b.WriteString("[]")
		} else {
			b.WriteString("[" + strconv.Itoa(len(list)) + "]")
		}
	default:
		b.WriteString(boundedPreview(v.String(), budget))
	}
}

// boundedPreview is at most budget bytes of s (on a rune boundary).
func boundedPreview(s string, budget int) string {
	if budget < 0 {
		budget = 0
	}
	if len(s) <= budget {
		return s
	}
	for budget > 0 && !runeStart(s[budget]) {
		budget--
	}
	return s[:budget]
}

// unionIndex merges page columns cols into the grid's columns, appending
// the ones it lacks, and returns for each page column its index in the
// union. A repeated name matches by occurrence: the second "id" of the page
// is the second "id" of the union.
func (m *Model) unionIndex(cols []driver.Column) []int {
	idx := make([]int, len(cols))
	seen := map[string]int{}
	for i, c := range cols {
		n := seen[c.Name]
		seen[c.Name] = n + 1
		idx[i] = -1
		k := 0
		for j, u := range m.cols {
			if u.Name != c.Name {
				continue
			}
			if k == n {
				idx[i] = j
				break
			}
			k++
		}
		if idx[i] < 0 {
			idx[i] = len(m.cols)
			m.cols = append(m.cols, c)
		}
	}
	return idx
}

// placePage lays a fetched page out in the grid's columns. The first page
// of a fresh load sets them; a later page with other columns grows them to
// the union (first-seen order), and its rows are spread onto the union with
// the fields it lacked recorded as absent.
func (m *Model) placePage(pg Page) []driver.Row {
	if len(m.cols) == 0 || (len(m.pages) == 0 && !sameColumns(m.cols, pg.Cols)) {
		m.SetColumns(pg.Cols)
		delete(m.present, pg.Index)
		return pg.Rows
	}
	if sameColumns(m.cols, pg.Cols) {
		delete(m.present, pg.Index)
		return pg.Rows
	}
	before := len(m.cols)
	idx := m.unionIndex(pg.Cols)
	if len(m.cols) != before {
		m.SetColumns(m.cols)
	}
	present := make([]bool, len(m.cols))
	for _, j := range idx {
		present[j] = true
	}
	m.present[pg.Index] = present
	rows := make([]driver.Row, len(pg.Rows))
	for r, src := range pg.Rows {
		row := make(driver.Row, len(m.cols))
		for i, v := range src {
			if i < len(idx) {
				row[idx[i]] = v
			}
		}
		rows[r] = row
	}
	// A column first seen here takes its width from this page's cells too.
	for i := before; i < len(m.cols); i++ {
		w := m.widths[i]
		for _, r := range rows {
			w = max(w, m.cellWidth(r, i))
		}
		m.seenW[m.cols[i].Name] = max(m.seenW[m.cols[i].Name], w)
	}
	m.recomputeWidths()
	return rows
}

// Preview is the compact one-line preview of an Object or List, built from
// at most about budget bytes: {name: "Ada", tags: [2]}. It is not escaped;
// a Format returning it is escaped by the grid.
func Preview(v driver.Value, budget int) string { return previewText(v, budget) }
