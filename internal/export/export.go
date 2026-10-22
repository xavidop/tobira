// Package export streams a query result or a whole collection into a csv,
// json or ndjson file, in constant memory, through internal/query's row
// writers. The file is written to a temp file next to the target and renamed
// over it, so a failed or cancelled export never leaves a partial file.
package export

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
)

// Source is what to export: a QuerySource, a CollectionSource or a
// RowsSource.
type Source interface{ isSource() }

// QuerySource exports the rows of one query. Query.Limit caps the rows; 0
// exports them all.
type QuerySource struct {
	Conn   driver.Conn
	Query  driver.Query
	Driver string // driver name, used only to attribute errors raised here
}

// CollectionSource exports a whole collection, paging through Browser with
// Opts (filter, sort and ReadOnly as given; Opts.Limit is the page size,
// default 1000).
type CollectionSource struct {
	Conn   driver.Conn
	Ref    driver.CollectionRef
	Opts   driver.BrowseOptions
	Driver string // driver name, used only to attribute errors raised here
}

// RowsSource exports rows already collected, such as a TUI result tab. Its
// statement is never run again. Stats is passed to the writer as is (the
// json format prints it).
type RowsSource struct {
	Columns []driver.Column
	Rows    []driver.Row
	Stats   driver.Stats
}

func (QuerySource) isSource()      {}
func (CollectionSource) isSource() {}
func (RowsSource) isSource()       {}

// Progress is a running count of an export. Rows is -1 on the native path,
// where the rows are never seen.
type Progress struct {
	Rows  int64
	Bytes int64
}

// Options configures ToFile.
type Options struct {
	Format query.Format // csv, json or ndjson
	Path   string       // target file; a leading "~" is expanded
	// Progress, when set, is called at most every 100ms and once more when
	// the export succeeds. It gets no final call on failure: the returned
	// error is the outcome. It runs on the caller's goroutine.
	Progress func(Progress)
	NoNative bool // never use the driver's native Exporter

	// rewriteSample, set by tests only (WithRewriteSample), is called
	// during a collection export's second-pass rewrite every
	// rewriteSampleEvery bytes written, so a memory test can sample the
	// heap there too.
	rewriteSample func()
}

// rewriteSampleEvery is how many rewritten bytes pass between two calls of
// Options.rewriteSample.
const rewriteSampleEvery = 256 << 10

// Result describes a finished export. Rows is -1 when Native.
type Result struct {
	Path   string // the file written, after "~" and symlink resolution
	Rows   int64
	Bytes  int64
	Native bool
	// NoColumns is set when the statement returned no result set (an
	// UPDATE, say): no file is written and RowsAffected is its count.
	NoColumns    bool
	RowsAffected int64
	// Previews counts the values that were previews rather than full
	// values: rows whose _truncated marker (a Bool column the collection's
	// Describe lists as Generated, see the driver guide) is true. Callers
	// warn with PreviewWarning.
	Previews int64
}

// truncatedCol is the column a driver adds to mark a value preview.
const truncatedCol = "_truncated"

// PreviewWarning is the warning for an export that holds n previews, or ""
// when n is 0.
func PreviewWarning(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 value was a preview, not a full value"
	}
	return strconv.FormatInt(n, 10) + " values were previews, not full values"
}

// markers decides, once per collection, whether a _truncated column (the
// name matched case-insensitively, as the TUI does) is a driver's preview
// marker: its Describe lists the field as Generated. A failed Describe
// counts as a marker, so the export warns rather than passing previews off
// as full values. The decision is made only after the result set holding
// the column is closed (see progress.settle): on a pool of one connection,
// a Describe while a page is open would wait for it.
type markers struct {
	conn    driver.Conn
	decided map[driver.CollectionRef]bool
}

// find returns the position of a candidate marker among cols (-1 for none)
// and, when its collection is not decided yet, that collection. ref is the
// collection the columns come from; nil takes the column's own Source, and
// no Source means no marker.
func (m *markers) find(cols []driver.Column, ref *driver.CollectionRef) (int, *driver.CollectionRef) {
	if m == nil || m.conn == nil {
		return -1, nil
	}
	for i, c := range cols {
		if !strings.EqualFold(c.Name, truncatedCol) || c.Type != driver.TypeBool {
			continue
		}
		r := ref
		if r == nil {
			r = c.Source
		}
		if r == nil {
			return -1, nil
		}
		gen, ok := m.decided[*r]
		switch {
		case !ok:
			pending := *r
			return i, &pending
		case gen:
			return i, nil
		}
		return -1, nil
	}
	return -1, nil
}

