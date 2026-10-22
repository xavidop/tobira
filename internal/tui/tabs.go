package tui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/grid"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

const (
	// maxTabs is how many run tabs are kept; a new run past it closes the
	// oldest. The browse grid is tab 0 on top of these.
	maxTabs = 9
	// labelLen is how many characters of the statement a tab label shows.
	labelLen = 12
)

type tabState int

const (
	tabRunning tabState = iota
	tabDone
	tabFailed
	tabCancelled
)

// resultTab is one editor run: a grid fed by the collected result, or text
// (rows affected, a plan, an error).
type resultTab struct {
	stmt    string // as run
	conn    string
	done    chan struct{} // closed when the run's command has returned
	explain bool
	label   string // escaped, at most labelLen characters plus "…"
	state   tabState
	started time.Time

	gr        grid.Model
	isText    bool
	text      []string // escaped lines of a text tab
	off       int      // scroll offset of a text tab
	rows      int
	truncated bool
	took      time.Duration
	inTx      bool // ran inside the open transaction
	// A failed run's error as the driver returned it, and the redacted text
	// shown, for "fix with AI".
	err     error
	errText string

	// The collected result, which :export writes without running the
	// statement again.
	cols   []driver.Column
	result []driver.Row
	stats  driver.Stats

	// A similarity search's tab (similar.go): the collection it searched,
	// which S on its rows searches again; it is not recorded in the
	// history.
	simRef    *driver.CollectionRef
	noHistory bool

	// A live tail tab's watch (watch.go); nil on every other tab.
	watch *watchState
}

// tabLabel is the first labelLen characters of stmt, whitespace collapsed,
// escaped.
func tabLabel(stmt string) string { return cutLabel(stmt, labelLen) }

// cutLabel is the first n characters of s, whitespace collapsed, escaped,
// with "…" when there was more.
func cutLabel(stmt string, limit int) string {
	s := query.EscapeControls(strings.Join(strings.Fields(stmt), " "))
	n, end := 0, 0
	for end < len(s) && n < limit {
		c, _ := ansi.FirstGraphemeCluster(s[end:], ansi.GraphemeWidth)
		if c == "" {
			break
		}
		end += len(c)
		n++
	}
	if end < len(s) {
		return s[:end] + "…"
	}
	return s
}

// setText makes t a text tab showing text, one escaped line per line.
func (t *resultTab) setText(text string) {
	t.isText, t.off = true, 0
	t.text = t.text[:0]
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		t.text = append(t.text, query.EscapeControls(strings.ReplaceAll(l, "\t", "    ")))
	}
}

// addTab opens a run tab and makes it active, closing the oldest past
// maxTabs.
func (a *Model) addTab(conn, stmt string, explain bool) *resultTab {
	t := &resultTab{stmt: stmt, conn: conn, explain: explain, label: tabLabel(stmt), started: time.Now(),
		gr: grid.New(a.th, a.km)}
	t.setText("running…")
	if len(a.tabs) >= maxTabs {
		a.stopWatch(a.tabs[0], "")
		a.tabs = a.tabs[1:]
	}
	a.tabs = append(a.tabs, t)
	a.active = len(a.tabs)
	return t
}

// activeTab is the run tab shown, or nil on the browse tab.
func (a *Model) activeTab() *resultTab {
	if a.active < 1 || a.active > len(a.tabs) {
		return nil
	}
	return a.tabs[a.active-1]
}

// activeGrid is the grid the grid pane shows: the browse grid or the active
// run tab's.
func (a *Model) activeGrid() *grid.Model {
	if t := a.activeTab(); t != nil {
		return &t.gr
	}
	return &a.gr
}

// switchTab moves by d tabs, wrapping, the browse tab included.
func (a *Model) switchTab(d int) {
	n := len(a.tabs) + 1
	a.active = ((a.active+d)%n + n) % n
	a.dropDetail()
}

// closeTab closes the active run tab, cancelling its run.
func (a *Model) closeTab() tea.Cmd {
	t := a.activeTab()
	if t == nil {
		return a.flash("the browse tab cannot be closed", false)
	}
	var record tea.Cmd
	if a.running == t {
		record, _ = a.cancelRun() // recorded like a ctrl+c cancel
	}
	a.stopWatch(t, "")
	i := a.active - 1
	a.tabs = append(a.tabs[:i], a.tabs[i+1:]...)
	a.active = min(a.active, len(a.tabs))
	a.dropDetail()
	return record
}

