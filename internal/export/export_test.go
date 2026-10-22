package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/query"
)

const peopleSQL = "select * from tobira_people order by id"

var peopleRef = driver.CollectionRef{Namespace: "main", Name: drivertest.PeopleCollection()}

func open(t *testing.T, f *drivertest.Fake) driver.Conn {
	t.Helper()
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertNoTemp fails when a temp file of an export is left in dir.
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tobira-export-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

func assertMode0600(t *testing.T, p string) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

// writeFormat renders the people rows the way `tobira query` does.
func writeFormat(t *testing.T, c driver.Conn, f query.Format) string {
	t.Helper()
	res, err := query.Run(context.Background(), c, peopleSQL, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := query.Write(&b, f, res); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestFormatForPath(t *testing.T) {
	cases := map[string]query.Format{
		"a.csv": query.FormatCSV, "A.CSV": query.FormatCSV, "/x/y.json": query.FormatJSON,
		"r.ndjson": query.FormatNDJSON, "r.jsonl": query.FormatNDJSON,
	}
	for p, want := range cases {
		if got, ok := export.FormatForPath(p); !ok || got != want {
			t.Errorf("FormatForPath(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
	for _, p := range []string{"a.txt", "noext", "a.table", ""} {
		if _, ok := export.FormatForPath(p); ok {
			t.Errorf("FormatForPath(%q) ok, want false", p)
		}
	}
}

func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]string{
		"~":          home,
		"~/out.csv":  filepath.Join(home, "out.csv"),
		"/abs/x.csv": "/abs/x.csv",
		"rel/x.csv":  "rel/x.csv",
		"~user/x":    "~user/x",
	}
	for in, want := range cases {
		got, err := export.ExpandPath(in)
		if err != nil || got != want {
			t.Errorf("ExpandPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := export.ExpandPath(""); err == nil {
		t.Error("ExpandPath(\"\") = nil error")
	}
}

func TestToFileQueryMatchesQueryFormats(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	c := open(t, f)
	for _, format := range []query.Format{query.FormatCSV, query.FormatNDJSON} {
		t.Run(string(format), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "out")
			res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}, Driver: "fake"},
				export.Options{Format: format, Path: p, NoNative: true})
			if err != nil {
				t.Fatal(err)
			}
			got := read(t, p)
			if want := writeFormat(t, c, format); got != want {
				t.Fatalf("export differs from query output\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			if res.Native || res.Rows != int64(len(drivertest.People.Rows)) || res.Bytes != int64(len(got)) || res.Path != p {
				t.Errorf("result = %+v", res)
			}
			assertMode0600(t, p)
			assertNoTemp(t, filepath.Dir(p))
		})
	}
}

func TestToFileQueryJSON(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	p := filepath.Join(t.TempDir(), "out.json")
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatJSON, Path: p})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Columns []map[string]string `json:"columns"`
		Rows    []map[string]any    `json:"rows"`
		Stats   map[string]any      `json:"stats"`
	}
	if err := json.Unmarshal([]byte(read(t, p)), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rows) != len(drivertest.People.Rows) || len(doc.Columns) != len(drivertest.People.Columns) || doc.Stats["truncated"] != false {
		t.Errorf("doc: %d rows, %d columns, stats %v", len(doc.Rows), len(doc.Columns), doc.Stats)
	}
	if res.Native {
		t.Error("json must never take the native path")
	}
}

func TestToFileNativeCSV(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	c := open(t, f)
	p := filepath.Join(t.TempDir(), "out.csv")
	var last export.Progress
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatCSV, Path: p, Progress: func(pr export.Progress) { last = pr }})
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	ex, _ := driver.As[driver.Exporter](c)
	if err := ex.Export(context.Background(), driver.Query{Text: peopleSQL}, "csv", &want); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	if got != want.String() {
		t.Fatalf("native export differs\n--- got ---\n%s\n--- want ---\n%s", got, want.String())
	}
	if !strings.HasPrefix(got, "id,name,") {
		t.Errorf("native csv has no header: %q", got[:min(len(got), 40)])
	}
	if !res.Native || res.Rows != -1 || res.Bytes != int64(len(got)) {
		t.Errorf("result = %+v", res)
	}
	if last.Rows != -1 || last.Bytes != int64(len(got)) {
		t.Errorf("final progress = %+v", last)
	}
	assertMode0600(t, p)
	assertNoTemp(t, filepath.Dir(p))
}

