package grid

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

func mem(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }

func TestObjectAndListCellsPreviewOnOneLine(t *testing.T) {
	str := driver.String
	tests := []struct {
		name string
		v    driver.Value
		w    int
		want string
	}{
		{"object", driver.ObjectValue(mem("name", str("Ada")), mem("tags", driver.List(str("a"), str("b")))), 40, `{name: "Ada", tags: [2]}`},
		{"nested object", driver.ObjectValue(mem("a", driver.ObjectValue(mem("b", driver.Int(1)))), mem("n", driver.Null())), 40, `{a: {…}, n: null}`},
		{"empty containers", driver.ObjectValue(mem("o", driver.ObjectValue()), mem("l", driver.List())), 40, `{o: {}, l: []}`},
		{"list", driver.List(str("j1"), driver.Int(2), driver.Bool(true), driver.ObjectValue(mem("x", driver.Int(1)))), 40, `["j1", 2, true, {…}]`},
		{"empty object", driver.ObjectValue(), 40, `{}`},
		{"cut", driver.ObjectValue(mem("name", str("Ada Lovelace")), mem("city", str("London"))), 16, `{name: "Ada Lov…`},
		{"escaped", driver.ObjectValue(mem("k\x1b[31m", str("v\x07\nx"))), 40, `{k\x1b[31m: "v\x07 x"}`},
	}
	m := New(theme.Dark(), keymap.Default())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, kind := m.cellText(driver.Row{tt.v}, 0, tt.w)
			if got != tt.want || kind != cellPlain {
				t.Errorf("cellText = %q (%v), want %q", got, kind, tt.want)
			}
			tuitest.NoInjectedControls(t, got)
		})
	}
}

// A preview reads only what it shows. The check compares objects of 20k and
// 200k members instead of using a wall-clock limit, so a slow or
// race-instrumented runner does not fail it: bounded work takes about as
// long for both, a walk over every member ten times longer.
func TestPreviewOfAHugeObjectIsBounded(t *testing.T) {
	object := func(n int) driver.Value {
		ms := make([]driver.Member, n)
		for i := range ms {
			ms[i] = mem(fmt.Sprintf("k%d", i), driver.String(strings.Repeat("x", 50)))
		}
		return driver.ObjectValue(ms...)
	}
	small, large := object(20000), object(200000)
	if got := previewText(large, 4*maxColWidth); len(got) > 8*maxColWidth {
		t.Fatalf("preview is %d bytes long", len(got))
	}
	ts, tl := previewTime(small), previewTime(large)
	if tl > 4*ts+10*time.Millisecond {
		t.Fatalf("preview took %v at 20k members and %v at 200k: not bounded", ts, tl)
	}
}

// previewTime is the fastest of three previews of v.
func previewTime(v driver.Value) time.Duration {
	best := time.Duration(math.MaxInt64)
	for range 3 {
		start := time.Now()
		previewText(v, 4*maxColWidth)
		best = min(best, time.Since(start))
	}
	return best
}

func TestFormatHookRendersEscapedText(t *testing.T) {
	m := New(theme.Dark(), keymap.Default())
	m.SetFormat(func(cols []driver.Column, row driver.Row, col int) (string, bool) {
		if cols[col].Name != "ttl" {
			return "", false
		}
		return "∞\x1b[31m", true
	})
	m.OpenStatic([]driver.Column{{Name: "key"}, {Name: "ttl"}}, []driver.Row{{driver.String("a"), driver.Int(-1)}})
	raw := m.View(60, 4, false)
	tuitest.NoInjectedControls(t, raw)
	if v := ansi.Strip(raw); !strings.Contains(v, `∞\x1b[31m`) || strings.Contains(v, "-1") {
		t.Fatalf("view:\n%s", v)
	}
}

// docsFake holds one schemaless collection: 200 documents with fields _id,
// a and b, then 20 with _id, a and c, so the second page brings a new
// column and lacks b.
func docsFake() (*drivertest.Fake, driver.CollectionRef) {
	f := drivertest.NewFake()
	var docs []driver.Object
	for i := range 220 {
		d := driver.Object{mem("_id", driver.Int(int64(i))), mem("a", driver.Int(int64(i)))}
		if i < 200 {
			b := driver.String(fmt.Sprintf("b%d", i))
			if i == 1 {
				b = driver.Null()
			}
			d = append(d, mem("b", b))
		} else {
			d = append(d, mem("c", driver.String(fmt.Sprintf("c%d", i))))
		}
		docs = append(docs, d)
	}
	f.AddDocuments("main", "docs", docs)
	return f, driver.CollectionRef{Namespace: "main", Name: "docs"}
}

func colNames(cols []driver.Column) string {
	var out []string
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return strings.Join(out, ",")
}

