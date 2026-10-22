package detail

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
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

func key(m Model, ks ...string) (Model, []tea.Cmd) {
	var cmds []tea.Cmd
	for _, k := range ks {
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
		case "bottom":
			msg = tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "space":
			msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		var c tea.Cmd
		m, c = m.Update(msg)
		cmds = append(cmds, c)
	}
	return m, cmds
}

// flatten runs cmds and collects every message, expanding batches.
func flatten(cmds ...tea.Cmd) []tea.Msg {
	var out []tea.Msg
	for _, c := range cmds {
		if c == nil {
			continue
		}
		msg := c()
		if b, ok := msg.(tea.BatchMsg); ok {
			out = append(out, flatten(b...)...)
			continue
		}
		if msg != nil {
			out = append(out, msg)
		}
	}
	return out
}

// clipboard returns the text of the SetClipboard message in msgs.
func clipboard(t *testing.T, msgs []tea.Msg) string {
	t.Helper()
	for _, m := range msgs {
		if v := reflect.ValueOf(m); v.Kind() == reflect.String && v.Type().Name() == "setClipboardMsg" {
			return v.String()
		}
	}
	t.Fatalf("no clipboard message in %#v", msgs)
	return ""
}

func view(m Model, w, h int) string { return ansi.Strip(m.View(w, h)) }

func newModel(cols []driver.Column, row driver.Row) Model {
	return New(cols, row, theme.Dark(), keymap.Default()).Resize(60, 14)
}

func TestScalarRowKeyValue(t *testing.T) {
	cols := []driver.Column{{Name: "id"}, {Name: "name"}, {Name: "age"}, {Name: "note"}}
	row := driver.Row{driver.Int(1), driver.String("Ada\x1b]0;x\x07\x1b[31m\nL"), driver.Null(), driver.String("NULL")}
	m := newModel(cols, row)
	v := view(m, 60, 8)
	for _, want := range []string{"id   │ 1", `name │ Ada\x1b]0;x\x07\x1b[31m L`, "age  │ NULL", "note │ NULL"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in:\n%s", want, v)
		}
	}
	tuitest.NoInjectedControls(t, m.View(60, 8))
}

func peopleModel() Model {
	return newModel(drivertest.People.Columns, drivertest.People.Rows[0])
}

func TestJSONTreeToggle(t *testing.T) {
	m := peopleModel()
	m, _ = key(m, "down", "down", "down", "down", "down", "down", "down") // tags
	if v := view(m, 60, 14); strings.Contains(v, "admin") || !strings.Contains(v, "▸") {
		t.Fatalf("tags should start collapsed:\n%s", v)
	}
	m, _ = key(m, "enter")
	v := view(m, 60, 14)
	if !strings.Contains(v, `[0]: "admin"`) || !strings.Contains(v, `[1]: "beta"`) || !strings.Contains(v, "▾") {
		t.Fatalf("enter should expand:\n%s", v)
	}
	m, _ = key(m, "space")
	if strings.Contains(view(m, 60, 14), "admin") {
		t.Fatal("space should collapse again")
	}
	m, _ = key(m, "right")
	if !strings.Contains(view(m, 60, 14), "admin") {
		t.Fatal("right should expand")
	}
	m, _ = key(m, "left")
	if strings.Contains(view(m, 60, 14), "admin") {
		t.Fatal("left should collapse")
	}
	// Left on a leaf moves to its parent.
	m, _ = key(m, "right", "down", "down", "left")
	_, cmds := key(m, "y")
	if got := clipboard(t, flatten(cmds...)); got != `["admin","beta"]` {
		t.Fatalf("left on a leaf should select the parent; y copied %q", got)
	}
}