func TestToFileNativeFallsBack(t *testing.T) {
	for _, kind := range []error{driver.ErrUnsupported, driver.ErrSyntax} {
		f := drivertest.NewFake(drivertest.WithPeople())
		c := open(t, f)
		f.FailNext("export", driver.Wrap(kind, "fake", "export", errors.New("not a select")))
		p := filepath.Join(t.TempDir(), "out.csv")
		res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
			export.Options{Format: query.FormatCSV, Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if res.Native || res.Rows != int64(len(drivertest.People.Rows)) {
			t.Errorf("%v: result = %+v, want streamed fallback", kind, res)
		}
		if got, want := read(t, p), writeFormat(t, c, query.FormatCSV); got != want {
			t.Errorf("%v: fallback output differs", kind)
		}
	}
}

// The pg Exporter always runs in BEGIN READ ONLY, so a query that writes
// (nextval, a data-modifying CTE) is refused there with ErrReadOnly. On a
// writable call the export falls back to streaming; a read-only call must
// still fail.
func TestToFileNativeReadOnlyFallsBackOnlyWhenWritable(t *testing.T) {
	for _, ro := range []bool{false, true} {
		f := drivertest.NewFake(drivertest.WithPeople())
		c := open(t, f)
		f.FailNext("export", driver.Wrap(driver.ErrReadOnly, "fake", "export", errors.New("read-only transaction")))
		dir := t.TempDir()
		p := filepath.Join(dir, "out.csv")
		res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL, ReadOnly: ro}},
			export.Options{Format: query.FormatCSV, Path: p})
		if ro {
			if driver.KindOf(err) != driver.ErrReadOnly {
				t.Fatalf("read-only: err = %v, want ErrReadOnly", err)
			}
			if _, serr := os.Stat(p); !os.IsNotExist(serr) {
				t.Error("read-only: target created on failure")
			}
			assertNoTemp(t, dir)
			continue
		}
		if err != nil {
			t.Fatalf("writable: %v", err)
		}
		if res.Native || res.Rows != int64(len(drivertest.People.Rows)) {
			t.Errorf("writable: result = %+v, want streamed fallback", res)
		}
		if got, want := read(t, p), writeFormat(t, c, query.FormatCSV); got != want {
			t.Error("writable: fallback output differs")
		}
	}
}

// headerThenFail is an Exporter that writes a CSV header and then fails, as
// COPY ... WITH (HEADER) does when the server refuses the first row.
type headerThenFail struct {
	driver.Conn
	err error
}

func (headerThenFail) ExportFormats() []string { return []string{"csv"} }

func (h headerThenFail) Export(_ context.Context, _ driver.Query, _ string, w io.Writer) error {
	if _, err := io.WriteString(w, "id,name\n"); err != nil {
		return err
	}
	return h.err
}

// postgres sends the COPY header before the first row fails, so the refusal
// arrives with the header still in the export buffer. Nothing has reached the
// file, so the fallback still applies and the header is not duplicated.
func TestToFileNativeFallsBackAfterBufferedHeader(t *testing.T) {
	for _, kind := range []error{driver.ErrReadOnly, driver.ErrSyntax} {
		c := open(t, drivertest.NewFake(drivertest.WithPeople()))
		hc := headerThenFail{Conn: c, err: driver.Wrap(kind, "fake", "export", errors.New("refused"))}
		p := filepath.Join(t.TempDir(), "out.csv")
		res, err := export.ToFile(context.Background(), export.QuerySource{Conn: hc, Query: driver.Query{Text: peopleSQL}},
			export.Options{Format: query.FormatCSV, Path: p})
		if err != nil {
			t.Fatalf("%v: %v", kind, err)
		}
		if res.Native || res.Rows != int64(len(drivertest.People.Rows)) {
			t.Errorf("%v: result = %+v, want streamed fallback", kind, res)
		}
		got, want := read(t, p), writeFormat(t, c, query.FormatCSV)
		if got != want {
			t.Errorf("%v: fallback output differs\n--- got ---\n%s\n--- want ---\n%s", kind, got, want)
		}
		if res.Bytes != int64(len(want)) {
			t.Errorf("%v: bytes = %d, want %d", kind, res.Bytes, len(want))
		}
	}
}

