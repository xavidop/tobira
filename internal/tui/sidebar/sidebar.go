// Package sidebar is the connection tree: connection, then namespace, then
// collection. Children load lazily when a node is expanded, every driver call
// runs as a tea.Cmd, and results for superseded requests are dropped.
package sidebar

import (
	"context"
	"errors"
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/async"
	"github.com/xavidop/tobira/internal/tui/click"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// OpenCollectionMsg asks the parent to open a collection in the grid. It is
// emitted when enter (or a second click) lands on a collection.
type OpenCollectionMsg struct {
	Conn string
	Ref  driver.CollectionRef
	// RowEstimate is the collection's row estimate, 0 when unknown.
	RowEstimate int64
	// Type is the collection type the driver listed (table, key_pattern,
	// measurement…), which picks a kind-specific view.
	Type driver.CollectionType
}

// StatusMsg is text for the status bar; Err marks it as an error. Text is
// already redacted and has control characters escaped.
type StatusMsg struct {
	Text string
	Err  bool
}

type kind int

const (
	kindConn kind = iota
	kindNamespace
	kindCollection
)

// node is one tree row. Nodes are shared by pointer between copies of Model,
// which is safe because a Model only ever lives on the Update goroutine.
type node struct {
	kind     kind
	key      string // unique; also the tracker key
	name     string
	conn     string
	ns       string // namespace name, for namespaces and collections
	parent   *node
	children []*node
	expanded bool
	loaded   bool
	loading  bool
	failed   bool
	// broken marks a config entry that failed validation: it cannot be
	// expanded and shows its error in the grid pane instead.
	broken   bool
	estimate int64 // collections: the driver's row estimate, 0 = unknown
	ctype    driver.CollectionType
}

func (n *node) expandable() bool { return n.kind != kindCollection && !n.broken }

// row is a node plus its depth, as laid out on screen.
type row struct {
	n     *node
	depth int
	// open is whether the node is drawn expanded: its own flag normally, or
	// "has a visible child" while a filter is active.
	open bool
}

type connLoaded struct {
	conn string
	nss  []driver.Namespace
}

type nsLoaded struct {
	conn, ns string
	cols     []driver.Collection
}

const (
	wheelStep       = 3
	defaultHeight   = 24
	minViewWidth    = 4
	minViewHeight   = 2
	filterPrompt    = "/ "
	defaultNSLabel  = "(default)"
	statusDotGlyph  = "●"
	statusDotPlain  = "*"
	collapsedMarker = "▸ "
	expandedMarker  = "▾ "
)

// Model is the sidebar component. The zero value is not usable; call New.
//
// The parent must forward every message it does not handle itself to Update:
// load results arrive as async.Result messages the parent cannot interpret,
// and dropping them leaves nodes loading forever. The parent must also call
// Resize with the pane size before paging (and click hit-testing of a
// scrolled tree) is accurate.
type Model struct {
	mgr *session.Manager
	th  theme.Theme
	km  keymap.Map

	roots    []*node
	index    map[string]*node
	trackers map[string]*async.Tracker

	cursor, offset int
	w, h           int
	filter         textinput.Model
	plain          bool // ASCII status dots

	seq    keymap.Seq           // first key of a pending two-key sequence (gg)
	clicks click.Tracker[*node] // the last click, for double-click detection
	now    func() time.Time
}

// New builds a sidebar with one collapsed node per connection in m.
func New(m *session.Manager, th theme.Theme, km keymap.Map) Model {
	in := textinput.New()
	in.Prompt = filterPrompt
	// A steady cursor: blinking would keep a timer command alive for as long
	// as the filter has focus.
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	s := Model{
		mgr: m, th: th, km: km,
		index:    map[string]*node{},
		trackers: map[string]*async.Tracker{},
		h:        defaultHeight,
		filter:   in,
	}
	for _, name := range m.Names() {
		n := &node{kind: kindConn, key: name, name: name, conn: name, broken: m.State(name).Broken}
		s.roots = append(s.roots, n)
		s.index[n.key] = n
	}
	return s
}

// Refresh rebuilds the connection list from the session manager after a
// Reload. A connection that is still configured keeps its node, so its
// expansion and loaded children survive (a connection the reload closed
// collapses and reloads on the next expand); removed connections disappear
// together with their loads in flight, and new ones are added collapsed. The
// cursor stays on the same node when it still exists.
func (m Model) Refresh() Model {
	var cur *node
	if rows := m.rows(); len(rows) > 0 {
		cur = rows[m.clamp(m.cursor, len(rows))].n
	}
	old := make(map[string]*node, len(m.roots))
	for _, n := range m.roots {
		old[n.conn] = n
	}
	names := m.mgr.Names()
	keep := make(map[string]bool, len(names))
	roots := make([]*node, 0, len(names))
	for _, name := range names {
		keep[name] = true
		n := old[name]
		if n == nil {
			n = &node{kind: kindConn, key: name, name: name, conn: name}
			m.index[n.key] = n
		}
		if n.broken = m.mgr.State(name).Broken; n.broken {
			n.expanded, n.loaded, n.loading, n.children = false, false, false, nil
		}
		roots = append(roots, n)
	}
	for key, n := range m.index {
		if keep[n.conn] {
			continue
		}
		if t := m.trackers[key]; t != nil {
			t.Cancel()
			delete(m.trackers, key)
		}
		delete(m.index, key)
	}
	// A connection the reload closed while its node was loading: abandon it.
	for _, n := range roots {
		if n.loading && m.mgr.State(n.conn).Status == session.Idle {
			m.collapse(n)
		}
	}
	m.roots = roots
	m.syncSessions()
	rows := m.rows()
	m.cursor = m.clamp(m.cursor, len(rows))
	if cur != nil {
		if i := indexOfNode(rows, cur); i >= 0 {
			m.cursor = i
		}
	}
	m.ensureVisible()
	return m
}

// Select moves the cursor to connection conn's node, if it is shown.
func (m Model) Select(conn string) Model {
	rows := m.rows()
	for i, r := range rows {
		if r.n.kind == kindConn && r.n.conn == conn {
			m.moveTo(i, len(rows))
			break
		}
	}
	return m
}

func indexOfNode(rows []row, n *node) int {
	for i, r := range rows {
		if r.n == n {
			return i
		}
	}
	return -1
}

// Resize tells the sidebar how big its pane is. Paging, scrolling and mouse
// hit-testing use this size, so the parent must call it (on every window or
// layout change) before relying on them. View clamps to the size it is given
// either way.
func (m Model) Resize(w, h int) Model {
	m.w, m.h = w, h
	m.ensureVisible()
	return m
}

// Loading reports whether any node is waiting for a driver call.
func (m Model) Loading() bool {
	for _, n := range m.index {
		if n.loading {
			return true
		}
	}
	return false
}

// CancelAll abandons every load in flight (a connection opening, a node
// expanding) and collapses those nodes so they can be retried. It reports
// whether there was one.
func (m Model) CancelAll() bool {
	was := false
	for _, n := range m.index {
		if n.loading {
			was = true
			m.collapse(n)
		}
	}
	return was
}

// FilterFocused reports whether the "/" filter input has focus, in which
// case every key belongs to it.
func (m Model) FilterFocused() bool { return m.filter.Focused() }

// SetTheme restyles the tree; the tree state is kept.
func (m Model) SetTheme(th theme.Theme) Model { m.th = th; return m }

// SetKeymap swaps the key bindings.
func (m Model) SetKeymap(km keymap.Map) Model { m.km = km; m.seq = keymap.Seq{}; return m }

// WithClock sets the time source used for double-clicks and key sequences
// (time.Now by default); tests inject their own.
func (m Model) WithClock(now func() time.Time) Model { m.now = now; return m }

// ClearSeq forgets a pending first key of a sequence (see grid.Model.ClearSeq).
func (m Model) ClearSeq() Model { m.seq = keymap.Seq{}; return m }

func (m Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// Sync forgets the children of connections the session has closed. Update
// does this after a load result; the parent calls it on its connecting tick,
// and Refresh does it after a reload. Those are the only moments a
// connection's status can change under the tree, so a key press or a mouse
// move does not poll the session manager.
func (m Model) Sync() Model {
	m.syncSessions()
	return m
}

// Selected returns the connection under the cursor and, when the cursor is on
// a collection, its ref.
func (m Model) Selected() (conn string, ref *driver.CollectionRef) {
	rows := m.rows()
	if len(rows) == 0 {
		return "", nil
	}
	n := rows[m.clamp(m.cursor, len(rows))].n
	if n.kind == kindCollection {
		r := driver.CollectionRef{Namespace: n.ns, Name: n.name}
		return n.conn, &r
	}
	return n.conn, nil
}

// SelectedConn returns the connection under the cursor when the cursor is on
// a connection node itself (not on a namespace or collection inside it).
func (m Model) SelectedConn() (string, bool) {
	rows := m.rows()
	if len(rows) == 0 {
		return "", false
	}
	n := rows[m.clamp(m.cursor, len(rows))].n
	return n.conn, n.kind == kindConn
}

// Loaded returns the namespaces and collections of conn that the tree has
// already loaded, in tree order. It never loads anything: the editor's
// completion uses it as a schema cache.
func (m Model) Loaded(conn string) (namespaces []string, collections []driver.CollectionRef) {
	n := m.index[conn]
	if n == nil || !n.loaded {
		return nil, nil
	}
	for _, ns := range n.children {
		namespaces = append(namespaces, ns.ns)
		if !ns.loaded {
			continue
		}
		for _, c := range ns.children {
			collections = append(collections, driver.CollectionRef{Namespace: c.ns, Name: c.name})
		}
	}
	return namespaces, collections
}

// SetPlain switches the status dot between "●" and the ASCII "*".
func (m Model) SetPlain(plain bool) Model { m.plain = plain; return m }

// Expand expands connection conn, opening it and loading its namespaces
// when they are not loaded yet. It returns nil for an unknown connection or
// when nothing needs loading. Used by `tobira connect`.
func (m *Model) Expand(conn string) tea.Cmd {
	n := m.index[conn]
	if n == nil {
		return nil
	}
	return m.expand(n)
}

// Update handles keys, mouse, and load results.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case async.Result[connLoaded]:
		m.syncSessions()
		return m, m.applyConn(msg)
	case async.Result[nsLoaded]:
		m.syncSessions()
		return m, m.applyNS(msg)
	case tea.KeyPressMsg:
		if m.filter.Focused() {
			return m.updateFilter(msg)
		}
		return m.updateKey(msg)
	case tea.MouseClickMsg:
		return m.click(tea.Mouse(msg))
	case tea.MouseWheelMsg:
		return m.wheel(tea.Mouse(msg)), nil
	}
	if m.filter.Focused() {
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		return m, cmd
	}
	return m, nil
}

