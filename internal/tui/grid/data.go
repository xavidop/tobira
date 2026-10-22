package grid

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/async"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

const (
	prefetchAhead = 50 // load the next page when the cursor is this close to the loaded end
	maxPages      = 10 // pages kept in memory (LRU)
)

// Page is one fetched page of rows.
type Page struct {
	Index    int
	Cols     []driver.Column
	Rows     []driver.Row
	Last     bool  // no page follows
	Estimate int64 // the collection's row estimate, 0 when unknown
}

// PageMsg is the result of a page fetch. Its ID is checked against the
// grid's live request, so a result for a source or sort the user has left
// is dropped.
type PageMsg = async.Result[Page]

// Source pages one collection through driver.Browser. It chains cursors: page
// N's cursor is page N-1's NextCursor, so fetching a page the grid has not
// reached walks the pages before it. Cursors are small and are kept for every
// sort and filter seen (keyed by a precise signature of them), which lets an
// evicted page be fetched again and keeps a fetch for an old sort from
// touching the cursors of the current one.
type Source struct {
	conn       driver.Conn
	driverName string
	ref        driver.CollectionRef
	ro         bool
	roFunc     func() bool // when set, asked at every Browse instead of ro
	estimate   int64
	redact     func(string) string

	maxCursors int // bound on the cursor map; 0 means defaultMaxCursors

	// schema is the collection's Describe result, read once in a command
	// when the collection is opened and set on the Update goroutine, which
	// is the only one that reads it.
	schema    *driver.CollectionSchema
	schemaErr error
	described bool

	mu      sync.Mutex // guards cursors only; never held across a Browse
	cursors map[cursorKey]string
}

// cursorKey names the cursor that fetches page of the browse with signature
// sig.
type cursorKey struct {
	sig  string
	page int
}

// defaultMaxCursors bounds the cursor map when a session cycles through many sorts
// and filters.
const defaultMaxCursors = 4096

// NewSource returns a Source for ref on conn. driverName is only used in the
// "not supported" message; ro is passed to BrowseOptions.ReadOnly.
func NewSource(conn driver.Conn, driverName string, ref driver.CollectionRef, ro bool) *Source {
	return &Source{conn: conn, driverName: driverName, ref: ref, ro: ro}
}

// WithEstimate sets the row estimate (Collection.RowEstimate; <= 0 = unknown)
// reported with every page.
func (s *Source) WithEstimate(n int64) *Source {
	s.estimate = max(n, 0)
	return s
}

// WithReadOnly makes every Browse ask f for its ReadOnly flag, so a
// runtime switch (:readonly) applies to the next page. f is called off the
// Update goroutine and must be safe for that.
func (s *Source) WithReadOnly(f func() bool) *Source {
	s.roFunc = f
	return s
}

// readOnly is the ReadOnly flag of the next Browse.
func (s *Source) readOnly() bool {
	if s.roFunc != nil {
		return s.roFunc()
	}
	return s.ro
}

// WithRedact sets the function that scrubs credentials from error text
// before the grid shows it.
func (s *Source) WithRedact(f func(string) string) *Source {
	s.redact = f
	return s
}

// SetSchema records the collection's Describe result (or its error).
func (s *Source) SetSchema(sch *driver.CollectionSchema, err error) {
	s.schema, s.schemaErr, s.described = sch, err, true
}

// Schema returns the collection's schema once Describe has succeeded.
func (s *Source) Schema() (*driver.CollectionSchema, bool) {
	return s.schema, s.described && s.schemaErr == nil && s.schema != nil
}

// SchemaErr is the error Describe returned, if any.
func (s *Source) SchemaErr() error { return s.schemaErr }

// Described reports whether Describe has returned, with or without error.
func (s *Source) Described() bool { return s.described }

// Conn is the connection the source browses.
func (s *Source) Conn() driver.Conn { return s.conn }

// Ref is the collection the source browses.
func (s *Source) Ref() driver.CollectionRef { return s.ref }

