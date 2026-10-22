package query_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/query"
)

var update = flag.Bool("update", false, "update golden files")

func render(t *testing.T, f query.Format, r *query.Result) string {
	t.Helper()
	var b bytes.Buffer
	if err := query.Write(&b, f, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
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

func TestFormats(t *testing.T) {
	_, c := openFake(t, drivertest.WithPeople())
	res, err := query.Run(context.Background(), c, "select * from tobira_people order by id limit 3", query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res.Stats.Duration = 12 * time.Millisecond
	for _, f := range []query.Format{"table", "json", "ndjson", "csv"} {
		t.Run(string(f), func(t *testing.T) {
			golden(t, string(f)+".golden", render(t, f, res))
		})
	}
}

func TestUnknownFormat(t *testing.T) {
	err := query.Write(&bytes.Buffer{}, "yaml", &query.Result{})
	if err == nil || !strings.Contains(err.Error(), "table, json, ndjson, csv") {
		t.Fatalf("err = %v", err)
	}
}

func nullEmptyResult() *query.Result {
	return &query.Result{
		Columns: []driver.Column{{Name: "a", Type: driver.TypeString}, {Name: "b", Type: driver.TypeString}},
		Rows: []driver.Row{
			{driver.Null(), driver.String("")},
			{driver.String("x"), driver.Null()},
		},
		Stats: driver.Stats{RowsAffected: -1, Duration: 2 * time.Millisecond},
	}
}

func TestFormatsNullVsEmpty(t *testing.T) {
	r := nullEmptyResult()

	csv := render(t, "csv", r)
	if want := "a,b\n,\"\"\nx,\n"; csv != want {
		t.Errorf("csv = %q, want %q", csv, want)
	}

	nd := render(t, "ndjson", r)
	if want := "{\"a\":null,\"b\":\"\"}\n{\"a\":\"x\",\"b\":null}\n"; nd != want {
		t.Errorf("ndjson = %q, want %q", nd, want)
	}

	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(render(t, "json", r)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Rows[0]["a"] != nil || doc.Rows[0]["b"] != "" {
		t.Errorf("json row 0 = %#v", doc.Rows[0])
	}

	tbl := render(t, "table", r)
	lines := strings.Split(tbl, "\n")
	if !strings.HasPrefix(lines[2], "NULL") || strings.Contains(lines[2], "NULL ") && strings.Count(lines[2], "NULL") != 1 {
		t.Errorf("table row 0 = %q", lines[2])
	}
	if strings.TrimSpace(strings.TrimPrefix(lines[2], "NULL")) != "" {
		t.Errorf("empty string must render blank, got %q", lines[2])
	}
	if !strings.HasSuffix(lines[3], "NULL") {
		t.Errorf("table row 1 = %q", lines[3])
	}
}

func TestCSVQuoting(t *testing.T) {
	r := &query.Result{
		Columns: []driver.Column{{Name: "v"}},
		Rows: []driver.Row{
			{driver.String("a,b")}, {driver.String(`q"q`)}, {driver.String("l1\nl2")},
			{driver.String(" lead")}, {driver.String("trail ")}, {driver.String("plain")},
		},
	}
	want := "v\n\"a,b\"\n\"q\"\"q\"\n\"l1\nl2\"\n\" lead\"\n\"trail \"\nplain\n"
	if got := render(t, "csv", r); got != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
}

func TestJSONDuplicateColumns(t *testing.T) {
	r := &query.Result{
		Columns: []driver.Column{{Name: "a"}, {Name: "a"}, {Name: "a_2"}},
		Rows:    []driver.Row{{driver.Int(1), driver.Int(2), driver.Int(3)}},
	}
	got := render(t, "ndjson", r)
	if want := "{\"a\":1,\"a_2\":2,\"a_2_2\":3}\n"; got != want {
		t.Errorf("ndjson = %q, want %q", got, want)
	}
}

func TestJSONKeepsColumnOrderAndStats(t *testing.T) {
	r := &query.Result{
		Columns: []driver.Column{{Name: "z", Type: driver.TypeInt}, {Name: "a", Type: driver.TypeString}},
		Rows:    []driver.Row{{driver.Int(1), driver.String("x")}},
		Stats:   driver.Stats{RowsAffected: -1, Duration: 1500 * time.Microsecond, Truncated: true},
	}
	out := render(t, "json", r)
	if strings.Index(out, `"z": 1`) > strings.Index(out, `"a": "x"`) {
		t.Errorf("order lost:\n%s", out)
	}
	if !strings.Contains(out, `"duration_ms": 2`) || !strings.Contains(out, `"truncated": true`) {
		t.Errorf("stats:\n%s", out)
	}
	if !strings.HasSuffix(out, "}\n") || strings.HasSuffix(out, "\n\n") {
		t.Errorf("trailing newline: %q", out[len(out)-4:])
	}
}

func TestEmptyRowsJSON(t *testing.T) {
	r := &query.Result{Columns: []driver.Column{{Name: "a"}}}
	if out := render(t, "json", r); !strings.Contains(out, `"rows": []`) {
		t.Errorf("rows must be [] not null:\n%s", out)
	}
}

func TestTableFooters(t *testing.T) {
	one := &query.Result{Columns: []driver.Column{{Name: "a"}}, Rows: []driver.Row{{driver.Int(1)}}, Stats: driver.Stats{Duration: 300 * time.Microsecond}}
	if out := render(t, "table", one); !strings.HasSuffix(out, "1 row · <1ms\n") {
		t.Errorf("one row: %q", out)
	}
	tr := &query.Result{Columns: []driver.Column{{Name: "a"}}, Stats: driver.Stats{Duration: 1230 * time.Millisecond, Truncated: true}}
	if out := render(t, "table", tr); !strings.HasSuffix(out, "0 rows · 1.23s · truncated\n") {
		t.Errorf("truncated: %q", out)
	}
	w := &query.Result{Stats: driver.Stats{RowsAffected: 3, Duration: 12 * time.Millisecond}}
	if out := render(t, "table", w); out != "3 rows affected · 12ms\n" {
		t.Errorf("write: %q", out)
	}
}

func TestExplainOutput(t *testing.T) {
	r := &query.Result{Plan: &driver.Plan{Format: "text", Text: "Seq Scan"}}
	if got := render(t, "table", r); got != "Seq Scan\n" {
		t.Errorf("table = %q", got)
	}
	if got := render(t, "csv", r); got != "Seq Scan\n" {
		t.Errorf("csv = %q", got)
	}
	want := "{\"plan\":\"Seq Scan\",\"format\":\"text\"}\n"
	for _, f := range []query.Format{"json", "ndjson"} {
		if got := render(t, f, r); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
}

// displayWidth mirrors what a terminal shows for the strings used below.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
			r >= 0xFF00 && r <= 0xFF60, r >= 0x1F300:
			w += 2
		case r == 0x0301:
		default:
			w++
		}
	}
	return w
}

func TestTableWideValues(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"k": strings.Repeat("v", 10*1024)})
	r := &query.Result{
		Columns: []driver.Column{{Name: "name"}, {Name: "doc"}},
		Rows: []driver.Row{
			{driver.String("山田太郎"), driver.String("ok")},
			{driver.String("🙂🙂 smile"), driver.String("e\u0301")},
			{driver.String("plain"), driver.JSON(big)},
			{driver.String("multi\nline\ttab"), driver.String(strings.Repeat("あ", 40))},
		},
		Stats: driver.Stats{RowsAffected: -1, Duration: time.Millisecond},
	}
	out := render(t, "table", r)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	// header, underline, 4 rows, blank, footer
	if len(lines) != 8 {
		t.Fatalf("want 8 lines, got %d:\n%s", len(lines), out)
	}
	// the second column must start at the same display column on every row
	markers := []string{"doc", "─", "ok", "e\u0301", "{", "あ"}
	for i, l := range lines[:6] {
		idx := strings.Index(l, "  "+markers[i]) + 2
		if i == 1 {
			idx = strings.Index(l, "  ─") + 2
		}
		if got := displayWidth(l[:idx]); got != 14+2 {
			t.Errorf("line %d: second column starts at %d, want 16: %q", i, got, l)
		}
	}
	for _, l := range lines[:6] {
		parts := strings.SplitN(l, "  ", 2)
		if len(parts) < 2 {
			continue
		}
		if w := displayWidth(strings.TrimSpace(parts[1])); w > 60 {
			t.Errorf("cell wider than 60: %d %q", w, l)
		}
	}
	if !strings.Contains(lines[4], "…") || !strings.Contains(lines[5], "…") {
		t.Errorf("long cells must end with …:\n%s", out)
	}
	if strings.Contains(strings.Join(lines[:6], ""), "\t") || !strings.Contains(lines[5], "multi line tab") && !strings.Contains(lines[5], "multi line") {
		t.Errorf("control chars must become spaces:\n%s", out)
	}
	if !utf8.ValidString(out) {
		t.Error("invalid utf8")
	}
}

