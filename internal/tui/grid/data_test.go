package grid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

var peopleRef = driver.CollectionRef{Namespace: "main", Name: drivertest.PeopleCollection()}

func openConn(t *testing.T, f *drivertest.Fake, ro bool) driver.Conn {
	t.Helper()
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{ReadOnly: ro})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// bigFake has one collection of n rows with a single int column "n".
func bigFake(n int) (*drivertest.Fake, driver.CollectionRef) {
	f := drivertest.NewFake()
	rows := make([]driver.Row, n)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i))}
	}
	f.AddCollection("main", "big", []driver.Column{{Name: "n", Type: driver.TypeInt}}, rows)
	return f, driver.CollectionRef{Namespace: "main", Name: "big"}
}

// harness runs the grid's commands the way Bubble Tea would.
type harness struct {
	t *testing.T
	m Model
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, m: New(theme.Dark(), keymap.Vim()).Resize(60, 12)}
}

func (h *harness) exec(cmd tea.Cmd) {
	h.t.Helper()
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				h.exec(c)
			}
			return
		}
		h.send(msg)
	case <-time.After(10 * time.Second):
		h.t.Fatal("command did not finish")
	}
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	var cmd tea.Cmd
	h.m, cmd = h.m.Update(msg)
	h.exec(cmd)
}

func (h *harness) open(src *Source) {
	h.t.Helper()
	h.exec(h.m.Open(src))
}

func (h *harness) keys(ks ...string) {
	h.t.Helper()
	for _, k := range ks {
		var msg tea.KeyPressMsg
		switch k {
		case "pgdown":
			msg = tea.KeyPressMsg{Code: tea.KeyPgDown}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		h.send(msg)
	}
}

func browseCalls(f *drivertest.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Op == "browse" {
			n++
		}
	}
	return n
}

func TestOpenFetchesFirstPage(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	h := newHarness(t)
	src := NewSource(openConn(t, f, false), "fake", peopleRef, false).WithEstimate(19)
	h.open(src)
	st := h.m.Status()
	if st.Loaded != len(drivertest.People.Rows) || !st.Exact || st.Loading {
		t.Fatalf("status = %+v", st)
	}
	if browseCalls(f) != 1 {
		t.Fatalf("browse calls = %d, want 1", browseCalls(f))
	}
	row, ok := h.m.CurrentRow()
	if !ok || len(row) != len(drivertest.People.Columns) {
		t.Fatalf("current row = %v %v", row, ok)
	}
	if v := ansi.Strip(h.m.View(100, 8, true)); !strings.Contains(v, "Ada Lovelace") || !strings.Contains(v, "email") {
		t.Fatalf("view lacks data:\n%s", v)
	}
}

func TestPrefetchNearEnd(t *testing.T) {
	f, ref := bigFake(1000)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false).WithEstimate(1000))
	if got := h.m.Status(); got.Loaded != pageSize || got.Exact || got.Total != 1000 {
		t.Fatalf("after open: %+v", got)
	}
	// Far from the end: nothing more is fetched.
	h.keys("j", "j", "j")
	if browseCalls(f) != 1 {
		t.Fatalf("browse calls = %d, want 1", browseCalls(f))
	}
	// Within 50 rows of the loaded end: the next page is prefetched.
	for h.m.row < 150 {
		h.keys("pgdown")
	}
	if got := h.m.Status().Loaded; got != 2*pageSize {
		t.Fatalf("loaded = %d, want %d", got, 2*pageSize)
	}
	if browseCalls(f) != 2 {
		t.Fatalf("browse calls = %d, want 2", browseCalls(f))
	}
}

func TestLastPageGivesExactTotal(t *testing.T) {
	f, ref := bigFake(450)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false).WithEstimate(9999))
	for i := 0; i < 5 && !h.m.Status().Exact; i++ {
		h.keys("G")
	}
	if st := h.m.Status(); !st.Exact || st.Total != 450 || st.Loaded != 450 {
		t.Fatalf("status = %+v", st)
	}
	h.keys("G")
	if r, _ := h.m.Cursor(); r != 449 {
		t.Fatalf("cursor row = %d, want 449", r)
	}
}

