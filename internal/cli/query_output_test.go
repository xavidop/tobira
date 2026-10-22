package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestQueryOutputInfersFormat(t *testing.T) {
	h := newHarness(t)
	want, _, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--format", "ndjson")
	if code != 0 {
		t.Fatal(code)
	}
	for _, ext := range []string{".ndjson", ".jsonl"} {
		p := filepath.Join(t.TempDir(), "out"+ext)
		out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--output", p)
		if code != 0 || out != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
		}
		if errOut != "wrote 3 rows to "+p+"\n" {
			t.Errorf("stderr = %q", errOut)
		}
		if got := readFile(t, p); got != want {
			t.Errorf("file = %q, want the stdout output %q", got, want)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", st.Mode().Perm())
		}
	}
}

func TestQueryOutputExplicitFormatWins(t *testing.T) {
	h := newHarness(t)
	p := filepath.Join(t.TempDir(), "out.txt")
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--output", p, "--format", "json", "--limit", "2")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if got := readFile(t, p); !strings.HasPrefix(got, "{\n  \"columns\": [") {
		t.Errorf("file is not json: %q", got[:min(40, len(got))])
	}
	if errOut != "wrote 2 rows to "+p+"\n" {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestQueryOutputNativeCSV(t *testing.T) {
	h := newHarness(t)
	p := filepath.Join(t.TempDir(), "out.csv")
	out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--output", p)
	if code != 0 || out != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	got := readFile(t, p)
	if want := "wrote " + strconv.Itoa(len(got)) + " bytes to " + p + "\n"; errOut != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
	if !strings.HasPrefix(got, "id,name,") {
		t.Errorf("file = %q", got)
	}
	exported := false
	for _, c := range h.fake.Calls() {
		exported = exported || c.Op == "export"
	}
	if !exported {
		t.Error("csv with no --limit did not use the native exporter")
	}
}

func TestQueryOutputLimitForcesStreaming(t *testing.T) {
	h := newHarness(t)
	p := filepath.Join(t.TempDir(), "out.csv")
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--output", p, "--limit", "2")
	if code != 0 || errOut != "wrote 2 rows to "+p+"\n" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, c := range h.fake.Calls() {
		if c.Op == "export" {
			t.Error("--limit must force the streaming path")
		}
	}
}

// genRows serves n synthetic rows for the query "gen".
func genRows(h *harness, n int) {
	h.fake.OnQuery("gen", func(driver.Query) (driver.ResultSet, error) {
		rows := make([]driver.Row, n)
		for i := range rows {
			rows[i] = driver.Row{driver.Int(int64(i))}
		}
		return driver.NewSliceResultSet([]driver.Column{{Name: "n", Type: driver.TypeInt}}, rows, driver.Stats{RowsAffected: -1}), nil
	})
}

func TestQueryOutputIgnoresDefaultLimit(t *testing.T) {
	h := newHarness(t)
	genRows(h, 1500)
	p := filepath.Join(t.TempDir(), "out.ndjson")
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", "gen", "--output", p)
	if code != 0 || errOut != "wrote 1500 rows to "+p+"\n" {
		t.Fatalf("code=%d stderr=%q, want all 1500 rows", code, errOut)
	}
	if n := strings.Count(readFile(t, p), "\n"); n != 1500 {
		t.Errorf("%d lines", n)
	}
}

func TestQueryOutputUsageErrors(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		args []string
		msg  string
	}{
		"unknown extension": {[]string{"--output", filepath.Join(dir, "x.txt")}, "choose --format csv|json|ndjson"},
		"table":             {[]string{"--output", filepath.Join(dir, "x.csv"), "--format", "table"}, "--format table"},
		"explain":           {[]string{"--output", filepath.Join(dir, "x.csv"), "--explain"}, "--explain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			out, errOut, code := h.exec(t, context.Background(), append([]string{"query", "local-pg", selectPeople}, tc.args...)...)
			if code != 1 || out != "" || !strings.Contains(errOut, tc.msg) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want 1 and %q", code, out, errOut, tc.msg)
			}
			if len(h.opened) != 0 {
				t.Error("a usage error opened the connection")
			}
		})
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("usage errors wrote files: %v", ents)
	}
}

// TestQueryOutputBadTarget: a bad target is a usage error found before the
// connection is opened.
func TestQueryOutputBadTarget(t *testing.T) {
	dir := t.TempDir()
	for name, p := range map[string]string{
		"missing directory": filepath.Join(dir, "nope", "x.csv"),
		"directory target":  dir,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", selectPeople, "--output", p, "--format", "csv")
			if code != 1 || out != "" || errOut == "" {
				t.Fatalf("code=%d stdout=%q stderr=%q, want exit 1", code, out, errOut)
			}
			if len(h.opened) != 0 {
				t.Error("the connection was opened before the target was checked")
			}
		})
	}
}