func TestTableEscapesControlCharacters(t *testing.T) {
	r := &query.Result{
		Columns: []driver.Column{{Name: "h\x1b[2J", Type: driver.TypeString}, {Name: "b", Type: driver.TypeString}},
		Rows: []driver.Row{
			{driver.String("\x1b[31mRED"), driver.String("a\x00b\x7fc\u009bd\u0085e")},
			{driver.String("tab\there\nnl"), driver.String("ok")},
		},
		Stats: driver.Stats{RowsAffected: -1, Duration: time.Millisecond},
	}
	out := render(t, query.FormatTable, r)
	for _, r := range out {
		if r != '\n' && (r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f) {
			t.Fatalf("table output has raw control %U:\n%q", r, out)
		}
	}
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], `h\x1b[2J`) || !strings.Contains(lines[2], `a\x00b\x7fc\u009bd\u0085e`) ||
		!strings.HasPrefix(lines[2], `\x1b[31mRED`) || !strings.HasPrefix(lines[3], "tab here nl") {
		t.Fatalf("unexpected table:\n%s", out)
	}
	// Escapes count towards the width: the second column starts at the same
	// offset on every line.
	col := strings.Index(lines[2], "a\\x00")
	if strings.LastIndex(lines[0], "b") != col || strings.Index(lines[3], "ok") != col {
		t.Errorf("columns misaligned:\n%s", out)
	}
}