func TestPagesEvicted(t *testing.T) {
	const n = 50_000
	f, ref := bigFake(n)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false))
	peak := 0
	for h.m.row < n-1 {
		before := h.m.row
		h.keys("G")
		peak = max(peak, h.m.CachedPages())
		if h.m.row == before {
			t.Fatalf("stuck at row %d", before)
		}
	}
	if peak > maxPages {
		t.Fatalf("held %d pages, want at most %d", peak, maxPages)
	}
	if got := h.m.Status(); !got.Exact || got.Total != n {
		t.Fatalf("status = %+v", got)
	}
	if _, ok := h.m.CurrentRow(); !ok {
		t.Fatal("the row under the cursor must stay loaded")
	}
	// Scrolling back re-fetches an evicted page through its stored cursor.
	before := browseCalls(f)
	h.keys("g", "g")
	if r, _ := h.m.Cursor(); r != 0 {
		t.Fatalf("gg should go to the top, at %d", r)
	}
	if _, ok := h.m.CurrentRow(); !ok {
		t.Fatal("page 0 should be fetched again")
	}
	if browseCalls(f) <= before {
		t.Fatal("expected a re-fetch of the evicted first page")
	}
	if v := ansi.Strip(h.m.View(60, 12, true)); !strings.Contains(v, "\n0 ") {
		t.Fatalf("row 0 should show:\n%s", v)
	}
}

func TestSortCycles(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	h := newHarness(t)
	h.m = h.m.Resize(100, 8)
	h.open(NewSource(openConn(t, f, false), "fake", peopleRef, false))
	h.keys("l", "l", "l") // age
	firstAge := func() string {
		r, _ := h.m.rowAt(0)
		return r[3].String()
	}
	natural := firstAge()
	h.keys("s")
	if s := h.m.Status().Sort; len(s) != 1 || s[0].Field != "age" || s[0].Desc {
		t.Fatalf("after 1st s: %v", s)
	}
	asc := firstAge()
	if v := ansi.Strip(h.m.View(100, 8, true)); !strings.Contains(v, "age ▲") {
		t.Fatalf("header should mark ascending sort:\n%s", v)
	}
	h.keys("s")
	if s := h.m.Status().Sort; len(s) != 1 || !s[0].Desc {
		t.Fatalf("after 2nd s: %v", s)
	}
	desc := firstAge()
	if asc == desc {
		t.Fatalf("asc and desc show the same first row %q", asc)
	}
	h.keys("s")
	if s := h.m.Status().Sort; len(s) != 0 {
		t.Fatalf("after 3rd s: %v", s)
	}
	if firstAge() != natural {
		t.Fatalf("sort off should restore natural order: %q vs %q", firstAge(), natural)
	}
	if got := browseCalls(f); got != 4 {
		t.Fatalf("browse calls = %d, want 4 (open + 3 sorts)", got)
	}
	if _, c := h.m.Cursor(); c != 3 {
		t.Fatalf("cursor column = %d, want 3 kept", c)
	}
}

// spyConn records the BrowseOptions of every Browse.
type spyConn struct {
	driver.Conn
	mu   sync.Mutex
	opts []driver.BrowseOptions
}

func (s *spyConn) Browse(ctx context.Context, ref driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
	s.mu.Lock()
	s.opts = append(s.opts, o)
	s.mu.Unlock()
	b, _ := driver.As[driver.Browser](s.Conn)
	return b.Browse(ctx, ref, o)
}

func TestReadOnlyPassesFlag(t *testing.T) {
	for _, ro := range []bool{true, false} {
		t.Run(fmt.Sprint("ro=", ro), func(t *testing.T) {
			f := drivertest.NewFake(drivertest.WithPeople())
			spy := &spyConn{Conn: openConn(t, f, ro)}
			h := newHarness(t)
			h.open(NewSource(spy, "fake", peopleRef, ro))
			h.exec(h.m.SetFilter(driver.Filter{Raw: "x"}))
			if len(spy.opts) == 0 {
				t.Fatal("no Browse call")
			}
			for _, o := range spy.opts {
				if o.ReadOnly != ro || o.Limit != pageSize {
					t.Fatalf("options = %+v, want ReadOnly=%v Limit=%d", o, ro, pageSize)
				}
			}
		})
	}
}

