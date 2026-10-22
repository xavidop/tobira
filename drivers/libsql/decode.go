package libsql

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// The decoders below bound what a server's response can cost in memory: a
// value must have one of the Hrana shapes (nothing else is kept), and a
// statement result is decoded row by row against a budget of cells and
// bytes, so a hostile or merely huge result fails with a driver.ErrLimit instead of
// expanding several times past the response cap.

const (
	// resultBudgetBytes bounds the decoded size of one pipeline response,
	// shared by all its results and batch steps: cellCost per cell, column
	// and step error, rowCost per row, plus the bytes of text and blob
	// values, column names and declared types.
	resultBudgetBytes = 64 << 20
	// resultBudgetCells bounds the cells of one response.
	resultBudgetCells = 2 << 20
	// maxColumns is the protocol's column cap.
	maxColumns = 4096
	// maxBatchSteps bounds the steps of a batch result; the driver sends at
	// most a handful.
	maxBatchSteps = 1024
	cellCost      = 48 // a hranaValue
	rowCost       = 24 // a slice header
	// maxErrorBytes bounds the text of a server error.
	maxErrorBytes = 1024
)

// errBudget reports a result over the driver's memory budget. It is a
// driver.ErrLimit, and so is the driver.Error it is classified as.
var errBudget error = limitError("the result exceeds the libsql driver's memory budget (64 MiB decoded): add a LIMIT to the query")

// limitError is an error with a fixed text that is a driver.ErrLimit.
type limitError string

func (e limitError) Error() string { return string(e) }
func (limitError) Unwrap() error   { return driver.ErrLimit }

var errBadValue = errors.New("the server sent a malformed value")

// Interned type names, so decoding a value allocates no type string.
const (
	typeNull    = "null"
	typeInteger = "integer"
	typeFloat   = "float"
	typeText    = "text"
	typeBlob    = "blob"
)

// UnmarshalJSON accepts only the Hrana value shapes: {"type":"null"},
// {"type":"integer","value":"<decimal>"}, {"type":"float","value":<number>},
// {"type":"text","value":"<string>"} and {"type":"blob","base64":"<b64>"}.
// A blob is decoded here and kept as bytes in Value.
func (v *hranaValue) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	out, err := decodeValue(dec)
	if err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errBadValue
	}
	*v = out
	return nil
}

// decodeValue reads one value object token by token, so a value that is
// not a scalar (an array or object packed into "value") is refused at its
// first token, never materialised. dec must use UseNumber.
func decodeValue(dec *json.Decoder) (hranaValue, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return hranaValue{}, errBadValue
	}
	var typ, val, b64 any
	var hasType bool
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return hranaValue{}, errBadValue
		}
		tok, err := dec.Token()
		if err != nil {
			return hranaValue{}, errBadValue
		}
		if _, isDelim := tok.(json.Delim); isDelim {
			return hranaValue{}, errBadValue
		}
		switch k {
		case "type":
			typ, hasType = tok, true
		case "value":
			val = tok
		case "base64":
			b64 = tok
		}
	}
	if err := expectDelim(dec, '}'); err != nil || !hasType {
		return hranaValue{}, errBadValue
	}
	return makeValue(typ, val, b64)
}

// makeValue validates the tokens of one value object.
func makeValue(typ, val, b64 any) (hranaValue, error) {
	t, ok := typ.(string)
	if !ok || t != typeBlob && b64 != nil {
		return hranaValue{}, errBadValue
	}
	switch t {
	case typeNull:
		if val != nil {
			return hranaValue{}, errBadValue
		}
		return hranaValue{Type: typeNull}, nil
	case typeInteger:
		s, ok := val.(string)
		if !ok {
			return hranaValue{}, errBadValue
		}
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return hranaValue{}, errBadValue
		}
		return hranaValue{Type: typeInteger, Value: s}, nil
	case typeFloat:
		n, ok := val.(json.Number)
		if !ok {
			return hranaValue{}, errBadValue
		}
		f, err := strconv.ParseFloat(string(n), 64)
		if err != nil {
			return hranaValue{}, errBadValue
		}
		return hranaValue{Type: typeFloat, Value: f}, nil
	case typeText:
		s, ok := val.(string)
		if !ok {
			return hranaValue{}, errBadValue
		}
		return hranaValue{Type: typeText, Value: s}, nil
	case typeBlob:
		s, ok := b64.(string)
		if !ok || val != nil {
			return hranaValue{}, errBadValue
		}
		b, err := base64.RawStdEncoding.DecodeString(trimPadding(s))
		if err != nil {
			return hranaValue{}, errBadValue
		}
		return hranaValue{Type: typeBlob, Value: b}, nil
	}
	return hranaValue{}, errBadValue
}

func trimPadding(s string) string {
	for len(s) > 0 && s[len(s)-1] == '=' {
		s = s[:len(s)-1]
	}
	return s
}

// budget is what a statement result may still cost.
type budget struct{ bytes, cells int64 }

func newBudget(bytes, cells int64) *budget { return &budget{bytes: bytes, cells: cells} }

func (b *budget) take(n int64) error {
	b.bytes -= n
	if b.bytes < 0 {
		return errBudget
	}
	return nil
}

func (b *budget) row() error { return b.take(rowCost) }

func (b *budget) cell(v hranaValue) error {
	b.cells--
	if b.cells < 0 {
		return errBudget
	}
	n := int64(cellCost)
	switch x := v.Value.(type) {
	case string:
		n += int64(len(x))
	case []byte:
		n += int64(len(x))
	}
	return b.take(n)
}

