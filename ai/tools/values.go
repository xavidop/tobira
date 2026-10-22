package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// encodeValue turns v into what the model sees. Numbers and booleans stay
// JSON numbers and booleans. Text is redacted, then cut to MaxValueRunes.
// Bytes become a size summary. A vector becomes its display text: its
// first 8 components, then "…". JSON, lists and objects are serialised,
// then redacted and cut like text.
func (t *Toolset) encodeValue(v driver.Value, redact func(string) string) any {
	switch v.Type() {
	case driver.TypeNull:
		return nil
	case driver.TypeBool, driver.TypeInt, driver.TypeFloat, driver.TypeDecimal:
		b, _ := v.MarshalJSON() // Value.MarshalJSON never fails
		return json.RawMessage(b)
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return bytesSummary(len(b))
	case driver.TypeTime:
		return v.String()
	case driver.TypeString, driver.TypeUnknown, driver.TypeJSON:
		return truncate(redact(v.String()), MaxValueRunes)
	case driver.TypeVector:
		return v.String() // its first 8 components, then "…"
	}
	b, _ := v.MarshalJSON()
	return truncate(redact(string(b)), MaxValueRunes)
}

// encodeRows encodes rows as a JSON array of arrays.
func (t *Toolset) encodeRows(conn string, rows []driver.Row) json.RawMessage {
	redact := t.redactor(conn)
	out := make([][]any, len(rows))
	for i, r := range rows {
		out[i] = make([]any, len(r))
		for j, v := range r {
			out[i][j] = t.encodeValue(v, redact)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out) // plain values: cannot fail
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// bytesSummary describes n bytes: "<bytes 10 B>", "<bytes 1.2 KiB>".
func bytesSummary(n int) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("<bytes %d B>", n)
	case n < 1<<20:
		return fmt.Sprintf("<bytes %.1f KiB>", float64(n)/(1<<10))
	}
	return fmt.Sprintf("<bytes %.1f MiB>", float64(n)/(1<<20))
}

// truncate cuts s to maxRunes runes, marking a cut with "…".
func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i] + "…"
		}
		n++
	}
	return s
}