func TestObjectWithNestedList(t *testing.T) {
	obj := driver.ObjectValue(
		driver.Member{Key: "z", Value: driver.Int(1)},
		driver.Member{Key: "a", Value: driver.List(driver.String("x"), driver.ObjectValue(driver.Member{Key: "k", Value: driver.Bool(true)}))},
	)
	m := newModel([]driver.Column{{Name: "doc"}}, driver.Row{obj})
	if v := view(m, 60, 10); !strings.Contains(v, "{2 keys}") {
		t.Fatalf("summary missing:\n%s", v)
	}
	m, _ = key(m, "enter", "down", "down", "enter") // expand doc, move to a, expand
	v := view(m, 60, 10)
	zi, ai := strings.Index(v, "z: 1"), strings.Index(v, "a: ")
	if zi < 0 || ai < 0 || zi > ai {
		t.Fatalf("members should keep insertion order:\n%s", v)
	}
	if !strings.Contains(v, `[0]: x`) && !strings.Contains(v, `[0]: "x"`) {
		t.Fatalf("list not expanded:\n%s", v)
	}
	m, _ = key(m, "down", "down", "enter") // the nested object
	if v := view(m, 60, 10); !strings.Contains(v, "k: true") {
		t.Fatalf("nested object not expanded:\n%s", v)
	}
	_, cmds := key(m, "y")
	if got := clipboard(t, flatten(cmds...)); got != `{"k":true}` {
		t.Fatalf("copied %q", got)
	}
}

func TestJSONKeyOrderPreserved(t *testing.T) {
	m := newModel([]driver.Column{{Name: "j"}}, driver.Row{driver.JSON([]byte(`{"z": 1, "a": [1, 2.50, null, "s"], "m": {}}`))})
	m, _ = key(m, "enter")
	v := view(m, 60, 12)
	zi, ai, mi := strings.Index(v, "z: 1"), strings.Index(v, "a: "), strings.Index(v, "m: ")
	if zi < 0 || zi >= ai || ai >= mi {
		t.Fatalf("key order lost:\n%s", v)
	}
	_, cmds := key(m, "y")
	if got := clipboard(t, flatten(cmds...)); got != `{"z":1,"a":[1,2.50,null,"s"],"m":{}}` {
		t.Fatalf("copied %q", got)
	}
}

func TestInvalidJSONIsScalar(t *testing.T) {
	m := newModel([]driver.Column{{Name: "j"}}, driver.Row{driver.JSON([]byte(`{oops`))})
	m, _ = key(m, "enter", "right")
	if v := view(m, 60, 6); !strings.Contains(v, "{oops") {
		t.Fatalf("invalid JSON should show as text:\n%s", v)
	}
}

func TestDeepAndHugeJSON(t *testing.T) {
	deep := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	m := newModel([]driver.Column{{Name: "j"}}, driver.Row{driver.JSON([]byte(deep))})
	for range 100 {
		m, _ = key(m, "right", "down")
	}
	_ = view(m, 40, 10)
	_, cmds := key(m, "up", "y")
	_ = cmds

	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 5000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("1")
	}
	sb.WriteByte(']')
	m = newModel([]driver.Column{{Name: "j"}}, driver.Row{driver.JSON([]byte(sb.String()))})
	m, _ = key(m, "enter")
	v := view(m, 60, 14)
	if !strings.Contains(v, "[0]: 1") {
		t.Fatalf("children missing:\n%s", v)
	}
	m, _ = key(m, "bottom")
	if v := view(m, 60, 6); !strings.Contains(v, "4000 more") {
		t.Fatalf("large containers should cap rendered children:\n%s", v)
	}
	_, cmds = key(m, "y")
	_ = cmds
}

func TestCopyEmitsClipboard(t *testing.T) {
	m := peopleModel()
	m, _ = key(m, "down") // name
	_, cmds := key(m, "y")
	msgs := flatten(cmds...)
	if got := clipboard(t, msgs); got != drivertest.People.Rows[0][1].String() {
		t.Fatalf("y copied %q", got)
	}
	var copied *CopiedMsg
	for _, msg := range msgs {
		if c, ok := msg.(CopiedMsg); ok {
			copied = &c
		}
	}
	if copied == nil || copied.What == "" {
		t.Fatalf("y should emit CopiedMsg, got %#v", msgs)
	}

	_, cmds = key(m, "Y")
	got := clipboard(t, flatten(cmds...))
	var want strings.Builder
	want.WriteByte('{')
	for i, c := range drivertest.People.Columns {
		if i > 0 {
			want.WriteByte(',')
		}
		want.WriteString(`"` + c.Name + `":`)
		b, _ := drivertest.People.Rows[0][i].MarshalJSON()
		want.Write(b)
	}
	want.WriteByte('}')
	if got != want.String() {
		t.Fatalf("Y copied\n%s\nwant\n%s", got, want.String())
	}
}

