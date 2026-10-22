package export

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
)

// tempOut is the temp file an export writes, which a collection export may
// replace once with a rewritten copy (see streamCollection).
type tempOut struct {
	dir    string
	f      *os.File
	cw     *countWriter
	sample func() // see Options.rewriteSample
}

// sampleWriter calls fn every rewriteSampleEvery bytes, before it passes
// on the write that crosses the mark: whatever the rewrite holds for that
// write is still live then.
type sampleWriter struct {
	w       io.Writer
	fn      func()
	n, last int64
}

func (s *sampleWriter) Write(p []byte) (int, error) {
	if s.n += int64(len(p)); s.n-s.last >= rewriteSampleEvery {
		s.last = s.n
		s.fn()
	}
	return s.w.Write(p)
}

// createTemp creates a mode-0600 temp file in dir.
func createTemp(dir string) (*os.File, error) {
	f, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return nil, fmt.Errorf("export: create temp file in %s: %w", query.EscapeControls(dir), unwrapPath(err))
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("export: chmod temp file: %w", unwrapPath(err))
	}
	return f, nil
}

// discard closes and removes the current temp file.
func (o *tempOut) discard() {
	_ = o.f.Close()
	_ = os.Remove(o.f.Name())
}

// replace writes a new temp file with fill, which may read the current one,
// then swaps it in and removes the current one. On error the new file is
// removed and the current one kept, for the caller's cleanup.
func (o *tempOut) replace(pr *progress, fill func(w io.Writer) error) error {
	nf, err := createTemp(o.dir)
	if err != nil {
		return err
	}
	// The rewrite is at least as large as the first pass (it only adds
	// header names and padding), so holding the count at the first pass's
	// total keeps progress monotonic and ends at the real size.
	ncw := &countWriter{w: nf, p: pr, floor: o.cw.n}
	var w io.Writer = ncw
	if o.sample != nil {
		w = &sampleWriter{w: ncw, fn: o.sample}
	}
	if err := fill(w); err != nil {
		_ = nf.Close()
		_ = os.Remove(nf.Name())
		return err
	}
	old := o.f
	o.f, o.cw = nf, ncw
	_ = old.Close()
	_ = os.Remove(old.Name())
	return nil
}

// columnUnion is the union of the columns of a collection's pages, in
// first-seen order. A name repeated within a page maps to the union column
// of the same occurrence, so duplicate names are kept apart.
type columnUnion struct {
	cols   []driver.Column
	byName map[string][]int // union positions of each name, in order
}

// add maps cols onto the union, appending the names it lacks, and returns
// each column's union position.
func (u *columnUnion) add(cols []driver.Column) []int {
	if u.byName == nil {
		u.byName = map[string][]int{}
	}
	idx := make([]int, len(cols))
	seen := map[string]int{}
	for i, c := range cols {
		k := seen[c.Name]
		seen[c.Name]++
		if pos := u.byName[c.Name]; k < len(pos) {
			idx[i] = pos[k]
			continue
		}
		idx[i] = len(u.cols)
		u.byName[c.Name] = append(u.byName[c.Name], len(u.cols))
		u.cols = append(u.cols, c)
	}
	return idx
}

// identity reports whether idx maps a page onto the whole union in order.
func (u *columnUnion) identity(idx []int) bool {
	if len(idx) != len(u.cols) {
		return false
	}
	for i, p := range idx {
		if p != i {
			return false
		}
	}
	return true
}

func sameNames(a, b []driver.Column) bool {
	return slices.EqualFunc(a, b, func(x, y driver.Column) bool { return x.Name == y.Name })
}

// streamMapped writes every row of rs to rw spread onto the union positions
// idx, width columns wide, NULL where the page has no value.
func streamMapped(ctx context.Context, rs driver.ResultSet, rw query.RowWriter, pr *progress, idx []int, width int) error {
	buf := make(driver.Row, width)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, ok, err := rs.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		clear(buf)
		for i, v := range row {
			if i < len(idx) {
				buf[idx[i]] = v
			}
		}
		if err := rw.Row(buf); err != nil {
			return err
		}
		pr.tick(row)
	}
}

// ctxCheckEvery is how many bytes a rewrite copies between context checks.
const ctxCheckEvery = 64 << 10

// rewriteCSV writes the csv in src with the union header: src's header
// (its first headLen bytes) is replaced, and every record is padded with
// empty trailing fields up to the union's width. Records are found by
// tracking csv quoting, so a quoted newline or comma is not a separator.
func rewriteCSV(ctx context.Context, src io.ReaderAt, headLen int64, union []driver.Column, w io.Writer) error {
	if err := writeHeader(w, query.FormatCSV, union); err != nil {
		return err
	}
	r := bufio.NewReaderSize(io.NewSectionReader(src, headLen, 1<<62), 64<<10)
	bw := bufio.NewWriterSize(w, 64<<10)
	inQuote, fields, n := false, 1, 0
	for {
		b, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if n++; n%ctxCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		switch {
		case b == '"':
			inQuote = !inQuote
		case inQuote:
		case b == ',':
			fields++
		case b == '\n':
			for ; fields < len(union); fields++ {
				_ = bw.WriteByte(',')
			}
			fields = 1
		}
		if err := bw.WriteByte(b); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// rewriteJSONHeader writes the json document in src with the union in its
// "columns" header: src's header (its first headLen bytes) is replaced and
// the rest, rows and stats, is copied as is.
func rewriteJSONHeader(ctx context.Context, src io.ReaderAt, headLen int64, union []driver.Column, w io.Writer) error {
	if err := writeHeader(w, query.FormatJSON, union); err != nil {
		return err
	}
	r := io.NewSectionReader(src, headLen, 1<<62)
	buf := make([]byte, ctxCheckEvery)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// writeHeader writes what a RowWriter for f writes in Begin(cols).
func writeHeader(w io.Writer, f query.Format, cols []driver.Column) error {
	rw, err := query.NewRowWriter(w, f)
	if err != nil {
		return err
	}
	if err := rw.Begin(cols); err != nil {
		return err
	}
	return flush(rw)
}

// flush flushes rw's buffered output.
func flush(rw query.RowWriter) error {
	if fl, ok := rw.(query.Flusher); ok {
		return fl.Flush()
	}
	return nil
}