func TestSortAndFilterReachBrowse(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	spy := &spyConn{Conn: openConn(t, f, false)}
	h := newHarness(t)
	h.open(NewSource(spy, "fake", peopleRef, false))
	h.keys("s")
	h.exec(h.m.SetFilter(driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpGt, Value: driver.Int(15)}}}))
	last := spy.opts[len(spy.opts)-1]
	if len(last.Sort) != 1 || last.Sort[0].Field != "id" || len(last.Filter.Conditions) != 1 || last.Cursor != "" {
		t.Fatalf("last options = %+v", last)
	}
	if got := h.m.Status().Loaded; got != len(drivertest.People.Rows)-15 {
		t.Fatalf("filtered rows = %d, want all but 15", got)
	}
}

func TestNotSupported(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople(), drivertest.WithoutCapabilities(driver.CapBrowser))
	h := newHarness(t)
	src := NewSource(openConn(t, f, false), "fake", peopleRef, false)
	if src.Browser() {
		t.Fatal("Browser() should be false")
	}
	h.open(src)
	const want = "browsing not supported by fake; open the editor with ctrl+e"
	if v := ansi.Strip(h.m.View(80, 6, true)); !strings.Contains(v, want) {
		t.Fatalf("want %q in:\n%s", want, v)
	}
	if browseCalls(f) != 0 {
		t.Fatal("no Browse call expected")
	}
	h.keys("s", "j", "G") // must not panic or fetch
}

func TestOpenStaticShowsAllRowsAndNeverFetches(t *testing.T) {
	h := newHarness(t)
	cols := drivertest.People.Columns
	rows := drivertest.People.Rows
	h.exec(h.m.Open(nil)) // a leftover message must not survive
	h.m.OpenStatic(cols, rows)
	if !h.m.Static() {
		t.Fatal("Static() = false after OpenStatic")
	}
	st := h.m.Status()
	if st.Loaded != len(rows) || st.Total != len(rows) || !st.Exact || st.Loading {
		t.Fatalf("Status = %+v", st)
	}
	v := ansi.Strip(h.m.View(120, 6, true))
	if strings.Contains(v, "browsing not supported") || !strings.Contains(v, "Ada Lovelace") {
		t.Fatalf("static view:\n%s", v)
	}
	h.keys("G")
	if r, _ := h.m.Cursor(); r != len(rows)-1 {
		t.Fatalf("G moved to row %d, want %d", r, len(rows)-1)
	}
	if row, ok := h.m.CurrentRow(); !ok || len(row) != len(cols) {
		t.Fatalf("CurrentRow = %v, %v", row, ok)
	}
	h.keys("s") // sorting a static result is the parent's call: nothing happens here
	if h.m.Loading() || len(h.m.Status().Sort) != 0 {
		t.Fatalf("sort on a static grid: loading=%v sort=%v", h.m.Loading(), h.m.Status().Sort)
	}
	if h.m.SetFilter(driver.Filter{Raw: "x"}) != nil {
		t.Fatal("SetFilter on a static grid started a fetch")
	}
}

func TestOpenStaticEmpty(t *testing.T) {
	h := newHarness(t)
	h.m.OpenStatic([]driver.Column{{Name: "a"}}, nil)
	v := ansi.Strip(h.m.View(40, 4, true))
	if !strings.Contains(v, "a") || !strings.Contains(v, "no rows") {
		t.Fatalf("empty static view:\n%s", v)
	}
}

