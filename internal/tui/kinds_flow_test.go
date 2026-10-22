package tui

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// kindsOpts configures newKindsHarness.
type kindsOpts struct {
	embedder    driver.Embedder
	embedderErr error
	without     []driver.Capability
	// kind, when set, makes the Fake report it and seeds that kind's
	// fixture: KindWideColumn the events table, KindSearch the articles
	// index.
	kind driver.Kind
}

// newKindsHarness is the harness on a Fake holding every kind fixture, plus
// "pages", a schemaless collection whose second page brings a new field.
func newKindsHarness(t *testing.T, o kindsOpts) *harness {
	t.Helper()
	fopts := []drivertest.FakeOption{
		drivertest.WithPeople(), drivertest.WithDocuments(), drivertest.WithKeyValues(),
		drivertest.WithVectors(), drivertest.WithTimeSeries(), drivertest.WithoutCapabilities(o.without...),
	}
	switch o.kind {
	case driver.KindWideColumn:
		fopts = append(fopts, drivertest.WithKind(o.kind), drivertest.WithWideColumns())
	case driver.KindSearch:
		fopts = append(fopts, drivertest.WithKind(o.kind), drivertest.WithSearch())
	case "":
	default:
		fopts = append(fopts, drivertest.WithKind(o.kind))
	}
	f := drivertest.NewFake(fopts...)
	docs := drivertest.Documents()
	for i := len(docs); i < 200; i++ {
		docs = append(docs, driver.Object{
			{Key: "_id", Value: driver.String(fmt.Sprintf("f%03d", i))},
			{Key: "name", Value: driver.String(fmt.Sprintf("filler %d", i))},
		})
	}
	for i := range 10 {
		docs = append(docs, driver.Object{
			{Key: "_id", Value: driver.String(fmt.Sprintf("x%02d", i))},
			{Key: "extra", Value: driver.Int(int64(i))},
		})
	}
	f.AddDocuments("main", "pages", docs)
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}
	var sopts []session.Option
	if o.embedder != nil {
		sopts = append(sopts, session.WithEmbedder(o.embedder))
	}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false, sopts...)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark", EmbedderError: o.embedderErr})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.run(h.m.Init())
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	return h
}

// openKind expands local and main in the sidebar and opens collection name.
func (h *harness) openKind(name string) {
	h.t.Helper()
	if _, ok := h.mgr.Conn("local"); !ok {
		h.keys("enter")
		h.until("main")
		h.keys("down", "enter")
		h.until("tobira_people")
	}
	h.m.focus = focusSidebar
	for range 40 {
		h.keys("up")
	}
	for range 40 {
		if _, ref := h.m.sb.Selected(); ref != nil && ref.Name == name {
			h.keys("enter")
			h.pump(func() bool { return h.m.cur.ref.Name == name && !h.m.gr.Loading() })
			return
		}
		h.keys("down")
	}
	h.t.Fatalf("collection %s not found:\n%s", name, h.text())
}

// command runs a palette command.
func (h *harness) command(line string) {
	h.t.Helper()
	h.keys(":")
	h.typ(line)
	h.keys("enter")
}

