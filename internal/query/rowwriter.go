package query

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/xavidop/tobira/driver"
)

// RowWriter renders a result one row at a time, in constant memory: it holds
// at most one row and batches output through a buffer. Call Begin once, Row
// for each row, then End, which flushes. The bytes are exactly those Write
// produces for the same data.
type RowWriter interface {
	Begin(cols []driver.Column) error
	Row(row driver.Row) error
	End(stats driver.Stats) error
}

// Flusher is implemented by every RowWriter NewRowWriter returns: Flush
// writes the buffered output through without ending the result, so a caller
// can learn how many bytes the header took.
type Flusher interface{ Flush() error }

// Rekeyer is implemented by the json and ndjson RowWriters: Rekey keys the
// rows written after it by cols instead of the columns given to Begin, for a
// result whose columns change between pages (a schemaless collection). The
// json "columns" header written by Begin is left as it was. csv has no
// Rekeyer: its header fixes the fields of every row.
type Rekeyer interface{ Rekey(cols []driver.Column) }

// NewRowWriter returns a streaming writer for csv, json or ndjson. The table
// format needs every row to size its columns, so it has no RowWriter.
func NewRowWriter(w io.Writer, f Format) (RowWriter, error) {
	bw := bufio.NewWriterSize(w, 64<<10)
	switch f {
	case FormatCSV:
		return &csvWriter{w: bw}, nil
	case FormatJSON:
		return &jsonWriter{w: bw}, nil
	case FormatNDJSON:
		return &ndjsonWriter{w: bw}, nil
	case FormatTable:
		return nil, fmt.Errorf("format %q cannot be streamed (use csv, json or ndjson)", f)
	}
	return nil, fmt.Errorf("unknown format %q (streamable formats: csv, json, ndjson)", f)
}

// ---- csv ----

type csvWriter struct {
	w   *bufio.Writer
	buf []byte
}

func (c *csvWriter) Begin(cols []driver.Column) error {
	for i, col := range cols {
		if i > 0 {
			_ = c.w.WriteByte(',')
		}
		_, _ = c.w.WriteString(csvField(col.Name))
	}
	if len(cols) > 0 {
		_ = c.w.WriteByte('\n')
	}
	return nil
}

func (c *csvWriter) Row(row driver.Row) error {
	b := c.buf[:0]
	for i, v := range row {
		if i > 0 {
			b = append(b, ',')
		}
		if !v.IsNull() {
			b = append(b, csvField(v.String())...)
		}
	}
	b = append(b, '\n')
	c.buf = b
	_, err := c.w.Write(b)
	return err
}

func (c *csvWriter) End(driver.Stats) error { return c.w.Flush() }

func (c *csvWriter) Flush() error { return c.w.Flush() }

// ---- ndjson ----

type ndjsonWriter struct {
	w    *bufio.Writer
	keys []string
	buf  []byte
}

func (n *ndjsonWriter) Begin(cols []driver.Column) error {
	n.keys = jsonKeys(cols)
	return nil
}

func (n *ndjsonWriter) Row(row driver.Row) error {
	n.buf = append(appendRowObject(n.buf[:0], n.keys, row), '\n')
	_, err := n.w.Write(n.buf)
	return err
}

func (n *ndjsonWriter) End(driver.Stats) error { return n.w.Flush() }

func (n *ndjsonWriter) Flush() error { return n.w.Flush() }

func (n *ndjsonWriter) Rekey(cols []driver.Column) { n.keys = jsonKeys(cols) }

// ---- json ----

// jsonWriter emits the document json.Indent(…, "", "  ") made of the whole
// result: each piece is built compact and indented on its own with the prefix
// of its nesting depth, which yields the same bytes.
type jsonWriter struct {
	w       *bufio.Writer
	keys    []string
	rows    int64
	compact []byte
	indent  bytes.Buffer
}

// writeIndented indents the compact JSON value b as if it sat at the depth
// whose line prefix is prefix.
func (j *jsonWriter) writeIndented(b []byte, prefix string) error {
	j.indent.Reset()
	if err := json.Indent(&j.indent, b, prefix, "  "); err != nil {
		return err
	}
	_, err := j.w.Write(j.indent.Bytes())
	return err
}

func (j *jsonWriter) Begin(cols []driver.Column) error {
	j.keys = jsonKeys(cols)
	b := append(j.compact[:0], '[')
	for i, c := range cols {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = appendString(b, c.Name)
		b = append(b, `,"type":`...)
		b = appendString(b, c.Type.String())
		b = append(b, '}')
	}
	b = append(b, ']')
	j.compact = b
	_, _ = j.w.WriteString("{\n  \"columns\": ")
	if err := j.writeIndented(b, "  "); err != nil {
		return err
	}
	_, err := j.w.WriteString(",\n  \"rows\": [")
	return err
}

func (j *jsonWriter) Flush() error { return j.w.Flush() }

func (j *jsonWriter) Rekey(cols []driver.Column) { j.keys = jsonKeys(cols) }

func (j *jsonWriter) Row(row driver.Row) error {
	if j.rows > 0 {
		_ = j.w.WriteByte(',')
	}
	j.rows++
	_, _ = j.w.WriteString("\n    ")
	j.compact = appendRowObject(j.compact[:0], j.keys, row)
	return j.writeIndented(j.compact, "    ")
}

func (j *jsonWriter) End(stats driver.Stats) error {
	if j.rows > 0 {
		_, _ = j.w.WriteString("\n  ")
	}
	b := append(j.compact[:0], `{"rows_affected":`...)
	b = strconv.AppendInt(b, stats.RowsAffected, 10)
	b = append(b, `,"duration_ms":`...)
	b = strconv.AppendInt(b, stats.Duration.Round(time.Millisecond).Milliseconds(), 10)
	b = append(b, `,"truncated":`...)
	b = strconv.AppendBool(b, stats.Truncated)
	b = append(b, '}')
	j.compact = b
	_, _ = j.w.WriteString("],\n  \"stats\": ")
	if err := j.writeIndented(b, "  "); err != nil {
		return err
	}
	_, _ = j.w.WriteString("\n}\n")
	return j.w.Flush()
}

// writeRows renders r through a RowWriter for format f into memory, then
// copies it to w in one Write: Write stays all-or-nothing, so a failure
// part way never leaves partial output on stdout.
func writeRows(w io.Writer, f Format, r *Result) error {
	var buf bytes.Buffer
	if err := streamRows(&buf, f, r); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func streamRows(w io.Writer, f Format, r *Result) error {
	rw, err := NewRowWriter(w, f)
	if err != nil {
		return err
	}
	if err := rw.Begin(r.Columns); err != nil {
		return err
	}
	for _, row := range r.Rows {
		if err := rw.Row(row); err != nil {
			return err
		}
	}
	return rw.End(r.Stats)
}
