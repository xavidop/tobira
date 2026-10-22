package detail

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

func m(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }

// docModel is the detail of document d4 of the fixture, the most nested.
func docModel(t *testing.T) Model {
	t.Helper()
	d := drivertest.Documents()[3]
	cols := make([]driver.Column, len(d))
	row := make(driver.Row, len(d))
	for i, mem := range d {
		cols[i], row[i] = driver.Column{Name: mem.Key}, mem.Value
	}
	return newModel(cols, row)
}

func TestExpandAllAndCollapseAll(t *testing.T) {
	md := docModel(t)
	if v := view(md, 60, 10); strings.Contains(v, "deeper") {
		t.Fatalf("starts expanded:\n%s", v)
	}
	md, _ = key(md, "]")
	v := view(md, 60, 10)
	for _, want := range []string{"▾ {1 key}", "deep", "deeper", "value: 1"} {
		if !strings.Contains(v, want) {
			t.Fatalf("expand-all lacks %q:\n%s", want, v)
		}
	}
	md, _ = key(md, "bottom")
	md, _ = key(md, "[")
	v = view(md, 60, 10)
	if strings.Contains(v, "deeper") || !strings.Contains(v, "▸ {1 key}") {
		t.Fatalf("collapse-all left nodes open:\n%s", v)
	}
	// The cursor lands on the root of the node it was on.
	if n := md.selected(); n == nil || n.depth != 0 || n.label != "nested" {
		t.Fatalf("cursor on %+v after collapse-all", n)
	}
}

func TestExpandAllIsBoundedOnAHugeTree(t *testing.T) {
	// Small enough to build on the spot, too many lines to open in full.
	inner := make([]driver.Value, 200)
	for i := range inner {
		inner[i] = driver.List(driver.Int(1))
	}
	outer := make([]driver.Value, 50)
	for i := range outer {
		outer[i] = driver.List(inner...)
	}
	md := newModel([]driver.Column{{Name: "v"}}, driver.Row{driver.List(outer...)})
	md, _ = key(md, "]")
	if len(md.lines) > maxExpandLines+maxShown+2 {
		t.Fatalf("expand-all drew %d lines", len(md.lines))
	}
	// The shallow levels open first.
	if !md.roots[0].expanded || !md.roots[0].children[0].expanded {
		t.Fatal("the top levels were not expanded")
	}
}

func TestWideArrayStaysUsable(t *testing.T) {
	list := make([]driver.Value, 5000)
	for i := range list {
		list[i] = driver.Int(int64(i))
	}
	md := newModel([]driver.Column{{Name: "v"}}, driver.Row{driver.List(list...)})
	md, _ = key(md, "]", "bottom")
	if v := view(md, 60, 5); !strings.Contains(v, "… 4000 more") {
		t.Fatalf("wide array:\n%s", v)
	}
}

func kvRow(t *testing.T, typ string) ([]driver.Column, driver.Row) {
	t.Helper()
	ds := drivertest.KeyValueDataset()
	for _, r := range ds.Rows {
		if s, _ := r[1].AsText(); s == typ {
			return ds.Columns, r
		}
	}
	t.Fatalf("no %s row", typ)
	return nil, nil
}

func kvView(cols []driver.Column, row driver.Row) string {
	md := NewKeyValue(cols, row, theme.Dark(), keymap.Default()).Resize(60, 12)
	raw := md.View(60, 12)
	return raw
}

func TestKeyValueDetailPerType(t *testing.T) {
	tests := []struct {
		typ   string
		value driver.Value // nil: the fixture's
		want  []string
		order []string // must appear in this order
	}{
		{typ: "string", want: []string{"_key", "user:1:name", "type", "string", "ttl", "∞", "Ada"}},
		{typ: "hash", want: []string{"▾ {2 keys}", "name  Ada", "lang  en"}},
		{typ: "list", want: []string{"[0]  j1", "[2]  j3", "ttl", "1h"}, order: []string{"j1", "j2", "j3"}},
		{typ: "set", value: driver.List(driver.String("zed"), driver.String("db"), driver.String("tui")), order: []string{"db", "tui", "zed"}},
		{typ: "zset", want: []string{"bob  1", "ada  3.5"}, order: []string{"bob", "ada"}},
		{typ: "stream", value: driver.List(
			driver.ObjectValue(m("id", driver.String("1-0")), m("fields", driver.ObjectValue(m("temp", driver.String("21"))))),
			driver.ObjectValue(m("id", driver.String("2-0")), m("fields", driver.ObjectValue(m("temp", driver.String("22"))))),
		), want: []string{"▾ 1-0", "temp  21", "▾ 2-0", "temp  22"}},
		{typ: "json", value: driver.JSON([]byte(`{"a":{"b":[1,2]}}`)), want: []string{"▾ {1 key}", "a: {1 key}"}},
		{typ: "string", value: driver.String("line one\nline two\x1b[2J"), want: []string{"2 lines", "line one", `line two\x1b[2J`}},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			base := tt.typ
			if tt.value.Type() != driver.TypeNull {
				base = "hash" // a fixture row whose type and value are replaced
			}
			cols, row := kvRow(t, base)
			row = append(driver.Row(nil), row...)
			if tt.value.Type() != driver.TypeNull {
				row[1], row[3] = driver.String(tt.typ), tt.value
			}
			raw := kvView(cols, row)
			tuitest.NoInjectedControls(t, raw)
			v := ansi.Strip(raw)
			for _, w := range tt.want {
				if !strings.Contains(v, w) {
					t.Errorf("lacks %q:\n%s", w, v)
				}
			}
			last := -1
			for _, w := range tt.order {
				i := strings.Index(v, w)
				if i < last {
					t.Errorf("%q out of order:\n%s", w, v)
				}
				last = i
			}
		})
	}
}