// decodeResults decodes every execute and batch result of one pipeline
// response against a single budget, so the steps of a batch and the
// requests of a pipeline share it. A failure is kept on the response (the
// stream's baton stays valid) and reported by executeResult/batchOutcome.
func decodeResults(results []streamResult) {
	b := newBudget(resultBudgetBytes, resultBudgetCells)
	for i := range results {
		resp := results[i].Response
		if resp == nil || results[i].Type == "error" || results[i].Error != nil {
			continue
		}
		switch resp.Type {
		case "execute":
			resp.stmt, resp.err = decodeStmtResult(resp.Result, b)
		case "batch":
			resp.batch, resp.err = decodeBatchResult(resp.Result, b)
		}
		resp.Result = nil // the decoded form replaces the raw bytes
		if resp.err != nil && !errors.Is(resp.err, errBudget) {
			resp.err = errBadResult
		}
	}
}

var errBadResult = errors.New("the server sent an invalid result")

// decodeStmtResult decodes one statement result within b.
func decodeStmtResult(data []byte, b *budget) (*stmtResult, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	r, err := decodeStmtBody(dec, b)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// decodeStmtBody decodes a statement result object whose '{' was read.
// Fields other than cols, rows and affected_row_count are skipped.
func decodeStmtBody(dec *json.Decoder, b *budget) (*stmtResult, error) {
	r := &stmtResult{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		switch key {
		case "cols":
			if err := r.decodeCols(dec, b); err != nil {
				return nil, err
			}
		case "rows":
			if err := r.decodeRows(dec, b); err != nil {
				return nil, err
			}
		case "affected_row_count":
			var n json.Number
			if err := dec.Decode(&n); err != nil {
				return nil, err
			}
			c, err := n.Int64()
			if err != nil {
				return nil, errBadValue
			}
			r.AffectedRowCount = c
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	return r, nil
}

// decodeCols decodes the column list one element at a time, charging each
// (its name and declared type included) and refusing more than maxColumns.
func (r *stmtResult) decodeCols(dec *json.Decoder, b *budget) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	for dec.More() {
		if len(r.Cols) >= maxColumns {
			return fmt.Errorf("%w (more than %d columns)", errBudget, maxColumns)
		}
		var col hranaCol
		if err := dec.Decode(&col); err != nil {
			return err
		}
		n := int64(cellCost)
		if col.Name != nil {
			n += int64(len(*col.Name))
		}
		if col.DeclType != nil {
			n += int64(len(*col.DeclType))
		}
		if err := b.take(n); err != nil {
			return err
		}
		r.Cols = append(r.Cols, col)
	}
	return expectDelim(dec, ']')
}

// decodeBatchResult decodes a batch result within b: step_results (each
// null or a statement result) and step_errors.
func decodeBatchResult(data []byte, b *budget) (*batchResult, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	out := &batchResult{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		switch key {
		case "step_results":
			if err := expectDelim(dec, '['); err != nil {
				return nil, err
			}
			for dec.More() {
				if len(out.StepResults) >= maxBatchSteps {
					return nil, fmt.Errorf("%w (more than %d batch steps)", errBudget, maxBatchSteps)
				}
				t, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if t == nil {
					out.StepResults = append(out.StepResults, nil)
					continue
				}
				if d, ok := t.(json.Delim); !ok || d != '{' {
					return nil, errBadValue
				}
				r, err := decodeStmtBody(dec, b)
				if err != nil {
					return nil, err
				}
				out.StepResults = append(out.StepResults, r)
			}
			if err := expectDelim(dec, ']'); err != nil {
				return nil, err
			}
		case "step_errors":
			if err := expectDelim(dec, '['); err != nil {
				return nil, err
			}
			for dec.More() {
				if len(out.StepErrors) >= maxBatchSteps {
					return nil, fmt.Errorf("%w (more than %d batch steps)", errBudget, maxBatchSteps)
				}
				var e *hranaError
				if err := dec.Decode(&e); err != nil {
					return nil, err
				}
				if e != nil {
					if err := b.take(int64(cellCost + len(e.Message) + len(e.Code))); err != nil {
						return nil, err
					}
				}
				out.StepErrors = append(out.StepErrors, e)
			}
			if err := expectDelim(dec, ']'); err != nil {
				return nil, err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *stmtResult) decodeRows(dec *json.Decoder, b *budget) error {
	if tok, err := dec.Token(); err != nil {
		return err
	} else if tok == nil {
		return nil // "rows": null
	} else if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errBadValue
	}
	for dec.More() {
		if err := expectDelim(dec, '['); err != nil {
			return err
		}
		if err := b.row(); err != nil {
			return err
		}
		row := make([]hranaValue, 0, len(r.Cols))
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return err
			}
			if err := b.cell(v); err != nil {
				return err
			}
			row = append(row, v)
		}
		if err := expectDelim(dec, ']'); err != nil {
			return err
		}
		r.Rows = append(r.Rows, row)
	}
	return expectDelim(dec, ']')
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("%w: expected %q", errBadValue, want)
	}
	return nil
}

// UnmarshalJSON keeps at most maxErrorBytes of the server's message.
func (e *hranaError) UnmarshalJSON(data []byte) error {
	var raw struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	e.Message, e.Code = truncateText(raw.Message, maxErrorBytes), truncateText(raw.Code, 64)
	return nil
}

// truncateText cuts s to at most n bytes on a rune boundary, marking the cut
// with "…".
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