// decide runs Describe for ref and records whether its _truncated field is
// Generated. No result set may be open.
func (m *markers) decide(ctx context.Context, ref driver.CollectionRef) bool {
	gen := true
	if sch, err := m.conn.Describe(ctx, ref); err == nil {
		gen = false
		for _, f := range sch.Fields {
			if strings.EqualFold(f.Name, truncatedCol) {
				gen = f.Generated
			}
		}
	}
	if m.decided == nil {
		m.decided = map[driver.CollectionRef]bool{}
	}
	m.decided[ref] = gen
	return gen
}

const (
	defaultPageSize  = 1000
	progressInterval = 100 * time.Millisecond
	tempPattern      = ".tobira-export-*"
)

// FormatForPath infers the format from the file extension: .csv, .json,
// .ndjson or .jsonl.
func FormatForPath(p string) (query.Format, bool) {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".csv":
		return query.FormatCSV, true
	case ".json":
		return query.FormatJSON, true
	case ".ndjson", ".jsonl":
		return query.FormatNDJSON, true
	}
	return "", false
}

// ExpandPath expands a leading "~" or "~/" to the home directory. Other
// paths, "~user" included, are returned unchanged.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("export: no output path")
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("export: expand ~: %w", err)
	}
	return filepath.Join(home, p[1:]), nil
}

// ToFile exports src to opts.Path in opts.Format.
//
// The native path, the driver's own Exporter, is taken only for a
// QuerySource in csv with Query.Limit 0, when the driver exports csv and
// NoNative is false. Native csv carries the server's own value text (postgres
// writes booleans as t/f, for example), so it can differ from tobira's csv.
// If the native export fails with ErrSyntax or ErrUnsupported before writing
// a byte, ToFile falls back to streaming.
//
// A symlink at the target is followed to the file it points to, which is
// created if missing (like a shell ">"); its directory must exist. A query
// that returns no result set (an UPDATE, say) writes no file and reports
// Result.NoColumns with its RowsAffected; on the native path the driver
// decides (postgres refuses anything but a single SELECT-like statement, and
// ToFile then streams).
//
// On any error, cancellation included, the temp file is removed and an
// existing target keeps its old content. On success the target has mode 0600.
func ToFile(ctx context.Context, src Source, opts Options) (Result, error) {
	switch opts.Format {
	case query.FormatCSV, query.FormatJSON, query.FormatNDJSON:
	default:
		return Result{}, fmt.Errorf("export: format %q cannot be exported (use csv, json or ndjson)", opts.Format)
	}
	target, err := resolveTarget(opts.Path)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	tmp, err := createTemp(filepath.Dir(target))
	if err != nil {
		return Result{}, err
	}
	out := &tempOut{dir: filepath.Dir(target), f: tmp, cw: &countWriter{w: tmp}, sample: opts.rewriteSample}
	done := false
	defer func() {
		if !done {
			out.discard()
		}
	}()

	pr := &progress{fn: opts.Progress, mark: -1}
	cw := out.cw
	res := Result{Path: target}

	native := false
	if qs, ok := src.(QuerySource); ok && !opts.NoNative {
		native, err = exportNative(ctx, qs, opts.Format, cw, pr)
		if err != nil {
			return Result{}, ctxErr(ctx, err)
		}
	}
	if native {
		res.Native, res.Rows = true, -1
	} else {
		cw.p = pr
		rw, err := query.NewRowWriter(cw, opts.Format)
		if err != nil {
			return Result{}, err
		}
		switch s := src.(type) {
		case QuerySource:
			var noCols bool
			var affected int64
			pr.markers = &markers{conn: s.Conn}
			if noCols, affected, err = streamQuery(ctx, s, rw, pr); err == nil && noCols {
				// Nothing to write: the deferred cleanup drops the temp file.
				pr.report()
				return Result{Path: target, NoColumns: true, RowsAffected: affected}, nil
			}
		case CollectionSource:
			pr.markers = &markers{conn: s.Conn}
			err = streamCollection(ctx, s, rw, opts.Format, out, pr)
		case RowsSource:
			err = streamRowsSource(ctx, s, rw, pr)
		default:
			err = fmt.Errorf("export: unknown source %T", src)
		}
		if err != nil {
			return Result{}, ctxErr(ctx, err)
		}
		res.Rows, res.Previews = pr.rows, pr.previews
	}

	// A collection export may have swapped in a rewritten temp file.
	tmp, cw = out.f, out.cw
	if err := tmp.Sync(); err != nil {
		return Result{}, fmt.Errorf("export: sync: %w", unwrapPath(err))
	}
	if err := tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("export: close: %w", unwrapPath(err))
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return Result{}, fmt.Errorf("export: rename to %s: %w", query.EscapeControls(target), unwrapPath(err))
	}
	done = true
	res.Bytes = cw.n
	pr.bytes = cw.n
	pr.report()
	return res, nil
}