// syncSessions forgets the children of connections the session has closed,
// so expanding them again reloads instead of showing stale data.
func (m Model) syncSessions() {
	for _, n := range m.roots {
		if n.loaded && !n.loading && m.mgr.State(n.conn).Status == session.Idle {
			n.loaded, n.expanded, n.children = false, false, nil
		}
	}
}

// updateFilter sends every key to the input except enter (keep) and esc
// (clear), so no bare-letter action can fire while typing.
func (m Model) updateFilter(k tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case m.km.Matches(k, keymap.Back):
		m.clearFilter()
		return m, nil
	case m.km.Matches(k, keymap.Open):
		m.filter.Blur()
		return m, nil
	}
	before := m.filter.Value()
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(k)
	if m.filter.Value() != before {
		m.cursor, m.offset = 0, 0
	}
	return m, cmd
}

// updateKey is the single place sidebar actions are bound, keyed by keymap
// action.
func (m Model) updateKey(k tea.KeyPressMsg) (Model, tea.Cmd) {
	rows := m.rows()
	cur := m.clamp(m.cursor, len(rows))
	var sel *node
	if len(rows) > 0 {
		sel = rows[cur].n
	}
	page := max(1, m.treeRows()-1)

	var seqAct keymap.Action
	var held bool
	m.seq, seqAct, held = m.km.Step(m.seq, k, m.clock())
	if held && seqAct == "" {
		return m, nil // the first key of a sequence
	}

	switch {
	case seqAct == keymap.Top:
		m.moveTo(0, len(rows))
	case m.km.Matches(k, keymap.SidebarFilter):
		return m, m.filter.Focus()
	case m.km.Matches(k, keymap.Up):
		m.moveTo(cur-1, len(rows))
	case m.km.Matches(k, keymap.Down):
		m.moveTo(cur+1, len(rows))
	case m.km.Matches(k, keymap.PageUp):
		m.moveTo(cur-page, len(rows))
	case m.km.Matches(k, keymap.PageDown):
		m.moveTo(cur+page, len(rows))
	case m.km.Matches(k, keymap.Top):
		m.moveTo(0, len(rows))
	case m.km.Matches(k, keymap.Bottom):
		m.moveTo(len(rows)-1, len(rows))
	case m.km.Matches(k, keymap.Right):
		if sel == nil {
			return m, nil
		}
		if sel.kind == kindCollection {
			return m, openCmd(sel)
		}
		if !rows[cur].open {
			return m, m.expand(sel)
		}
	case m.km.Matches(k, keymap.Left):
		if sel == nil {
			return m, nil
		}
		if sel.expandable() && rows[cur].open && m.filter.Value() == "" {
			m.collapse(sel)
		} else if sel.parent != nil {
			m.moveTo(indexOf(rows, sel.parent), len(rows))
		}
	case m.km.Matches(k, keymap.Open):
		if sel == nil {
			return m, nil
		}
		return m, m.activate(rows[cur])
	case m.km.Matches(k, keymap.Back):
		if m.filter.Value() != "" {
			m.clearFilter()
		}
	}
	return m, nil
}

