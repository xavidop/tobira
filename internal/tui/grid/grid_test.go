package grid

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

var update = flag.Bool("update", false, "update golden files")

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "left":
			msg = tea.KeyPressMsg{Code: tea.KeyLeft}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "pgdown":
			msg = tea.KeyPressMsg{Code: tea.KeyPgDown}
		case "pgup":
			msg = tea.KeyPressMsg{Code: tea.KeyPgUp}
		case "home":
			msg = tea.KeyPressMsg{Code: tea.KeyHome}
		case "end":
			msg = tea.KeyPressMsg{Code: tea.KeyEnd}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		m, _ = m.Update(msg)
	}
	return m
}

func intCols(n int) []driver.Column {
	cols := make([]driver.Column, n)
	for i := range cols {
		cols[i] = driver.Column{Name: fmt.Sprintf("col%d", i), Type: driver.TypeInt}
	}
	return cols
}

func intRows(n, ncol int) []driver.Row {
	rows := make([]driver.Row, n)
	for i := range rows {
		r := make(driver.Row, ncol)
		for c := range r {
			r[c] = driver.Int(int64(i*10 + c))
		}
		rows[i] = r
	}
	return rows
}

func filled(rows, cols int) Model {
	m := New(theme.Dark(), keymap.Vim()).Resize(60, 12)
	m.SetColumns(intCols(cols))
	m.SetRows(0, intRows(rows, cols))
	m.SetTotal(rows, true)
	return m
}

func TestMovementBounds(t *testing.T) {
	m := filled(5, 3)
	tests := []struct {
		name         string
		keys         []string
		wantR, wantC int
	}{
		{"up at top", []string{"up"}, 0, 0},
		{"left at edge", []string{"left"}, 0, 0},
		{"down", []string{"down", "down"}, 2, 0},
		{"down past end", []string{"j", "j", "j", "j", "j", "j", "j"}, 4, 0},
		{"right past end", []string{"right", "right", "right", "right"}, 0, 2},
		{"end and home", []string{"end", "home"}, 0, 0},
		{"end", []string{"end"}, 0, 2},
		{"pgdown clamps", []string{"pgdown"}, 4, 0},
		{"bottom then gg", []string{"G", "g", "g"}, 0, 0},
		{"single g does nothing", []string{"G", "g"}, 4, 0},
		{"g then j", []string{"G", "k", "g", "k"}, 2, 0},
		{"bottom", []string{"G"}, 4, 0},
		{"pgup clamps", []string{"G", "pgup", "pgup"}, 0, 0},
		{"hjkl", []string{"j", "l", "j", "h", "k"}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := press(m, tt.keys...)
			r, c := got.Cursor()
			if r != tt.wantR || c != tt.wantC {
				t.Fatalf("cursor = (%d,%d), want (%d,%d)", r, c, tt.wantR, tt.wantC)
			}
		})
	}
}

func TestEmptyGridDoesNotPanic(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(20, 5)
	m = press(m, "down", "right", "end", "pgdown", "G")
	_ = m.View(20, 5, true)
	_ = m.View(1, 1, true)
	_ = m.View(0, 0, true)
	if _, ok := m.CurrentRow(); ok {
		t.Fatal("empty grid has no current row")
	}
}

func TestHorizontalScrollKeepsCursorVisible(t *testing.T) {
	m := filled(5, 12)
	m = m.Resize(30, 8)
	for i := 0; i < 11; i++ {
		m = press(m, "right")
		_, c := m.Cursor()
		v := ansi.Strip(m.View(30, 8, true))
		if !strings.Contains(strings.SplitN(v, "\n", 2)[0], fmt.Sprintf("col%d", c)) {
			t.Fatalf("cursor column %d not in header after scrolling:\n%s", c, v)
		}
	}
	if m.colOff == 0 {
		t.Fatal("expected horizontal scroll")
	}
	m = press(m, "home")
	if m.colOff != 0 {
		t.Fatalf("home should scroll back, colOff = %d", m.colOff)
	}
}

func TestColumnWidthClamp(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(80, 10)
	m.SetColumns([]driver.Column{{Name: "a"}, {Name: "this_is_a_really_long_header_name_that_exceeds_forty_columns"}, {Name: "mid"}})
	m.SetRows(0, []driver.Row{{driver.String("x"), driver.String("y"), driver.String(strings.Repeat("z", 100))}})
	if got := m.widths; got[0] != 4 || got[1] != 40 || got[2] != 40 {
		t.Fatalf("auto widths = %v, want [4 40 40]", got)
	}
	m = press(m, ">", ">")
	if m.widths[0] != 8 {
		t.Fatalf("after 2x '>' width = %d, want 8", m.widths[0])
	}
	m = press(m, "<", "<", "<", "<", "<")
	if m.widths[0] != 4 {
		t.Fatalf("narrow clamps at 4, got %d", m.widths[0])
	}
	m = press(m, "right", "right")
	for i := 0; i < 30; i++ {
		m = press(m, ">")
	}
	if m.widths[2] != 80 {
		t.Fatalf("widen clamps at 80, got %d", m.widths[2])
	}
	// A manual width survives new data.
	m.SetRows(0, []driver.Row{{driver.String("x"), driver.String("y"), driver.String("z")}})
	if m.widths[2] != 80 || m.widths[0] != 4 {
		t.Fatalf("manual widths lost: %v", m.widths)
	}
}

