// Package detail is the full-pane row view: the row's columns as key/value
// pairs, with Object, List and JSON values as an expandable tree.
package detail

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

const (
	sep         = " │ "
	sepW        = 3 // display width of sep
	maxLabelCol = 24
)

// ClosedMsg is emitted on esc.
type ClosedMsg struct{}

// CopiedMsg reports a copy to the clipboard, for the status bar. What names
// the copied thing ("value" or "row as JSON").
type CopiedMsg struct{ What string }

// asyncThreshold is the size, in bytes, from which a value's tree is built in
// a command instead of on the update goroutine.
const asyncThreshold = 256 * 1024

// token tells the results of one Model's builds from another's.
type token struct{ _ byte } // not zero-sized, so every pointer is distinct

// BuiltMsg is the result of building a huge value's tree in the background.
// The parent forwards it to the detail it came from (Update drops it
// otherwise).
type BuiltMsg struct {
	tok  *token
	idx  int
	node *tnode
	err  error
}

// line is one visible tree row, or the "… N more" marker of a long container.
type line struct {
	n    *tnode // nil for a marker
	more int
	of   *tnode // the container a marker belongs to
}

// Model is the row detail. Its tree nodes are shared between copies of the
// Model, which is safe because a Model only lives on the Update goroutine.
type Model struct {
	th   theme.Theme
	km   keymap.Map
	cols []driver.Column
	row  driver.Row

	roots  []*tnode
	lines  []line
	labelW int

	cursor, off int
	onRender    func() // test hook: called once per rendered line
	w, h        int

	seq keymap.Seq // first key of a pending two-key sequence (gg)
	now func() time.Time

	// Huge values are built in Init's commands. tok identifies this detail,
	// pending lists the columns still loading and cancel stops the builds.
	tok     *token
	ctx     context.Context
	cancel  context.CancelFunc
	pending []int
}

// New builds the detail for row. Every container starts collapsed. A value
// over 256 KB is not built here: its row shows "loading…" until the command
// from Init delivers the tree.
func New(cols []driver.Column, row driver.Row, th theme.Theme, km keymap.Map) Model {
	m := Model{th: th, km: km, cols: cols, row: row, w: 60, h: 20, tok: &token{}}
	for i, c := range cols {
		v := driver.Null()
		if i < len(row) {
			v = row[i]
		}
		if isHuge(v) {
			m.roots = append(m.roots, placeholder(v, c.Name))
			m.pending = append(m.pending, i)
		} else {
			m.roots = append(m.roots, buildValue(v, c.Name, nil, 0, false))
		}
		m.labelW = max(m.labelW, query.DisplayWidth(boundedText(c.Name)))
	}
	if len(m.pending) > 0 {
		m.ctx, m.cancel = context.WithCancel(context.Background())
	}
	m.labelW = min(m.labelW, maxLabelCol)
	m.flatten()
	return m
}

// WithClock sets the time source used for key sequences (time.Now by
// default); tests inject their own.
func (m Model) WithClock(now func() time.Time) Model { m.now = now; return m }