// activate is enter: open a collection, or toggle a container. While a
// filter is active, containers drawn open because a descendant matches have
// nothing to toggle, so enter leaves them alone.
func (m Model) activate(r row) tea.Cmd {
	n := r.n
	switch {
	case n.kind == kindCollection:
		return openCmd(n)
	case n.broken:
		return nil // nothing to open or retry; the grid pane shows the error
	case n.kind == kindConn && m.mgr.State(n.conn).Status == session.Failed:
		// Enter on a failed connection retries the open.
		n.loaded = false
		return m.expand(n)
	case m.filter.Value() != "":
		if !n.loaded {
			return m.expand(n)
		}
		return nil
	case n.expanded:
		m.collapse(n)
		return nil
	}
	return m.expand(n)
}

func openCmd(n *node) tea.Cmd {
	msg := OpenCollectionMsg{Conn: n.conn, Ref: driver.CollectionRef{Namespace: n.ns, Name: n.name}, RowEstimate: n.estimate, Type: n.ctype}
	return func() tea.Msg { return msg }
}

// click selects the row under the pointer; a double-click (a second click on
// the same node within click.Window) is enter.
func (m Model) click(ms tea.Mouse) (Model, tea.Cmd) {
	if ms.Button != tea.MouseLeft {
		return m, nil
	}
	rows := m.rows()
	idx := m.offsetNow(len(rows)) + ms.Y - m.treeTop()
	if ms.Y < m.treeTop() || idx < 0 || idx >= len(rows) {
		return m, nil
	}
	var dbl bool
	m.clicks, dbl = m.clicks.Hit(rows[idx].n, m.clock())
	if dbl && idx == m.clamp(m.cursor, len(rows)) {
		return m, m.activate(rows[idx])
	}
	m.moveTo(idx, len(rows))
	return m, nil
}

