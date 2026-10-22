package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/filter"
	"github.com/xavidop/tobira/internal/tui/palette"
)

const bigRows = 2500

// newExportHarness is the edit harness plus a "big" collection of bigRows
// rows (one int column n), more than one export page and many grid pages.
func newExportHarness(t *testing.T) *harness {
	t.Helper()
	h := newEditHarness(t, false, nil)
	rows := make([]driver.Row, bigRows)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i + 1))}
	}
	h.fake.AddCollection("main", "big", []driver.Column{{Name: "n", Type: driver.TypeInt}}, rows, "n")
	return h
}

// ndjsonNs reads the n values of an ndjson export.
func ndjsonNs(t *testing.T, p string) []int64 {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row struct{ N int64 }
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row.N)
	}
	return out
}

func assertNoExportTemp(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tobira-export-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

// exportDone waits for the latest export to finish. The target is checked
// in a command first, so the export may not have started yet.
func (h *harness) exportDone() {
	h.t.Helper()
	h.pump(func() bool {
		if h.m.export != nil || h.m.exportSeq == 0 {
			return false
		}
		for _, m := range h.msgs {
			if d, ok := m.(exportDoneMsg); ok && d.id == h.m.exportSeq {
				return true
			}
		}
		return false
	})
}

func TestExportBrowseTabWholeCollection(t *testing.T) {
	h := newExportHarness(t)
	h.openColl("local", "big")
	h.keys("s", "s") // sort by n, descending
	h.pump(func() bool { return !h.m.gr.Loading() && strings.Contains(h.text(), "n ▼") })
	h.send(filter.AppliedMsg{Text: "n > 100", Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "n", Op: driver.OpGt, Value: driver.Int(100)},
	}}})
	h.pump(func() bool { return !h.m.gr.Loading() && h.m.gr.Status().Loaded > 0 })
	p := filepath.Join(t.TempDir(), "big.ndjson")
	h.palette("export ndjson " + p)
	h.exportDone()
	want := bigRows - 100
	if f := h.flashText(); !strings.Contains(f, "exported "+commas(want)+" rows") {
		t.Fatalf("flash = %q", f)
	}
	ns := ndjsonNs(t, p)
	if len(ns) != want || ns[0] != bigRows || ns[len(ns)-1] != 101 {
		t.Fatalf("exported %d rows from %d to %d, want %d from %d to 101", len(ns), ns[0], ns[len(ns)-1], want, bigRows)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", st, err)
	}
}

func TestExportResultTab(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.palette("set limit 10")
	h.runInEditor("select * from tobira_people")
	queries := 0
	for _, c := range h.fake.Calls() {
		if c.Op == "query" {
			queries++
		}
	}
	p := filepath.Join(t.TempDir(), "out.json")
	h.palette("export json " + p)
	h.exportDone()
	const want = "exported 10 rows (the result was cut at the limit; raise :set limit or use tobira query --output)"
	if f := h.flashText(); !strings.Contains(f, want) {
		t.Fatalf("flash = %q", f)
	}
	var doc struct{ Rows []map[string]any }
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Rows) != 10 {
		t.Fatalf("export holds %d rows (%v)", len(doc.Rows), err)
	}
	after := 0
	for _, c := range h.fake.Calls() {
		if c.Op == "query" {
			after++
		}
	}
	if after != queries {
		t.Fatal("the export ran the statement again")
	}
}

func TestExportRefusesTextTabs(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.runInEditor("delete from tobira_people where id = 999")
	p := filepath.Join(t.TempDir(), "x.csv")
	h.palette("export csv " + p)
	if f := h.flashText(); !strings.Contains(f, "only a result with rows can be exported") {
		t.Fatalf("flash = %q", f)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("a text tab was exported")
	}
}