func TestToFileNativeOtherErrorFails(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	c := open(t, f)
	f.FailNext("export", driver.Wrap(driver.ErrPermission, "fake", "export", errors.New("denied")))
	dir := t.TempDir()
	_, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatCSV, Path: filepath.Join(dir, "out.csv")})
	if driver.KindOf(err) != driver.ErrPermission {
		t.Fatalf("err = %v, want permission", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "out.csv")); !os.IsNotExist(serr) {
		t.Error("target created on failure")
	}
	assertNoTemp(t, dir)
}

func TestToFileNativeSkipped(t *testing.T) {
	cases := map[string]struct {
		q        driver.Query
		noNative bool
		rows     int64
	}{
		"limit":    {driver.Query{Text: peopleSQL, Limit: 5}, false, 5},
		"noNative": {driver.Query{Text: peopleSQL}, true, int64(len(drivertest.People.Rows))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := drivertest.NewFake(drivertest.WithPeople())
			c := open(t, f)
			p := filepath.Join(t.TempDir(), "out.csv")
			res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: tc.q},
				export.Options{Format: query.FormatCSV, Path: p, NoNative: tc.noNative})
			if err != nil {
				t.Fatal(err)
			}
			if res.Native || res.Rows != tc.rows {
				t.Errorf("result = %+v, want %d streamed rows", res, tc.rows)
			}
			if n := strings.Count(read(t, p), "\n"); int64(n) != tc.rows+1 {
				t.Errorf("%d lines, want %d", n, tc.rows+1)
			}
			for _, call := range f.Calls() {
				if call.Op == "export" {
					t.Error("native Export was called")
				}
			}
		})
	}
}

func TestToFileNoExporterStreams(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople(), drivertest.WithoutCapabilities(driver.CapExporter)))
	p := filepath.Join(t.TempDir(), "out.csv")
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatCSV, Path: p})
	if err != nil || res.Native {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestToFileCollectionPages(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	c := open(t, f)
	p := filepath.Join(t.TempDir(), "out.ndjson")
	opts := driver.BrowseOptions{Sort: []driver.SortKey{{Field: "id", Desc: true}}, Limit: 7, ReadOnly: true}
	res, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: peopleRef, Opts: opts, Driver: "fake"},
		export.Options{Format: query.FormatNDJSON, Path: p})
	if err != nil {
		t.Fatal(err)
	}
	n := len(drivertest.People.Rows)
	lines := strings.Split(strings.TrimSuffix(read(t, p), "\n"), "\n")
	if len(lines) != n || res.Rows != int64(n) {
		t.Fatalf("%d lines, result %+v, want %d", len(lines), res, n)
	}
	for i, l := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(l), &obj); err != nil {
			t.Fatal(err)
		}
		if int(obj["id"].(float64)) != n-i {
			t.Fatalf("line %d id = %v, want %d (sort kept)", i, obj["id"], n-i)
		}
	}
	browses := 0
	for _, call := range f.Calls() {
		if call.Op == "browse" {
			browses++
		}
	}
	if want := (n + 6) / 7; browses != want {
		t.Errorf("%d browse calls, want %d pages of 7", browses, want)
	}
}

func TestToFileCollectionFilterAndDefaultPage(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	c := open(t, f)
	p := filepath.Join(t.TempDir(), "out.csv")
	opts := driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpLe, Value: driver.Int(10)}}}}
	res, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: peopleRef, Opts: opts},
		export.Options{Format: query.FormatCSV, Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 10 || res.Native {
		t.Errorf("result = %+v, want 10 streamed rows", res)
	}
	for _, call := range f.Calls() {
		if call.Op == "export" {
			t.Error("a collection export must not use the native Exporter")
		}
	}
}

func TestToFileCollectionNoBrowser(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople(), drivertest.WithoutCapabilities(driver.CapBrowser)))
	dir := t.TempDir()
	_, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: peopleRef},
		export.Options{Format: query.FormatCSV, Path: filepath.Join(dir, "x.csv")})
	if driver.KindOf(err) != driver.ErrUnsupported {
		t.Fatalf("err = %v, want unsupported", err)
	}
	assertNoTemp(t, dir)
}

