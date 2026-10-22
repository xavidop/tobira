package query_test

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/query"
)

// streamed renders r through a RowWriter, one row at a time.
func streamed(t *testing.T, f query.Format, r *query.Result) string {
	t.Helper()
	var b bytes.Buffer
	rw, err := query.NewRowWriter(&b, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.Begin(r.Columns); err != nil {
		t.Fatal(err)
	}
	for _, row := range r.Rows {
		if err := rw.Row(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := rw.End(r.Stats); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// equivalenceFixtures covers every shape the buffered writers handled: the
// golden people rows, the whole people dataset (every value type), NULL vs
// empty, quoting, duplicate keys, no rows, no columns, short rows, nested
// JSON, HTML-significant and control characters, and the stats fields.
func equivalenceFixtures(t *testing.T) map[string]*query.Result {
	t.Helper()
	_, c := openFake(t, drivertest.WithPeople())
	golden, err := query.Run(context.Background(), c, "select * from tobira_people order by id limit 3", query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	golden.Stats.Duration = 12 * time.Millisecond
	all, err := query.Run(context.Background(), c, "select * from tobira_people order by id", query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	all.Stats.Duration = 1500 * time.Microsecond
	str := driver.Column{Name: "v", Type: driver.TypeString}
	return map[string]*query.Result{
		"golden":    golden,
		"people":    all,
		"nullEmpty": nullEmptyResult(),
		"quoting": {
			Columns: []driver.Column{{Name: "v, \"q\""}, {Name: " pad "}},
			Rows: []driver.Row{
				{driver.String("a,b"), driver.String(`say "hi"`)},
				{driver.String("line1\nline2"), driver.String("cr\r")},
				{driver.String(" lead"), driver.String("trail ")},
				{driver.String(""), driver.Null()},
			},
		},
		"duplicates": {
			Columns: []driver.Column{{Name: "a"}, {Name: "a"}, {Name: "a_2"}, {Name: "a"}},
			Rows:    []driver.Row{{driver.Int(1), driver.Int(2), driver.Int(3), driver.Int(4)}},
		},
		"noRows":    {Columns: []driver.Column{str}, Stats: driver.Stats{RowsAffected: -1}},
		"noColumns": {Stats: driver.Stats{RowsAffected: 3, Duration: 12 * time.Millisecond}},
		"emptyRows": {Rows: []driver.Row{{}, {}}},
		"shortRow": {
			Columns: []driver.Column{str, {Name: "w", Type: driver.TypeInt}},
			Rows:    []driver.Row{{driver.String("only")}, {}},
		},
		"nested": {
			Columns: []driver.Column{{Name: "j", Type: driver.TypeJSON}, {Name: "l", Type: driver.TypeList}, {Name: "o", Type: driver.TypeObject}},
			Rows: []driver.Row{
				{
					driver.JSON([]byte(`{"a": [1, {"b": []}, {}], "c": null}`)),
					driver.List(driver.Int(1), driver.List(), driver.String("x")),
					driver.ObjectValue(driver.Member{Key: "k", Value: driver.ObjectValue()}),
				},
				{driver.JSON([]byte(`[]`)), driver.List(), driver.ObjectValue()},
			},
		},
		"special": {
			Columns: []driver.Column{{Name: "<b>&"}, str, {Name: "f", Type: driver.TypeFloat}, {Name: "by", Type: driver.TypeBytes}},
			Rows: []driver.Row{
				{driver.String("<tag> &    "), driver.String("\x1b[31mred\x7f\u009b"), driver.Float(1.5), driver.Bytes([]byte{0, 1, 0xff})},
				{driver.String("日本語 🧑‍💻"), driver.String("\t"), driver.Float(math.Copysign(0, -1)), driver.Bytes(nil)},
			},
		},
		"stats": {
			Columns: []driver.Column{str},
			Rows:    []driver.Row{{driver.String("x")}},
			Stats:   driver.Stats{RowsAffected: 42, Duration: 2499 * time.Microsecond, Truncated: true},
		},
	}
}

// TestRowWriterMatchesLegacyWrite proves the streaming writers emit exactly
// the bytes of the buffered pre-M3 writers (legacy_test.go holds a verbatim copy).
func TestRowWriterMatchesLegacyWrite(t *testing.T) {
	for name, r := range equivalenceFixtures(t) {
		for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
			t.Run(name+"/"+string(f), func(t *testing.T) {
				var want bytes.Buffer
				if err := query.LegacyWrite(&want, f, r); err != nil {
					t.Fatal(err)
				}
				if got := streamed(t, f, r); got != want.String() {
					t.Fatalf("streamed output differs\n--- got ---\n%q\n--- want ---\n%q", got, want.String())
				}
				if got := render(t, f, r); got != want.String() {
					t.Fatalf("Write output differs\n--- got ---\n%q\n--- want ---\n%q", got, want.String())
				}
			})
		}
	}
}

// TestRowWriterMatchesGoldens checks the streamed output against the
// committed goldens directly.
func TestRowWriterMatchesGoldens(t *testing.T) {
	r := equivalenceFixtures(t)["golden"]
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		want, err := os.ReadFile(filepath.Join("testdata", string(f)+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if got := streamed(t, f, r); got != string(want) {
			t.Errorf("%s: streamed output differs from golden\n%s", f, got)
		}
	}
}

func TestNewRowWriterRefusesTable(t *testing.T) {
	for _, f := range []query.Format{query.FormatTable, "yaml"} {
		if _, err := query.NewRowWriter(io.Discard, f); err == nil {
			t.Errorf("NewRowWriter(%q) = nil error", f)
		}
	}
}

// countingWriter counts the Write calls it receives.
type countingWriter struct{ writes int }

func (c *countingWriter) Write(p []byte) (int, error) { c.writes++; return len(p), nil }

// TestRowWriterBuffers checks rows are batched through a buffer instead of
// one write per row.
func TestRowWriterBuffers(t *testing.T) {
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		cw := &countingWriter{}
		rw, err := query.NewRowWriter(cw, f)
		if err != nil {
			t.Fatal(err)
		}
		cols := []driver.Column{{Name: "a", Type: driver.TypeInt}}
		if err := rw.Begin(cols); err != nil {
			t.Fatal(err)
		}
		for i := range 1000 {
			if err := rw.Row(driver.Row{driver.Int(int64(i))}); err != nil {
				t.Fatal(err)
			}
		}
		if err := rw.End(driver.Stats{}); err != nil {
			t.Fatal(err)
		}
		if cw.writes > 10 {
			t.Errorf("%s: %d writes for 1000 rows, want buffered", f, cw.writes)
		}
	}
}

// TestWriteIsAllOrNothing checks that Write, the stdout path, renders the
// whole result before writing: w gets exactly one Write, so a rendering
// failure can never leave partial output. (Invalid raw JSON cannot fail the
// render: Value.MarshalJSON encodes it as a JSON string, checked here too.)
func TestWriteIsAllOrNothing(t *testing.T) {
	cols := []driver.Column{{Name: "j", Type: driver.TypeJSON}, {Name: "s", Type: driver.TypeString}}
	r := &query.Result{Columns: cols}
	for i := range 5000 {
		r.Rows = append(r.Rows, driver.Row{driver.JSON([]byte(`{bad`)), driver.String(strings.Repeat("x", 40) + strconv.Itoa(i))})
	}
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		cw := &countingWriter{}
		if err := query.Write(cw, f, r); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if cw.writes != 1 {
			t.Errorf("%s: %d writes, want the whole output in one", f, cw.writes)
		}
	}
	var b bytes.Buffer
	if err := query.Write(&b, query.FormatNDJSON, &query.Result{Columns: cols[:1], Rows: r.Rows[:1]}); err != nil {
		t.Fatal(err)
	}
	if want := "{\"j\":\"{bad\"}\n"; b.String() != want {
		t.Errorf("invalid raw JSON = %q, want %q", b.String(), want)
	}
}

// Flush writes the buffered header out without ending the result, for every
// streamable format.
func TestRowWriterFlush(t *testing.T) {
	cols := []driver.Column{{Name: "id", Type: driver.TypeInt}}
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		var b bytes.Buffer
		rw, err := query.NewRowWriter(&b, f)
		if err != nil {
			t.Fatal(err)
		}
		if err := rw.Begin(cols); err != nil {
			t.Fatal(err)
		}
		fl, ok := rw.(query.Flusher)
		if !ok {
			t.Fatalf("%s writer is not a Flusher", f)
		}
		if err := fl.Flush(); err != nil {
			t.Fatal(err)
		}
		header := b.String()
		if f != query.FormatNDJSON && header == "" {
			t.Errorf("%s: Flush wrote nothing", f)
		}
		if f == query.FormatJSON && strings.Contains(header, "stats") {
			t.Errorf("json: Flush ended the document: %q", header)
		}
	}
}

// Rekey keys later json and ndjson rows by new columns, for results whose
// columns change between pages.
func TestRowWriterRekey(t *testing.T) {
	a := []driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "b", Type: driver.TypeString}}
	b := []driver.Column{{Name: "c", Type: driver.TypeString}, {Name: "id", Type: driver.TypeInt}}
	for _, f := range []query.Format{query.FormatJSON, query.FormatNDJSON} {
		var buf bytes.Buffer
		rw, _ := query.NewRowWriter(&buf, f)
		_ = rw.Begin(a)
		_ = rw.Row(driver.Row{driver.Int(1), driver.String("x")})
		rk, ok := rw.(query.Rekeyer)
		if !ok {
			t.Fatalf("%s writer is not a Rekeyer", f)
		}
		rk.Rekey(b)
		_ = rw.Row(driver.Row{driver.String("C"), driver.Int(2)})
		_ = rw.End(driver.Stats{})
		out := strings.Join(strings.Fields(buf.String()), "")
		if !strings.Contains(out, `{"id":1,"b":"x"}`) || !strings.Contains(out, `{"c":"C","id":2}`) {
			t.Errorf("%s output = %s", f, out)
		}
	}
	var buf bytes.Buffer
	rw, _ := query.NewRowWriter(&buf, query.FormatCSV)
	if _, ok := rw.(query.Rekeyer); ok {
		t.Error("csv writer must not be a Rekeyer: its header is fixed")
	}
}