// wheel scrolls the viewport and keeps the cursor inside it, so the view
// does not snap back to the cursor.
func (m Model) wheel(ms tea.Mouse) Model {
	n := len(m.rows())
	rows := m.treeRows()
	off := m.offsetNow(n)
	switch ms.Button {
	case tea.MouseWheelUp:
		off = max(0, off-wheelStep)
	case tea.MouseWheelDown:
		off = min(max(0, n-rows), off+wheelStep)
	default:
		return m
	}
	m.offset = off
	m.cursor = max(off, min(m.clamp(m.cursor, n), off+rows-1))
	m.cursor = m.clamp(m.cursor, n)
	return m
}

// expand marks n expanded and, when its children are not loaded, starts the
// load. Re-expanding a node whose load is in flight supersedes that request.
func (m *Model) expand(n *node) tea.Cmd {
	if !n.expandable() {
		return nil
	}
	n.expanded = true
	n.failed = false
	if n.loaded {
		return nil
	}
	tr := m.tracker(n.key)
	id, ctx := tr.Start(context.Background())
	n.loading = true
	mgr := m.mgr
	switch n.kind {
	case kindConn:
		name := n.conn
		return async.Do(id, ctx, func(ctx context.Context) (connLoaded, error) {
			out := connLoaded{conn: name}
			c, err := mgr.Open(ctx, name)
			if err != nil {
				return out, err
			}
			out.nss, err = c.Namespaces(ctx)
			return out, err
		})
	default: // namespace
		conn, ns := n.conn, n.ns
		return async.Do(id, ctx, func(ctx context.Context) (nsLoaded, error) {
			out := nsLoaded{conn: conn, ns: ns}
			c, err := mgr.Open(ctx, conn)
			if err != nil {
				return out, err
			}
			out.cols, err = c.Collections(ctx, driver.NamespaceRef{Name: ns})
			return out, err
		})
	}
}

// collapse hides n's children and abandons its load if one is running.
func (m *Model) collapse(n *node) {
	n.expanded = false
	if n.loading {
		m.tracker(n.key).Cancel()
		n.loading = false
	}
}