func oneCell(vals ...driver.Value) (Model, string) {
	m := New(theme.Dark(), keymap.Default()).Resize(40, 10)
	cols := make([]driver.Column, len(vals))
	for i := range cols {
		cols[i] = driver.Column{Name: fmt.Sprintf("c%d", i)}
	}
	m.SetColumns(cols)
	m.SetRows(0, []driver.Row{vals, vals})
	m.SetTotal(2, true)
	m.row = 1 // keep row 0 free of the current-row background
	return m, m.View(40, 10, false)
}

func TestGridCellRendering(t *testing.T) {
	t.Run("wide characters stay aligned", func(t *testing.T) {
		m := New(theme.Dark(), keymap.Default()).Resize(40, 10)
		m.SetColumns([]driver.Column{{Name: "name"}, {Name: "tail"}})
		m.SetRows(0, []driver.Row{
			{driver.String("李小龍"), driver.String("end")},
			{driver.String("ab"), driver.String("end")},
			{driver.String("😀😀"), driver.String("end")},
		})
		m.SetTotal(3, true)
		lines := strings.Split(ansi.Strip(m.View(40, 10, false)), "\n")
		want := strings.Index(lines[0], "tail")
		for _, l := range lines[1:4] {
			if got := ansi.StringWidth(l[:strings.Index(l, "end")]); got != ansi.StringWidth(lines[0][:want]) {
				t.Fatalf("tail column misaligned in %q: %d vs %d", l, got, want)
			}
		}
	})
	t.Run("escape is printed visibly", func(t *testing.T) {
		_, v := oneCell(driver.String("a\x1b[31mb\nc"))
		tuitest.NoInjectedControls(t, v)
		if !strings.Contains(ansi.Strip(v), `a\x1b[31mb c`) {
			t.Fatalf("want escaped text, got:\n%s", ansi.Strip(v))
		}
	})
	t.Run("raw C1 byte is printed visibly", func(t *testing.T) {
		_, v := oneCell(driver.String("a\x9b[2Jb"))
		tuitest.NoInjectedControls(t, v)
		if !strings.Contains(ansi.Strip(v), `a\x{9B}[2Jb`) {
			t.Fatalf("want escaped text, got:\n%s", ansi.Strip(v))
		}
	})
	t.Run("long JSON is truncated", func(t *testing.T) {
		big := `{"k":"` + strings.Repeat("v", 10*1024) + `"}`
		_, v := oneCell(driver.JSON([]byte(big)))
		lines := strings.Split(ansi.Strip(v), "\n")
		if !strings.Contains(lines[1], "…") {
			t.Fatalf("expected ellipsis: %q", lines[1])
		}
		if w := ansi.StringWidth(lines[1]); w != 40 {
			t.Fatalf("line width = %d, want 40", w)
		}
	})
	t.Run("NULL is styled, the string NULL is not", func(t *testing.T) {
		m, _ := oneCell(driver.Null(), driver.String("NULL"))
		v := m.View(40, 10, false)
		nullSeq := m.th.Styles.Null.Render("NULL")
		if !hasItalicSGR(nullSeq) {
			t.Fatalf("Null style should be italic (SGR 3): %q", nullSeq)
		}
		row := strings.Split(v, "\n")[1]
		if got := strings.Count(row, nullSeq); got != 1 {
			t.Fatalf("styled NULL appears %d times in %q, want exactly 1 (the real NULL)", got, row)
		}
		if got := strings.Count(ansi.Strip(row), "NULL"); got != 2 {
			t.Fatalf("both cells should read NULL, got %q", ansi.Strip(row))
		}
	})
}

var sgrRE = regexp.MustCompile(`\x1b\[([0-9;:]*)m`)

// hasItalicSGR reports whether s sets italic (SGR parameter 3). Parameters
// that belong to an extended colour (38;5;n, 38;2;r;g;b and the same for 48
// and 58) are skipped, so a colour that happens to contain a 3 does not count.
func hasItalicSGR(s string) bool {
	for _, m := range sgrRE.FindAllStringSubmatch(s, -1) {
		params := strings.Split(strings.ReplaceAll(m[1], ":", ";"), ";")
		for i := 0; i < len(params); i++ {
			switch params[i] {
			case "3":
				return true
			case "38", "48", "58":
				if i+1 < len(params) && params[i+1] == "5" {
					i += 2
				} else if i+1 < len(params) && params[i+1] == "2" {
					i += 4
				}
			}
		}
	}
	return false
}