func TestKindDocumentPreviewAbsentAndDetail(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	h.openKind("pages")
	for _, want := range []string{`{city: "London", zip: "N1"}`, `["math", "cod…`, `{city: "Helsinki", geo: {…}}`, "[]"} {
		if !strings.Contains(h.text(), want) {
			t.Fatalf("grid lacks the preview %q:\n%s", want, h.text())
		}
	}
	// The bottom of page 0 fetches page 1, which brings the extra field;
	// page 0's rows read it as absent, not NULL.
	h.keys("ctrl+end")
	h.pump(func() bool { return h.m.gr.Status().Exact })
	h.keys("ctrl+home", "end") // the first row, the last column: extra
	if !strings.Contains(h.text(), "extra") || !strings.Contains(h.text(), "—") || !strings.Contains(h.text(), "NULL") {
		t.Fatalf("absent and NULL not shown:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	golden(t, "kind_document_grid", h.text())

	h.keys("home", "down", "down", "down", "enter") // d4: nested objects
	h.until("row detail")
	h.keys("]")
	h.until("deeper")
	checkFrame(t, h, winW, winH)
	golden(t, "kind_document_detail_expanded", h.text())
	// With a result tab open, [ and ] still act on the detail and never
	// switch tabs under it.
	h.m.addTab("local", "select 1", false)
	h.m.active = 0
	h.keys("[")
	if strings.Contains(h.text(), "deeper") {
		t.Fatalf("collapse-all left nodes open:\n%s", h.text())
	}
	h.keys("]")
	if !strings.Contains(h.text(), "deeper") {
		t.Fatalf("expand-all did not reopen the nodes:\n%s", h.text())
	}
	if h.m.det == nil || h.m.active != 0 {
		t.Fatal("[ or ] switched tabs under the detail")
	}
}

func TestKindKeyValueGridAndDetailPerType(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	h.openKind("kv")
	txt := h.text()
	for _, want := range []string{"∞", "1h", "1m", `{name: "Ada", lang: "en"}`, `{bob: 1, ada: 3.5}`} {
		if !strings.Contains(txt, want) {
			t.Fatalf("grid lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, " -1 ") {
		t.Fatalf("TTL shown raw:\n%s", txt)
	}
	checkFrame(t, h, winW, winH)
	golden(t, "kind_kv_grid", h.text())

	for i, typ := range []string{"string", "hash", "list", "set", "zset"} {
		if i > 0 {
			h.keys("down")
		}
		h.keys("enter")
		h.until("row detail")
		checkFrame(t, h, winW, winH)
		golden(t, "kind_kv_detail_"+typ, h.text())
		h.keys("esc")
	}
}

func TestKindKeyValueEditsNonStringsAsJSON(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	h.openKind("kv")
	h.pump(func() bool { return h.m.src.Described() })
	h.keys("down", "right", "right", "right", "e") // the hash's value
	if h.m.cell == nil || !h.m.cell.multi() {
		t.Fatalf("the hash did not open in the JSON editor:\n%s", h.text())
	}
	if v := h.m.cell.ed.Value(); !strings.Contains(v, `"name": "Ada"`) {
		t.Fatalf("editor text = %q", v)
	}
	h.keys("esc")
	h.keys("up", "e") // the string's value: a one-line edit
	if h.m.cell == nil || h.m.cell.multi() {
		t.Fatal("a string value did not open in the line editor")
	}
}

func TestKVEditFieldRefusesStreams(t *testing.T) {
	cols := drivertest.KeyValueDataset().Columns
	row := driver.Row{driver.String("events"), driver.String("stream"), driver.Int(-1), driver.List()}
	if _, why := kvEditField(cols, row, 3, fieldInfo{Name: "value"}); !strings.Contains(why, "stream") {
		t.Fatalf("why = %q", why)
	}
	row[1], row[3] = driver.String("list"), driver.List(driver.String("a"))
	f, why := kvEditField(cols, row, 3, fieldInfo{Name: "value", Type: driver.TypeUnknown})
	if why != "" || f.Type != driver.TypeList {
		t.Fatalf("list field = %+v, %q", f, why)
	}
}

func chartClock(h *harness) {
	end := drivertest.TimeSeriesStart().Add(2 * time.Hour)
	h.m.now = func() time.Time { return end }
}

func TestKindChartLineAndSparkline(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	chartClock(h)
	h.openKind("cpu")
	h.command("chart usage_user")
	h.until("last 1h")
	h.pump(func() bool { return !h.m.chart.loading })
	txt := h.text()
	for _, want := range []string{"usage_user", "max 22.5", "min 0.5", "last"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("chart lacks %q:\n%s", want, txt)
		}
	}
	checkFrame(t, h, winW, winH)
	golden(t, "kind_chart_line", h.text())

	// A range in dates: the title names it once the command has landed,
	// and the points are that hour's.
	h.command("range 2026-01-01T00:00..2026-01-01T01:00")
	h.until("chart · usage_user · mean 1m · 2026-01-01 00:00..2026-01-01 01:00")
	h.pump(func() bool { return !h.m.chart.loading })
	if txt := h.text(); !strings.Contains(txt, "00:00 … 00:59") || !strings.Contains(txt, "last 22.5") {
		t.Fatalf("the date range was not drawn:\n%s", txt)
	}
	// A narrow pane: a sparkline, its window recomputed for the width.
	h.m.focus = focusGrid
	h.send(tea.WindowSizeMsg{Width: 38, Height: winH})
	h.pump(func() bool { return !h.m.chart.loading && h.m.chart.used == 2*time.Minute })
	if !strings.ContainsAny(h.text(), "▁▂▃▄▅▆▇█") {
		t.Fatalf("no sparkline at 38 columns:\n%s", h.text())
	}
	checkFrame(t, h, 38, winH)
	golden(t, "kind_chart_sparkline", h.text())

	h.command("chart off")
	h.pump(func() bool { return !h.m.chart.open })
	if h.m.chart.open || strings.Contains(h.text(), "usage_user ·") {
		t.Fatalf("chart still open:\n%s", h.text())
	}
}

func TestKindChartIsCancellableAndNeedsATimeSeriesDriver(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	chartClock(h)
	h.openKind("cpu")
	h.fake.SetLatency(time.Hour)
	h.command("chart usage_user 5m")
	h.pump(func() bool { return h.m.chart.loading })
	h.keys("ctrl+c")
	if h.m.chart.loading || h.didQuit {
		t.Fatal("ctrl+c did not cancel the chart load")
	}
	h.fake.SetLatency(0)

	h2 := newKindsHarness(t, kindsOpts{without: []driver.Capability{driver.CapTimeSeriesReader}})
	h2.openKind("cpu")
	h2.command("chart usage_user")
	h2.until("charts need a time-series driver")
	if h2.m.chart.open {
		t.Fatal("chart opened without a TimeSeriesReader")
	}
}

func TestKindVectorFindSimilar(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	h.openKind("vecs")
	h.keys("S")
	h.until("similar to 1")
	h.pump(func() bool { return h.m.running == nil })
	tab := h.m.activeTab()
	if tab == nil || tab.rows != 6 || tab.state != tabDone {
		t.Fatalf("similar tab = %+v:\n%s", tab, h.text())
	}
	if cols := tab.cols; cols[len(cols)-1].Name != "_distance" {
		t.Fatalf("columns = %v", cols)
	}
	if first := tab.result[0][0].String(); first != "1" {
		t.Fatalf("nearest row is %s, want row 1 itself", first)
	}
	checkFrame(t, h, winW, winH)
	golden(t, "kind_vector_similar", h.text())

	// S works again from the similar tab, on its rows.
	h.keys("down", "S")
	h.until("similar to 2")
	h.pump(func() bool { return h.m.running == nil })
	if len(h.m.tabs) != 2 {
		t.Fatalf("%d tabs", len(h.m.tabs))
	}
	// A row without a vector.
	h.keys("[", "[")
	h.openKind("tobira_people")
	h.keys("S")
	h.until("no vector")
}

func TestKindVectorNeedsAVectorSearcher(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{without: []driver.Capability{driver.CapVectorSearcher}})
	h.openKind("vecs")
	h.keys("S")
	h.until("find similar needs a vector driver")
	if len(h.m.tabs) != 0 {
		t.Fatal("a tab opened")
	}
	h.command("similar red")
	h.until("find similar needs a vector driver")
}

func TestKindSimilarTextWithAndWithoutEmbedder(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{embedder: drivertest.HashEmbedder{Dims: 4}})
	h.openKind("vecs")
	h.command("similar a red bicycle")
	h.until(`similar to "a red bicycle"`)
	h.pump(func() bool { return h.m.running == nil })
	if tab := h.m.activeTab(); tab == nil || tab.state != tabDone || tab.rows == 0 {
		t.Fatalf("text similar failed:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	golden(t, "kind_vector_similar_text", h.text())

	h2 := newKindsHarness(t, kindsOpts{})
	h2.openKind("vecs")
	h2.command("similar a red bicycle")
	h2.pump(func() bool { return h2.m.running == nil && len(h2.m.tabs) == 1 })
	h2.until(driver.NeedEmbedderHint)
	checkFrame(t, h2, winW, winH)
	golden(t, "kind_vector_similar_no_embedder", h2.text())

	// A factory error the session logged is shown with the hint.
	h3 := newKindsHarness(t, kindsOpts{embedderErr: errors.New("ai.embedder: unknown model \x1b[31mx")})
	h3.openKind("vecs")
	h3.command("similar a red bicycle")
	h3.until("unknown model")
	checkFrame(t, h3, winW, winH)
}

// TestProgramFindSimilar runs S in the real program under teatest.
func TestProgramFindSimilar(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithVectors())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(winW, winH))
	waitFor := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return strings.Contains(ansi.Strip(string(b)), s) }, teatest.WithDuration(5*time.Second))
	}
	waitFor("connections")
	tm.Send(key("enter"))
	waitFor("main")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("vecs")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("aligned")
	tm.Send(key("S"))
	waitFor("_distance")
	tm.Send(key("q"))
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if tab := final.activeTab(); tab == nil || tab.label != "similar to 1" || tab.rows != 6 {
		t.Fatalf("final tab = %+v", tab)
	}
}

