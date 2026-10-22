package query

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
)

// Format is an output format of `tobira query`.
type Format string

// The supported formats.
const (
	FormatTable  Format = "table"
	FormatJSON   Format = "json"
	FormatNDJSON Format = "ndjson"
	FormatCSV    Format = "csv"
)

const maxCellWidth = 60

// Formats lists the valid formats in help order.
func Formats() []Format { return []Format{FormatTable, FormatJSON, FormatNDJSON, FormatCSV} }

// Write renders r in format f. Every format ends with exactly one newline.
func Write(w io.Writer, f Format, r *Result) error {
	switch f {
	case FormatTable:
		return writeTable(w, r)
	case FormatJSON:
		return writeJSON(w, r)
	case FormatNDJSON:
		return writeNDJSON(w, r)
	case FormatCSV:
		return writeCSV(w, r)
	}
	names := make([]string, 0, 4)
	for _, v := range Formats() {
		names = append(names, string(v))
	}
	return fmt.Errorf("unknown format %q (valid formats: %s)", f, strings.Join(names, ", "))
}

// ---- JSON / NDJSON ----

func appendString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // cannot fail for a string
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}

// jsonKeys returns one unique object key per column. Repeated names get
// "_2", "_3"… suffixes (`select 1 as a, 2 as a` -> a, a_2).
func jsonKeys(cols []driver.Column) []string {
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

func appendRowObject(dst []byte, keys []string, row driver.Row) []byte {
	dst = append(dst, '{')
	for i, k := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendString(dst, k)
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

func writePlanJSON(w io.Writer, p *driver.Plan) error {
	format := p.Format
	if format == "" {
		format = "text"
	}
	b := append([]byte(`{"plan":`), appendString(nil, p.Text)...)
	b = append(b, `,"format":`...)
	b = appendString(b, format)
	b = append(b, "}\n"...)
	_, err := w.Write(b)
	return err
}

func writeJSON(w io.Writer, r *Result) error {
	if r.Plan != nil {
		return writePlanJSON(w, r.Plan)
	}
	return writeRows(w, FormatJSON, r)
}

func writeNDJSON(w io.Writer, r *Result) error {
	if r.Plan != nil {
		return writePlanJSON(w, r.Plan)
	}
	return writeRows(w, FormatNDJSON, r)
}

// ---- CSV ----

// csvField quotes per RFC 4180 and also quotes the empty string, so that it
// differs from NULL (an empty unquoted field).
func csvField(s string) string {
	if s == "" || strings.ContainsAny(s, ",\"\r\n") || s[0] == ' ' || s[len(s)-1] == ' ' {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func writeCSV(w io.Writer, r *Result) error {
	if r.Plan != nil {
		return writePlanText(w, r.Plan)
	}
	return writeRows(w, FormatCSV, r)
}

// ---- table ----

func writePlanText(w io.Writer, p *driver.Plan) error {
	_, err := io.WriteString(w, escape(strings.TrimRight(p.Text, "\n"), true)+"\n")
	return err
}

// EscapeControls makes s safe to print on a terminal as one line: '\n', '\r'
// and '\t' become a space, and every other C0 control, DEL and C1 control
// becomes a visible escape ("\x1b", "\x7f", "\u009b"), so database text
// cannot inject terminal escape sequences. Zero-width and bidirectional
// controls (U+200B-U+200F, U+202A-U+202E, U+2066-U+2069, U+FEFF) become
// "\u{XXXX}", which blocks visual spoofing.
func EscapeControls(s string) string { return escape(s, false) }

// EscapeControlsMultiline is EscapeControls for text that spans lines: '\n'
// and '\t' are kept, every other control is escaped as there. It is for model
// output shown as paragraphs, never for a table cell.
func EscapeControlsMultiline(s string) string { return escape(s, true) }

// escape is EscapeControls; with multiline, '\n' and '\t' are kept as is.
func escape(s string, multiline bool) string {
	if clean(s) {
		return s
	}
	var sb strings.Builder
	for rest := s; rest != ""; {
		cluster, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if cluster == "" {
			cluster = rest[:1]
		}
		rest = rest[len(cluster):]
		if utf8.ValidString(cluster) {
			escapeCluster(&sb, cluster, multiline)
			continue
		}
		// Bytes that are not UTF-8 never reach the terminal raw: a lone 0x9b
		// is the C1 CSI introducer on 8-bit terminals.
		for i := 0; i < len(cluster); {
			r, n := utf8.DecodeRuneInString(cluster[i:])
			if r == utf8.RuneError && n == 1 {
				fmt.Fprintf(&sb, "\\x{%02X}", cluster[i])
			} else {
				escapeCluster(&sb, cluster[i:i+n], multiline)
			}
			i += n
		}
	}
	return sb.String()
}

// clean reports whether s is valid UTF-8 with no control to escape.
func clean(s string) bool {
	for i, r := range s {
		if isControl(r) || r == utf8.RuneError && !strings.HasPrefix(s[i:], "\uFFFD") {
			return false
		}
	}
	return true
}

// escapeCluster writes one grapheme cluster of s, escaped. A zero width
// joiner is kept only where it joins an emoji sequence: inside the cluster,
// followed by a pictographic character. A bare, trailing or letter-joining
// ZWJ is escaped, since it is invisible and can disguise text.
func escapeCluster(sb *strings.Builder, cluster string, multiline bool) {
	runes := []rune(cluster)
	for i, r := range runes {
		switch {
		case multiline && (r == '\n' || r == '\t'):
			sb.WriteRune(r)
		case !multiline && (r == '\n' || r == '\r' || r == '\t'):
			sb.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(sb, "\\x%02x", r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(sb, "\\u%04x", r)
		case r == zwj && i+1 < len(runes) && isPictographic(runes[i+1]):
			sb.WriteRune(r)
		case isBidi(r):
			fmt.Fprintf(sb, "\\u{%04X}", r)
		default:
			sb.WriteRune(r)
		}
	}
}

const zwj = 0x200d

// extendedPictographic is the Extended_Pictographic property of Unicode 15.0,
// merged into ranges. Source: emoji-data.txt,
// https://unicode.org/Public/15.0.0/ucd/emoji/emoji-data.txt (the same table
// rivo/uniseg v0.4.7 generates its grapheme rules from).
var extendedPictographic = [][2]rune{
	{0x00A9, 0x00A9},
	{0x00AE, 0x00AE},
	{0x203C, 0x203C},
	{0x2049, 0x2049},
	{0x2122, 0x2122},
	{0x2139, 0x2139},
	{0x2194, 0x2199},
	{0x21A9, 0x21AA},
	{0x231A, 0x231B},
	{0x2328, 0x2328},
	{0x2388, 0x2388},
	{0x23CF, 0x23CF},
	{0x23E9, 0x23F3},
	{0x23F8, 0x23FA},
	{0x24C2, 0x24C2},
	{0x25AA, 0x25AB},
	{0x25B6, 0x25B6},
	{0x25C0, 0x25C0},
	{0x25FB, 0x25FE},
	{0x2600, 0x2605},
	{0x2607, 0x2612},
	{0x2614, 0x2685},
	{0x2690, 0x2705},
	{0x2708, 0x2712},
	{0x2714, 0x2714},
	{0x2716, 0x2716},
	{0x271D, 0x271D},
	{0x2721, 0x2721},
	{0x2728, 0x2728},
	{0x2733, 0x2734},
	{0x2744, 0x2744},
	{0x2747, 0x2747},
	{0x274C, 0x274C},
	{0x274E, 0x274E},
	{0x2753, 0x2755},
	{0x2757, 0x2757},
	{0x2763, 0x2767},
	{0x2795, 0x2797},
	{0x27A1, 0x27A1},
	{0x27B0, 0x27B0},
	{0x27BF, 0x27BF},
	{0x2934, 0x2935},
	{0x2B05, 0x2B07},
	{0x2B1B, 0x2B1C},
	{0x2B50, 0x2B50},
	{0x2B55, 0x2B55},
	{0x3030, 0x3030},
	{0x303D, 0x303D},
	{0x3297, 0x3297},
	{0x3299, 0x3299},
	{0x1F000, 0x1F0FF},
	{0x1F10D, 0x1F10F},
	{0x1F12F, 0x1F12F},
	{0x1F16C, 0x1F171},
	{0x1F17E, 0x1F17F},
	{0x1F18E, 0x1F18E},
	{0x1F191, 0x1F19A},
	{0x1F1AD, 0x1F1E5},
	{0x1F201, 0x1F20F},
	{0x1F21A, 0x1F21A},
	{0x1F22F, 0x1F22F},
	{0x1F232, 0x1F23A},
	{0x1F23C, 0x1F23F},
	{0x1F249, 0x1F3FA},
	{0x1F400, 0x1F53D},
	{0x1F546, 0x1F64F},
	{0x1F680, 0x1F6FF},
	{0x1F774, 0x1F77F},
	{0x1F7D5, 0x1F7FF},
	{0x1F80C, 0x1F80F},
	{0x1F848, 0x1F84F},
	{0x1F85A, 0x1F85F},
	{0x1F888, 0x1F88F},
	{0x1F8AE, 0x1F8FF},
	{0x1F90C, 0x1F93A},
	{0x1F93C, 0x1F945},
	{0x1F947, 0x1FAFF},
	{0x1FC00, 0x1FFFD},
}

// isPictographic reports whether r is Extended_Pictographic: a character a
// zero width joiner may join into an emoji sequence.
func isPictographic(r rune) bool {
	i, j := 0, len(extendedPictographic)
	for i < j {
		h := (i + j) / 2
		switch {
		case r < extendedPictographic[h][0]:
			j = h
		case r > extendedPictographic[h][1]:
			i = h + 1
		default:
			return true
		}
	}
	return false
}

func isControl(r rune) bool { return r < 0x20 || r >= 0x7f && r <= 0x9f || isBidi(r) }

// isBidi reports zero-width and bidirectional-control characters, which can
// make text render differently from what it holds (visual spoofing).
func isBidi(r rune) bool {
	return r >= 0x200b && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0xfeff
}

// DisplayWidth is the number of terminal columns s occupies. It is
// ansi.StringWidth, the algorithm lipgloss and the TUI layout use, so every
// place that measures text agrees: a ZWJ emoji sequence or a base letter with
// combining marks counts as one grapheme cluster.
func DisplayWidth(s string) int { return ansi.StringWidth(s) }

// Truncate shortens s to at most w columns, ending it with "…" when it had to
// cut. It never splits a grapheme cluster. A w below 1 yields "".
func Truncate(s string, w int) string {
	if w < 1 {
		return ""
	}
	if DisplayWidth(s) <= w {
		return s
	}
	end, used := 0, 0
	for rest := s; rest != ""; {
		cluster, cw := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if cluster == "" || used+cw > w-1 {
			break
		}
		end += len(cluster)
		used += cw
		rest = rest[len(cluster):]
	}
	return s[:end] + "…"
}

// cellText makes s a single-line cell of at most maxCellWidth columns.
func cellText(s string) string {
	s = EscapeControls(s)
	return Truncate(s, maxCellWidth)
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + "s"
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatInt(n, 10) + " " + many
}

func writeTable(w io.Writer, r *Result) error {
	if r.Plan != nil {
		return writePlanText(w, r.Plan)
	}
	dur := formatDuration(r.Stats.Duration)
	if len(r.Columns) == 0 && r.Stats.RowsAffected >= 0 {
		_, err := fmt.Fprintf(w, "%s affected · %s\n", plural(r.Stats.RowsAffected, "row", "rows"), dur)
		return err
	}

	n := len(r.Columns)
	widths := make([]int, n)
	header := make([]string, n)
	for i, c := range r.Columns {
		header[i] = cellText(c.Name)
		widths[i] = DisplayWidth(header[i])
	}
	cells := make([][]string, len(r.Rows))
	for ri, row := range r.Rows {
		cells[ri] = make([]string, n)
		for i := 0; i < n && i < len(row); i++ {
			cells[ri][i] = cellText(row[i].String())
			widths[i] = max(widths[i], DisplayWidth(cells[ri][i]))
		}
	}

	var sb strings.Builder
	line := func(vals []string) {
		for i, v := range vals {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(v)
			if i < n-1 {
				sb.WriteString(strings.Repeat(" ", widths[i]-DisplayWidth(v)))
			}
		}
		sb.WriteByte('\n')
	}
	if n > 0 {
		line(header)
		rules := make([]string, n)
		for i, wd := range widths {
			rules[i] = strings.Repeat("─", wd)
		}
		line(rules)
		for _, c := range cells {
			line(c)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(plural(int64(len(r.Rows)), "row", "rows") + " · " + dur)
	if r.Stats.Truncated {
		sb.WriteString(" · truncated")
	}
	sb.WriteByte('\n')
	_, err := io.WriteString(w, sb.String())
	return err
}
