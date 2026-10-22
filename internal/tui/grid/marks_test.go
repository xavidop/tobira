package grid

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

// stubMarks edits the row whose first cell is 10 (column 1 shows "X") and
// deletes the row whose first cell is 20.
type stubMarks struct{}

func (stubMarks) Row(cols []driver.Column, row driver.Row) RowMark {
	n, _ := row[0].AsInt()
	switch n {
	case 10:
		vals := append(driver.Row(nil), row...)
		vals[1] = driver.String("X\x1b[31m")
		return RowMark{Values: vals, Changed: []bool{false, true, false}}
	case 20:
		return RowMark{Deleted: true}
	}
	return RowMark{}
}

func viewLines(m Model, w, h int, focused bool) []string {
	return strings.Split(m.View(w, h, focused), "\n")
}

func TestPinnedRowsComeFirstWithAGutter(t *testing.T) {
	m := filled(5, 3)
	m = press(m, "down", "down") // data row 2
	m.SetMarks(stubMarks{})
	m.SetPinned([]Pinned{{Values: driver.Object{{Key: "col2", Value: driver.String("a")}, {Key: "col0", Value: driver.Int(99)}}}})
	if r, _ := m.Cursor(); r != 3 {
		t.Fatalf("cursor row = %d, want 3 (the same data row below one pinned row)", r)
	}
	if _, ok := m.PinnedAt(); ok {
		t.Fatal("cursor reported on a pinned row")
	}
	if r, ok := m.CurrentRow(); !ok || r[0].String() != "20" {
		t.Fatalf("current row = %v", r)
	}
	lines := viewLines(m, 60, 8, true)
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = ansi.Strip(l)
	}
	if !strings.HasPrefix(plain[0], "  col0") {
		t.Fatalf("header without gutter: %q", plain[0])
	}
	if !strings.HasPrefix(plain[1], "+ 99") || !strings.Contains(plain[1], "def") {
		t.Fatalf("pinned row: %q", plain[1])
	}
	if !strings.HasPrefix(plain[2], "  0") {
		t.Fatalf("first data row: %q", plain[2])
	}
	if !strings.HasPrefix(plain[3], "~ 10") || !strings.Contains(plain[3], `X\x`) {
		t.Fatalf("edited row: %q", plain[3])
	}
	if !strings.HasPrefix(plain[4], "- 20") {
		t.Fatalf("deleted row: %q", plain[4])
	}
	tuitest.NoInjectedControls(t, m.View(60, 8, true))

	m.SetCursor(0, 2)
	if i, ok := m.PinnedAt(); !ok || i != 0 {
		t.Fatalf("PinnedAt = %d, %v", i, ok)
	}
	if r, ok := m.CurrentRow(); !ok || r[0].String() != "99" {
		t.Fatalf("current pinned row = %v", r)
	}
	// Dropping the pinned row keeps the cursor on the row below it.
	m.SetCursor(3, 0)
	m.SetPinned(nil)
	if r, _ := m.Cursor(); r != 2 {
		t.Fatalf("cursor after unpinning = %d, want 2", r)
	}
	if !strings.HasPrefix(ansi.Strip(viewLines(m, 60, 8, true)[1]), "  0") {
		t.Fatal("gutter lost while marks are set")
	}
	m.SetMarks(nil)
	if !strings.HasPrefix(ansi.Strip(viewLines(m, 60, 8, true)[1]), "0") {
		t.Fatal("gutter kept without marks")
	}
}

var (
	warnBG = regexp.MustCompile(`\x1b\[[0-9;]*48;2;251;191;36[0-9;]*m`)
	strike = regexp.MustCompile(`\x1b\[(?:[0-9;]*;)?9(?:;[0-9;]*)?m`)
)

func TestMarksHighlightChangedCellsAndStrikeDeletedRows(t *testing.T) {
	m := filled(5, 3)
	m.SetMarks(stubMarks{})
	lines := viewLines(m, 60, 8, false)
	if !warnBG.MatchString(lines[2]) {
		t.Fatalf("edited cell without the Warn background: %q", lines[2])
	}
	if warnBG.MatchString(lines[1]) || strike.MatchString(lines[1]) {
		t.Fatalf("plain row decorated: %q", lines[1])
	}
	if !strike.MatchString(lines[3]) {
		t.Fatalf("deleted row not struck through: %q", lines[3])
	}
}