// Browser reports whether the connection can browse.
func (s *Source) Browser() bool {
	if s == nil || s.conn == nil {
		return false
	}
	_, ok := driver.As[driver.Browser](s.conn)
	return ok
}

// Fetch returns page (0-based, pageSize rows). Pages between the last known
// cursor and page are fetched and dropped. It is safe to call concurrently;
// the lock covers only the cursor map, so a slow Browse never blocks a fetch
// for another sort.
func (s *Source) Fetch(ctx context.Context, page int, sort []driver.SortKey, filter driver.Filter) (Page, error) {
	b, ok := driver.As[driver.Browser](s.conn)
	if !ok {
		return Page{}, fmt.Errorf("browsing is not supported by %s", s.driverName)
	}
	sig := signature(sort, filter)
	start, cursor := s.resume(sig, page)
	for p := start; ; p++ {
		rs, err := b.Browse(ctx, s.ref, driver.BrowseOptions{
			Filter: filter, Sort: sort, Limit: pageSize, Cursor: cursor, ReadOnly: s.readOnly(),
		})
		if err != nil {
			return Page{}, err
		}
		cols := rs.Columns()
		rows, st, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			return Page{}, err
		}
		if st.NextCursor != "" {
			s.remember(cursorKey{sig, p + 1}, st.NextCursor)
		}
		if p == page {
			return Page{
				Index: page, Cols: cols, Rows: rows, Last: st.NextCursor == "",
				Estimate: s.estimate,
			}, nil
		}
		if st.NextCursor == "" {
			return Page{}, fmt.Errorf("page %d is past the end of %s", page, s.ref.Name)
		}
		cursor = st.NextCursor
	}
}

// resume finds the nearest page at or before page whose cursor is known and
// returns it with that cursor. Page 0 always is (its cursor is empty).
func (s *Source) resume(sig string, page int) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := page; p > 0; p-- {
		if c, ok := s.cursors[cursorKey{sig, p}]; ok {
			return p, c
		}
	}
	return 0, ""
}

func (s *Source) remember(k cursorKey, cursor string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = map[cursorKey]string{}
	}
	limit := cmp.Or(s.maxCursors, defaultMaxCursors)
	if len(s.cursors) >= limit {
		// Make room by forgetting the chains of other sorts and filters, never
		// the chain being walked. If that chain alone fills the map the new
		// cursor is simply not kept: a later fetch walks a little further.
		for old := range s.cursors {
			if old.sig != k.sig {
				delete(s.cursors, old)
			}
		}
		if _, replacing := s.cursors[k]; !replacing && len(s.cursors) >= limit {
			return
		}
	}
	s.cursors[k] = cursor
}

// signature encodes sort and filter exactly: values go in with their type and
// their JSON form, so Int(1) and String("1") differ even though both print
// as 1.
func signature(sort []driver.SortKey, filter driver.Filter) string {
	var sb strings.Builder
	for _, k := range sort {
		fmt.Fprintf(&sb, "s%q:%t;", k.Field, k.Desc)
	}
	fmt.Fprintf(&sb, "|r%q", filter.Raw)
	for _, c := range filter.Conditions {
		j, _ := c.Value.MarshalJSON() // never fails
		fmt.Fprintf(&sb, "|c%q:%s:%s:%s", c.Field, c.Op, c.Value.Type(), j)
	}
	return sb.String()
}

// fail renders err for the grid: redacted, then made one safe line.
func (s *Source) fail(err error) string {
	text := err.Error()
	if s.redact != nil {
		text = s.redact(text)
	}
	return query.EscapeControls(text)
}