func TestHasItalicSGR(t *testing.T) {
	for in, want := range map[string]bool{
		"\x1b[3m":               true,
		"\x1b[1;3m":             true,
		"\x1b[3;38;5;8m":        true,
		"\x1b[38;5;3m":          false, // 256-colour index 3
		"\x1b[38;2;3;3;3m":      false, // RGB
		"\x1b[38;2;1;2;3m":      false,
		"\x1b[38;2;1;2;3;3m":    true, // the last 3 is a parameter of its own
		"\x1b[1;38;5;3;3m":      true,
		"\x1b[31m":              false,
		"\x1b[38;2;99;99;99mhi": false,
		"plain":                 false,
	} {
		if got := hasItalicSGR(in); got != want {
			t.Errorf("hasItalicSGR(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestVirtualisedView(t *testing.T) {
	m := New(theme.Dark(), keymap.Vim()).Resize(60, 12)
	m.SetColumns(intCols(4))
	m.SetRows(0, intRows(10_000, 4))
	m.SetTotal(10_000, true)
	m = press(m, "G")
	if r, _ := m.Cursor(); r != 9_999 {
		t.Fatalf("G should reach the last row, at %d", r)
	}
	v := ansi.Strip(m.View(60, 12, true))
	lines := strings.Split(v, "\n")
	if len(lines) != 12 {
		t.Fatalf("view has %d lines, want 12", len(lines))
	}
	if !strings.HasPrefix(lines[11], "999") {
		t.Fatalf("last row should be visible after G:\n%s", v)
	}
	if strings.Contains(v, "\n"+"0 ") {
		t.Fatalf("row 0 should be scrolled away")
	}
	for _, l := range lines {
		if ansi.StringWidth(l) != 60 {
			t.Fatalf("line width %d != 60: %q", ansi.StringWidth(l), l)
		}
	}
}

func TestUnloadedRowsShowPlaceholder(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(30, 6)
	m.SetColumns(intCols(2))
	m.SetRows(200, intRows(3, 2)) // page 0 missing
	m.row, m.rowOff = 199, 198
	v := ansi.Strip(m.View(30, 6, false))
	lines := strings.Split(v, "\n")
	if !strings.HasPrefix(lines[1], "…") {
		t.Fatalf("unloaded row should show a placeholder:\n%s", v)
	}
	if !strings.HasPrefix(lines[3], "0 ") {
		t.Fatalf("loaded row 200 should show:\n%s", v)
	}
}

func TestNeedRows(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(30, 6)
	m.SetColumns(intCols(2))
	m.SetRows(0, intRows(100, 2))
	if _, _, ok := m.NeedRows(); ok {
		t.Fatal("nothing missing")
	}
	m.SetRows(300, intRows(50, 2))
	m.rowOff = 98
	from, to, ok := m.NeedRows()
	if !ok || from != 100 || to != 153 {
		t.Fatalf("NeedRows = %d..%d %v, want 100..153", from, to, ok)
	}
}

func TestMouse(t *testing.T) {
	m := filled(20, 3)
	m, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 4})
	if r, c := m.Cursor(); r != 3 || c != 0 {
		t.Fatalf("click -> (%d,%d), want (3,0)", r, c)
	}
	_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 4})
	if cmd == nil {
		t.Fatal("second click on the same cell should activate")
	}
	if _, ok := cmd().(RowActivatedMsg); !ok {
		t.Fatal("want RowActivatedMsg")
	}
	m, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.rowOff != 3 {
		t.Fatalf("wheel rowOff = %d, want 3", m.rowOff)
	}
	m, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 0}) // header: ignored
	if r, _ := m.Cursor(); r != 3 {
		t.Fatalf("header click moved cursor to %d", r)
	}
}

func TestGridPeopleGolden(t *testing.T) {
	p := drivertest.People
	m := New(theme.Dark(), keymap.Default()).Resize(100, 20)
	m.SetColumns(p.Columns)
	m.SetRows(0, p.Rows)
	m.SetTotal(len(p.Rows), true)
	m = press(m, "down", "down", "right")
	golden(t, "grid_people.golden", ansi.Strip(m.View(100, 20, true)))
}

