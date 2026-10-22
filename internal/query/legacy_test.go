package query

// This file holds the csv, json and ndjson writers exactly as they were
// before M3 (git show 812f4c6:internal/query/format.go, lines 52-208),
// copied verbatim except that every function name gained a "legacy" prefix
// so it can sit next to the real ones. TestRowWriterMatchesLegacyWrite
// compares the streaming writers against them.

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// LegacyWrite renders r with the pre-M3 buffered writers (csv, json, ndjson).
var LegacyWrite = legacyWrite

func legacyWrite(w io.Writer, f Format, r *Result) error {
	switch f {
	case FormatJSON:
		return legacyWriteJSON(w, r)
	case FormatNDJSON:
		return legacyWriteNDJSON(w, r)
	case FormatCSV:
		return legacyWriteCSV(w, r)
	}
	panic("legacyWrite: " + string(f))
}

// ---- JSON / NDJSON ----

func legacyAppendString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // cannot fail for a string
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}

// legacyJsonKeys returns one unique object key per column. Repeated names get
// "_2", "_3"… suffixes (`select 1 as a, 2 as a` -> a, a_2).
func legacyJsonKeys(cols []driver.Column) []string {
	keys := make([]string, len(cols))
	used := make(map[string]bool, len(cols))
	for i, c := range cols {
		k := c.Name
		for n := 2; used[k]; n++ {
			k = c.Name + "_" + strconv.Itoa(n)
		}
		used[k] = true
		keys[i] = k
	}
	return keys
}

func legacyAppendRowObject(dst []byte, keys []string, row driver.Row) []byte {
	dst = append(dst, '{')
	for i, k := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = legacyAppendString(dst, k)
		dst = append(dst, ':')
		var v driver.Value
		if i < len(row) {
			v = row[i]
		}
		b, _ := v.MarshalJSON() // never fails
		dst = append(dst, b...)
	}
	return append(dst, '}')
}

func legacyWritePlanJSON(w io.Writer, p *driver.Plan) error {
	format := p.Format
	if format == "" {
		format = "text"
	}
	b := append([]byte(`{"plan":`), legacyAppendString(nil, p.Text)...)
	b = append(b, `,"format":`...)
	b = legacyAppendString(b, format)
	b = append(b, "}\n"...)
	_, err := w.Write(b)
	return err
}

func legacyWriteJSON(w io.Writer, r *Result) error {
	if r.Plan != nil {
		return legacyWritePlanJSON(w, r.Plan)
	}
	keys := legacyJsonKeys(r.Columns)
	b := []byte(`{"columns":[`)
	for i, c := range r.Columns {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = legacyAppendString(b, c.Name)
		b = append(b, `,"type":`...)
		b = legacyAppendString(b, c.Type.String())
		b = append(b, '}')
	}
	b = append(b, `],"rows":[`...)
	for i, row := range r.Rows {
		if i > 0 {
			b = append(b, ',')
		}
		b = legacyAppendRowObject(b, keys, row)
	}
	b = append(b, `],"stats":{"rows_affected":`...)
	b = strconv.AppendInt(b, r.Stats.RowsAffected, 10)
	b = append(b, `,"duration_ms":`...)
	b = strconv.AppendInt(b, r.Stats.Duration.Round(time.Millisecond).Milliseconds(), 10)
	b = append(b, `,"truncated":`...)
	b = strconv.AppendBool(b, r.Stats.Truncated)
	b = append(b, "}}"...)

	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err := w.Write(out.Bytes())
	return err
}

func legacyWriteNDJSON(w io.Writer, r *Result) error {
	if r.Plan != nil {
		return legacyWritePlanJSON(w, r.Plan)
	}
	keys := legacyJsonKeys(r.Columns)
	var b []byte
	for _, row := range r.Rows {
		b = legacyAppendRowObject(b, keys, row)
		b = append(b, '\n')
	}
	_, err := w.Write(b)
	return err
}

// ---- CSV ----

// legacyCsvField quotes per RFC 4180 and also quotes the empty string, so that it
// differs from NULL (an empty unquoted field).
func legacyCsvField(s string) string {
	if s == "" || strings.ContainsAny(s, ",\"\r\n") || s[0] == ' ' || s[len(s)-1] == ' ' {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func legacyWriteCSV(w io.Writer, r *Result) error {
	if r.Plan != nil {
		return legacyWritePlanText(w, r.Plan)
	}
	var sb strings.Builder
	for i, c := range r.Columns {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(legacyCsvField(c.Name))
	}
	if len(r.Columns) > 0 {
		sb.WriteByte('\n')
	}
	for _, row := range r.Rows {
		for i, v := range row {
			if i > 0 {
				sb.WriteByte(',')
			}
			if !v.IsNull() {
				sb.WriteString(legacyCsvField(v.String()))
			}
		}
		sb.WriteByte('\n')
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

func legacyWritePlanText(w io.Writer, p *driver.Plan) error {
	_, err := io.WriteString(w, escape(strings.TrimRight(p.Text, "\n"), true)+"\n")
	return err
}