// ctxErr prefers the context's error once it is done, so a cancelled export
// always reports context.Canceled (or DeadlineExceeded) whatever error the
// driver or the writer raised on the way out.
func ctxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		return fmt.Errorf("%w (%v)", cerr, err)
	}
	return err
}

// ResolveTarget is the target check ToFile runs first, for callers that want
// to refuse a bad path before doing any work. It returns the file ToFile
// would write.
func ResolveTarget(p string) (string, error) { return resolveTarget(p) }

// resolveTarget expands "~", follows a symlink at the target to the file it
// points to (which may not exist yet, like a shell ">"), and checks the
// target is not a directory and its parent exists.
func resolveTarget(p string) (string, error) {
	target, err := ExpandPath(p)
	if err != nil {
		return "", err
	}
	esc := query.EscapeControls
	if target, err = followLinks(target); err != nil {
		return "", fmt.Errorf("export: %s: %w", esc(p), err)
	}
	if st, err := os.Stat(target); err == nil {
		if st.IsDir() {
			return "", fmt.Errorf("export: %s is a directory", esc(target))
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("export: %s is not a regular file", esc(target))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("export: %s: %w", esc(target), unwrapPath(err))
	}
	dir := filepath.Dir(target)
	st, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("export: directory %s does not exist", esc(dir))
	case err != nil:
		return "", fmt.Errorf("export: directory %s: %w", esc(dir), unwrapPath(err))
	case !st.IsDir():
		return "", fmt.Errorf("export: %s is not a directory", esc(dir))
	}
	return target, nil
}

// maxLinks bounds symlink chains, as the kernel does (ELOOP).
const maxLinks = 40

// followLinks resolves p while it is a symlink and returns the final path,
// which may not exist yet. A path that is not a symlink is returned as is.
// When the chain ends at an existing file, filepath.EvalSymlinks resolves it
// whole. Only a dangling chain is walked by hand, and then each relative link
// is joined to the physical directory of the link (its directory with every
// symlink resolved), never lexically: a/ld/l -> ../x where a/ld is itself a
// link must land where the kernel would put it.
func followLinks(p string) (string, error) {
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return "", unwrapPath(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		return p, nil
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real, nil
	}
	for range maxLinks {
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", unwrapPath(err)
		}
		if st.Mode()&os.ModeSymlink == 0 {
			return p, nil
		}
		dest, err := os.Readlink(p)
		if err != nil {
			return "", unwrapPath(err)
		}
		if !filepath.IsAbs(dest) {
			dir, err := filepath.EvalSymlinks(filepath.Dir(p))
			if err != nil {
				return "", unwrapPath(err)
			}
			dest = filepath.Join(dir, dest)
		}
		p = dest
	}
	return "", errors.New("too many levels of symbolic links")
}

// unwrapPath drops the raw paths an *os.PathError or *os.LinkError would
// print, since callers print the escaped path themselves.
func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

// exportNative runs the driver's Exporter when the native path applies. It
// reports false, with no error, when the path does not apply or the driver
// refused the query (ErrSyntax/ErrUnsupported) before anything reached the
// file. Bytes still in the export buffer (a COPY header sent before the first
// row failed) are dropped, so the fallback starts from an empty file. An
// Exporter may run in a read-only transaction of its own (postgres does), so
// ErrReadOnly falls back the same way, but only on a writable call: on a
// read-only one the refusal is the answer.
func exportNative(ctx context.Context, s QuerySource, f query.Format, cw *countWriter, pr *progress) (bool, error) {
	if f != query.FormatCSV || s.Query.Limit != 0 {
		return false, nil
	}
	ex, ok := driver.As[driver.Exporter](s.Conn)
	if !ok || !slices.Contains(ex.ExportFormats(), "csv") {
		return false, nil
	}
	pr.rows = -1
	bw := bufio.NewWriterSize(cw, 64<<10)
	counted := &countWriter{w: bw, p: pr}
	if err := ex.Export(ctx, s.Query, "csv", counted); err != nil {
		if cw.n == 0 && ctx.Err() == nil {
			switch kind := driver.KindOf(err); {
			case kind == driver.ErrSyntax, kind == driver.ErrUnsupported,
				kind == driver.ErrReadOnly && !s.Query.ReadOnly:
				pr.rows, pr.bytes = 0, 0
				return false, nil
			}
		}
		return false, err
	}
	if err := bw.Flush(); err != nil {
		return false, err
	}
	return true, nil
}