func TestQueryOutputNoResultSet(t *testing.T) {
	h := newHarness(t)
	h.fake.OnQuery("upd", func(driver.Query) (driver.ResultSet, error) {
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: 2}), nil
	})
	dir := t.TempDir()
	p := filepath.Join(dir, "x.ndjson")
	out, errOut, code := h.exec(t, context.Background(), "query", "local-pg", "upd", "--output", p)
	if code != 0 || out != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if want := "no result set: 2 rows affected; nothing written to " + p + "\n"; errOut != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files written: %v", ents)
	}
}

// deadlineRS records whether the context of its first Next had a deadline.
type deadlineRS struct {
	has  *bool
	done bool
}

func (d *deadlineRS) Columns() []driver.Column {
	return []driver.Column{{Name: "n", Type: driver.TypeInt}}
}
func (d *deadlineRS) Next(ctx context.Context) (driver.Row, bool, error) {
	if d.done {
		return nil, false, nil
	}
	d.done = true
	_, *d.has = ctx.Deadline()
	return driver.Row{driver.Int(1)}, true, nil
}
func (d *deadlineRS) Stats() driver.Stats { return driver.Stats{RowsAffected: -1} }
func (d *deadlineRS) Close() error        { return nil }

// TestQueryOutputTimeoutDefault: with --output the 30s default timeout does
// not apply; an explicit --timeout does.
func TestQueryOutputTimeoutDefault(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--timeout", "5m"}, true},
	} {
		h := newHarness(t)
		var has bool
		h.fake.OnQuery("dl", func(driver.Query) (driver.ResultSet, error) { return &deadlineRS{has: &has}, nil })
		args := append([]string{"query", "local-pg", "dl", "--output", filepath.Join(t.TempDir(), "x.ndjson")}, tc.args...)
		if _, errOut, code := h.exec(t, context.Background(), args...); code != 0 {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
		if has != tc.want {
			t.Errorf("args %v: deadline = %v, want %v", tc.args, has, tc.want)
		}
	}
	// Without --output the default still applies.
	h := newHarness(t)
	var has bool
	h.fake.OnQuery("dl", func(driver.Query) (driver.ResultSet, error) { return &deadlineRS{has: &has}, nil })
	if _, _, code := h.exec(t, context.Background(), "query", "local-pg", "dl", "--format", "json"); code != 0 || !has {
		t.Errorf("plain query: code=%d deadline=%v, want the 30s default", code, has)
	}
}

func TestQueryOutputInterruptLeavesNoFile(t *testing.T) {
	h := newHarness(t)
	h.fake.SetLatency(10 * time.Second)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, errOut, code := h.exec(t, ctx, "query", "local-pg", selectPeople, "--output", filepath.Join(dir, "x.ndjson"))
	if code != 3 || !strings.Contains(errOut, "canceled") {
		t.Fatalf("code=%d stderr=%q, want 3 and canceled", code, errOut)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left: %v", ents)
	}
}

func TestQueryOutputQueryErrorExitCode(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	_, _, code := h.exec(t, context.Background(), "query", "local-pg", "select * from missing_table", "--output", filepath.Join(dir, "x.ndjson"))
	if code != 3 {
		t.Fatalf("code=%d, want 3", code)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left: %v", ents)
	}
}

// An export that holds value previews (a _truncated marker that Describe
// lists as Generated) says how many on stderr.
func TestQueryOutputWarnsAboutPreviews(t *testing.T) {
	h := newHarness(t)
	ref := driver.CollectionRef{Namespace: "main", Name: "notes"}
	h.fake.AddCollection("main", "notes", []driver.Column{{Name: "value", Type: driver.TypeString}, {Name: "_truncated", Type: driver.TypeBool}},
		[]driver.Row{{driver.String("cut…"), driver.Bool(true)}})
	h.fake.OnDescribe("main", "notes", func(s *driver.CollectionSchema) {
		for i := range s.Fields {
			s.Fields[i].Generated = s.Fields[i].Name == "_truncated"
		}
	})
	h.fake.OnQuery("notes", func(driver.Query) (driver.ResultSet, error) {
		cols := []driver.Column{{Name: "value", Type: driver.TypeString, Source: &ref}, {Name: "_truncated", Type: driver.TypeBool, Source: &ref}}
		rows := []driver.Row{{driver.String("cut…"), driver.Bool(true)}, {driver.String("a…"), driver.Bool(true)}, {driver.String("b"), driver.Bool(false)}}
		return driver.NewSliceResultSet(cols, rows, driver.Stats{RowsAffected: -1}), nil
	})
	p := filepath.Join(t.TempDir(), "out.json")
	_, errOut, code := h.exec(t, context.Background(), "query", "local-pg", "notes", "--output", p)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if want := "wrote 3 rows to " + p + "\nwarning: 2 values were previews, not full values\n"; errOut != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
}