func (m *Model) tracker(key string) *async.Tracker {
	tr := m.trackers[key]
	if tr == nil {
		tr = &async.Tracker{}
		m.trackers[key] = tr
	}
	return tr
}

func (m Model) applyConn(r async.Result[connLoaded]) tea.Cmd {
	n := m.index[r.Val.conn]
	if n == nil || !m.tracker(n.key).Finish(r.ID) {
		return nil
	}
	n.loading = false
	if r.Err != nil {
		return m.fail(n, r.Err)
	}
	n.loaded = true
	n.children = n.children[:0]
	for _, ns := range r.Val.nss {
		c := &node{kind: kindNamespace, key: n.conn + "\x00" + ns.Name, name: nsLabel(ns.Name), conn: n.conn, ns: ns.Name, parent: n}
		n.children = append(n.children, c)
		m.index[c.key] = c
	}
	return nil
}

func (m Model) applyNS(r async.Result[nsLoaded]) tea.Cmd {
	n := m.index[r.Val.conn+"\x00"+r.Val.ns]
	if n == nil || !m.tracker(n.key).Finish(r.ID) {
		return nil
	}
	n.loading = false
	if r.Err != nil {
		return m.fail(n, r.Err)
	}
	n.loaded = true
	n.children = n.children[:0]
	for _, col := range r.Val.cols {
		c := &node{kind: kindCollection, key: n.key + "\x00" + col.Ref.Name, name: col.Ref.Name, conn: n.conn, ns: n.ns, parent: n, estimate: max(col.RowEstimate, 0), ctype: col.Type}
		n.children = append(n.children, c)
	}
	return nil
}

// fail marks n failed, collapses it so it can be retried, and reports the
// redacted error.
func (m Model) fail(n *node, err error) tea.Cmd {
	n.expanded = false
	if errors.Is(err, context.Canceled) {
		return nil
	}
	n.failed = true
	text := m.mgr.Redact(n.conn, err.Error())
	if st := m.mgr.State(n.conn); n.kind == kindConn && st.Status == session.Failed && st.Err != "" {
		text = st.Err
	}
	label := n.conn
	if n.kind != kindConn {
		label += "/" + n.name
	}
	msg := StatusMsg{Text: query.EscapeControls(label + ": " + text), Err: true}
	return func() tea.Msg { return msg }
}

func nsLabel(name string) string {
	if name == "" {
		return defaultNSLabel
	}
	return name
}

// rows lays out the visible nodes, honouring expansion and the filter.
func (m Model) rows() []row {
	var out []row
	f := strings.ToLower(m.filter.Value())
	var walk func(n *node, depth int)
	walk = func(n *node, depth int) {
		if f == "" {
			out = append(out, row{n, depth, n.expanded})
			if n.expanded {
				for _, c := range n.children {
					walk(c, depth+1)
				}
			}
			return
		}
		if !matches(n, f) {
			return
		}
		shown := false
		for _, c := range n.children {
			shown = shown || matches(c, f)
		}
		out = append(out, row{n, depth, shown})
		for _, c := range n.children {
			walk(c, depth+1)
		}
	}
	for _, r := range m.roots {
		walk(r, 0)
	}
	return out
}

// matches reports whether n or a loaded descendant contains f (lowercase).
// A name matches by its raw text or by the escaped text the tree shows, so
// what the user sees on screen can be typed into the filter.
func matches(n *node, f string) bool {
	if strings.Contains(strings.ToLower(n.name), f) ||
		strings.Contains(strings.ToLower(query.EscapeControls(n.name)), f) {
		return true
	}
	for _, c := range n.children {
		if matches(c, f) {
			return true
		}
	}
	return false
}

func indexOf(rows []row, n *node) int {
	for i, r := range rows {
		if r.n == n {
			return i
		}
	}
	return 0
}

func (m *Model) clearFilter() {
	m.filter.SetValue("")
	m.filter.Blur()
	m.cursor, m.offset = 0, 0
}

func (m Model) filterShown() bool { return m.filter.Focused() || m.filter.Value() != "" }

func (m Model) treeTop() int {
	if m.filterShown() {
		return 1
	}
	return 0
}

func (m Model) treeRows() int { return max(1, m.h-m.treeTop()) }

func (m Model) clamp(i, n int) int { return max(0, min(i, n-1)) }

func (m *Model) moveTo(i, n int) {
	m.cursor = m.clamp(i, n)
	m.ensureVisible()
}

func (m *Model) ensureVisible() {
	m.offset = scrollTo(m.offset, m.cursor, m.treeRows())
}