// streamQuery runs the query and streams its rows into rw, enforcing
// Query.Limit like driver.Collect: one extra row decides Truncated. A
// statement with no result set writes nothing and reports true with its
// RowsAffected.
func streamQuery(ctx context.Context, s QuerySource, rw query.RowWriter, pr *progress) (bool, int64, error) {
	start := time.Now()
	rs, err := s.Conn.Query(ctx, s.Query)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = rs.Close() }()
	if len(rs.Columns()) == 0 {
		for {
			_, ok, err := rs.Next(ctx)
			if err != nil {
				return false, 0, err
			}
			if !ok {
				break
			}
		}
		affected := rs.Stats().RowsAffected
		return true, affected, rs.Close()
	}
	if err := rw.Begin(rs.Columns()); err != nil {
		return false, 0, err
	}
	pr.begin(rs.Columns(), nil)
	truncated, err := streamRows(ctx, rs, rw, pr, int64(s.Query.Limit))
	if err != nil {
		return false, 0, err
	}
	st := rs.Stats()
	st.Truncated = st.Truncated || truncated
	if st.Duration == 0 {
		st.Duration = time.Since(start)
	}
	if err := rs.Close(); err != nil {
		return false, 0, err
	}
	pr.settle(ctx)
	return false, 0, rw.End(st)
}

// streamCollection pages through the collection with Browser until
// Stats.NextCursor is empty. Pages of a schemaless collection may differ in
// columns, so the export keeps the union of their columns in first-seen
// order: json and ndjson key each row by its own page's columns; csv
// spreads each row onto the union as it stands. When a page adds a column
// after the first, the finished temp file is rewritten once, in constant
// memory, to carry the final union: csv gets the union header and every
// shorter record padded with empty fields, json gets the union in its
// "columns" header. ndjson needs no rewrite.
func streamCollection(ctx context.Context, s CollectionSource, rw query.RowWriter, f query.Format, out *tempOut, pr *progress) error {
	br, ok := driver.As[driver.Browser](s.Conn)
	if !ok {
		return driver.Wrap(driver.ErrUnsupported, s.Driver, "export", errors.New("browsing is not supported by this driver"))
	}
	opts := s.Opts
	if opts.Limit <= 0 {
		opts.Limit = defaultPageSize
	}
	opts.Cursor = ""
	start := time.Now()
	var union columnUnion
	var keyed []driver.Column // the columns json/ndjson rows are keyed by
	var headLen int64         // bytes of the header written by Begin
	grew := false
	// Brent's cycle detection in constant memory: mark is the cursor seen at
	// the last power-of-two step, so a driver whose cursors cycle (A, B, A…)
	// is caught within a few laps instead of paging forever.
	mark, power, steps := "", 1, 0
	for page := 0; ; page++ {
		rs, err := br.Browse(ctx, s.Ref, opts)
		if err != nil {
			return err
		}
		cols := rs.Columns()
		pr.begin(cols, &s.Ref)
		if page == 0 {
			union.add(cols)
			keyed = cols
			err = rw.Begin(cols)
			if err == nil {
				err = flush(rw)
			}
			headLen = out.cw.n
			if err == nil {
				_, err = streamRows(ctx, rs, rw, pr, 0)
			}
		} else {
			before := len(union.cols)
			idx := union.add(cols)
			grew = grew || len(union.cols) > before
			switch {
			case f != query.FormatCSV:
				if rk, ok := rw.(query.Rekeyer); ok && !sameNames(cols, keyed) {
					rk.Rekey(cols)
					keyed = cols
				}
				_, err = streamRows(ctx, rs, rw, pr, 0)
			case union.identity(idx):
				_, err = streamRows(ctx, rs, rw, pr, 0)
			default:
				err = streamMapped(ctx, rs, rw, pr, idx, len(union.cols))
			}
		}
		next := rs.Stats().NextCursor
		if cerr := rs.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		pr.settle(ctx)
		if next == "" {
			break
		}
		if next == mark {
			return driver.Wrap(driver.ErrUnsupported, s.Driver, "export", errors.New("browse cursors cycle; stopping the export"))
		}
		if steps++; steps == power {
			mark, power, steps = next, power*2, 0
		}
		opts.Cursor = next
	}
	if err := rw.End(driver.Stats{RowsAffected: -1, Duration: time.Since(start)}); err != nil {
		return err
	}
	if !grew || f == query.FormatNDJSON {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	src := out.f
	return out.replace(pr, func(w io.Writer) error {
		if f == query.FormatCSV {
			return rewriteCSV(ctx, src, headLen, union.cols, w)
		}
		return rewriteJSONHeader(ctx, src, headLen, union.cols, w)
	})
}

// streamRowsSource writes the collected rows of s to rw.
func streamRowsSource(ctx context.Context, s RowsSource, rw query.RowWriter, pr *progress) error {
	if err := rw.Begin(s.Columns); err != nil {
		return err
	}
	for _, row := range s.Rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rw.Row(row); err != nil {
			return err
		}
		pr.tickRow()
	}
	return rw.End(s.Stats)
}

