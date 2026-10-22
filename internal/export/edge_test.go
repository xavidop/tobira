package export_test

import (
	"context"
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

const evil = "evil\x1b[31m"

// assertEscaped fails unless err is set and shows the ESC of evil only in its
// escaped form.
func assertEscaped(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("nil error")
	}
	if strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `\x1b`) {
		t.Fatalf("err = %q, want the escape shown as \\x1b and never raw", err)
	}
}

// TestToFileErrorsEscapePaths covers every error that names a path: the
// target checks, temp file creation and the final rename.
func TestToFileErrorsEscapePaths(t *testing.T) {
	src := func(t *testing.T) export.QuerySource {
		return export.QuerySource{Conn: open(t, drivertest.NewFake(drivertest.WithPeople())), Query: driver.Query{Text: peopleSQL}}
	}
	run := func(t *testing.T, s export.Source, p string) error {
		_, err := export.ToFile(context.Background(), s, export.Options{Format: query.FormatNDJSON, Path: p})
		return err
	}

	t.Run("missing parent", func(t *testing.T) {
		assertEscaped(t, run(t, src(t), filepath.Join(t.TempDir(), evil, "x.ndjson")))
	})
	t.Run("directory target", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), evil)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		assertEscaped(t, run(t, src(t), p))
	})
	t.Run("parent is a file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), evil)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		assertEscaped(t, run(t, src(t), filepath.Join(p, "x.ndjson")))
	})
	t.Run("create temp", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := filepath.Join(t.TempDir(), evil)
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		assertEscaped(t, run(t, src(t), filepath.Join(dir, "x.ndjson")))
	})
	t.Run("rename", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), evil)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "x.ndjson")
		f := drivertest.NewFake()
		// Mid-export the target turns into a non-empty directory, so the
		// final rename fails.
		f.OnQuery("gen", func(driver.Query) (driver.ResultSet, error) {
			return &genRS{n: 100, onRow: func(i int) {
				if i == 10 {
					_ = os.Mkdir(p, 0o700)
					_ = os.WriteFile(filepath.Join(p, "f"), nil, 0o600)
				}
			}}, nil
		})
		err := run(t, export.QuerySource{Conn: open(t, f), Query: driver.Query{Text: "gen"}}, p)
		assertEscaped(t, err)
		assertNoTemp(t, dir)
	})
}

func TestResolveTarget(t *testing.T) {
	dir := t.TempDir()
	if got, err := export.ResolveTarget(filepath.Join(dir, "x.csv")); err != nil || got != filepath.Join(dir, "x.csv") {
		t.Errorf("ResolveTarget = %q, %v", got, err)
	}
	for _, p := range []string{dir, filepath.Join(dir, "nope", "x.csv"), ""} {
		if _, err := export.ResolveTarget(p); err == nil {
			t.Errorf("ResolveTarget(%q) = nil error", p)
		}
	}
}

// TestToFileDanglingSymlink: like a shell ">", a symlink to a missing file
// creates that file, provided its directory exists.
func TestToFileDanglingSymlink(t *testing.T) {
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	realDir, linkDir := t.TempDir(), t.TempDir()
	real := filepath.Join(realDir, "new.ndjson")
	link := filepath.Join(linkDir, "link.ndjson")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatNDJSON, Path: link})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != real {
		t.Errorf("path = %q, want %q", res.Path, real)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v", err)
	}
	if n := strings.Count(read(t, real), "\n"); n != len(drivertest.People.Rows) {
		t.Errorf("%d lines", n)
	}
	assertMode0600(t, real)

	// A relative link resolves against the link's own directory.
	rel := filepath.Join(linkDir, "rel.ndjson")
	if err := os.Symlink("sub.ndjson", rel); err != nil {
		t.Fatal(err)
	}
	if res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatNDJSON, Path: rel}); err != nil || res.Path != filepath.Join(physical(t, linkDir), "sub.ndjson") {
		t.Fatalf("relative link: %+v %v", res, err)
	}

	// The link points into a missing directory: refused, nothing created.
	bad := filepath.Join(linkDir, "bad.ndjson")
	if err := os.Symlink(filepath.Join(realDir, "nope", "x.ndjson"), bad); err != nil {
		t.Fatal(err)
	}
	if _, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatNDJSON, Path: bad}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(realDir, "nope")); !os.IsNotExist(err) {
		t.Error("a missing directory was created")
	}

	// A symlink loop is refused.
	loop := filepath.Join(linkDir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatNDJSON, Path: loop}); err == nil {
		t.Fatal("symlink loop: nil error")
	}
}

