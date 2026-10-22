package export_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/query"
)

// schemalessPage is one scripted Browse page.
type schemalessPage struct {
	cols []driver.Column
	rows []driver.Row
}

func strCol(name string) driver.Column { return driver.Column{Name: name, Type: driver.TypeString} }

var idCol = driver.Column{Name: "id", Type: driver.TypeInt}

// schemalessPages change columns between pages: page 2 reorders, drops b
// and adds a; page 3 adds c first. The union in first-seen order is
// id, b, a, c. A pre-growth value spans lines and holds quotes and commas,
// and one is NULL, to exercise the csv rewrite.
var schemalessPages = []schemalessPage{
	{[]driver.Column{idCol, strCol("b")}, []driver.Row{
		{driver.Int(1), driver.String("multi\nline, \"q\"")},
		{driver.Int(2), driver.Null()},
	}},
	{[]driver.Column{strCol("a"), idCol}, []driver.Row{
		{driver.String("A3"), driver.Int(3)},
	}},
	{[]driver.Column{strCol("c"), idCol, strCol("b")}, []driver.Row{
		{driver.String("C4"), driver.Int(4), driver.String("z")},
	}},
}

// pagedConn serves pages as Browse pages, cursor = page index.
func pagedConn(t *testing.T, pages []schemalessPage, onPage func(i int)) driver.Conn {
	t.Helper()
	inner := open(t, drivertest.NewFake())
	return &browseConn{Conn: inner, browse: func(ctx context.Context, _ driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
		i := 0
		if o.Cursor != "" {
			i, _ = strconv.Atoi(o.Cursor)
		}
		if onPage != nil {
			onPage(i)
		}
		next := ""
		if i+1 < len(pages) {
			next = strconv.Itoa(i + 1)
		}
		return driver.NewSliceResultSet(pages[i].cols, pages[i].rows, driver.Stats{RowsAffected: -1, NextCursor: next}), nil
	}}
}

func exportPages(t *testing.T, c driver.Conn, f query.Format) (string, export.Result) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "out."+string(f))
	res, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: peopleRef, Driver: "fake"},
		export.Options{Format: f, Path: p})
	if err != nil {
		t.Fatal(err)
	}
	assertNoTemp(t, dir)
	assertMode0600(t, p)
	return read(t, p), res
}

// csv takes the union of the columns across pages, in first-seen order, and
// pads rows written before a column appeared.
func TestToFileCollectionUnionCSV(t *testing.T) {
	got, res := exportPages(t, pagedConn(t, schemalessPages, nil), query.FormatCSV)
	want := "id,b,a,c\n" +
		"1,\"multi\nline, \"\"q\"\"\",,\n" +
		"2,,,\n" +
		"3,,A3,\n" +
		"4,z,,C4\n"
	if got != want {
		t.Fatalf("csv:\n%s\nwant:\n%s", got, want)
	}
	if res.Rows != 4 || res.Bytes != int64(len(want)) {
		t.Fatalf("result = %+v, want 4 rows and %d bytes", res, len(want))
	}
	recs, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("not valid csv: %v", err)
	}
	for i, r := range recs {
		if len(r) != 4 {
			t.Errorf("record %d has %d fields", i, len(r))
		}
	}
}