func TestSplitDialectClickHouse(t *testing.T) {
	spans := sqlcomplete.Split(splitDialect("ClickHouse"), "SELECT 1; SELECT 'a;b' FROM t")
	if len(spans) != 2 {
		t.Fatalf("clickhouse split into %d statements, want 2", len(spans))
	}
}

// ClickHouse strings take backslash escapes, so \' does not end the string
// and the ; inside it does not split; the SQLite rules would cut it in two.
func TestSplitDialectClickHouseBackslashEscape(t *testing.T) {
	d := splitDialect("clickhouse")
	if d.Name != "clickhouse" {
		t.Fatalf("splitDialect(clickhouse).Name = %q, want clickhouse", d.Name)
	}
	src := `SELECT 'a\';b' FROM t; SELECT 2`
	spans := sqlcomplete.Split(d, src)
	if len(spans) != 2 {
		t.Fatalf("split into %d statements, want 2: %+v", len(spans), spans)
	}
	if got := src[spans[0].Start:spans[0].End]; !strings.Contains(got, `'a\';b'`) {
		t.Fatalf("first statement = %q, want it to hold the whole string", got)
	}
}

// A time-series view is keyed on the capability, not the collection type:
// a driver may list its series as tables.
func TestChartOnATableTypedSeries(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	ds := drivertest.TimeSeriesDataset()
	h.fake.AddCollection("main", "series_table", ds.Columns, ds.Rows) // listed as a table
	chartClock(h)
	h.openKind("series_table")
	h.command("chart usage_user")
	h.until("chart · usage_user")
	h.pump(func() bool { return !h.m.chart.loading })
	if h.m.chart.err != "" || len(h.m.chart.pts.vals) == 0 {
		t.Fatalf("chart on a table: err %q, %d points", h.m.chart.err, len(h.m.chart.pts.vals))
	}
}