// dataState is the paging half of Model. Maps and pointers are shared
// between copies of the Model, scalars are not.
type dataState struct {
	src     *Source
	tracker *async.Tracker // one live page load for the current source
	sort    []driver.SortKey
	filter  driver.Filter

	static   bool // the rows came from OpenStatic
	loading  bool
	paused   bool // CancelLoad stopped a running load; Resume restarts it
	inflight int  // page being fetched while loading, or last fetched
	errPage  int  // page whose fetch failed; not retried automatically
	msg      string
	msgErr   bool
	took     time.Duration

	tick  uint64
	stamp map[int]uint64 // page index -> last time it was on screen

	want   int // the loaded row Refresh puts the cursor back on; -1 when none
	placed int // where restoreCursor last put the cursor
}

func newDataState() dataState {
	return dataState{tracker: &async.Tracker{}, stamp: map[int]uint64{}, errPage: -1, want: -1}
}

// Status is what the status bar shows about the data.
type Status struct {
	Loaded  int // rows known to exist so far
	Total   int // exact total, or an estimate (0 = unknown)
	Exact   bool
	Loading bool
	Took    time.Duration // of the last page load
	Sort    []driver.SortKey
}

// Status returns the loading state and counts.
func (m Model) Status() Status {
	return Status{Loaded: m.known, Total: m.total, Exact: m.exact, Loading: m.data.loading, Took: m.data.took, Sort: m.data.sort}
}

// Filter is the filter the grid browses with.
func (m Model) Filter() driver.Filter { return m.data.filter }

// FailText returns the redacted, escaped text of r's error when r is the
// grid's live request and failed for a reason other than cancellation. A
// stale result, or a success, yields ok == false, so the parent never shows
// an error for a source the user has left.
func (m Model) FailText(r PageMsg) (text string, ok bool) {
	if r.Err == nil || errors.Is(r.Err, context.Canceled) || !m.data.tracker.Current(r.ID) || m.data.src == nil {
		return "", false
	}
	return m.data.src.fail(r.Err), true
}

// CachedPages is how many pages are held in memory.
func (m Model) CachedPages() int { return len(m.pages) }

// Open makes src the grid's source: it resets everything and fetches page 0.
// A load still running for the previous source is cancelled.
func (m *Model) Open(src *Source) tea.Cmd {
	m.data.tracker.Cancel()
	m.data.src, m.data.static = src, false
	m.data.sort, m.data.filter = nil, driver.Filter{}
	m.cols, m.widths, m.manual = nil, nil, map[string]int{}
	m.seenW = map[string]int{}
	m.col = 0
	m.clearRows()
	if src == nil || !src.Browser() {
		name := ""
		if src != nil {
			name = src.driverName
		}
		m.data.msg, m.data.msgErr = fmt.Sprintf("browsing not supported by %s; open the editor with ctrl+e", query.EscapeControls(name)), false
		return nil
	}
	return m.fetch(0)
}

// OpenStatic shows an already-collected result, such as an editor run: every
// row is in memory, nothing is ever fetched, and sort and filter do nothing
// (the parent decides what they mean for a static result). A load still
// running for a previous source is cancelled.
func (m *Model) OpenStatic(cols []driver.Column, rows []driver.Row) {
	m.data.tracker.Cancel()
	m.data.src = nil
	m.data.sort, m.data.filter = nil, driver.Filter{}
	m.manual = map[string]int{}
	m.seenW = map[string]int{}
	m.col = 0
	m.clearRows()
	m.data.static = true
	m.SetColumns(cols)
	m.SetRows(0, rows)
	m.SetTotal(len(rows), true)
	if len(rows) == 0 {
		m.data.msg = "no rows"
	}
	m.settle()
}

// Static reports whether the grid shows a result given to OpenStatic.
func (m Model) Static() bool { return m.data.static }

// CancelLoad abandons the running page load, if any, and reports whether it
// was a foreground load: page 0, or a page the viewport shows. A background
// prefetch is cancelled silently (false), so ctrl+c does not count it as
// work the user was waiting for.
func (m *Model) CancelLoad() bool {
	was := m.data.loading && m.foreground(m.data.inflight)
	m.data.paused = m.data.paused || m.data.loading
	m.data.tracker.Cancel()
	m.data.loading = false
	return was
}