func benchModel() Model {
	cols := intCols(12)
	rows := intRows(10_000, 12)
	for i := range rows {
		rows[i][1] = driver.String(fmt.Sprintf("name-%d 李小龍 some text", i))
		if i%7 == 0 {
			rows[i][2] = driver.Null()
		}
	}
	m := New(theme.Dark(), keymap.Default()).Resize(200, 60)
	m.SetColumns(cols)
	m.SetRows(0, rows)
	m.SetTotal(len(rows), true)
	m.row, m.rowOff = 5000, 4990
	return m
}

func BenchmarkGridView10k(b *testing.B) {
	m := benchModel()
	b.ReportAllocs()
	for b.Loop() {
		_ = m.View(200, 60, true)
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGridAgreesWithLayoutOnClusterWidths(t *testing.T) {
	// ZWJ itself is escaped for display, so the clusters here are a skin-tone
	// emoji, a flag and combining accents.
	thumb, flagJP := "\U0001F44D\U0001F3FD", "\U0001F1EF\U0001F1F5"
	m := New(theme.Dark(), keymap.Default()).Resize(30, 6)
	m.SetColumns([]driver.Column{{Name: "name"}, {Name: "n"}})
	m.SetRows(0, []driver.Row{
		{driver.String(thumb + flagJP + "x"), driver.Int(1)},
		{driver.String("ééé"), driver.Int(2)},
	})
	m.SetTotal(2, true)
	for i, line := range strings.Split(m.View(30, 6, true), "\n") {
		if w := ansi.StringWidth(line); w != 30 {
			t.Errorf("line %d is %d columns wide, want 30: %q", i, w, ansi.Strip(line))
		}
	}
	if got := m.widths[0]; got != 5 { // thumb (2) + flag (2) + x (1)
		t.Errorf("column width = %d, want 5", got)
	}
}

// clock is a hand-wound time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestGGNeedsTheSecondKeyInTime(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	m := filled(20, 3).WithClock(c.now)
	m = press(m, "G")
	m = press(m, "g")
	c.t = c.t.Add(500 * time.Millisecond)
	m = press(m, "g")
	if r, _ := m.Cursor(); r != 0 {
		t.Fatalf("gg inside the window left the cursor at %d", r)
	}

	m = press(m, "G", "g")
	c.t = c.t.Add(501 * time.Millisecond)
	m = press(m, "g")
	if r, _ := m.Cursor(); r != 19 {
		t.Fatalf("a late second g moved the cursor to %d, want it to stay at 19", r)
	}
}

func TestDoubleClickWindow(t *testing.T) {
	clickMsg := tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 4}
	for _, tc := range []struct {
		name string
		gap  time.Duration
		want bool
	}{
		{"inside the window", 399 * time.Millisecond, true},
		{"at the window", 400 * time.Millisecond, true},
		{"too slow", 401 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Unix(1000, 0)}
			m := filled(20, 3).WithClock(c.now)
			m, _ = m.Update(clickMsg)
			c.t = c.t.Add(tc.gap)
			_, cmd := m.Update(clickMsg)
			got := false
			if cmd != nil {
				_, got = cmd().(RowActivatedMsg)
			}
			if got != tc.want {
				t.Fatalf("activated = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClickOnKeyboardCursorDoesNotActivate(t *testing.T) {
	m := filled(20, 3)
	m = press(m, "down", "down", "down") // cursor on row 3 without a click
	_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 4})
	if cmd != nil {
		if _, ok := cmd().(RowActivatedMsg); ok {
			t.Fatal("a single click must not open detail even on the cursor cell")
		}
	}
}

func TestDoubleClickNeedsTheSameCell(t *testing.T) {
	m := filled(20, 3)
	m, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 4})
	m, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 5})
	_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 4})
	if cmd != nil {
		if _, ok := cmd().(RowActivatedMsg); ok {
			t.Fatal("clicks on different cells are not a double-click")
		}
	}
}

func TestGridLinesStayExactWidthForTrickyClusters(t *testing.T) {
	inputs := []string{
		"\U0001F600︎", "1️⃣", "กำ", "नमस्ते",
		"क्ष", "\U0001F468\u200D\U0001F469\u200D\U0001F467", "é", "\U0001F1EF\U0001F1F5",
	}
	for _, in := range inputs {
		for _, w := range []int{30, 7, 4} {
			m := New(theme.Dark(), keymap.Default()).Resize(w, 5)
			m.SetColumns([]driver.Column{{Name: "name"}, {Name: "n"}})
			m.SetRows(0, []driver.Row{
				{driver.String(in + in + "x"), driver.Int(1)},
				{driver.String("ab" + in), driver.Int(2)},
			})
			m.SetTotal(2, true)
			for i, line := range strings.Split(m.View(w, 5, true), "\n") {
				if got := ansi.StringWidth(line); got != w {
					t.Errorf("%q w=%d: line %d is %d wide: %q", in, w, i, got, ansi.Strip(line))
				}
			}
		}
	}
}