func TestEditRefusesATruncatedValue(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	// Any driver marks a cut preview with a Bool _truncated column right
	// after the value; this collection is a plain table.
	cols := []driver.Column{
		{Name: "id", Type: driver.TypeInt, Key: true}, {Name: "body", Type: driver.TypeString},
		{Name: "_truncated", Type: driver.TypeBool},
	}
	h.fake.AddCollection("main", "notes", cols, []driver.Row{
		{driver.Int(1), driver.String("whole"), driver.Bool(false)},
		{driver.Int(2), driver.String("the first 4 KiB…"), driver.Bool(true)},
	}, "id")
	markTruncatedGenerated(h.fake, "notes")
	h.openKind("notes")
	h.pump(func() bool { return h.m.src.Described() })
	h.keys("down", "right", "e")
	h.until(truncatedEditText)
	if h.m.cell != nil {
		t.Fatal("the cell editor opened on a truncated value")
	}
	h.keys("up", "e") // a whole value
	if h.m.cell == nil {
		t.Fatal("a whole value was refused")
	}
}

// The _truncated marker is hidden from the grid but kept in the row
// detail, and the truncation refusal still reads it.
func TestTruncatedColumnHiddenInGridShownInDetail(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	cols := []driver.Column{
		{Name: "id", Type: driver.TypeInt, Key: true}, {Name: "body", Type: driver.TypeString},
		{Name: "_truncated", Type: driver.TypeBool},
	}
	h.fake.AddCollection("main", "notes", cols, []driver.Row{
		{driver.Int(1), driver.String("whole"), driver.Bool(false)},
		{driver.Int(2), driver.String("the first 4 KiB…"), driver.Bool(true)},
	}, "id")
	markTruncatedGenerated(h.fake, "notes")
	h.openKind("notes")
	h.pump(func() bool { return h.m.src.Described() })
	if txt := h.text(); strings.Contains(txt, "_truncated") || !strings.Contains(txt, "the first 4 KiB") {
		t.Fatalf("grid shows the marker or lacks the value:\n%s", txt)
	}
	checkFrame(t, h, winW, winH)
	golden(t, "kind_truncated_grid", h.text())
	h.keys("down", "right", "right") // the cursor stops on body
	if _, c := h.m.gr.Cursor(); c != 1 {
		t.Fatalf("cursor col = %d, want 1 (body)", c)
	}
	h.keys("e")
	h.until(truncatedEditText)
	h.keys("enter")
	h.until("row detail")
	if txt := h.text(); !strings.Contains(txt, "_truncated") {
		t.Fatalf("row detail lacks _truncated:\n%s", txt)
	}
	checkFrame(t, h, winW, winH)
	golden(t, "kind_truncated_detail", h.text())
}