// TestToFileNoColumns: a statement with no result set writes no file and
// reports its RowsAffected.
func TestToFileNoColumns(t *testing.T) {
	for _, format := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		t.Run(string(format), func(t *testing.T) {
			f := drivertest.NewFake()
			f.OnQuery("upd", func(driver.Query) (driver.ResultSet, error) {
				return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: 2}), nil
			})
			dir := t.TempDir()
			p := filepath.Join(dir, "x")
			res, err := export.ToFile(context.Background(), export.QuerySource{Conn: open(t, f), Query: driver.Query{Text: "upd", Limit: 5}},
				export.Options{Format: format, Path: p})
			if err != nil {
				t.Fatal(err)
			}
			if !res.NoColumns || res.RowsAffected != 2 || res.Rows != 0 || res.Bytes != 0 {
				t.Errorf("result = %+v", res)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("files written: %v", ents)
			}
		})
	}
}

// browseConn wraps a Conn to script or observe Browse calls.
type browseConn struct {
	driver.Conn
	browse func(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error)
}

func (b *browseConn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	return b.browse(ctx, ref, opts)
}

func TestToFileCursorCycle(t *testing.T) {
	inner := open(t, drivertest.NewFake())
	next := map[string]string{"": "A", "A": "B", "B": "A"}
	calls := 0
	c := &browseConn{Conn: inner, browse: func(_ context.Context, _ driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
		calls++
		if calls > 10_000 {
			t.Fatal("cursor cycle not detected")
		}
		return driver.NewSliceResultSet(genCols[:1], []driver.Row{{driver.Int(int64(calls))}},
			driver.Stats{RowsAffected: -1, NextCursor: next[o.Cursor]}), nil
	}}
	dir := t.TempDir()
	_, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: peopleRef, Driver: "fake"},
		export.Options{Format: query.FormatCSV, Path: filepath.Join(dir, "x.csv")})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a cursor cycle error", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left: %v", ents)
	}
}

func TestToFileCollectionPassesReadOnly(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	inner := open(t, f)
	br, _ := driver.As[driver.Browser](inner)
	var seen []driver.BrowseOptions
	c := &browseConn{Conn: inner, browse: func(ctx context.Context, ref driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
		seen = append(seen, o)
		return br.Browse(ctx, ref, o)
	}}
	opts := driver.BrowseOptions{ReadOnly: true, Limit: 20, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpGt, Value: driver.Int(0)}}}}
	if _, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: peopleRef, Opts: opts},
		export.Options{Format: query.FormatCSV, Path: filepath.Join(t.TempDir(), "x.csv")}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("%d browse calls, want 3", len(seen))
	}
	for i, o := range seen {
		if !o.ReadOnly || o.Limit != 20 || len(o.Filter.Conditions) != 1 {
			t.Errorf("call %d opts = %+v", i, o)
		}
	}
}

func TestToFileCancelMidPaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := open(t, drivertest.NewFake(drivertest.WithPeople()))
	br, _ := driver.As[driver.Browser](inner)
	calls := 0
	c := &browseConn{Conn: inner, browse: func(ctx context.Context, ref driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
		if calls++; calls == 3 {
			cancel()
		}
		return br.Browse(ctx, ref, o)
	}}
	dir := t.TempDir()
	p := filepath.Join(dir, "x.csv")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := export.ToFile(ctx, export.CollectionSource{Conn: c, Ref: peopleRef, Opts: driver.BrowseOptions{Limit: 5}},
		export.Options{Format: query.FormatCSV, Path: p})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 3 {
		t.Errorf("%d browse calls, want paging to stop at the cancel", calls)
	}
	if got := read(t, p); got != "old" {
		t.Errorf("target = %q", got)
	}
	assertNoTemp(t, dir)
}

// exportConn wraps a Conn with a scripted native csv Exporter.
type exportConn struct {
	driver.Conn
	export func(ctx context.Context, w io.Writer) error
}