func TestFetchErrorIsRedactedEscapedAndGridStaysUsable(t *testing.T) {
	f, ref := bigFake(1000)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false).
		WithRedact(func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") }))
	f.FailNext("browse", errors.New("boom hunter2 \x1b[31mred\nline"))
	for h.m.row < 150 {
		h.keys("pgdown")
	}
	v := h.m.View(80, 8, true)
	if strings.Contains(v, "hunter2") || strings.Contains(v, "\x1b[31mred") {
		t.Fatalf("error not redacted/escaped: %q", v)
	}
	if s := ansi.Strip(v); !strings.Contains(s, `boom *** \x1b[31mred line`) {
		t.Fatalf("error not shown:\n%s", s)
	}
	// Still usable: moves within the loaded rows, and the failed page is not
	// hammered on every key press.
	calls := browseCalls(f)
	r0, _ := h.m.Cursor()
	h.keys("j", "k", "j")
	if browseCalls(f) != calls {
		t.Fatalf("failed page was retried automatically (%d -> %d)", calls, browseCalls(f))
	}
	if r, _ := h.m.Cursor(); r != r0+1 {
		t.Fatalf("cursor row = %d, want %d", r, r0+1)
	}
	// A sort restarts cleanly.
	h.keys("s")
	if h.m.data.msg != "" || h.m.Status().Loaded != pageSize {
		t.Fatalf("restart should clear the error: %q %+v", h.m.data.msg, h.m.Status())
	}
}

func TestStalePageDropped(t *testing.T) {
	f, ref := bigFake(1000)
	h := newHarness(t)
	conn := openConn(t, f, false)
	cmd1 := h.m.Open(NewSource(conn, "fake", ref, false))
	cmd2 := h.m.Open(NewSource(conn, "fake", ref, false))
	stale := cmd1()
	h.send(stale)
	if h.m.Status().Loaded != 0 {
		t.Fatal("stale page must be dropped")
	}
	h.send(cmd2())
	if h.m.Status().Loaded != pageSize {
		t.Fatal("current page must be applied")
	}
}

func TestFastSwitchNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)
	f := drivertest.NewFake()
	const n = 50
	var refs []driver.CollectionRef
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("t%02d", i)
		f.AddCollection("main", name, []driver.Column{{Name: "who"}},
			[]driver.Row{{driver.String(name)}, {driver.String(name)}})
		refs = append(refs, driver.CollectionRef{Namespace: "main", Name: name})
	}
	f.SetLatency(20 * time.Millisecond)
	conn, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	m := New(theme.Dark(), keymap.Vim()).Resize(40, 6)
	msgs := make(chan tea.Msg, n)
	var wg sync.WaitGroup
	for _, ref := range refs {
		cmd := m.Open(NewSource(conn, "fake", ref, false))
		wg.Add(1)
		go func() { defer wg.Done(); msgs <- cmd() }()
	}
	wg.Wait()
	close(msgs)
	for msg := range msgs {
		m, _ = m.Update(msg)
	}
	v := ansi.Strip(m.View(40, 6, true))
	if !strings.Contains(v, "t49") || strings.Contains(v, "t48") || m.Status().Loaded != 2 {
		t.Fatalf("only the last source should show:\n%s", v)
	}
	if m.Loading() {
		t.Fatal("should not be loading")
	}
}

func TestCancelLoad(t *testing.T) {
	f, ref := bigFake(10)
	f.SetLatency(5 * time.Millisecond)
	m := New(theme.Dark(), keymap.Vim())
	cmd := m.Open(NewSource(openConn(t, f, false), "fake", ref, false))
	if !m.CancelLoad() {
		t.Fatal("a load was running")
	}
	if m.CancelLoad() {
		t.Fatal("nothing left to cancel")
	}
	m, _ = m.Update(cmd())
	if m.Status().Loaded != 0 {
		t.Fatal("a cancelled load must not apply")
	}
}

// shortConn answers every Browse with n rows and a next cursor.
type shortConn struct {
	driver.Conn
	n     int
	calls int
}

func (s *shortConn) Browse(_ context.Context, _ driver.CollectionRef, _ driver.BrowseOptions) (driver.ResultSet, error) {
	s.calls++
	rows := make([]driver.Row, s.n)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i))}
	}
	return driver.NewSliceResultSet([]driver.Column{{Name: "n", Type: driver.TypeInt}}, rows, driver.Stats{NextCursor: "more"}), nil
}