func (m Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// Init starts building the trees of huge values, one command each. It returns
// nil when every value was built by New.
func (m Model) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.pending))
	for _, i := range m.pending {
		v, name, ctx, tok := m.row[i], m.cols[i].Name, m.ctx, m.tok
		cmds = append(cmds, func() tea.Msg {
			n, err := (&builder{ctx: ctx}).build(v, name, nil, 0, false)
			return BuiltMsg{tok: tok, idx: i, node: n, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// Cancel stops the builds Init started. It is safe to call at any time, any
// number of times.
func (m Model) Cancel() {
	if m.cancel != nil {
		m.cancel()
	}
}

// placeholder is the root shown for a value whose tree is still being built.
func placeholder(v driver.Value, label string) *tnode {
	return &tnode{label: label, dlabel: boundedText(label), val: v, fromVal: true, text: "loading…", loading: true}
}

// isHuge reports whether building v's tree is too big for the update
// goroutine. It stops counting once past the threshold.
// Only containers and JSON have a tree worth building off-thread; a long
// string or byte value is bounded text and stays synchronous.
func isHuge(v driver.Value) bool {
	switch v.Type() {
	case driver.TypeJSON, driver.TypeObject, driver.TypeList:
		budget := asyncThreshold
		return overBudget(v, &budget)
	}
	return false
}

func overBudget(v driver.Value, budget *int) bool {
	switch v.Type() {
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		*budget -= len(raw)
	case driver.TypeObject:
		obj, _ := v.AsObject()
		for _, m := range obj {
			*budget -= len(m.Key) + 8
			if overBudget(m.Value, budget) {
				return true
			}
		}
	case driver.TypeList:
		list, _ := v.AsList()
		for _, e := range list {
			*budget -= 8
			if overBudget(e, budget) {
				return true
			}
		}
	case driver.TypeString:
		s, _ := v.AsText()
		*budget -= len(s)
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		*budget -= len(b)
	default:
		*budget -= 8
	}
	return *budget < 0
}

// Resize sets the pane size; paging and scrolling use it.
func (m Model) Resize(w, h int) Model {
	m.w, m.h = w, h
	m.scroll()
	return m
}

func (m *Model) flatten() {
	m.lines = m.lines[:0:0]
	var add func(n *tnode)
	add = func(n *tnode) {
		m.lines = append(m.lines, line{n: n})
		if !n.container || !n.expanded {
			return
		}
		for i, c := range n.children {
			if i == maxShown {
				m.lines = append(m.lines, line{more: len(n.children) - maxShown + n.hidden, of: n})
				break
			}
			add(c)
		}
		if n.hidden > 0 && len(n.children) <= maxShown {
			m.lines = append(m.lines, line{more: n.hidden, of: n})
		}
	}
	for _, r := range m.roots {
		add(r)
	}
	m.cursor = min(max(m.cursor, 0), max(len(m.lines)-1, 0))
}

// scroll keeps the cursor inside the window.
func (m *Model) scroll() {
	h := max(m.h, 1)
	if m.cursor < m.off {
		m.off = m.cursor
	}
	if m.cursor >= m.off+h {
		m.off = m.cursor - h + 1
	}
	m.off = max(m.off, 0)
}

// Column is the index of the column whose value the cursor is on: the root
// of the selected node, or of the container a marker line belongs to.
func (m Model) Column() (int, bool) {
	if m.cursor < 0 || m.cursor >= len(m.lines) {
		return 0, false
	}
	n := m.lines[m.cursor].n
	if n == nil {
		n = m.lines[m.cursor].of
	}
	for n != nil && n.parent != nil {
		n = n.parent
	}
	for i, r := range m.roots {
		if r != nil && r == n {
			return i, true
		}
	}
	return 0, false
}

// Cursor is the index of the selected line.
func (m Model) Cursor() int { return m.cursor }

// WithCursor selects line i, clamped, and scrolls it into view: a rebuilt
// detail keeps its place.
func (m Model) WithCursor(i int) Model {
	m.cursor = min(max(i, 0), max(len(m.lines)-1, 0))
	m.scroll()
	return m
}

func (m Model) selected() *tnode {
	if m.cursor < 0 || m.cursor >= len(m.lines) {
		return nil
	}
	return m.lines[m.cursor].n
}

// wheelStep is how many lines one wheel notch moves the cursor.
const wheelStep = 3

// Update handles keys and the mouse wheel.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if b, ok := msg.(BuiltMsg); ok {
		if b.tok == m.tok && b.err == nil && b.node != nil && b.idx >= 0 && b.idx < len(m.roots) {
			m.roots[b.idx] = b.node
			m.flatten()
			m.scroll()
		}
		return m, nil
	}
	if w, ok := msg.(tea.MouseWheelMsg); ok {
		switch w.Button {
		case tea.MouseWheelUp:
			m.cursor -= wheelStep
		case tea.MouseWheelDown:
			m.cursor += wheelStep
		default:
			return m, nil
		}
		m.cursor = min(max(m.cursor, 0), max(len(m.lines)-1, 0))
		m.scroll()
		return m, nil
	}
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	page := max(m.h-1, 1)
	n := m.selected()
	var seqAct keymap.Action
	var held bool
	m.seq, seqAct, held = m.km.Step(m.seq, k, m.clock())
	if held && seqAct == "" {
		return m, nil // the first key of a sequence
	}
	switch {
	case seqAct == keymap.Top:
		m.cursor = 0
	case m.km.Matches(k, keymap.Back):
		return m, func() tea.Msg { return ClosedMsg{} }
	case m.km.Matches(k, keymap.Up):
		m.cursor--
	case m.km.Matches(k, keymap.Down):
		m.cursor++
	case m.km.Matches(k, keymap.PageUp):
		m.cursor -= page
	case m.km.Matches(k, keymap.PageDown):
		m.cursor += page
	case m.km.Matches(k, keymap.Top), m.km.Matches(k, keymap.Home):
		m.cursor = 0
	case m.km.Matches(k, keymap.Bottom), m.km.Matches(k, keymap.End):
		m.cursor = len(m.lines) - 1
	case m.km.Matches(k, keymap.Open), k.String() == "space":
		if n != nil && n.container {
			n.expanded = !n.expanded
			m.flatten()
		}
	case m.km.Matches(k, keymap.Right):
		switch {
		case n == nil || !n.container:
		case !n.expanded:
			n.expanded = true
			m.flatten()
		default:
			m.cursor++
		}
	case m.km.Matches(k, keymap.Left):
		switch {
		case n == nil:
		case n.container && n.expanded:
			n.expanded = false
			m.flatten()
		case n.parent != nil:
			m.selectNode(n.parent)
		}
	case m.km.Matches(k, keymap.CollapseAll):
		m.collapseAll()
	case m.km.Matches(k, keymap.ExpandAll):
		m.expandAll()
	case m.km.Matches(k, keymap.CopyCell):
		if n == nil {
			return m, nil
		}
		return m, copyCmd(n.copyText(), "value")
	case m.km.Matches(k, keymap.CopyRow):
		return m, copyCmd(m.rowJSON(), "row as JSON")
	default:
		return m, nil
	}
	m.cursor = min(max(m.cursor, 0), max(len(m.lines)-1, 0))
	m.scroll()
	return m, nil
}

func (m *Model) selectNode(n *tnode) {
	for i, l := range m.lines {
		if l.n == n {
			m.cursor = i
			return
		}
	}
}

func copyCmd(text, what string) tea.Cmd {
	return tea.Batch(tea.SetClipboard(text), func() tea.Msg { return CopiedMsg{What: what} })
}

// rowJSON is the row as one JSON object keyed by column name, in column
// order. A repeated name gets "_2", "_3"… so the keys stay unique.
func (m Model) rowJSON() string {
	var b bytes.Buffer
	used := map[string]bool{}
	b.WriteByte('{')
	for i, c := range m.cols {
		if i > 0 {
			b.WriteByte(',')
		}
		key := c.Name
		for n := 2; used[key]; n++ {
			key = c.Name + "_" + strconv.Itoa(n)
		}
		used[key] = true
		b.WriteString(quote(key))
		b.WriteByte(':')
		v := driver.Null()
		if i < len(m.row) {
			v = m.row[i]
		}
		out, err := v.MarshalJSON()
		if err != nil {
			out = []byte("null")
		}
		b.Write(out)
	}
	b.WriteByte('}')
	return b.String()
}

// View draws the detail into a width x height box.
func (m Model) View(width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	m.w, m.h = width, height
	m.scroll()
	labelW := min(m.labelW, max(width/3, 4))
	end := min(m.off+height, len(m.lines))
	lines := make([]string, 0, max(end-m.off, 0))
	for i := m.off; i < end; i++ {
		if m.onRender != nil {
			m.onRender()
		}
		lines = append(lines, m.render(m.lines[i], i == m.cursor, labelW, width))
	}
	// Only the window is rendered, so the viewport just frames it.
	vp := viewport.New(viewport.WithWidth(width), viewport.WithHeight(height))
	vp.SetContentLines(lines)
	return vp.View()
}

// render draws one line, exactly width columns wide.
func (m Model) render(l line, cursor bool, labelW, width int) string {
	st := m.th.Styles
	label, value := "", ""
	var valSt lipgloss.Style
	switch {
	case l.n == nil:
		value = strings.Repeat("  ", l.of.depth+1) + "  … " + strconv.Itoa(l.more) + " more"
		valSt = st.Dim
	case l.n.depth == 0:
		label = l.n.dlabel
		value, valSt = m.content(l.n, "")
	default:
		pre := strings.Repeat("  ", l.n.depth-1)
		value, valSt = m.content(l.n, pre)
	}
	label = padTo(query.Truncate(label, labelW), labelW)
	value = query.Truncate(value, max(width-labelW-sepW, 1))
	if cursor {
		return st.CellCursor.Render(padTo(query.Truncate(label+sep+value, width), width))
	}
	out := query.Truncate(label+sep, width)
	rest := width - query.DisplayWidth(out)
	prefix := st.Dim.Render(out)
	if rest <= 0 {
		return prefix
	}
	return prefix + valSt.Render(query.Truncate(value, rest))
}

// content is the value half of a line and its style. indent is the tree
// indentation for depth >= 1.
func (m Model) content(n *tnode, indent string) (string, lipgloss.Style) {
	st := m.th.Styles
	glyph := ""
	switch {
	case n.container && n.expanded:
		glyph = "▾ "
	case n.container:
		glyph = "▸ "
	case n.depth > 0:
		glyph = "  "
	}
	text, style := n.text, st.Cell
	switch {
	case n.container:
		text = n.summary()
	case n.loading, n.absent:
		style = st.Dim
	case n.null:
		style = st.Null
	case n.leaf == leafString:
		text = `"` + text + `"`
	}
	switch {
	case n.depth == 0:
		return glyph + text, style
	case n.bare:
		return indent + glyph + text, style
	case n.parent != nil && n.parent.alignW > 0:
		return indent + glyph + padTo(query.Truncate(n.dlabel, n.parent.alignW), n.parent.alignW) + "  " + text, style
	}
	return indent + glyph + n.dlabel + ": " + text, style
}

func padTo(s string, w int) string {
	if d := w - query.DisplayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// RowJSON is row as one JSON object keyed by column name, the text `Y` copies.
func RowJSON(cols []driver.Column, row driver.Row) string {
	return Model{cols: cols, row: row}.rowJSON()
}

// CellText is the text `y` copies for v: a scalar's text, compact JSON for
// objects, lists and JSON, and the empty string for NULL. It builds no tree.
func CellText(v driver.Value) string {
	if v.IsNull() {
		return ""
	}
	return valueText(v)
}