// tabKey handles the tab keys of the grid pane. It reports whether k was one.
func (a *Model) tabKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case a.km.Matches(k, keymap.PrevTab):
		a.switchTab(-1)
	case a.km.Matches(k, keymap.NextTab):
		a.switchTab(1)
	case a.km.Matches(k, keymap.CloseTab):
		return a.closeTab(), true
	default:
		return nil, false
	}
	return nil, true
}

// textKey scrolls a text tab.
func (a *Model) textKey(t *resultTab, k tea.KeyPressMsg) {
	page := max(a.lay.grid.h-1, 1)
	switch {
	case a.km.Matches(k, keymap.Up):
		t.off--
	case a.km.Matches(k, keymap.Down):
		t.off++
	case a.km.Matches(k, keymap.PageUp):
		t.off -= page
	case a.km.Matches(k, keymap.PageDown):
		t.off += page
	case a.km.Matches(k, keymap.Top):
		t.off = 0
	case a.km.Matches(k, keymap.Bottom):
		t.off = len(t.text)
	}
	t.off = clampInt(t.off, 0, max(0, len(t.text)-a.lay.grid.h))
}

func clampInt(v, lo, hi int) int { return max(lo, min(v, hi)) }

// tabItem is one label of the tab bar.
type tabItem struct {
	idx  int // 0 browse, i run tab i
	text string
	x, w int
}

// tabItems lays out the tab bar labels from the left.
func (a *Model) tabItems() []tabItem {
	base := "browse"
	if a.cur.set {
		base = query.EscapeControls(a.cur.ref.Name)
	}
	items := []tabItem{{idx: 0, text: " " + base + " "}}
	for i, t := range a.tabs {
		label := strconv.Itoa(i+1) + ":" + t.label
		if t.watch != nil {
			label += watchSuffix(t.watch)
			items = append(items, tabItem{idx: i + 1, text: " " + label + " "})
			continue
		}
		switch t.state {
		case tabRunning:
			label += " …"
		case tabFailed:
			label += " ✗"
		case tabCancelled:
			label += " cancelled"
		}
		items = append(items, tabItem{idx: i + 1, text: " " + label + " "})
	}
	x := 0
	for i := range items {
		items[i].x = x
		items[i].w = query.DisplayWidth(items[i].text)
		x += items[i].w + 1
	}
	return items
}

// tabBar draws the tab bar w columns wide.
func (a *Model) tabBar(w int) string {
	st := a.th.Styles
	var sb strings.Builder
	for i, it := range a.tabItems() {
		if i > 0 {
			sb.WriteString(" ")
		}
		style := st.Dim
		switch {
		case it.idx == a.active:
			style = st.Header
		case it.idx > 0 && a.tabs[it.idx-1].state == tabFailed:
			style = st.Error
		}
		sb.WriteString(style.Render(it.text))
	}
	return ansi.Truncate(sb.String(), w, "…")
}

// tabAt returns the tab under column x of the tab bar.
func (a *Model) tabAt(x int) (int, bool) {
	for _, it := range a.tabItems() {
		if x >= it.x && x < it.x+it.w {
			return it.idx, true
		}
	}
	return 0, false
}

// tabTitle is the pane title of a run tab.
func (a *Model) tabTitle(t *resultTab) string {
	if t.watch != nil {
		return a.watchTitle(t)
	}
	title := query.EscapeControls(t.conn) + " ▸ " + t.label
	switch {
	case t.state == tabRunning:
		title += " · running…"
	case t.isText:
	case t.truncated:
		title += " · " + commas(t.rows) + " " + plural(t.rows, "row", "rows") + " · Truncated"
	default:
		title += " · " + commas(t.rows) + " " + plural(t.rows, "row", "rows")
	}
	return title
}

// tabView draws a run tab's content in a w x h box.
func (a *Model) tabView(t *resultTab, w, h int) string {
	if t.watch != nil {
		return a.watchView(t, w, h)
	}
	if !t.isText {
		return t.gr.View(w, h, a.focus == focusGrid)
	}
	st := a.th.Styles.Cell
	switch t.state {
	case tabFailed:
		st = a.th.Styles.Error
	case tabRunning, tabCancelled:
		st = a.th.Styles.Dim
	}
	text := t.text
	if t.state == tabFailed {
		// An error is read in full: wrap it rather than cut it.
		text = strings.Split(ansi.Wrap(strings.Join(text, "\n"), max(w, 1), ""), "\n")
	}
	end := min(len(text), t.off+h)
	lines := make([]string, 0, h)
	for _, l := range text[min(t.off, end):end] {
		lines = append(lines, st.Render(query.Truncate(l, w)))
	}
	return strings.Join(lines, "\n")
}