func TestEmptyOrShortNonLastPageIsAnError(t *testing.T) {
	for _, tt := range []struct {
		name string
		n    int
		want string
	}{
		{"empty", 0, "driver returned an empty page before the end"},
		{"short", 5, "driver returned a short page before the end"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, ref := bigFake(10)
			spy := &shortConn{Conn: openConn(t, f, false), n: tt.n}
			h := newHarness(t)
			h.open(NewSource(spy, "fake", ref, false))
			if v := ansi.Strip(h.m.View(80, 8, true)); !strings.Contains(v, tt.want) {
				t.Fatalf("want %q in:\n%s", tt.want, v)
			}
			if h.m.Status().Exact {
				t.Fatal("an error page must not make the total exact")
			}
			h.keys("j", "G", "pgdown")
			if spy.calls > 2 {
				t.Fatalf("browse calls = %d; the grid is looping", spy.calls)
			}
		})
	}
}

func TestCancelLoadPrefetchIsSilent(t *testing.T) {
	f, ref := bigFake(1000)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false).WithEstimate(1000))
	// Near the loaded end but with page 1 off screen: a prefetch starts.
	h.m.row = 160
	h.m.ensureVisible()
	if cmd := h.m.next(); cmd == nil || !h.m.Loading() {
		t.Fatal("no prefetch started")
	}
	if h.m.CancelLoad() {
		t.Fatal("cancelling a background prefetch must not count as a running load")
	}
	if h.m.Loading() {
		t.Fatal("prefetch still running after CancelLoad")
	}
	// A page the viewport shows is foreground work.
	h.m.rowOff, h.m.row = 195, 196
	if cmd := h.m.fetch(1); cmd == nil || !h.m.CancelLoad() {
		t.Fatal("a visible page load must count as running")
	}
}

func TestPrefetchBoundaryIsWithinFiftyRows(t *testing.T) {
	f, ref := bigFake(1000)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false).WithEstimate(1000))
	known := h.m.Status().Loaded
	if known != pageSize {
		t.Fatalf("known = %d, want %d", known, pageSize)
	}
	h.m.row = known - prefetchAhead - 1
	h.m.settle()
	if cmd := h.m.next(); cmd != nil {
		t.Fatalf("row known-%d must not prefetch", prefetchAhead+1)
	}
	h.m.row = known - prefetchAhead
	h.m.settle()
	if cmd := h.m.next(); cmd == nil {
		t.Fatalf("row known-%d must prefetch", prefetchAhead)
	} else {
		h.m.CancelLoad()
	}
	if prefetchAhead != 50 {
		t.Fatalf("prefetchAhead = %d, the contract is 50 rows", prefetchAhead)
	}
}

// gateConn blocks a Browse sorted by "slow" until release is closed.
type gateConn struct {
	driver.Conn
	release chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	cursors []string
}

func (g *gateConn) Browse(ctx context.Context, _ driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
	g.mu.Lock()
	g.cursors = append(g.cursors, o.Cursor)
	g.mu.Unlock()
	if len(o.Sort) > 0 && o.Sort[0].Field == "slow" {
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	rows := make([]driver.Row, pageSize)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i))}
	}
	return driver.NewSliceResultSet([]driver.Column{{Name: "n", Type: driver.TypeInt}}, rows,
		driver.Stats{NextCursor: o.Cursor + "n"}), nil
}

func (g *gateConn) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.cursors...)
}

