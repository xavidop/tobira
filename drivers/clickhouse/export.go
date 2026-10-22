package clickhouse

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

var _ driver.Exporter = (*conn)(nil)

// ExportFormats lists the formats Export writes: "csv" is ClickHouse's
// CSVWithNames.
func (*conn) ExportFormats() []string { return []string{"csv"} }

// Export streams the result of one read statement to w as CSVWithNames, in
// constant memory. It always runs as a read-only call, whatever the
// connection's mode: the allow-list and readonly=1 apply, so a write is
// ErrReadOnly.
//
// Over HTTP the server formats the rows (FORMAT CSVWithNames) and the bytes
// are copied to w as they arrive. The native protocol only carries Native
// blocks, so there the driver writes the same layout itself: a header of
// column names, then one line per row, NULL as \N.
//
// The query must not carry a FORMAT clause of its own, and q.Limit must be 0
// (put LIMIT in the query). ctx cancels the export.
func (c *conn) Export(ctx context.Context, q driver.Query, format string, w io.Writer) error {
	const op = "export"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if format != "csv" {
		return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("unknown export format %q (have csv)", format))
	}
	if q.Limit > 0 {
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("export cannot limit rows; put LIMIT in the query"))
	}
	if err := checkReadOnly(q.Text); err != nil {
		return err
	}
	if err := checkParams(q.Text, q.Params); err != nil {
		return err
	}
	if hasFormatClause(q.Text) {
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("remove the FORMAT clause: export chooses the format"))
	}
	if c.opts.Protocol == ch.HTTP {
		return c.exportHTTP(ctx, op, q, w)
	}
	q.ReadOnly = true
	rs, err := c.run(ctx, op, q, true, checkReadOnly)
	if err != nil {
		return err
	}
	defer func() { _ = rs.Close() }()
	cw := csv.NewWriter(w)
	cols := rs.Columns()
	rec := make([]string, len(cols))
	for i, col := range cols {
		rec[i] = col.Name
	}
	if err := cw.Write(rec); err != nil {
		return err
	}
	for {
		row, ok, err := rs.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		for i, v := range row {
			rec[i] = csvField(v)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	return rs.Close()
}

// exportHTTP has the server format the rows: QueryFormat sends the query
// with the format as an HTTP parameter.
func (c *conn) exportHTTP(ctx context.Context, op string, q driver.Query, w io.Writer) error {
	args, err := toArgs(op, q.Params)
	if err != nil {
		return err
	}
	p, err := c.pool(ctx, q.Namespace)
	if err != nil {
		return err
	}
	if p.serverRO {
		if err := p.checkTableFunctions(ctx, op, q.Text, args); err != nil {
			return err
		}
	}
	dctx, done := detach(ctx)
	defer done()
	stream, err := p.db.QueryFormat(p.roCtx(dctx), "CSVWithNames", q.Text, args...)
	if err != nil {
		return mapError(ctx, op, err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := io.Copy(w, stream); err != nil {
		return mapError(ctx, op, err)
	}
	return mapError(ctx, op, stream.Close())
}

// csvField renders one value the way CSVWithNames does: NULL as \N, text
// and JSON as is, times as "YYYY-MM-DD hh:mm:ss[.fraction]" in UTC.
func csvField(v driver.Value) string {
	switch v.Type() {
	case driver.TypeNull:
		return `\N`
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return strconv.FormatFloat(f, 'g', -1, 64)
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return string(b)
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return string(raw)
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format("2006-01-02 15:04:05.999999999")
	}
	return v.String()
}

// hasFormatClause reports whether text has a top-level FORMAT <name>
// clause, which would override the export format: FORMAT and a name at
// parenthesis depth 0, followed by the end of the statement or by SETTINGS
// (ClickHouse accepts the two clauses in either order). A column named
// format (SELECT format FROM t) is not one. Should a spelling slip past, the
// export only comes out in another format: it is still a read-only call.
func hasFormatClause(text string) bool {
	toks, _ := lex(text)
	for len(toks) > 0 && toks[len(toks)-1].kind == sqlscan.Semicolon {
		toks = toks[:len(toks)-1]
	}
	depth := 0
	for i, t := range toks {
		switch {
		case t.kind == sqlscan.Punct && t.text == "(":
			depth++
		case t.kind == sqlscan.Punct && t.text == ")":
			depth--
		case depth == 0 && t.kind == sqlscan.Word && t.text == "FORMAT" && i+1 < len(toks) &&
			(toks[i+1].kind == sqlscan.Word || toks[i+1].kind == sqlscan.QuotedIdent):
			if i+2 == len(toks) || toks[i+2].kind == sqlscan.Word && toks[i+2].text == "SETTINGS" {
				return true
			}
		}
	}
	return false
}