func TestToFileRefusesBadTargets(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	dir := t.TempDir()
	src := export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}}
	cases := map[string]string{
		"directory":      dir,
		"missing parent": filepath.Join(dir, "nope", "x.csv"),
		"parent is file": filepath.Join(dir, "file", "x.csv"),
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, p := range cases {
		_, err := export.ToFile(context.Background(), src, export.Options{Format: query.FormatCSV, Path: p})
		if err == nil {
			t.Errorf("%s: nil error", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "nope")); !os.IsNotExist(err) {
		t.Error("a missing parent was created")
	}
	if _, err := export.ToFile(context.Background(), src, export.Options{Format: query.FormatTable, Path: filepath.Join(dir, "t")}); err == nil {
		t.Error("table format: nil error")
	}
	assertNoTemp(t, dir)
}

func TestToFileOverwritesWithMode0600(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	p := filepath.Join(t.TempDir(), "out.csv")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatCSV, Path: p}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); !strings.HasPrefix(got, "id,") {
		t.Errorf("content = %q", got[:min(40, len(got))])
	}
	assertMode0600(t, p)
}

func TestToFileFollowsSymlink(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	realDir, linkDir := t.TempDir(), t.TempDir()
	real := filepath.Join(realDir, "real.csv")
	link := filepath.Join(linkDir, "link.csv")
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatCSV, Path: link})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", st, err)
	}
	if got := read(t, real); !strings.HasPrefix(got, "id,") {
		t.Errorf("real target not written: %q", got)
	}
	if want, _ := filepath.EvalSymlinks(real); res.Path != want {
		t.Errorf("result path = %q, want %q", res.Path, want)
	}
	assertNoTemp(t, realDir)
	assertNoTemp(t, linkDir)
}

func TestToFileExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatNDJSON, Path: "~/out.ndjson"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "out.ndjson"); res.Path != want {
		t.Errorf("path = %q, want %q", res.Path, want)
	}
	read(t, filepath.Join(home, "out.ndjson"))
}

// TestToFileCancel cancels mid-export: ToFile returns context.Canceled, no
// temp file remains and the existing target keeps its old content.
func TestToFileCancel(t *testing.T) {
	for _, format := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		t.Run(string(format), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := drivertest.NewFake()
			f.OnQuery("gen", func(driver.Query) (driver.ResultSet, error) {
				return &genRS{n: 1_000_000, onRow: func(i int) {
					if i == 5000 {
						cancel()
					}
				}}, nil
			})
			c := open(t, f)
			dir := t.TempDir()
			p := filepath.Join(dir, "out")
			if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := export.ToFile(ctx, export.QuerySource{Conn: c, Query: driver.Query{Text: "gen"}},
				export.Options{Format: format, Path: p, NoNative: true})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if got := read(t, p); got != "old" {
				t.Errorf("target = %q, want old content", got[:min(40, len(got))])
			}
			assertNoTemp(t, dir)
		})
	}
}

func TestToFileCancelBeforeAnyRowLeavesNoFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	dir := t.TempDir()
	_, err := export.ToFile(ctx, export.CollectionSource{Conn: c, Ref: peopleRef},
		export.Options{Format: query.FormatCSV, Path: filepath.Join(dir, "x.csv")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("dir not empty: %v", ents)
	}
}

func TestToFileRowErrorRemovesTemp(t *testing.T) {
	f := drivertest.NewFake()
	boom := driver.Wrap(driver.ErrConnection, "fake", "query", errors.New("socket closed"))
	f.OnQuery("gen", func(driver.Query) (driver.ResultSet, error) { return &genRS{n: 10_000, failAt: 300, err: boom}, nil })
	c := open(t, f)
	dir := t.TempDir()
	_, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: "gen"}},
		export.Options{Format: query.FormatNDJSON, Path: filepath.Join(dir, "x.ndjson")})
	if driver.KindOf(err) != driver.ErrConnection {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("dir not empty: %v", ents)
	}
}