func TestPreviewCut(t *testing.T) {
	cols := []driver.Column{{Name: "_key"}, {Name: "type"}, {Name: "ttl"}, {Name: "value"}, {Name: "_truncated"}}
	row := driver.Row{driver.String("k"), driver.String("string"), driver.Int(-1), driver.String("aaa…"), driver.Bool(true)}
	marked := &driver.CollectionSchema{Fields: []driver.Field{{Name: "value"}, {Name: "_truncated", Type: driver.TypeBool, Generated: true}}}
	if !previewCut(marked, cols, row, 3) || previewCut(marked, cols, row, 0) {
		t.Error("only the value before _truncated is cut")
	}
	plain := &driver.CollectionSchema{Fields: []driver.Field{{Name: "value"}, {Name: "_truncated", Type: driver.TypeBool}}}
	if previewCut(plain, cols, row, 3) || previewCut(nil, cols, row, 3) {
		t.Error("a _truncated field the schema does not mark Generated is treated as the marker")
	}
	row[4] = driver.Bool(false)
	if previewCut(marked, cols, row, 3) {
		t.Error("a whole value is treated as cut")
	}
	if previewCut(marked, cols[:4], row[:4], 3) {
		t.Error("a row without _truncated is treated as cut")
	}
}

// markTruncatedGenerated has the Fake describe coll's _truncated field as
// Generated, as a driver that marks cut previews does.
func markTruncatedGenerated(f *drivertest.Fake, coll string) {
	f.OnDescribe("main", coll, func(s *driver.CollectionSchema) {
		for i := range s.Fields {
			if s.Fields[i].Name == "_truncated" {
				s.Fields[i].Generated = true
			}
		}
	})
}

// A real column named _truncated, which Describe does not mark Generated,
// is an ordinary column: drawn in the grid, and the column before it and a
// column named value stay editable when it is true.
func TestTruncatedColumnNotGeneratedIsPlain(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	cols := []driver.Column{
		{Name: "id", Type: driver.TypeInt, Key: true}, {Name: "value", Type: driver.TypeString},
		{Name: "_truncated", Type: driver.TypeBool},
	}
	h.fake.AddCollection("main", "notes", cols, []driver.Row{
		{driver.Int(1), driver.String("a user's text"), driver.Bool(true)},
	}, "id")
	h.openKind("notes")
	h.pump(func() bool { return h.m.src.Described() })
	if txt := h.text(); !strings.Contains(txt, "_truncated") {
		t.Fatalf("grid hides a real _truncated column:\n%s", txt)
	}
	h.keys("right", "e")
	if h.m.cell == nil {
		t.Fatalf("editing value was refused:\n%s", h.text())
	}
}

// The chart stops with its connection: a reload cancels its read and waits
// for it, the read resumes once the reload is done, and an edit that
// closes the connection closes the pane.
func TestChartStopsWithItsConnection(t *testing.T) {
	h, _ := newConnHarness(t)
	ds := drivertest.TimeSeriesDataset()
	h.fake.AddCollection("main", "cpu", ds.Columns, ds.Rows)
	chartClock(h)
	h.openKind("cpu")
	h.fake.SetLatency(time.Hour)
	h.command("chart usage_user")
	h.pump(func() bool { return h.m.chart.loading })
	done := h.m.chart.done
	h.send(palette.ReloadCmd{})
	if h.m.chart.loading || !h.m.chart.paused {
		t.Fatal("the reload did not stop the chart read")
	}
	select {
	case <-done: // the cancelled read returned; the write waited on it
	case <-time.After(5 * time.Second):
		t.Fatal("the chart read did not stop")
	}
	h.fake.SetLatency(0)
	h.until("reloaded the config file")
	h.pump(func() bool { return !h.m.chart.loading && len(h.m.chart.pts.vals) > 0 })
	if !h.m.chart.open || h.m.chart.err != "" {
		t.Fatalf("chart after the reload: open %v err %q", h.m.chart.open, h.m.chart.err)
	}

	// An edit of the connection (here a rename) closes it: the pane goes.
	h.m.focus = focusSidebar
	for range 40 {
		h.keys("up")
	}
	h.step("E")
	h.typ("2")
	h.step("ctrl+s")
	h.until("saved local2")
	h.settle()
	if h.m.chart.open || strings.Contains(h.text(), "chart · usage_user") {
		t.Fatalf("the chart outlived its connection:\n%s", h.text())
	}
}