func TestPinnedRowsDoNotShiftPaging(t *testing.T) {
	m := filled(5, 2)
	m.SetPinned([]Pinned{
		{Values: driver.Object{{Key: "col0", Value: driver.Int(1)}, {Key: "col1", Value: driver.Int(2)}}},
		{Values: driver.Object{{Key: "col0", Value: driver.Int(3)}, {Key: "col1", Value: driver.Int(4)}}},
	})
	if _, _, ok := m.NeedRows(); ok {
		t.Fatal("NeedRows reports rows with every data row loaded")
	}
	m = press(m, "end")
	m = press(m, "G")
	if r, _ := m.Cursor(); r != 6 {
		t.Fatalf("bottom = %d, want 6 (2 pinned + 5 rows - 1)", r)
	}
	if r, ok := m.CurrentRow(); !ok || r[0].String() != "40" {
		t.Fatalf("last row = %v", r)
	}
}

func TestRefreshKeepsTheCursor(t *testing.T) {
	f, ref := bigFake(500)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false))
	h.keys("pgdown", "pgdown", "pgdown")
	for range 20 {
		h.keys("j")
	}
	want, _ := h.m.Cursor()
	h.exec(h.m.Refresh())
	if r, _ := h.m.Cursor(); r != want {
		t.Fatalf("cursor after refresh = %d, want %d", r, want)
	}
	if row, ok := h.m.CurrentRow(); !ok || row[0].String() != strconv.Itoa(want) {
		t.Fatalf("row under the cursor = %v", row)
	}
	// A refresh of a grid whose data shrank clamps the cursor.
	f2, ref2 := bigFake(3)
	h2 := newHarness(t)
	src := NewSource(openConn(t, f2, false), "fake", ref2, false)
	h2.open(src)
	h2.keys("j", "j")
	f2.AddCollection("main", "big", []driver.Column{{Name: "n", Type: driver.TypeInt}}, []driver.Row{{driver.Int(0)}})
	h2.exec(h2.m.Refresh())
	if r, _ := h2.m.Cursor(); r != 0 {
		t.Fatalf("cursor after a shrinking refresh = %d", r)
	}
}

func TestSourceSchema(t *testing.T) {
	src := NewSource(nil, "fake", peopleRef, false)
	if _, ok := src.Schema(); ok {
		t.Fatal("schema before SetSchema")
	}
	src.SetSchema(&driver.CollectionSchema{PrimaryKey: []string{"id"}}, nil)
	if s, ok := src.Schema(); !ok || s.PrimaryKey[0] != "id" {
		t.Fatalf("schema = %+v", s)
	}
}

func TestPinnedValuesFollowColumnsByName(t *testing.T) {
	m := filled(3, 3)
	m.SetMarks(stubMarks{})
	m.SetPinned([]Pinned{{Values: driver.Object{{Key: "col1", Value: driver.Int(77)}}}})
	// The columns come back in another order: the value stays under col1.
	cols := intCols(3)
	cols[0], cols[1] = cols[1], cols[0]
	m.SetColumns(cols)
	line := ansi.Strip(viewLines(m, 60, 6, false)[1])
	f := strings.Fields(line)
	if len(f) < 3 || f[1] != "77" || !strings.HasPrefix(f[2], "def") {
		t.Fatalf("pinned row after a column reorder: %q", line)
	}
	m.SetCursor(0, 0)
	if r, ok := m.CurrentRow(); !ok || r[0].String() != "77" || !r[1].IsNull() {
		t.Fatalf("current pinned row = %v", r)
	}
}

func TestRefreshLeavesACursorTheUserMoved(t *testing.T) {
	f, ref := bigFake(500)
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false))
	for r, _ := h.m.Cursor(); r < 300; r, _ = h.m.Cursor() {
		h.keys("pgdown")
	}
	h.m.SetCursor(300, 0)
	cmd := h.m.Refresh()
	var next tea.Cmd
	h.m, next = h.m.Update(cmd()) // page 0: the cursor goes as far as it can
	if r, _ := h.m.Cursor(); r != 199 {
		t.Fatalf("cursor after page 0 = %d, want 199", r)
	}
	h.m, _ = h.m.Update(tea.KeyPressMsg{Code: 'k', Text: "k"}) // the user moves
	if next == nil {
		t.Fatal("no next page fetch")
	}
	h.m, _ = h.m.Update(next()) // page 1 arrives
	if r, _ := h.m.Cursor(); r != 198 {
		t.Fatalf("cursor after the user moved = %d, want 198", r)
	}
}