// Resume fetches again the page whose load CancelLoad stopped, and keeps
// everything else: sort, filter, column widths, loaded rows and the scroll
// position. It returns nil when no load was cancelled.
func (m *Model) Resume() tea.Cmd {
	if !m.data.paused || !m.ready() {
		return nil
	}
	return m.fetch(m.data.inflight)
}

// Failed reports whether the page the cursor needs failed to load, so that
// there is nothing under the cursor to show and Retry can try again.
func (m Model) Failed() bool {
	if m.data.errPage < 0 || m.data.loading {
		return false
	}
	_, ok := m.CurrentRow()
	return !ok
}

// Retry fetches the failed page again, keeping sort, filter and widths. It
// returns nil when nothing failed or a load is already running.
func (m *Model) Retry() tea.Cmd {
	if !m.Failed() || !m.ready() {
		return nil
	}
	m.data.msg = ""
	return m.fetch(m.data.errPage)
}

// foreground reports whether page p is needed now: it is the first page or
// overlaps the rows in view.
func (m Model) foreground(p int) bool {
	if p == 0 {
		return true
	}
	first, last := p*pageSize, (p+1)*pageSize
	off := m.rowOff - len(m.pinned)
	return first < off+m.bodyRows() && last > off
}

// Loading reports whether a page is being fetched.
func (m Model) Loading() bool { return m.data.loading }

// SetFilter applies f and refetches from page 0.
func (m *Model) SetFilter(f driver.Filter) tea.Cmd {
	if !m.ready() {
		return nil
	}
	m.data.filter = f
	return m.restart()
}

func (m Model) ready() bool { return m.data.src != nil && m.data.src.Browser() }

// restart drops the loaded rows and fetches page 0 again for the current sort
// and filter. The cursor keeps its column.
func (m *Model) restart() tea.Cmd {
	m.clearRows()
	return m.fetch(0)
}

func (m *Model) clearRows() {
	m.pages = map[int][]driver.Row{}
	m.present = map[int][]bool{}
	m.data.stamp = map[int]uint64{}
	m.known, m.total, m.exact, m.maxPage = 0, 0, false, -1
	m.row, m.rowOff = 0, 0
	m.data.msg, m.data.errPage, m.data.took = "", -1, 0
	m.data.loading, m.data.paused = false, false
	m.data.want = -1
}

// fetch starts loading page p, cancelling any load before it.
func (m *Model) fetch(p int) tea.Cmd {
	id, ctx := m.data.tracker.Start(context.Background())
	m.data.loading, m.data.paused, m.data.inflight = true, false, p
	src, sort, filter := m.data.src, m.data.sort, m.data.filter
	return async.Do(id, ctx, func(ctx context.Context) (Page, error) {
		return src.Fetch(ctx, p, sort, filter)
	})
}

// dataKey handles the keys that act on the data (sort).
func (m Model) dataKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if !m.km.Matches(msg, keymap.Sort) || !m.ready() || m.col >= len(m.cols) {
		return m, nil
	}
	field := m.cols[m.col].Name
	switch s := m.data.sort; {
	case len(s) == 0 || s[0].Field != field:
		m.data.sort = []driver.SortKey{{Field: field}}
	case !s[0].Desc:
		m.data.sort = []driver.SortKey{{Field: field, Desc: true}}
	default:
		m.data.sort = nil
	}
	cmd := m.restart()
	return m, cmd
}