func TestFetchDoesNotHoldTheLockAcrossBrowse(t *testing.T) {
	g := &gateConn{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	src := NewSource(g, "fake", driver.CollectionRef{Name: "x"}, false)
	slow := make(chan error, 1)
	go func() {
		_, err := src.Fetch(context.Background(), 0, []driver.SortKey{{Field: "slow"}}, driver.Filter{})
		slow <- err
	}()
	<-g.entered // the slow Browse is now in flight
	fast := make(chan error, 1)
	go func() {
		_, err := src.Fetch(context.Background(), 0, []driver.SortKey{{Field: "fast"}}, driver.Filter{})
		fast <- err
	}()
	select {
	case err := <-fast:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a Fetch after a sort change waited for the old sort's Browse")
	}
	close(g.release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
}

func TestCursorSignatureKeepsValueTypesApart(t *testing.T) {
	g := &gateConn{}
	src := NewSource(g, "fake", driver.CollectionRef{Name: "x"}, false)
	cond := func(v driver.Value) driver.Filter {
		return driver.Filter{Conditions: []driver.Condition{{Field: "f", Op: driver.OpEq, Value: v}}}
	}
	// Int(1) and String("1") print alike with %v but are different filters.
	if _, err := src.Fetch(context.Background(), 2, nil, cond(driver.Int(1))); err != nil {
		t.Fatal(err)
	}
	if got := g.seen(); len(got) != 3 || got[0] != "" {
		t.Fatalf("first fetch cursors = %q, want 3 calls from the start", got)
	}
	if _, err := src.Fetch(context.Background(), 2, nil, cond(driver.String("1"))); err != nil {
		t.Fatal(err)
	}
	got := g.seen()
	if len(got) != 6 || got[3] != "" {
		t.Fatalf("a filter with another value type must not reuse the cursors: %q", got)
	}
	// The same filter again does reuse them: one call, from page 2's cursor.
	if _, err := src.Fetch(context.Background(), 2, nil, cond(driver.String("1"))); err != nil {
		t.Fatal(err)
	}
	if got := g.seen(); len(got) != 7 || got[6] != "nn" {
		t.Fatalf("same signature should resume at the stored cursor: %q", got)
	}
}

func TestConcurrentFetchesAfterSortChange(t *testing.T) {
	g := &gateConn{}
	src := NewSource(g, "fake", driver.CollectionRef{Name: "x"}, false)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sort := []driver.SortKey{{Field: "a", Desc: i%2 == 0}}
			page := i % 4
			p, err := src.Fetch(context.Background(), page, sort, driver.Filter{})
			if err == nil && p.Index != page {
				err = fmt.Errorf("fetched page %d, want %d", p.Index, page)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	// Each signature kept its own, correct chain: a cursor for page n is n "n"s.
	for _, c := range g.seen() {
		if strings.Trim(c, "n") != "" {
			t.Fatalf("corrupt cursor %q", c)
		}
	}
}

func TestResumeRefetchesTheCancelledPageAndKeepsState(t *testing.T) {
	f, ref := bigFake(1000)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false))
	h.keys(">")   // widen the column: a manual width
	h.keys("s")   // sort ascending on it
	h.m.row = 160 // within the prefetch distance
	h.m.settle()
	wantWidth := h.m.widths[0]
	wantOff := h.m.rowOff
	cmd := h.m.next()
	if cmd == nil || !h.m.Loading() {
		t.Fatal("expected a prefetch to be running")
	}
	h.m.CancelLoad()
	if h.m.Loading() {
		t.Fatal("CancelLoad left the load running")
	}
	calls := browseCalls(f)
	resume := h.m.Resume()
	if resume == nil || !h.m.Loading() {
		t.Fatal("Resume should restart the cancelled load")
	}
	h.exec(resume)
	if got := h.m.Status().Loaded; got != 2*pageSize {
		t.Fatalf("loaded = %d, want the cancelled page 1 fetched again (%d)", got, 2*pageSize)
	}
	if browseCalls(f) <= calls {
		t.Fatal("Resume did not browse")
	}
	if s := h.m.Status().Sort; len(s) != 1 || s[0].Field != "n" {
		t.Fatalf("sort lost: %v", s)
	}
	if h.m.widths[0] != wantWidth || h.m.manual["n"] != wantWidth {
		t.Fatalf("width %d / manual %d, want %d kept", h.m.widths[0], h.m.manual["n"], wantWidth)
	}
	if h.m.row != 160 || h.m.rowOff != wantOff {
		t.Fatalf("scroll position moved: row %d off %d, want 160 / %d", h.m.row, h.m.rowOff, wantOff)
	}
	if h.m.Resume() != nil {
		t.Fatal("a second Resume has nothing to restart")
	}
}

func TestResumeKeepsTheFilter(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	spy := &spyConn{Conn: openConn(t, f, false)}
	h := newHarness(t)
	h.open(NewSource(spy, "fake", peopleRef, false))
	flt := driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpGt, Value: driver.Int(15)}}}
	h.exec(h.m.SetFilter(flt))
	loaded := h.m.Status().Loaded
	f.SetLatency(time.Second)
	cmd := h.m.SetFilter(flt) // a load is now running
	h.m.CancelLoad()
	_ = cmd
	f.SetLatency(0)
	h.exec(h.m.Resume())
	last := spy.opts[len(spy.opts)-1]
	if len(last.Filter.Conditions) != 1 || h.m.Status().Loaded != loaded {
		t.Fatalf("resume lost the filter: %+v, loaded %d want %d", last.Filter, h.m.Status().Loaded, loaded)
	}
}