func TestEscCloses(t *testing.T) {
	_, cmds := key(peopleModel(), "esc")
	msgs := flatten(cmds...)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	if _, ok := msgs[0].(ClosedMsg); !ok {
		t.Fatalf("esc should close, got %T", msgs[0])
	}
}

func TestMovementClamps(t *testing.T) {
	m := peopleModel()
	m, _ = key(m, "up", "up")
	m = func() Model {
		for range 40 {
			m, _ = key(m, "down")
		}
		return m
	}()
	if v := view(m, 60, 14); !strings.Contains(v, "tags") {
		t.Fatalf("last row should be visible:\n%s", v)
	}
}

func TestDetailViewTiny(t *testing.T) {
	m := peopleModel()
	m, _ = key(m, "down", "down", "down", "down", "down", "down", "down", "enter")
	for _, sz := range [][2]int{{-1, -1}, {0, 0}, {1, 1}, {3, 1}, {20, 5}, {5, 20}} {
		_ = m.Resize(sz[0], sz[1]).View(sz[0], sz[1])
	}
	empty := New(nil, nil, theme.Dark(), keymap.Default())
	_ = empty.View(20, 5)
	_, _ = key(empty, "down", "y", "Y", "enter", "left", "right")
}

func TestGoldenPeopleRow(t *testing.T) {
	m := newModel(drivertest.People.Columns, drivertest.People.Rows[0]).Resize(60, 12)
	m, _ = key(m, "down", "down", "down", "down", "down", "down", "down", "enter")
	golden(t, "detail_people_row.golden", view(m, 60, 12))
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

func TestLabelsAreEscaped(t *testing.T) {
	const k1, k2 = "\x1b]0;x\x07", "a\nb"
	js := `{"` + `\u001b]0;x\u0007` + `":1,"a\nb":2}`
	obj := driver.ObjectValue(driver.Member{Key: k1, Value: driver.Int(1)}, driver.Member{Key: k2, Value: driver.Int(2)})
	for name, v := range map[string]driver.Value{"json": driver.JSON([]byte(js)), "object": obj} {
		t.Run(name, func(t *testing.T) {
			m := newModel([]driver.Column{{Name: "co\x1bl"}}, driver.Row{v})
			m, _ = key(m, "enter")
			raw := m.View(60, 5)
			plain := ansi.Strip(raw)
			tuitest.NoInjectedControls(t, raw)
			if !strings.Contains(plain, `\x1b]0;x\x07`) || !strings.Contains(plain, "a b") {
				t.Fatalf("want escaped labels in view:\n%s", plain)
			}
			if n := len(strings.Split(plain, "\n")); n != 5 {
				t.Fatalf("view has %d lines, want 5:\n%s", n, plain)
			}
			_, cmds := key(m, "y")
			if got := clipboard(t, flatten(cmds...)); !strings.Contains(got, `\u001b]0;x\u0007`) && !strings.Contains(got, k1) {
				t.Fatalf("y lost the raw key: %q", got)
			}
			_, cmds = key(m, "Y")
			if got := clipboard(t, flatten(cmds...)); !strings.Contains(got, `co\u001bl`) {
				t.Fatalf("Y lost the raw column name: %q", got)
			}
		})
	}
}

func TestViewRendersOnlyWindow(t *testing.T) {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range 1000 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("1")
	}
	sb.WriteByte(']')
	cols := make([]driver.Column, 5)
	row := make(driver.Row, 5)
	for i := range cols {
		cols[i] = driver.Column{Name: string(rune('a' + i))}
		row[i] = driver.JSON([]byte(sb.String()))
	}
	m := newModel(cols, row)
	for _, r := range m.roots {
		r.expanded = true
	}
	m.flatten()
	m.cursor = len(m.lines) - 1
	if len(m.lines) < 5000 {
		t.Fatalf("only %d lines expanded", len(m.lines))
	}
	m = m.Resize(60, 10)
	rendered := 0
	m.onRender = func() { rendered++ }
	_ = m.View(60, 10)
	if rendered > 10 {
		t.Fatalf("rendered %d lines for a 10-line window of %d", rendered, len(m.lines))
	}
}