func (e *exportConn) ExportFormats() []string { return []string{"csv"} }
func (e *exportConn) Export(ctx context.Context, _ driver.Query, _ string, w io.Writer) error {
	return e.export(ctx, w)
}

// TestToFileNativeCancelAfterPartialBytes: a cancel after the native export
// wrote bytes returns context.Canceled, never falls back, and leaves no file.
func TestToFileNativeCancelAfterPartialBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := drivertest.NewFake(drivertest.WithPeople())
	c := &exportConn{Conn: open(t, f), export: func(ctx context.Context, w io.Writer) error {
		for i := range 20_000 { // more than the 64 KiB buffer, so bytes reach the temp file
			if _, err := io.WriteString(w, "row,"+strconv.Itoa(i)+"\n"); err != nil {
				return err
			}
		}
		cancel()
		return driver.Wrap(driver.ErrUnsupported, "fake", "export", ctx.Err())
	}}
	dir := t.TempDir()
	p := filepath.Join(dir, "x.csv")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := export.ToFile(ctx, export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatCSV, Path: p})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	for _, call := range f.Calls() {
		if call.Op == "query" {
			t.Error("fell back to streaming after partial native bytes")
		}
	}
	if got := read(t, p); got != "old" {
		t.Errorf("target = %q", got)
	}
	assertNoTemp(t, dir)
}

// physical returns dir with every symlink resolved (t.TempDir may sit under
// a symlinked directory, as /var is on macOS).
func physical(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func exportTo(t *testing.T, p string) export.Result {
	t.Helper()
	c := open(t, drivertest.NewFake(drivertest.WithPeople()))
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatNDJSON, Path: p})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestToFileLinkThroughSymlinkedDir: a relative link reached through a
// symlinked directory resolves against the physical directory, as the kernel
// does. a/ld -> ../real/deep and real/deep/l -> ../x, so a/ld/l is real/x,
// not a/x.
func TestToFileLinkThroughSymlinkedDir(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run("existing="+strconv.FormatBool(existing), func(t *testing.T) {
			base := physical(t, t.TempDir())
			for _, d := range []string{"a", "real/deep"} {
				if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			mustSymlink(t, "../real/deep", filepath.Join(base, "a", "ld"))
			mustSymlink(t, "../x", filepath.Join(base, "real", "deep", "l"))
			want := filepath.Join(base, "real", "x")
			if existing {
				if err := os.WriteFile(want, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			res := exportTo(t, filepath.Join(base, "a", "ld", "l"))
			if res.Path != want {
				t.Errorf("path = %q, want %q", res.Path, want)
			}
			if got := read(t, want); !strings.HasPrefix(got, `{"id":1,`) {
				t.Errorf("real/x = %q", got[:min(len(got), 40)])
			}
			if _, err := os.Lstat(filepath.Join(base, "a", "x")); !os.IsNotExist(err) {
				t.Error("a/x was written: the link was resolved lexically")
			}
		})
	}
}

func TestToFileLinkChainAndAbsolute(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run("existing="+strconv.FormatBool(existing), func(t *testing.T) {
			base := physical(t, t.TempDir())
			if err := os.MkdirAll(filepath.Join(base, "d1", "d2"), 0o700); err != nil {
				t.Fatal(err)
			}
			final := filepath.Join(base, "d1", "d2", "out.ndjson")
			if existing {
				if err := os.WriteFile(final, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// chain: l1 -> d1/l2 -> d2/l3 -> out.ndjson (each relative to its own dir)
			mustSymlink(t, "d1/l2", filepath.Join(base, "l1"))
			mustSymlink(t, "d2/l3", filepath.Join(base, "d1", "l2"))
			mustSymlink(t, "out.ndjson", filepath.Join(base, "d1", "d2", "l3"))
			if res := exportTo(t, filepath.Join(base, "l1")); res.Path != final {
				t.Errorf("chain: path = %q, want %q", res.Path, final)
			}
			if err := os.Remove(final); err != nil {
				t.Fatal(err)
			}
			// absolute link, from a directory elsewhere
			other := physical(t, t.TempDir())
			if existing {
				if err := os.WriteFile(final, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mustSymlink(t, final, filepath.Join(other, "abs"))
			if res := exportTo(t, filepath.Join(other, "abs")); res.Path != final {
				t.Errorf("absolute: path = %q, want %q", res.Path, final)
			}
			read(t, final)
		})
	}
}
