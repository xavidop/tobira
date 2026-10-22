package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

func newModel() Model { return New(theme.Dark(), keymap.Default()).Resize(60, 10) }

// press sends the named keys.
func press(t *testing.T, m Model, ks ...string) Model {
	t.Helper()
	for _, k := range ks {
		m, _ = m.Update(keyMsg(k))
	}
	return m
}

// typ types s rune by rune.
func typ(m Model, s string) Model {
	for _, r := range s {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func keyMsg(s string) tea.KeyPressMsg {
	mods := tea.KeyMod(0)
	for {
		switch {
		case strings.HasPrefix(s, "shift+"):
			mods |= tea.ModShift
			s = strings.TrimPrefix(s, "shift+")
			continue
		case strings.HasPrefix(s, "alt+"):
			mods |= tea.ModAlt
			s = strings.TrimPrefix(s, "alt+")
			continue
		case strings.HasPrefix(s, "ctrl+"):
			mods |= tea.ModCtrl
			s = strings.TrimPrefix(s, "ctrl+")
			continue
		}
		break
	}
	codes := map[string]rune{
		"enter": tea.KeyEnter, "tab": tea.KeyTab, "backspace": tea.KeyBackspace, "delete": tea.KeyDelete,
		"left": tea.KeyLeft, "right": tea.KeyRight, "up": tea.KeyUp, "down": tea.KeyDown,
		"home": tea.KeyHome, "end": tea.KeyEnd, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
	}
	c, ok := codes[s]
	if !ok {
		panic("unknown key " + s)
	}
	return tea.KeyPressMsg{Code: c, Mod: mods}
}

func TestTypingAndNewlines(t *testing.T) {
	m := typ(newModel(), "select 1")
	if got := m.Value(); got != "select 1" {
		t.Fatalf("Value = %q", got)
	}
	m = press(t, m, "enter")
	m = typ(m, "from t")
	if got := m.Value(); got != "select 1\nfrom t" {
		t.Fatalf("Value = %q", got)
	}
	if !m.LastLine() || m.FirstLine() {
		t.Fatalf("FirstLine=%v LastLine=%v on line 2 of 2", m.FirstLine(), m.LastLine())
	}
}

func TestEnterKeepsIndentAndTabInsertsTwoSpaces(t *testing.T) {
	m := press(t, newModel(), "tab")
	m = typ(m, "where a")
	m = press(t, m, "enter")
	m = typ(m, "and b")
	if got, want := m.Value(), "  where a\n  and b"; got != want {
		t.Fatalf("Value = %q, want %q", got, want)
	}
}

func TestBackspaceAndDelete(t *testing.T) {
	m := typ(newModel(), "ab")
	m = press(t, m, "enter")
	m = typ(m, "cd")
	m = press(t, m, "home", "backspace") // joins the lines
	if got := m.Value(); got != "abcd" {
		t.Fatalf("after join Value = %q", got)
	}
	m = press(t, m, "delete") // deletes c
	if got := m.Value(); got != "abd" {
		t.Fatalf("after delete Value = %q", got)
	}
	m = press(t, m, "end", "delete") // nothing after the end
	if got := m.Value(); got != "abd" {
		t.Fatalf("delete at end changed Value to %q", got)
	}
}

func TestSelectionWithShiftAndReplace(t *testing.T) {
	m := typ(newModel(), "select 1")
	m = press(t, m, "shift+left")
	start, end, ok := m.Selection()
	if !ok || start != 7 || end != 8 {
		t.Fatalf("Selection = %d,%d,%v; want 7,8,true", start, end, ok)
	}
	m = typ(m, "42")
	if got := m.Value(); got != "select 42" {
		t.Fatalf("Value = %q", got)
	}
	if _, _, ok := m.Selection(); ok {
		t.Fatal("selection survived typing")
	}
	m = press(t, m, "shift+home")
	start, end, ok = m.Selection()
	if !ok || start != 0 || end != 9 {
		t.Fatalf("shift+home Selection = %d,%d,%v", start, end, ok)
	}
	m = press(t, m, "backspace")
	if m.Value() != "" {
		t.Fatalf("backspace over the selection left %q", m.Value())
	}
}

func TestSelectionAcrossLinesAndMovementClears(t *testing.T) {
	m := newModel()
	m.SetValue("ab\ncd")
	m.SetCursor(1)
	m = press(t, m, "shift+down", "shift+right")
	start, end, ok := m.Selection()
	if !ok || start != 1 || end != 5 {
		t.Fatalf("Selection = %d,%d,%v; want 1,5", start, end, ok)
	}
	m = press(t, m, "left")
	if _, _, ok := m.Selection(); ok {
		t.Fatal("a plain movement kept the selection")
	}
}

func TestPasteInsertsAndNormalisesNewlines(t *testing.T) {
	m := typ(newModel(), "x")
	m, _ = m.Update(tea.PasteMsg{Content: "a\r\nb\rc"})
	if got, want := m.Value(), "xa\nb\nc"; got != want {
		t.Fatalf("Value = %q, want %q", got, want)
	}
	if got := m.Cursor(); got != len("xa\nb\nc") {
		t.Fatalf("Cursor = %d after paste", got)
	}
}

func TestPasteOverCapRefused(t *testing.T) {
	m := typ(newModel(), "keep")
	big := strings.Repeat("x", MaxBytes)
	m, cmd := m.Update(tea.PasteMsg{Content: big})
	if m.Value() != "keep" {
		t.Fatalf("over-cap paste changed the buffer (len %d)", len(m.Value()))
	}
	if cmd == nil {
		t.Fatal("no refusal message")
	}
	r, ok := cmd().(RefusedMsg)
	if !ok || !strings.Contains(r.Text, "1 MiB") {
		t.Fatalf("refusal = %#v", cmd())
	}
	// Exactly up to the cap is fine.
	m, cmd = m.Update(tea.PasteMsg{Content: big[:MaxBytes-len("keep")]})
	if cmd != nil || len(m.Value()) != MaxBytes {
		t.Fatalf("paste up to the cap: len %d, cmd %v", len(m.Value()), cmd)
	}
	m = typ(m, "z")
	if len(m.Value()) != MaxBytes {
		t.Fatal("typing past the cap was accepted")
	}
}

func TestCursorRoundTripAndInsertAt(t *testing.T) {
	m := newModel()
	m.SetValue("select a\nfrom t")
	m.SetCursor(12)
	if got := m.Cursor(); got != 12 {
		t.Fatalf("Cursor = %d, want 12", got)
	}
	m.InsertAt(7, "b, ")
	if got := m.Value(); got != "select b, a\nfrom t" {
		t.Fatalf("Value = %q", got)
	}
	if got := m.Cursor(); got != 10 {
		t.Fatalf("Cursor after InsertAt = %d, want 10 (end of the insert)", got)
	}
	m.SetCursor(-5)
	if m.Cursor() != 0 {
		t.Fatalf("negative SetCursor = %d", m.Cursor())
	}
	m.SetCursor(1 << 30)
	if m.Cursor() != len(m.Value()) {
		t.Fatalf("huge SetCursor = %d", m.Cursor())
	}
}

func TestGraphemeCursorMovement(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		stops      []int // offsets visited by repeated right presses
	}{
		{"emoji with skin tone", "a👍🏽b", []int{1, 1 + len("👍🏽"), 2 + len("👍🏽")}},
		{"combining accent", "éx", []int{len("é"), len("éx")}},
		{"zwj family", "👩‍👩‍👧z", []int{len("👩‍👩‍👧"), len("👩‍👩‍👧z")}},
		{"cjk", "日本", []int{3, 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel()
			m.SetValue(tc.text)
			m.SetCursor(0)
			for _, want := range tc.stops {
				m = press(t, m, "right")
				if got := m.Cursor(); got != want {
					t.Fatalf("right: Cursor = %d, want %d", got, want)
				}
			}
			for i := len(tc.stops) - 2; i >= -1; i-- {
				want := 0
				if i >= 0 {
					want = tc.stops[i]
				}
				m = press(t, m, "left")
				if got := m.Cursor(); got != want {
					t.Fatalf("left: Cursor = %d, want %d", got, want)
				}
			}
			// Backspace removes the whole first cluster.
			m.SetCursor(tc.stops[0])
			m = press(t, m, "backspace")
			if got, want := m.Value(), tc.text[tc.stops[0]:]; got != want {
				t.Fatalf("backspace left %q, want %q", got, want)
			}
		})
	}
}

func TestSetCursorSnapsToClusterStart(t *testing.T) {
	m := newModel()
	m.SetValue("a👍🏽b")
	m.SetCursor(3) // inside the emoji
	if got := m.Cursor(); got != 1 {
		t.Fatalf("Cursor = %d, want 1", got)
	}
	m.SetValue("e\u0301x")
	m.SetCursor(1) // between e and its accent
	if got := m.Cursor(); got != 0 {
		t.Fatalf("Cursor = %d, want 0", got)
	}
}

func TestVerticalMovementKeepsDisplayColumn(t *testing.T) {
	m := newModel()
	m.SetValue("日本語\nabcdef")
	m.SetCursor(len("日本")) // display column 4
	m = press(t, m, "down")
	if got := m.Cursor(); got != len("日本語\n")+4 {
		t.Fatalf("down: Cursor = %d", got)
	}
	m = press(t, m, "up")
	if got := m.Cursor(); got != len("日本") {
		t.Fatalf("up: Cursor = %d", got)
	}
}

func TestWordAndBufferMovement(t *testing.T) {
	m := newModel()
	m.SetValue("select name, id\nfrom people")
	m.SetCursor(0)
	m = press(t, m, "alt+right")
	if m.Cursor() != 6 {
		t.Fatalf("alt+right = %d, want 6", m.Cursor())
	}
	m = press(t, m, "alt+right")
	if m.Cursor() != 11 {
		t.Fatalf("alt+right = %d, want 11", m.Cursor())
	}
	m = press(t, m, "alt+left")
	if m.Cursor() != 7 {
		t.Fatalf("alt+left = %d, want 7", m.Cursor())
	}
	m = press(t, m, "ctrl+end")
	if m.Cursor() != len(m.Value()) {
		t.Fatalf("ctrl+end = %d", m.Cursor())
	}
	m = press(t, m, "ctrl+home")
	if m.Cursor() != 0 {
		t.Fatalf("ctrl+home = %d", m.Cursor())
	}
}

func TestViewEscapesControls(t *testing.T) {
	m := newModel()
	m.SetDialect("postgresql")
	m.SetValue("select '\x1b]0;pwned\x07\x1b[2J' as x, '\u202e' -- \x9b \u009b\nfrom t")
	for _, focused := range []bool{true, false} {
		raw := m.View(100, 5, focused)
		tuitest.NoInjectedControls(t, raw)
		plain := ansi.Strip(raw)
		if !strings.Contains(plain, `\x1b]0;pwned\x07\x1b[2J`) || !strings.Contains(plain, `\u{202E}`) ||
			!strings.Contains(plain, `\x{9B} \u009b`) {
			t.Fatalf("controls not shown as escapes:\n%s", plain)
		}
	}
}

func TestViewSizeAndScroll(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(20, 4)
	var b strings.Builder
	for i := range 50 {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("line " + strings.Repeat("x", i))
	}
	m.SetValue(b.String())
	m = press(t, m, "ctrl+end")
	raw := m.View(20, 4, true)
	lines := strings.Split(raw, "\n")
	if len(lines) != 4 {
		t.Fatalf("View has %d lines, want 4", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != 20 {
			t.Fatalf("line %d is %d wide: %q", i, w, ansi.Strip(l))
		}
	}
	plain := ansi.Strip(raw)
	if !strings.Contains(plain, "50 ") {
		t.Fatalf("last line not scrolled into view:\n%s", plain)
	}
	// The cursor sits after 49 x's: the view scrolled right, so the start of
	// the line ("line ") is out of view on the last row.
	if strings.Contains(ansi.Strip(lines[3]), "line") {
		t.Fatalf("horizontal scroll did not follow the cursor:\n%s", plain)
	}
}

func TestHighlightUsesThemeColours(t *testing.T) {
	m := newModel()
	m.SetDialect("postgresql")
	m.SetValue("select 'a' -- c")
	raw := m.View(60, 1, false)
	if !strings.Contains(raw, "\x1b[") {
		t.Fatalf("no styling in the highlighted view: %q", raw)
	}
	if ansi.Strip(raw) == raw {
		t.Fatal("highlighting produced no SGR")
	}
	m2 := newModel()
	m2.SetDialect("no-such-dialect")
	m2.SetValue("select 1")
	if !strings.Contains(ansi.Strip(m2.View(60, 1, false)), "select 1") {
		t.Fatal("unknown dialect lost the text")
	}
}

func TestClickPlacesCursor(t *testing.T) {
	m := newModel()
	m.SetValue("select 1\nfrom t")
	gw := m.gutterWidth(60)
	m, _ = m.Update(tea.MouseClickMsg{X: gw + 2, Y: 1, Button: tea.MouseLeft})
	if got := m.Cursor(); got != len("select 1\n")+2 {
		t.Fatalf("Cursor after click = %d", got)
	}
}

func TestTabsBecomeSpaces(t *testing.T) {
	m := newModel()
	m, _ = m.Update(tea.PasteMsg{Content: "\tselect\t1"})
	if got := m.Value(); got != "  select  1" {
		t.Fatalf("paste Value = %q", got)
	}
	m.SetValue("a\tb")
	if got := m.Value(); got != "a  b" {
		t.Fatalf("SetValue Value = %q", got)
	}
	m.InsertAt(0, "\t")
	if got := m.Value(); got != "  a  b" {
		t.Fatalf("InsertAt Value = %q", got)
	}
	if strings.Contains(ansi.Strip(m.View(40, 1, true)), `\t`) {
		t.Fatal("a tab is drawn as an escape")
	}
}

func TestWheelScrolls(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(40, 3)
	m.SetValue(strings.Repeat("x\n", 20) + "x")
	m.SetCursor(0)
	m, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.Cursor() == 0 || m.rowOff == 0 {
		t.Fatalf("wheel down: cursor %d rowOff %d", m.Cursor(), m.rowOff)
	}
	m, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.Cursor() != 0 {
		t.Fatalf("wheel up: cursor %d", m.Cursor())
	}
}

func TestWideClusterAtTheScrollEdge(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(5, 1) // too narrow for a gutter
	m.SetValue("a日本語日本語x")
	m.SetCursor(0)
	for i := 0; i < 10; i++ {
		raw := m.View(5, 1, true)
		tuitest.NoInjectedControls(t, raw)
		if w := ansi.StringWidth(raw); w != 5 {
			t.Fatalf("cursor %d: view is %d wide: %q", m.Cursor(), w, ansi.Strip(raw))
		}
		m = press(t, m, "right")
	}
}

func TestVersionChangesOnlyOnEdits(t *testing.T) {
	m := newModel()
	v0 := m.Version()
	m = typ(m, "ab")
	if m.Version() == v0 {
		t.Fatal("typing did not change the version")
	}
	v1 := m.Version()
	m = press(t, m, "left", "home", "end")
	if m.Version() != v1 {
		t.Fatal("moving the cursor changed the version")
	}
	m.SetValue("x")
	if m.Version() == v1 {
		t.Fatal("SetValue did not change the version")
	}
}

func TestReplaceRange(t *testing.T) {
	m := newModel()
	m.SetValue("select us from t\nwhere x")
	m.ReplaceRange(7, 9, "users")
	if got := m.Value(); got != "select users from t\nwhere x" {
		t.Fatalf("Value = %q", got)
	}
	if m.Cursor() != 12 {
		t.Fatalf("cursor = %d, want 12 (after the insert)", m.Cursor())
	}
	// Across a line break, clamped and swapped ends.
	m.ReplaceRange(len(m.Value())+5, 17, "")
	if got := m.Value(); got != "select users from" {
		t.Fatalf("Value = %q", got)
	}
	// A replacement past the cap changes nothing.
	before := m.Value()
	m.ReplaceRange(0, 0, strings.Repeat("x", MaxBytes))
	if m.Value() != before {
		t.Fatal("an over-cap replacement was applied")
	}
}

func TestCursorPos(t *testing.T) {
	m := New(theme.Dark(), keymap.Default()).Resize(40, 3)
	m.SetValue("a\nb\nc\nd\nxyz")
	m.SetCursor(len("a\nb\nc\nd\nxy"))
	x, y := m.CursorPos(40, 3)
	// 5 lines: a gutter of 3 ("NN " min 3), the view scrolled so line 5 is the last row.
	if y != 2 || x != m.gutterWidth(40)+2 {
		t.Fatalf("CursorPos = (%d, %d), want (%d, 2)", x, y, m.gutterWidth(40)+2)
	}
}

func TestCursorLine(t *testing.T) {
	m := newModel()
	m.SetValue("select 1\nfrom users")
	m.SetCursor(len("select 1\nfrom us"))
	line, col, start := m.CursorLine()
	if line != "from users" || col != 7 || start != 9 {
		t.Fatalf("CursorLine = %q, %d, %d", line, col, start)
	}
}

func TestCursorInLiteral(t *testing.T) {
	tests := []struct {
		dialect, text string
		want          bool
	}{
		{"sqlite", "select us", false},
		{"sqlite", "select 'abc\nde", true},
		{"sqlite", "select 'x' an", false},
		{"sqlite", "select 1 -- us", true},
		{"sqlite", "select /* a\nus", true},
		{"sqlite", `select "Us`, false}, // a quoted name completes
		{"postgresql", `select "Us`, false},
		{"postgresql", "select e'ab", true},
		{"mysql", "select 1 # us", true},
		{"mysql", `select "us`, true}, // a string in mysql
		{"mysql", "select `us", false},
	}
	for _, tt := range tests {
		m := newModel()
		m.SetDialect(tt.dialect)
		m.SetValue(tt.text)
		if _, known := m.CursorInLiteral(); known {
			t.Fatalf("%s %q: known before any frame lexed this version", tt.dialect, tt.text)
		}
		m.View(60, 10, true)
		in, known := m.CursorInLiteral()
		if !known || in != tt.want {
			t.Errorf("%s %q: CursorInLiteral = %v, %v; want %v, true", tt.dialect, tt.text, in, known, tt.want)
		}
	}
}

func TestJSONDialectUsesTheJSONLexer(t *testing.T) {
	if got := lexerFor("json").Config().Name; got != "JSON" {
		t.Fatalf("lexerFor(json) = %q, want JSON", got)
	}
	m := newModel()
	m.SetDialect("json")
	m.SetValue(`{"a": [1, "x"]}`)
	raw := m.View(60, 1, false)
	if ansi.Strip(raw) == raw || !strings.Contains(ansi.Strip(raw), `{"a": [1, "x"]}`) {
		t.Fatalf("json view not highlighted or lost text: %q", raw)
	}
}

// Every driver dialect gets the closest lexer chroma has; the JSON drivers
// get the JSON lexer.
func TestDialectLexers(t *testing.T) {
	for dialect, want := range map[string]string{
		"tsql": "Transact-SQL", "cql": "Cassandra CQL", "duckdb": "PostgreSQL SQL dialect",
		"googlesql": "SQL", "oracle": "SQL", "partiql": "SQL", "snowflake": "SQL", "vertica": "SQL", "cosmos-sql": "SQL",
		"elasticsearch": "JSON", "opensearch": "JSON", "firestore-json": "JSON", "pinecone": "JSON",
		"weaviate": "JSON", "chroma": "JSON", "milvus": "JSON", "mongodb": "JSON", "qdrant": "JSON",
	} {
		if got := lexerFor(dialect).Config().Name; got != want {
			t.Errorf("lexerFor(%s) = %q, want %q", dialect, got, want)
		}
	}
}