func TestWheelMovesCursor(t *testing.T) {
	cols := make([]driver.Column, 10)
	row := make(driver.Row, 10)
	for i := range cols {
		cols[i] = driver.Column{Name: string(rune('a' + i))}
		row[i] = driver.Int(int64(i))
	}
	m := newModel(cols, row).Resize(60, 4)
	m, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.cursor != wheelStep {
		t.Fatalf("cursor = %d after one notch down, want %d", m.cursor, wheelStep)
	}
	if v := view(m, 60, 4); !strings.Contains(v, "d") {
		t.Fatalf("view did not follow:\n%s", v)
	}
	for i := 0; i < 10; i++ {
		m, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	if m.cursor != 9 {
		t.Fatalf("cursor = %d, want clamped to 9", m.cursor)
	}
	for i := 0; i < 10; i++ {
		m, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	}
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.cursor)
	}
}

func TestCopyNullCellIsEmpty(t *testing.T) {
	cols := []driver.Column{{Name: "a"}, {Name: "b"}}
	m := newModel(cols, driver.Row{driver.Int(1), driver.Null()})
	m, _ = key(m, "down") // b, a SQL NULL
	_, cmds := key(m, "y")
	if got := clipboard(t, flatten(cmds...)); got != "" {
		t.Errorf("y on NULL copied %q, want the empty string", got)
	}
	_, cmds = key(m, "Y")
	if got := clipboard(t, flatten(cmds...)); got != `{"a":1,"b":null}` {
		t.Errorf("Y copied %q, want JSON null kept", got)
	}
	if got := CellText(driver.Null()); got != "" {
		t.Errorf("CellText(NULL) = %q, want empty", got)
	}
}

func TestCellTextCopiesJSONAsIs(t *testing.T) {
	raw := `{"k": "é", "n": [1, 2.50]}`
	want := `{"k":"é","n":[1,2.50]}`
	if got := CellText(driver.JSON([]byte(raw))); got != want {
		t.Errorf("CellText(JSON) = %q, want %q", got, want)
	}
	m := newModel([]driver.Column{{Name: "j"}}, driver.Row{driver.JSON([]byte(raw))})
	_, cmds := key(m, "y")
	if got := clipboard(t, flatten(cmds...)); got != want {
		t.Errorf("y in the detail copied %q, want %q", got, want)
	}
	if got := CellText(driver.JSON([]byte(`"s"`))); got != `"s"` {
		t.Errorf("CellText(JSON string) = %q", got)
	}
	if got := CellText(driver.JSON([]byte(`{bad`))); got != `{bad` {
		t.Errorf("CellText(invalid JSON) = %q, want the text as is", got)
	}
	obj := driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)})
	if got := CellText(obj); got != `{"a":1}` {
		t.Errorf("CellText(Object) = %q", got)
	}
	if got := CellText(driver.Int(7)); got != "7" {
		t.Errorf("CellText(Int) = %q", got)
	}
}