func TestStopChartOnHandsItsReadToTheWrite(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	chartClock(h)
	h.openKind("cpu")
	h.fake.SetLatency(time.Hour)
	h.command("chart usage_user")
	h.pump(func() bool { return h.m.chart.loading })
	h.m.stopChartOn("other")
	if !h.m.chart.loading {
		t.Fatal("a write on another connection stopped the chart")
	}
	h.m.stopChartOn("local")
	if !slices.Contains(h.m.stopping, (<-chan struct{})(h.m.chart.done)) || h.m.chart.loading {
		t.Fatal("the chart read is not waited for")
	}
	h.fake.SetLatency(0)
	cmd := h.m.resumeChart()
	if cmd == nil || !h.m.chart.loading {
		t.Fatal("the paused read did not resume")
	}
	h.run(cmd)
	h.pump(func() bool { return !h.m.chart.loading })
}

func TestRenameCarriesTheChartConnection(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	h.m.chart.conn = "local"
	h.m.renameEditor("local", "local2")
	if h.m.chart.conn != "local2" {
		t.Fatalf("chart conn = %q", h.m.chart.conn)
	}
}

func TestChartTitleMarksTruncationAndStaysCurrentOnError(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	chartClock(h)
	h.openKind("cpu")
	h.command("chart usage_user 5m")
	h.until("mean 5m")
	h.pump(func() bool { return !h.m.chart.loading })
	h.m.chart.pts.truncated = true
	if !strings.Contains(h.m.chartTitle(), "(truncated)") {
		t.Fatalf("title %q does not say truncated", h.m.chartTitle())
	}
	// An early error (the connection is gone) still shows the window asked.
	h.m.chart.conn = "gone"
	h.command("chart usage_user 10m")
	h.until("mean 10m")
}

func TestPointsOfSkipsNonFinite(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cols := []driver.Column{{Name: "_time", Type: driver.TypeTime}, {Name: "value", Type: driver.TypeFloat}}
	var rows []driver.Row
	for i, v := range []float64{1, 5, math.NaN(), 8, math.Inf(1), math.Inf(-1), 3} {
		rows = append(rows, driver.Row{driver.Time(at.Add(time.Duration(i) * time.Minute)), driver.Float(v)})
	}
	if p := pointsOf(cols, rows); fmt.Sprint(p.vals) != "[1 5 8 3]" {
		t.Fatalf("points = %v", p.vals)
	}
}

// TestProgramKeyValueDetail opens a hash's row detail in the real program
// under teatest.
func TestProgramKeyValueDetail(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithKeyValues())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(winW, winH))
	waitFor := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return strings.Contains(ansi.Strip(string(b)), s) }, teatest.WithDuration(5*time.Second))
	}
	waitFor("connections")
	tm.Send(key("enter"))
	waitFor("main")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("kv")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("leaderboard")
	tm.Send(key("down")) // user:1, the hash
	tm.Send(key("enter"))
	waitFor("name  Ada")
	tm.Send(key("esc"))
	tm.Send(key("q"))
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if !final.kv {
		t.Fatal("the collection did not open in the key-value view")
	}
}

// headerLine is the grid's header row in the plain text of the screen:
// the first line holding every name.
func (h *harness) headerLine(names ...string) string {
	h.t.Helper()
	for _, l := range strings.Split(h.text(), "\n") {
		all := true
		for _, n := range names {
			all = all && strings.Contains(l, n)
		}
		if all {
			return l
		}
	}
	return ""
}