// json lists the union in "columns" and keys each row by its own columns.
func TestToFileCollectionUnionJSON(t *testing.T) {
	got, _ := exportPages(t, pagedConn(t, schemalessPages, nil), query.FormatJSON)
	var doc struct {
		Columns []struct{ Name, Type string } `json:"columns"`
		Rows    []json.RawMessage             `json:"rows"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, got)
	}
	var names []string
	for _, c := range doc.Columns {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"id", "b", "a", "c"}) {
		t.Fatalf("columns = %v", names)
	}
	wantRows := []string{`{"id":1,"b":"multi\nline, \"q\""}`, `{"id":2,"b":null}`, `{"a":"A3","id":3}`, `{"c":"C4","id":4,"b":"z"}`}
	if len(doc.Rows) != len(wantRows) {
		t.Fatalf("%d rows", len(doc.Rows))
	}
	for i, r := range doc.Rows {
		var compact bytes.Buffer
		if err := json.Compact(&compact, r); err != nil {
			t.Fatal(err)
		}
		if compact.String() != wantRows[i] {
			t.Errorf("row %d = %s, want %s", i, compact.String(), wantRows[i])
		}
	}
	if !strings.Contains(got, `"stats": {`) {
		t.Errorf("stats lost:\n%s", got)
	}
}

// ndjson keys each row by its own columns.
func TestToFileCollectionUnionNDJSON(t *testing.T) {
	got, _ := exportPages(t, pagedConn(t, schemalessPages, nil), query.FormatNDJSON)
	want := `{"id":1,"b":"multi\nline, \"q\""}` + "\n" + `{"id":2,"b":null}` + "\n" + `{"a":"A3","id":3}` + "\n" + `{"c":"C4","id":4,"b":"z"}` + "\n"
	if got != want {
		t.Fatalf("ndjson:\n%s\nwant:\n%s", got, want)
	}
}

// The Fake's schemaless document collection, paged two at a time, exports
// every field in every format.
func TestToFileCollectionSchemalessFake(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithDocuments()))
	ref := driver.CollectionRef{Namespace: "main", Name: drivertest.DocumentsCollection}
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		t.Run(string(f), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "docs."+string(f))
			if _, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: ref, Opts: driver.BrowseOptions{Limit: 2}},
				export.Options{Format: f, Path: p}); err != nil {
				t.Fatal(err)
			}
			out := read(t, p)
			for _, field := range []string{"_id", "address", "scores", "nested", "title", "only a title"} {
				if !strings.Contains(out, field) {
					t.Errorf("%s export lost %q:\n%s", f, field, out)
				}
			}
			if f == query.FormatCSV {
				header, _, _ := strings.Cut(out, "\n")
				if header != "_id,name,age,address,tags,active,scores,nested,title" {
					t.Errorf("csv header = %q", header)
				}
			}
		})
	}
}

// Cancelling mid-way, before or during the csv rewrite, leaves no temp or
// spill file and keeps the old target.
func TestToFileCollectionUnionCancel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel int // page whose Browse call cancels; -1 = after the last page
	}{
		{"mid-paging", 2},
		{"before-rewrite", -1},
	} {
		for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
			t.Run(tc.name+"/"+string(f), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				pages := schemalessPages
				var c driver.Conn
				if tc.cancel >= 0 {
					c = pagedConn(t, pages, func(i int) {
						if i == tc.cancel {
							cancel()
						}
					})
				} else {
					c = &cancelAtEnd{Conn: pagedConn(t, pages, nil), cancel: cancel, last: len(pages) - 1}
				}
				dir := t.TempDir()
				p := filepath.Join(dir, "x."+string(f))
				if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := export.ToFile(ctx, export.CollectionSource{Conn: c, Ref: peopleRef}, export.Options{Format: f, Path: p})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
				if got := read(t, p); got != "old" {
					t.Errorf("target = %q", got)
				}
				if ents, _ := os.ReadDir(dir); len(ents) != 1 {
					t.Errorf("files left: %v", ents)
				}
			})
		}
	}
}

// cancelAtEnd cancels once the last page's rows are exhausted, so paging
// finishes and only the finishing step sees the cancel.
type cancelAtEnd struct {
	driver.Conn
	cancel func()
	last   int
}

func (c *cancelAtEnd) Browse(ctx context.Context, ref driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
	rs, err := c.Conn.(driver.Browser).Browse(ctx, ref, o)
	if err != nil || o.Cursor != strconv.Itoa(c.last) {
		return rs, err
	}
	return &endHook{ResultSet: rs, at: c.cancel}, nil
}

type endHook struct {
	driver.ResultSet
	at func()
}

func (e *endHook) Next(ctx context.Context) (driver.Row, bool, error) {
	row, ok, err := e.ResultSet.Next(ctx)
	if !ok && err == nil {
		e.at()
	}
	return row, ok, err
}

// cancelOnRewrite is a context that reports Canceled, from then on, as soon
// as a second export temp file exists in dir: the rewrite copy. Nothing
// else creates one, so the cancel lands inside the rewrite.
type cancelOnRewrite struct {
	context.Context
	dir   string
	fired *atomic.Bool
}

func (c cancelOnRewrite) Err() error {
	if c.fired.Load() {
		return context.Canceled
	}
	ents, _ := os.ReadDir(c.dir)
	n := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tobira-export-") {
			n++
		}
	}
	if n >= 2 {
		c.fired.Store(true)
		return context.Canceled
	}
	return nil
}

// Cancelling while the csv or json rewrite copies the file removes both
// temp files and keeps the old target. The pages hold well over 64 KiB, so
// the copy checks the context after the second file exists.
func TestToFileCollectionUnionCancelDuringRewrite(t *testing.T) {
	big := make([]driver.Row, 3000)
	for i := range big {
		big[i] = driver.Row{driver.Int(int64(i)), driver.String(strings.Repeat("v", 100))}
	}
	pages := []schemalessPage{
		{[]driver.Column{idCol, strCol("b")}, big},
		{[]driver.Column{idCol, strCol("b"), strCol("c")}, []driver.Row{{driver.Int(-1), driver.String("x"), driver.String("new")}}},
	}
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "x."+string(f))
			if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			fired := &atomic.Bool{}
			ctx := cancelOnRewrite{Context: context.Background(), dir: dir, fired: fired}
			_, err := export.ToFile(ctx, export.CollectionSource{Conn: pagedConn(t, pages, nil), Ref: peopleRef}, export.Options{Format: f, Path: p})
			if !errors.Is(err, context.Canceled) || !fired.Load() {
				t.Fatalf("err = %v (fired %v), want context.Canceled from inside the rewrite", err, fired.Load())
			}
			if got := read(t, p); got != "old" {
				t.Errorf("target = %q", got)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 1 {
				t.Errorf("files left: %v", ents)
			}
		})
	}
}