func TestVimGG(t *testing.T) {
	cols := []driver.Column{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	row := driver.Row{driver.Int(1), driver.Int(2), driver.Int(3)}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	m := New(cols, row, theme.Dark(), keymap.Vim()).Resize(60, 10).WithClock(clk.now)
	m, _ = key(m, "j", "j")
	m, _ = key(m, "g")
	if m.cursor != 2 {
		t.Fatalf("a single g moved the cursor to %d", m.cursor)
	}
	m, _ = key(m, "g")
	if m.cursor != 0 {
		t.Fatalf("gg: cursor = %d, want 0", m.cursor)
	}
	m, _ = key(m, "j", "j", "g")
	clk.t = clk.t.Add(501 * time.Millisecond)
	m, _ = key(m, "g")
	if m.cursor != 2 {
		t.Fatalf("late gg moved the cursor to %d, want 2", m.cursor)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// bigJSON is a JSON array just over the async threshold.
func bigJSON() driver.Value {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; sb.Len() <= asyncThreshold; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"x"`)
	}
	sb.WriteByte(']')
	return driver.JSON([]byte(sb.String()))
}

func TestHugeValueBuildsAsynchronously(t *testing.T) {
	cols := []driver.Column{{Name: "id"}, {Name: "blob"}}
	m := newModel(cols, driver.Row{driver.Int(1), bigJSON()})
	if v := view(m, 60, 6); !strings.Contains(v, "loading…") || !strings.Contains(v, "id") {
		t.Fatalf("a huge value should show a loading line while the rest is usable:\n%s", v)
	}
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init should return the build command")
	}
	msgs := flatten(cmd)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	m, _ = m.Update(msgs[0])
	v := view(m, 60, 6)
	if strings.Contains(v, "loading…") || !strings.Contains(v, "items]") {
		t.Fatalf("the built tree should replace the loading line:\n%s", v)
	}
	// And it works like any other tree.
	m, _ = key(m, "down", "enter")
	if v := view(m, 60, 6); !strings.Contains(v, `[0]: "x"`) {
		t.Fatalf("expanding the built tree failed:\n%s", v)
	}
}

func TestSmallValuesBuildSynchronously(t *testing.T) {
	m := peopleModel()
	if cmd := m.Init(); cmd != nil {
		t.Fatal("nothing is pending for ordinary rows")
	}
	if strings.Contains(view(m, 60, 14), "loading…") {
		t.Fatal("ordinary rows must not show a loading line")
	}
}

func TestHugeBuildCanBeCancelled(t *testing.T) {
	m := newModel([]driver.Column{{Name: "blob"}}, driver.Row{bigJSON()})
	cmd := m.Init()
	m.Cancel()
	msgs := flatten(cmd)
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	built, ok := msgs[0].(BuiltMsg)
	if !ok || !errors.Is(built.err, context.Canceled) {
		t.Fatalf("a cancelled build should report context.Canceled, got %#v", msgs[0])
	}
	m, _ = m.Update(built)
	if !strings.Contains(view(m, 60, 6), "loading…") {
		t.Fatal("a cancelled build must not replace the loading line")
	}
}

func TestBuiltFromAnotherDetailIsIgnored(t *testing.T) {
	a := newModel([]driver.Column{{Name: "blob"}}, driver.Row{bigJSON()})
	b := newModel([]driver.Column{{Name: "blob"}}, driver.Row{bigJSON()})
	msgs := flatten(a.Init())
	b, _ = b.Update(msgs[0])
	if !strings.Contains(view(b, 60, 6), "loading…") {
		t.Fatal("a result built for another detail must be dropped")
	}
}

func TestOnlyContainersGoAsync(t *testing.T) {
	big := strings.Repeat("x", asyncThreshold+1)
	for name, v := range map[string]driver.Value{
		"string": driver.String(big),
		"bytes":  driver.Bytes([]byte(big)),
	} {
		if isHuge(v) {
			t.Errorf("a huge %s is bounded text and must stay synchronous", name)
		}
	}
	for name, v := range map[string]driver.Value{
		"list of bytes":   driver.List(driver.Bytes([]byte(big))),
		"list of strings": driver.List(driver.String(big)),
		"object":          driver.ObjectValue(driver.Member{Key: "k", Value: driver.Bytes([]byte(big))}),
	} {
		if !isHuge(v) {
			t.Errorf("%s over the threshold should build asynchronously", name)
		}
	}
	m := newModel([]driver.Column{{Name: "s"}}, driver.Row{driver.String(big)})
	if m.Init() != nil {
		t.Error("a big string needs no background build")
	}
}