func TestExportOverwriteConfirm(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	dir := t.TempDir()
	p := filepath.Join(dir, "x\x1b[2J.csv")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.send(palette.ExportCmd{Format: "csv", Path: p})
	// The target is resolved and checked in a command, never on the
	// Update goroutine; the prompt opens from its result.
	if h.m.ask != nil {
		t.Fatal("the target was checked on the Update goroutine")
	}
	h.pump(func() bool { return h.m.ask != nil })
	if !strings.Contains(h.text(), `overwrite `) || !strings.Contains(h.text(), `x\x1b[2J.csv? y/N`) {
		t.Fatalf("no overwrite prompt:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	h.keys("n")
	h.drain(50 * time.Millisecond)
	if b, _ := os.ReadFile(p); string(b) != "old" || h.m.export != nil {
		t.Fatal("n overwrote the file")
	}
	h.send(palette.ExportCmd{Format: "csv", Path: p})
	h.pump(func() bool { return h.m.ask != nil })
	h.keys("y")
	h.pump(func() bool {
		b, _ := os.ReadFile(p)
		return h.m.export == nil && strings.HasPrefix(string(b), "id,name")
	})
}

func TestExportExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.send(palette.ExportCmd{Format: "csv", Path: "~/people.csv"})
	h.exportDone()
	if _, err := os.Stat(filepath.Join(home, "people.csv")); err != nil {
		t.Fatalf("~ not expanded: %v (%q)", err, h.flashText())
	}
}

func TestExportProgressAndCancel(t *testing.T) {
	h := newExportHarness(t)
	h.openColl("local", "big")
	dir := t.TempDir()
	p := filepath.Join(dir, "big.csv")
	h.fake.SetLatency(300 * time.Millisecond) // per page: the export takes a while
	h.palette("export csv " + p)
	h.pump(func() bool { return h.m.export != nil }) // the target is checked first
	h.pump(func() bool {
		return strings.Contains(h.statusLine(), "exporting… ") && strings.Contains(h.statusLine(), " rows")
	})
	h.palette("export csv " + p)
	if f := h.flashText(); !strings.Contains(f, "an export is already running") {
		t.Fatalf("second export: %q", f)
	}
	h.send(ctrl('c'))
	if h.quitted() {
		t.Fatal("ctrl+c quit during an export")
	}
	h.exportDone()
	if f := h.flashText(); !strings.Contains(f, "export cancelled") {
		t.Fatalf("flash = %q", f)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("a cancelled export left the file")
	}
	assertNoExportTemp(t, dir)
}

func TestExportQuitAsksAndCancels(t *testing.T) {
	h := newExportHarness(t)
	h.openColl("local", "big")
	dir := t.TempDir()
	p := filepath.Join(dir, "big.csv")
	h.fake.SetLatency(2 * time.Second)
	h.palette("export csv " + p)
	h.pump(func() bool { return h.m.export != nil })
	h.keys("q")
	if h.quitted() || h.m.ask == nil || h.m.ask.title != "an export is running; quit and cancel it? y/N" {
		t.Fatal("q quit during an export without asking")
	}
	h.keys("n")
	if h.quitted() || h.m.export == nil {
		t.Fatal("n did not keep the export")
	}
	h.keys("q")
	h.keys("y")
	if !h.quitted() {
		t.Fatal("y did not quit")
	}
	done := h.m.exportWait()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the export did not stop")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("a cancelled export left the file")
	}
	assertNoExportTemp(t, dir)
}

func TestExportProgressText(t *testing.T) {
	if got := exportingText(12345); got != "exporting… 12,345 rows" {
		t.Fatalf("exportingText = %q", got)
	}
	if got := exportingText(1); got != "exporting… 1 row" {
		t.Fatalf("exportingText(1) = %q", got)
	}
}

// Exporting a collection whose rows hold value previews (a _truncated
// marker that Describe lists as Generated) says how many in the flash.
func TestExportWarnsAboutPreviews(t *testing.T) {
	h := newExportHarness(t)
	cols := []driver.Column{{Name: "id", Type: driver.TypeInt, Key: true}, {Name: "value", Type: driver.TypeString}, {Name: "_truncated", Type: driver.TypeBool}}
	h.fake.AddCollection("main", "notes", cols, []driver.Row{
		{driver.Int(1), driver.String("whole"), driver.Bool(false)},
		{driver.Int(2), driver.String("cut…"), driver.Bool(true)},
		{driver.Int(3), driver.String("cut…"), driver.Bool(true)},
	}, "id")
	markTruncatedGenerated(h.fake, "notes")
	h.openColl("local", "notes")
	h.pump(func() bool { return !h.m.gr.Loading() && h.m.gr.Status().Loaded > 0 })
	h.palette("export csv " + filepath.Join(t.TempDir(), "notes.csv"))
	h.exportDone()
	if f := h.flashText(); !strings.Contains(f, "exported 3 rows") || !strings.Contains(f, "2 values were previews, not full values") {
		t.Fatalf("flash = %q", f)
	}
}

// No export starts while a config write or reload may close its
// connection.
func TestExportRefusedDuringAConfigWrite(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.m.saving = true
	h.send(palette.ExportCmd{Format: "csv", Path: t.TempDir() + "/x.csv"})
	if h.m.export != nil {
		t.Fatal("an export started during a config write")
	}
	if f := h.flashText(); !strings.Contains(f, busyText) {
		t.Fatalf("flash = %q", f)
	}
}