func TestColumnsAreTheUnionOfThePagesAndAbsentReadsAsADash(t *testing.T) {
	f, ref := docsFake()
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false))
	if got := colNames(h.m.Columns()); got != "_id,a,b" {
		t.Fatalf("page 0 columns = %s", got)
	}
	h.keys("G") // the bottom of page 0: page 1 is fetched
	if st := h.m.Status(); st.Loaded != 220 || !st.Exact {
		t.Fatalf("status = %+v", st)
	}
	if got := colNames(h.m.Columns()); got != "_id,a,b,c" {
		t.Fatalf("union = %s, want _id,a,b,c", got)
	}
	// A page-1 row keeps each value under its own column.
	h.m.row = 205
	row, _ := h.m.CurrentRow()
	if len(row) != 4 || row[3].String() != "c205" || row[1].String() != "205" {
		t.Fatalf("row 205 = %v", row)
	}
	for _, c := range []struct {
		row, col int
		want     cellKind
	}{
		{0, 3, cellAbsent},   // page 0 has no c
		{205, 2, cellAbsent}, // page 1 has no b
		{1, 2, cellNull},     // an explicit null
		{0, 2, cellPlain},
		{205, 3, cellPlain},
	} {
		r, _ := h.m.rowAt(c.row)
		if _, kind := h.m.cellAt(c.row, r, c.col, 20); kind != c.want {
			t.Errorf("row %d col %d: kind %v, want %v", c.row, c.col, kind, c.want)
		}
	}
	h.keys("g", "g")
	v := ansi.Strip(h.m.View(60, 6, false))
	lines := strings.Split(v, "\n")
	if !strings.Contains(lines[1], "—") || !strings.Contains(lines[2], "NULL") {
		t.Fatalf("absent and NULL not told apart:\n%s", v)
	}
}

func TestReopenedPageZeroResetsTheUnion(t *testing.T) {
	f, ref := docsFake()
	h := newHarness(t)
	h.open(NewSource(openConn(t, f, false), "fake", ref, false))
	h.keys("G")
	if got := colNames(h.m.Columns()); got != "_id,a,b,c" {
		t.Fatalf("union = %s", got)
	}
	h.exec(h.m.Refresh())
	if got := colNames(h.m.Columns()); !strings.HasPrefix(got, "_id,a,b") {
		t.Fatalf("after refresh = %s", got)
	}
	r, _ := h.m.rowAt(0)
	if r[1].String() != "0" || r[2].String() != "b0" {
		t.Fatalf("row 0 after refresh = %v", r)
	}
}

func TestDuplicateColumnNamesMatchByOccurrence(t *testing.T) {
	m := New(theme.Dark(), keymap.Default())
	m.cols = []driver.Column{{Name: "id"}, {Name: "id"}, {Name: "x"}}
	idx := m.unionIndex([]driver.Column{{Name: "id"}, {Name: "y"}, {Name: "id"}})
	if fmt.Sprint(idx) != "[0 3 1]" || colNames(m.cols) != "id,id,x,y" {
		t.Fatalf("idx = %v, cols = %s", idx, colNames(m.cols))
	}
}

// A hidden column is not drawn and the cursor skips it, but it stays in
// Columns and in the rows.
func TestHiddenColumnsAreSkipped(t *testing.T) {
	m := New(theme.Dark(), keymap.Default())
	m.SetHidden(func(c driver.Column) bool { return c.Name == "_truncated" })
	cols := []driver.Column{{Name: "id"}, {Name: "_truncated"}, {Name: "value"}}
	m.OpenStatic(cols, []driver.Row{{driver.Int(1), driver.Bool(true), driver.String("abc")}})
	m = m.Resize(40, 3)
	raw := ansi.Strip(m.View(40, 3, true))
	if strings.Contains(raw, "_truncated") || strings.Contains(raw, "true") {
		t.Fatalf("hidden column drawn:\n%s", raw)
	}
	if !strings.Contains(raw, "id") || !strings.Contains(raw, "value") || !strings.Contains(raw, "abc") {
		t.Fatalf("visible columns missing:\n%s", raw)
	}
	if len(m.Columns()) != 3 {
		t.Fatalf("Columns() = %d, want all 3", len(m.Columns()))
	}
	m = press(m, "right")
	if _, c := m.Cursor(); c != 2 {
		t.Fatalf("right from id: col = %d, want 2 (past the hidden one)", c)
	}
	m = press(m, "right")
	if _, c := m.Cursor(); c != 2 {
		t.Fatalf("right at the last visible column: col = %d", c)
	}
	m = press(m, "left")
	if _, c := m.Cursor(); c != 0 {
		t.Fatalf("left from value: col = %d, want 0", c)
	}
	m.SetColumns([]driver.Column{{Name: "value"}, {Name: "_truncated"}})
	m = press(m, "end")
	if _, c := m.Cursor(); c != 0 {
		t.Fatalf("end with a hidden last column: col = %d, want 0", c)
	}
}

// A header mark (a wide-column key badge) follows the column name, counts
// in the column width, is escaped, and clears with nil.
func TestHeaderMark(t *testing.T) {
	m := New(theme.Dark(), keymap.Default())
	m.OpenStatic([]driver.Column{{Name: "tenant"}, {Name: "ts"}, {Name: "kind"}},
		[]driver.Row{{driver.String("acme"), driver.Int(1), driver.String("login")}})
	m = m.Resize(80, 5)
	m.SetHeaderMark(func(c driver.Column) string {
		switch c.Name {
		case "tenant":
			return "[P]"
		case "ts":
			return "[C↓]\x1b[31m"
		}
		return ""
	})
	head := strings.Split(ansi.Strip(m.View(80, 5, true)), "\n")[0]
	for _, want := range []string{"tenant [P]", `ts [C↓]\x1b[31m`, "kind"} {
		if !strings.Contains(head, want) {
			t.Errorf("header %q lacks %q", head, want)
		}
	}
	if strings.Contains(head, "kind [") {
		t.Errorf("an empty mark adds nothing: %q", head)
	}
	tuitest.NoInjectedControls(t, m.View(80, 5, true))
	if w := m.widths[0]; w < len("tenant [P]") {
		t.Errorf("tenant width %d does not fit its mark", w)
	}
	m.SetHeaderMark(nil)
	if head := strings.Split(ansi.Strip(m.View(80, 5, true)), "\n")[0]; strings.Contains(head, "[P]") {
		t.Errorf("nil must clear the marks: %q", head)
	}
}