// streamRows writes every row of rs to rw. With limit > 0 it stops after
// limit rows and reports whether more existed.
func streamRows(ctx context.Context, rs driver.ResultSet, rw query.RowWriter, pr *progress, limit int64) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		row, ok, err := rs.Next(ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if limit > 0 && pr.rows >= limit {
			return true, nil
		}
		if err := rw.Row(row); err != nil {
			return false, err
		}
		pr.tick(row)
	}
}

// progress throttles the Progress callback to one call per interval. It
// also counts the previews: rows whose column mark (the result's preview
// marker, see markers; -1 for none) is true. While the marker's collection
// is undecided (pending), those rows are held as candidates, and settle
// adds them once the result set is closed and Describe has decided.
type progress struct {
	fn         func(Progress)
	rows       int64
	bytes      int64
	last       time.Time
	calls      uint
	markers    *markers
	mark       int
	pending    *driver.CollectionRef
	candidates int64
	previews   int64
}

// begin sets up preview counting for a result set's columns.
func (p *progress) begin(cols []driver.Column, ref *driver.CollectionRef) {
	p.mark, p.pending = p.markers.find(cols, ref)
	p.candidates = 0
}

// settle decides a pending marker, once the result set is closed, and
// counts its candidates as previews when it is one.
func (p *progress) settle(ctx context.Context) {
	if p.pending != nil && p.markers.decide(ctx, *p.pending) {
		p.previews += p.candidates
	}
	p.pending, p.candidates = nil, 0
}

// tick counts row toward the progress and, when it holds a preview, the
// previews (or the candidates, while the marker is undecided).
func (p *progress) tick(row driver.Row) {
	if p.mark >= 0 && p.mark < len(row) {
		if cut, ok := row[p.mark].AsBool(); ok && cut {
			if p.pending != nil {
				p.candidates++
			} else {
				p.previews++
			}
		}
	}
	p.tickRow()
}

// tickRow counts a row toward a throttled report. It reads the clock only
// every 64 rows.
func (p *progress) tickRow() {
	p.rows++
	if p.fn == nil {
		return
	}
	if p.calls++; p.calls%64 == 0 {
		p.maybeReport()
	}
}

// maybeReport reports when an interval has passed since the last report.
func (p *progress) maybeReport() {
	if now := time.Now(); now.Sub(p.last) >= progressInterval {
		p.last = now
		p.fn(Progress{Rows: p.rows, Bytes: p.bytes})
	}
}

func (p *progress) report() {
	if p.fn != nil {
		p.fn(Progress{Rows: p.rows, Bytes: p.bytes})
	}
}

// countWriter counts the bytes written through it. With p set it keeps the
// progress byte count current and may report it.
type countWriter struct {
	w io.Writer
	n int64
	p *progress
	// floor is the least byte count reported: a rewrite reports the first
	// pass's total until its copy passes it, so the count never goes back.
	floor int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	if c.p != nil {
		c.p.bytes = max(c.floor, c.n)
		if c.p.fn != nil {
			c.p.maybeReport()
		}
	}
	return n, err
}