func TestKeyValueDetailFallsBackWithoutTheColumns(t *testing.T) {
	cols := []driver.Column{{Name: "id"}, {Name: "name"}}
	md := NewKeyValue(cols, driver.Row{driver.Int(1), driver.String("x")}, theme.Dark(), keymap.Default())
	if v := view(md, 40, 4); !strings.Contains(v, "name") || !strings.Contains(v, "x") {
		t.Fatalf("fallback:\n%s", v)
	}
}

func TestTTLText(t *testing.T) {
	for _, c := range []struct {
		v    driver.Value
		want string
	}{
		{driver.Int(-1), "∞"}, {driver.Int(0), "0s"}, {driver.Int(12), "12s"}, {driver.Int(60), "1m"},
		{driver.Int(90), "1m30s"}, {driver.Int(10800), "3h"}, {driver.Int(3660), "1h1m"},
		{driver.Int(172800 + 3600), "2d1h"}, {driver.Float(12.7), "12s"}, {driver.Int(-2), "expired"},
	} {
		if got, ok := TTLText(c.v); !ok || got != c.want {
			t.Errorf("TTLText(%v) = %q %v, want %q", c.v, got, ok, c.want)
		}
	}
	if _, ok := TTLText(driver.String("x")); ok {
		t.Error("a string is not a TTL")
	}
}

func TestAbsentFieldsReadAsADash(t *testing.T) {
	cols := []driver.Column{{Name: "_id"}, {Name: "extra"}, {Name: "gone"}}
	md := newModel(cols, driver.Row{driver.String("d1"), driver.Null()}).WithAbsent([]bool{false, false, true})
	v := view(md, 40, 4)
	if !strings.Contains(v, "extra │ NULL") || !strings.Contains(v, "gone  │ —") {
		t.Fatalf("absent vs NULL:\n%s", v)
	}
	_, cmds := key(md, "down", "down", "y")
	if got := clipboard(t, flatten(cmds...)); got != "" {
		t.Fatalf("an absent field copies %q", got)
	}
}

func TestMultiLineStringBuildIsCapped(t *testing.T) {
	cols, row := kvRow(t, "string")
	row = append(driver.Row(nil), row...)
	row[3] = driver.String(strings.Repeat("line\n", 5000) + "end")
	md := NewKeyValue(cols, row, theme.Dark(), keymap.Default())
	if n := len(md.roots[3].children); n > maxShown+1 {
		t.Fatalf("built %d line nodes", n)
	}
	md, _ = key(md, "bottom")
	if v := view(md, 60, 4); !strings.Contains(v, "… 4001 more") {
		t.Fatalf("the cut is not counted:\n%s", v)
	}
	if md.roots[3].summary() != "5001 lines" {
		t.Fatalf("summary = %q", md.roots[3].summary())
	}
}

func TestExpandAllCountsWhatIsAlreadyOpen(t *testing.T) {
	// Five collapsed values of 1,000 elements, then one already open: the
	// open one's lines count toward the bound before anything else opens.
	list := make([]driver.Value, 1000)
	for i := range list {
		list[i] = driver.Int(int64(i))
	}
	var cols []driver.Column
	var row driver.Row
	for i := range 6 {
		cols = append(cols, driver.Column{Name: fmt.Sprintf("c%d", i)})
		row = append(row, driver.List(list...))
	}
	md := newModel(cols, row)
	md.roots[5].expanded = true
	md.flatten()
	md, _ = key(md, "]")
	if len(md.lines) > maxExpandLines {
		t.Fatalf("expand-all drew %d lines, bound %d", len(md.lines), maxExpandLines)
	}
}