func TestToFileProgress(t *testing.T) {
	f := drivertest.NewFake()
	f.OnQuery("gen", func(driver.Query) (driver.ResultSet, error) { return &genRS{n: 50_000}, nil })
	c := open(t, f)
	var calls []export.Progress
	p := filepath.Join(t.TempDir(), "x.csv")
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: "gen"}},
		export.Options{Format: query.FormatCSV, Path: p, NoNative: true, Progress: func(pr export.Progress) { calls = append(calls, pr) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("no progress reported")
	}
	last := calls[len(calls)-1]
	if last.Rows != 50_000 || last.Bytes != res.Bytes || res.Rows != 50_000 {
		t.Errorf("final progress %+v, result %+v", last, res)
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].Rows < calls[i-1].Rows || calls[i].Bytes < calls[i-1].Bytes {
			t.Errorf("progress went backwards: %+v then %+v", calls[i-1], calls[i])
		}
	}
}

func TestToFileQueryLimitTruncates(t *testing.T) {
	f := drivertest.NewFake()
	// The generator ignores Query.Limit, as a driver may: ToFile enforces it.
	f.OnQuery("gen", func(driver.Query) (driver.ResultSet, error) { return &genRS{n: 100}, nil })
	c := open(t, f)
	p := filepath.Join(t.TempDir(), "x.json")
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: "gen", Limit: 10}},
		export.Options{Format: query.FormatJSON, Path: p})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rows  []any          `json:"rows"`
		Stats map[string]any `json:"stats"`
	}
	if err := json.Unmarshal([]byte(read(t, p)), &doc); err != nil {
		t.Fatal(err)
	}
	if res.Rows != 10 || len(doc.Rows) != 10 || doc.Stats["truncated"] != true {
		t.Errorf("rows=%d doc rows=%d stats=%v", res.Rows, len(doc.Rows), doc.Stats)
	}
}

// genRS synthesises rows lazily, so a test can stream any number of rows
// without holding them.
type genRS struct {
	n, i   int
	failAt int // 0 = never
	err    error
	onRow  func(i int)
}

var genCols = []driver.Column{
	{Name: "id", Type: driver.TypeInt},
	{Name: "name", Type: driver.TypeString},
	{Name: "payload", Type: driver.TypeString},
}

func (g *genRS) Columns() []driver.Column { return genCols }

func (g *genRS) Next(ctx context.Context) (driver.Row, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if g.failAt > 0 && g.i == g.failAt {
		return nil, false, g.err
	}
	if g.i >= g.n {
		return nil, false, nil
	}
	g.i++
	if g.onRow != nil {
		g.onRow(g.i)
	}
	return driver.Row{
		driver.Int(int64(g.i)),
		driver.String("name-" + strconv.Itoa(g.i)),
		driver.String("payload, with \"quotes\" and some padding text " + strconv.Itoa(g.i*7)),
	}, true, nil
}

func (g *genRS) Stats() driver.Stats { return driver.Stats{RowsAffected: -1} }
func (g *genRS) Close() error        { return nil }

func TestToFileRows(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	res, err := query.Run(context.Background(), c, peopleSQL, query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		t.Run(string(format), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "out")
			var last export.Progress
			got, err := export.ToFile(context.Background(),
				export.RowsSource{Columns: res.Columns, Rows: res.Rows, Stats: res.Stats},
				export.Options{Format: format, Path: p, Progress: func(pr export.Progress) { last = pr }})
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err := query.Write(&want, format, res); err != nil {
				t.Fatal(err)
			}
			if body := read(t, p); body != want.String() {
				t.Fatalf("rows export differs from query output\n--- got ---\n%s\n--- want ---\n%s", body, want.String())
			}
			if got.Native || got.Rows != int64(len(res.Rows)) || last.Rows != got.Rows {
				t.Errorf("result = %+v, last progress %+v", got, last)
			}
			assertMode0600(t, p)
			assertNoTemp(t, filepath.Dir(p))
		})
	}
}

func TestToFileRowsCancelLeavesNoFile(t *testing.T) {
	rows := make([]driver.Row, 200_000)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i))}
	}
	cols := []driver.Column{{Name: "n", Type: driver.TypeInt}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	_, err := export.ToFile(ctx, export.RowsSource{Columns: cols, Rows: rows},
		export.Options{Format: query.FormatCSV, Path: filepath.Join(dir, "x.csv"), Progress: func(export.Progress) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("dir not empty: %v", ents)
	}
}