// applyPage stores a fetched page, or records why it failed.
func (m Model) applyPage(r PageMsg) (Model, tea.Cmd) {
	if !m.data.tracker.Finish(r.ID) {
		return m, nil
	}
	m.data.loading = false
	if r.Err != nil {
		m.data.msg, m.data.msgErr, m.data.errPage = m.data.src.fail(r.Err), true, m.data.inflight
		return m, nil
	}
	pg := r.Val
	// A page that is neither the last nor full would make the grid believe
	// rows exist where none do, or loop asking for the same page.
	if !pg.Last && len(pg.Rows) < pageSize {
		what := "a short page"
		if len(pg.Rows) == 0 {
			what = "an empty page"
		}
		m.data.msg, m.data.msgErr, m.data.errPage = "driver returned "+what+" before the end", true, pg.Index
		return m, nil
	}
	m.data.msg, m.data.errPage = "", -1
	m.data.took = r.Took
	m.SetRows(pg.Index*pageSize, m.placePage(pg))
	switch {
	case pg.Last:
		m.SetTotal(pg.Index*pageSize+len(pg.Rows), true)
	case pg.Estimate > 0:
		m.SetTotal(max(int(pg.Estimate), m.known), false)
	}
	m.data.tick++
	m.data.stamp[pg.Index] = m.data.tick
	m.restoreCursor()
	m.settle()
	m.evict()
	cmd := m.next()
	return m, cmd
}

func sameColumns(a, b []driver.Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// next decides which page to load now, if any: a visible one that is missing
// first, then the page after the loaded end when the cursor is close to it.
func (m *Model) next() tea.Cmd {
	if !m.ready() || m.data.loading {
		return nil
	}
	if p := m.wantedPage(); p >= 0 {
		return m.fetch(p)
	}
	return nil
}

func (m Model) wantedPage() int {
	from, to, ok := m.NeedRows()
	for ok && from < to {
		p := from / pageSize
		// A page with some rows is not asked for again, so a short page from
		// the driver cannot make the grid loop.
		if len(m.pages[p]) == 0 && p != m.data.errPage {
			return p
		}
		from = (p + 1) * pageSize
	}
	if !m.exact && m.known > 0 && m.row-len(m.pinned)+prefetchAhead >= m.known {
		if p := m.maxPage + 1; p != m.data.errPage {
			return p
		}
	}
	return -1
}

// touchVisible marks the pages on screen as most recently used.
func (m *Model) touchVisible() {
	m.data.tick++
	off := m.dataRow(m.rowOff)
	for p := off / pageSize; p <= (off+m.bodyRows()-1)/pageSize; p++ {
		m.data.stamp[p] = m.data.tick
	}
	m.data.stamp[m.dataRow(m.row)/pageSize] = m.data.tick
}

// evict drops the least recently used pages beyond maxPages, never one that
// is on screen.
func (m *Model) evict() {
	visible := map[int]bool{m.dataRow(m.row) / pageSize: true}
	off := m.dataRow(m.rowOff)
	for p := off / pageSize; p <= (off+m.bodyRows()-1)/pageSize; p++ {
		visible[p] = true
	}
	for len(m.pages) > maxPages {
		victim, best := -1, ^uint64(0)
		for p := range m.pages {
			if !visible[p] && m.data.stamp[p] < best {
				victim, best = p, m.data.stamp[p]
			}
		}
		if victim < 0 {
			return
		}
		delete(m.pages, victim)
		delete(m.present, victim)
		delete(m.data.stamp, victim)
	}
}

// sortMark is the header suffix for a sorted column.
func (m Model) sortMark(col int) string {
	if s := m.data.sort; len(s) > 0 && col < len(m.cols) && s[0].Field == m.cols[col].Name {
		if s[0].Desc {
			return " ▼"
		}
		return " ▲"
	}
	return ""
}

// overlayStatus draws the grid's message (an error, "not supported" or
// "loading") over a blank body line, leaving rows and header usable.
func (m Model) overlayStatus(lines []string, width, known int) {
	text, st := m.data.msg, m.dim
	switch {
	case text != "":
		if m.data.msgErr {
			st = m.errSt
		}
	case m.data.loading && known == 0:
		text = "loading…"
	default:
		return
	}
	line := st.Render(padTo(query.Truncate(text, width), width))
	if known == 0 && len(lines) > 1 {
		lines[1] = line
	} else {
		lines[len(lines)-1] = line
	}
}