// scrollTo returns the offset closest to off that shows cursor in rows lines.
func scrollTo(off, cursor, rows int) int {
	if cursor < off {
		return max(0, cursor)
	}
	if cursor >= off+rows {
		return cursor - rows + 1
	}
	return max(0, off)
}

// offsetNow is the effective scroll offset for the current cursor.
func (m Model) offsetNow(n int) int {
	return scrollTo(m.offset, m.clamp(m.cursor, n), m.treeRows())
}

// View renders the tree into exactly height lines of width columns. Panes too
// small to be useful render blank.
func (m Model) View(width, height int, focused bool) string {
	if width < minViewWidth || height < minViewHeight {
		return blank(width, height)
	}
	m.w, m.h = width, height
	rows := m.rows()
	lines := make([]string, 0, height)
	if m.filterShown() {
		f := m.filter
		f.SetWidth(max(1, width-lipgloss.Width(filterPrompt)-1))
		lines = append(lines, lipgloss.NewStyle().Width(width).MaxWidth(width).Render(f.View()))
	}
	cur := m.clamp(m.cursor, len(rows))
	off := m.offsetNow(len(rows))
	for i := off; i < len(rows) && len(lines) < height; i++ {
		lines = append(lines, m.renderRow(rows[i], width, i == cur, focused))
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return strings.Join(lines, "\n")
}

func blank(w, h int) string {
	if w < 0 {
		w = 0
	}
	if h < 1 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat(strings.Repeat(" ", w)+"\n", h), "\n")
}

func (m Model) renderRow(r row, width int, selected, focused bool) string {
	base := m.th.Styles.Cell
	if selected {
		base = m.th.Styles.CellCursor
		if !focused {
			base = m.th.Styles.Header
		}
	}
	n := r.n
	indent := strings.Repeat("  ", r.depth)
	marker := "  "
	if n.expandable() {
		marker = collapsedMarker
		if r.open {
			marker = expandedMarker
		}
	}
	lead := indent + marker
	dotW := 0
	if n.kind == kindConn {
		dotW = 2
	}
	suffix := ""
	switch {
	case n.failed:
		suffix = " (error)"
	case n.loading:
		suffix = " …"
	}
	avail := width - query.DisplayWidth(lead) - dotW
	if avail < 1 {
		return pad(base, query.Truncate(lead, width), width)
	}
	if query.DisplayWidth(suffix) >= avail {
		suffix = ""
	}
	name := query.Truncate(query.EscapeControls(n.name), avail-query.DisplayWidth(suffix))

	var sb strings.Builder
	used := 0
	put := func(s string, st lipgloss.Style) {
		sb.WriteString(st.Render(s))
		used += query.DisplayWidth(s)
	}
	put(lead, base)
	nameStyle := base
	if n.kind == kindConn {
		glyph := statusDotGlyph
		if m.plain {
			glyph = statusDotPlain
		}
		put(glyph+" ", base.Foreground(m.statusColor(m.mgr.State(n.conn).Status)))
		if !selected {
			nameStyle = base.Foreground(m.nameColor(n.conn))
		}
	}
	put(name, nameStyle)
	switch {
	case n.failed:
		put(suffix, base.Foreground(m.th.Danger))
	case suffix != "":
		put(suffix, base.Foreground(m.th.Muted))
	}
	if used < width {
		sb.WriteString(base.Render(strings.Repeat(" ", width-used)))
	}
	return sb.String()
}

func pad(st lipgloss.Style, s string, width int) string {
	if w := query.DisplayWidth(s); w < width {
		s += strings.Repeat(" ", width-w)
	}
	return st.Render(s)
}

// statusColor is the colour of a connection's status dot.
func (m Model) statusColor(s session.Status) color.Color {
	switch s {
	case session.Connecting:
		return m.th.Warn
	case session.Ready:
		return m.th.OK
	case session.Failed:
		return m.th.Danger
	}
	return m.th.Muted
}

// nameColor tints a connection name by its configured colour, falling back
// to the theme foreground for names it does not know.
func (m Model) nameColor(conn string) color.Color {
	switch strings.ToLower(m.mgr.Color(conn)) {
	case "red":
		return m.th.Danger
	case "green":
		return m.th.OK
	case "yellow", "orange":
		return m.th.Warn
	case "cyan":
		return m.th.Accent
	case "blue":
		return lipgloss.Blue
	case "magenta":
		return lipgloss.Magenta
	}
	return m.th.Fg
}