func TestRetryAfterPageZeroFailure(t *testing.T) {
	f, ref := bigFake(50)
	h := newHarness(t)
	f.FailNext("browse", errors.New("boom"))
	h.open(NewSource(openConn(t, f, false), "fake", ref, false))
	if !h.m.Failed() {
		t.Fatalf("a failed first page should be retryable; msg %q", h.m.data.msg)
	}
	if !strings.Contains(ansi.Strip(h.m.View(60, 6, true)), "boom") {
		t.Fatal("the error should be on screen")
	}
	h.m.col = 0
	h.exec(h.m.Retry())
	if h.m.Failed() || h.m.Status().Loaded != 50 {
		t.Fatalf("retry did not load: failed=%v %+v", h.m.Failed(), h.m.Status())
	}
	if h.m.Retry() != nil {
		t.Fatal("nothing left to retry")
	}
}

// ctxConn remembers the context of its last Browse.
type ctxConn struct {
	driver.Conn
	last context.Context
}

func (c *ctxConn) Browse(ctx context.Context, ref driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
	c.last = ctx
	b, _ := driver.As[driver.Browser](c.Conn)
	return b.Browse(ctx, ref, o)
}

func TestDeliveredPageReleasesItsContext(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	spy := &ctxConn{Conn: openConn(t, f, false)}
	h := newHarness(t)
	cmd := h.m.Open(NewSource(spy, "fake", peopleRef, false))
	msg := cmd()
	if spy.last == nil || spy.last.Err() != nil {
		t.Fatal("the request context should be live until its result is delivered")
	}
	h.send(msg)
	if spy.last.Err() == nil {
		t.Fatal("delivering the result should release the request's context")
	}
}

func TestRememberEvictsOtherSignaturesFirst(t *testing.T) {
	s := NewSource(nil, "fake", driver.CollectionRef{Name: "x"}, false)
	s.maxCursors = 4
	for p := 1; p <= 3; p++ {
		s.remember(cursorKey{"old", p}, "o")
	}
	s.remember(cursorKey{"live", 1}, "a")
	s.remember(cursorKey{"live", 2}, "b") // full: the old chain goes, the live one stays
	if p, c := s.resume("live", 2); p != 2 || c != "b" {
		t.Fatalf("live chain lost: page %d cursor %q", p, c)
	}
	if p, _ := s.resume("old", 3); p != 0 {
		t.Fatalf("the old signature should have been evicted, resumed at %d", p)
	}
	// A live chain that alone fills the map keeps what it has.
	for p := 3; p <= 8; p++ {
		s.remember(cursorKey{"live", p}, "x")
	}
	if p, c := s.resume("live", 2); p != 2 || c != "b" {
		t.Fatalf("existing live cursors must never be evicted: page %d cursor %q", p, c)
	}
}

// TestReadOnlyIsReadAtFetchTime: a source given a read-only func asks it on
// every Browse, so :readonly applies to the next page without reopening.
func TestReadOnlyIsReadAtFetchTime(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	spy := &spyConn{Conn: openConn(t, f, false)}
	var ro atomic.Bool
	h := newHarness(t)
	h.open(NewSource(spy, "fake", peopleRef, false).WithReadOnly(ro.Load))
	if o := spy.opts[len(spy.opts)-1]; o.ReadOnly {
		t.Fatalf("first page: ReadOnly = true")
	}
	ro.Store(true)
	h.exec(h.m.SetFilter(driver.Filter{Raw: "x"}))
	if o := spy.opts[len(spy.opts)-1]; !o.ReadOnly {
		t.Fatalf("after the switch: ReadOnly = false")
	}
}

func TestFilterAccessor(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", peopleRef, false))
	flt := driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpGt, Value: driver.Int(15)}}}
	h.exec(h.m.SetFilter(flt))
	if got := h.m.Filter(); len(got.Conditions) != 1 || got.Conditions[0].Field != "id" {
		t.Fatalf("Filter() = %+v", got)
	}
}