func TestPlanTextEscapesControlCharacters(t *testing.T) {
	r := &query.Result{Plan: &driver.Plan{Format: "text", Text: "Seq Scan\n  Filter: (x = '\x1b[31m')\t\n"}}
	out := render(t, query.FormatTable, r)
	if want := "Seq Scan\n  Filter: (x = '\\x1b[31m')\t\n"; out != want {
		t.Errorf("plan = %q, want %q", out, want)
	}
}

func TestMachineFormatsKeepControlBytes(t *testing.T) {
	r := &query.Result{
		Columns: []driver.Column{{Name: "v", Type: driver.TypeString}},
		Rows:    []driver.Row{{driver.String("\x1b[31m\u009b")}},
		Stats:   driver.Stats{RowsAffected: -1},
	}
	if out := render(t, query.FormatCSV, r); out != "v\n\x1b[31m\u009b\n" {
		t.Errorf("csv = %q", out)
	}
	if out := render(t, query.FormatNDJSON, r); !strings.Contains(out, `"\u001b[31m`+"\u009b"+`"`) {
		t.Errorf("ndjson = %q", out)
	}
}

func TestEscapeControls(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"a\nb\r\tc", "a b  c"},
		{"\x1b[0m", `\x1b[0m`},
		{"\x00\x1f\x7f", `\x00\x1f\x7f`},
		{"\u0080\u009f\u00a0é", `\u0080\u009f` + "\u00a0é"},
	}
	for _, tt := range tests {
		if got := query.EscapeControls(tt.in); got != tt.want {
			t.Errorf("EscapeControls(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 4, "abc…"},
		{"abcdef", 1, "…"},
		{"abcdef", 0, ""},
		{"日本語です", 5, "日本…"},
		{"日本", 4, "日本"},
	} {
		got := query.Truncate(tc.in, tc.w)
		if got != tc.want {
			t.Errorf("Truncate(%q,%d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
		if query.DisplayWidth(got) > max(tc.w, 0) {
			t.Errorf("Truncate(%q,%d) too wide: %q", tc.in, tc.w, got)
		}
	}
}

func TestEscapeControlsBidiAndZeroWidth(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a\u200bb", `a\u{200B}b`},
		{"\u200f\u202e", `\u{200F}\u{202E}`},
		{"x\u2066y\u2069", `x\u{2066}y\u{2069}`},
		{"\ufeffbom", `\u{FEFF}bom`},
		{"\u200a\u2010\u202f\u2065\u206a", "\u200a\u2010\u202f\u2065\u206a"}, // neighbours stay
		{"plain é", "plain é"},
	}
	for _, tt := range tests {
		if got := query.EscapeControls(tt.in); got != tt.want {
			t.Errorf("EscapeControls(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	r := &query.Result{
		Columns: []driver.Column{{Name: "v", Type: driver.TypeString}},
		Rows:    []driver.Row{{driver.String("ad\u202emin")}},
		Stats:   driver.Stats{RowsAffected: -1, Duration: time.Millisecond},
	}
	if out := render(t, query.FormatTable, r); strings.ContainsRune(out, '\u202e') || !strings.Contains(out, `ad\u{202E}min`) {
		t.Errorf("table leaves bidi control raw:\n%q", out)
	}
}

func TestDisplayWidthGraphemeClusters(t *testing.T) {
	for _, tc := range []struct {
		name, s string
		want    int
	}{
		{"zwj family", "\U0001F468\u200D\U0001F469\u200D\U0001F467", 2},
		{"combining accent", "é", 1},
		{"skin tone", "\U0001F44D\U0001F3FD", 2},
		{"flag", "\U0001F1EF\U0001F1F5", 2},
		{"cjk", "日本", 4},
		{"ascii", "abc", 3},
	} {
		if got := query.DisplayWidth(tc.s); got != tc.want {
			t.Errorf("%s: DisplayWidth(%q) = %d, want %d", tc.name, tc.s, got, tc.want)
		}
	}
}

func TestTruncateKeepsClustersWhole(t *testing.T) {
	family := "\U0001F468\u200D\U0001F469\u200D\U0001F467"
	// Room for the family (2) plus the ellipsis (1) but not the trailing "x".
	if got, want := query.Truncate(family+"xy", 3), family+"…"; got != want {
		t.Errorf("Truncate = %q, want %q", got, want)
	}
	// Too narrow for the family: nothing of it may remain half-drawn.
	if got := query.Truncate(family+"xy", 2); got != "…" {
		t.Errorf("Truncate = %q, want just the ellipsis", got)
	}
	accent := "ééé"
	if got, want := query.Truncate(accent, 2), "é…"; got != want {
		t.Errorf("Truncate = %q, want %q", got, want)
	}
	if w := query.DisplayWidth(query.Truncate(family+family, 4)); w > 4 {
		t.Errorf("truncated width %d exceeds 4", w)
	}
}

// clusterInputs are texts where rune-sum, uniseg and lipgloss-style width
// algorithms have disagreed: text-presentation selectors, keycaps, Thai and
// Devanagari clusters.
var clusterInputs = []string{
	"\U0001F600\uFE0E", "1\uFE0F\u20E3", "\u0E01\u0E33", "\u0928\u092E\u0938\u094D\u0924\u0947",
	"\u0915\u094D\u0937", "e\u0301", "\U0001F44D\U0001F3FD", "\U0001F1EF\U0001F1F5", "\u65E5\u672C",
}

func TestDisplayWidthAgreesWithLayoutWidth(t *testing.T) {
	for _, in := range clusterInputs {
		if got, want := query.DisplayWidth(in), ansi.StringWidth(in); got != want {
			t.Errorf("DisplayWidth(%q) = %d, ansi.StringWidth = %d", in, got, want)
		}
		for w := 1; w <= 8; w++ {
			tr := query.Truncate("ab"+in+in+"zz", w)
			if got := ansi.StringWidth(tr); got > w {
				t.Errorf("Truncate(%q, %d) = %q is %d wide", in, w, tr, got)
			}
		}
	}
}

func TestEscapeControlsZWJ(t *testing.T) {
	for _, keep := range []string{
		"\U0001F468\u200D\U0001F469\u200D\U0001F467",
		"\U0001F3F3\uFE0F\u200D\U0001F308",
	} {
		if got := query.EscapeControls(keep); got != keep {
			t.Errorf("EscapeControls(%q) = %q, want unchanged", keep, got)
		}
	}
	for _, esc := range []string{
		"\u200D", "a\u200Db", "abc\u200D", "a\u200D\U0001F600", "\u0915\u094D\u200D\u0937",
		"\U0001F600\u200D", "\u200D\U0001F600",
	} {
		got := query.EscapeControls(esc)
		if !strings.Contains(got, `\u{200D}`) || strings.Contains(got, "\u200D") {
			t.Errorf("EscapeControls(%q) = %q, want the ZWJ escaped", esc, got)
		}
	}
}

func TestEscapeControlsInvalidUTF8(t *testing.T) {
	tests := []struct{ in, want string }{
		{"\x9b", `\x{9B}`},
		{"a\x9b[2Jb", `a\x{9B}[2Jb`},
		{"\xff\xfe", `\x{FF}\x{FE}`},
		{"é\xc3", `é\x{C3}`},         // a truncated two-byte sequence
		{"\xe2\x80", `\x{E2}\x{80}`}, // a truncated three-byte sequence
		{"\ufffd", "\ufffd"},         // a real U+FFFD is text
	}
	for _, tt := range tests {
		if got := query.EscapeControls(tt.in); got != tt.want {
			t.Errorf("EscapeControls(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTableNeverEmitsRawInvalidBytes(t *testing.T) {
	var b strings.Builder
	r := &query.Result{
		Columns: []driver.Column{{Name: "c\x9b"}},
		Rows:    []driver.Row{{driver.String("x\x9b[31my")}},
		Stats:   driver.Stats{RowsAffected: -1},
	}
	if err := query.Write(&b, query.FormatTable, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "\x9b") || !strings.Contains(b.String(), `\x{9B}`) {
		t.Fatalf("table output: %q", b.String())
	}
}