// A wide-column table's header marks its partition key [P] and its
// clustering columns with their order [C↓]/[C↑], from Describe, even on a
// driver without a Mutator; another collection's header has no badge.
func TestKindWideColumnKeyBadges(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{kind: driver.KindWideColumn, without: []driver.Capability{driver.CapMutator}})
	h.openKind(drivertest.WideColumnCollection)
	h.pump(func() bool { return h.m.src != nil && h.m.src.Described() })
	head := h.headerLine("tenant", "seq")
	for _, want := range []string{"tenant [P]", "ts [C↓]", "seq [C↑]"} {
		if !strings.Contains(head, want) {
			t.Fatalf("header %q lacks the badge %q:\n%s", head, want, h.text())
		}
	}
	if strings.Contains(head, "kind [") || strings.Contains(head, "value [") {
		t.Errorf("a non-key column has a badge: %q", head)
	}
	h.openKind("tobira_people")
	if strings.Contains(h.text(), "[P]") || strings.Contains(h.text(), "[C") {
		t.Errorf("People keeps the wide-column badges:\n%s", h.text())
	}
}

// A non-wide-column driver's tables get no badge and no extra Describe
// when it has no Mutator.
func TestKindNoBadgesElsewhere(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{without: []driver.Capability{driver.CapMutator}})
	h.openKind("tobira_people")
	if h.m.descCancel != nil || (h.m.src != nil && h.m.src.Described()) {
		t.Error("a driver without a Mutator, not wide-column, must not read the schema")
	}
}

// A search driver's _score column reads as a relevance score with three
// decimals, in a query result and (cellFormat) in the browse grid.
func TestKindSearchScore(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{kind: driver.KindSearch})
	h.openEditor("search " + drivertest.SearchCollection + " for '" + drivertest.SearchTerm + "'")
	h.runUntilDone(keyF5)
	out := h.text()
	if !strings.Contains(out, "_score") || !strings.Contains(out, "4.000") || !strings.Contains(out, "1.000") {
		t.Fatalf("result lacks the formatted _score:\n%s", out)
	}
	if !strings.Contains(out, "Database internals") {
		t.Fatalf("result lacks the best hit:\n%s", out)
	}
	if got, _ := searchFormat([]driver.Column{{Name: "_score"}}, driver.Row{driver.Float(1.2876821)}, 0); got != "1.288" {
		t.Errorf("searchFormat(1.2876821) = %q, want 1.288", got)
	}
	if _, ok := searchFormat([]driver.Column{{Name: "_score"}}, driver.Row{driver.Null()}, 0); ok {
		t.Error("a NULL _score keeps the default rendering")
	}
	if _, ok := searchFormat([]driver.Column{{Name: "title"}}, driver.Row{driver.Float(1)}, 0); ok {
		t.Error("only _score is formatted")
	}
	if got, ok := searchFormat([]driver.Column{{Name: "_score"}}, driver.Row{driver.Decimal("2.5")}, 0); !ok || got != "2.500" {
		t.Errorf("searchFormat(decimal 2.5) = %q %v", got, ok)
	}
	score := []driver.Column{{Name: "_score"}}
	if f := cellFormat(driver.Info{Kind: driver.KindSearch}, driver.CollectionIndex); f == nil {
		t.Error("a search index browses with no format")
	} else if got, _ := f(score, driver.Row{driver.Float(0.5)}, 0); got != "0.500" {
		t.Errorf("browse format of _score 0.5 = %q", got)
	}
	if cellFormat(driver.Info{Kind: driver.KindDocument}, driver.CollectionCollection) != nil || resultFormat(driver.Info{Kind: driver.KindSQL}) != nil {
		t.Error("other kinds keep the default rendering")
	}
}

// Another kind's _score column keeps the default rendering.
func TestKindScoreOnlyOnSearch(t *testing.T) {
	h := newKindsHarness(t, kindsOpts{})
	h.fake.AddDocuments("main", "scored", []driver.Object{{{Key: "_id", Value: driver.String("x")}, {Key: "_score", Value: driver.Float(1.5)}}})
	h.openEditor("select * from scored")
	h.runUntilDone(keyF5)
	if out := h.text(); !strings.Contains(out, "1.5") || strings.Contains(out, "1.500") {
		t.Fatalf("a non-search driver formatted _score:\n%s", out)
	}
}
