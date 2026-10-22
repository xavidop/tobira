package d1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// D1's answers are decoded token by token against one budget per response,
// so a hostile or merely huge body cannot make the decoder allocate much
// more than the body itself: values are accepted only in D1's shapes (null,
// a number, a string, or a blob as an array of byte values), and anything
// else is refused at its first token.
const (
	// maxResponseBytes caps a response body.
	maxResponseBytes = 64 << 20
	// resultBudgetBytes and resultBudgetCells bound what one response may
	// decode to: an estimate of the Go memory of its columns, rows and
	// values, and its number of values.
	resultBudgetBytes = 64 << 20
	resultBudgetCells = 2 << 20
	// maxColumns caps a result's columns (SQLite's own maximum is 32767;
	// D1's is 100).
	maxColumns = 4096
	// maxResults caps the statement results in one response; the driver
	// sends one statement per request.
	maxResults = 16
	// maxErrors and maxErrorBytes cap the server's error list and each
	// message.
	maxErrors     = 16
	maxErrorBytes = 1024

	rowCost  = 72 // a row's slice header, three times over for the rows slice's growth
	cellCost = 48 // a value's interface plus bookkeeping
)

// errBudget reports a result over the driver's memory budget. It is a
// driver.ErrLimit, and so is the driver.Error it is classified as.
var errBudget error = limitError("the result exceeds the d1 driver's memory budget (64 MiB decoded): add a LIMIT to the query")

// limitError is an error with a fixed text that is a driver.ErrLimit.
type limitError string

func (e limitError) Error() string { return string(e) }
func (limitError) Unwrap() error   { return driver.ErrLimit }

var (
	errBadResponse = errors.New("the server sent an invalid response")
	errBadValue    = errors.New("the server sent a malformed value")
)

// budget is what is left of one response's decoding allowance.
type budget struct{ bytes, cells int64 }

func newBudget(bytes, cells int64) *budget { return &budget{bytes: bytes, cells: cells} }

func (b *budget) take(n int64) error {
	b.bytes -= n
	if b.bytes < 0 {
		return errBudget
	}
	return nil
}

// cell charges one value carrying extra bytes of text or blob.
func (b *budget) cell(extra int64) error {
	b.cells--
	if b.cells < 0 {
		return errBudget
	}
	return b.take(cellCost + extra)
}

// apiError is an error D1 or the Cloudflare API reported: the HTTP status,
// the first error's code and its message (truncated).
type apiError struct {
	Status  int
	Code    int
	Message string
}

func (e *apiError) Error() string {
	switch {
	case e.Code != 0:
		return fmt.Sprintf("D1 error %d: %s", e.Code, e.Message)
	case e.Status != 0:
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// meta is the part of a statement's meta the driver uses.
type meta struct {
	Changes      int64
	ChangesKnown bool // the server sent meta.changes
	LastRowID    int64
}

// stmtResult is one statement's result from the /raw endpoint.
type stmtResult struct {
	Columns []string
	Rows    [][]any // int64, float64, string, []byte or nil
	Meta    meta
}

// response is a decoded API envelope.
type response struct {
	Success bool
	Results []*stmtResult
	Errors  []apiError
}

// decodeResponse decodes a /raw response envelope:
//
//	{"result": [{"results": {"columns": [...], "rows": [[...]]},
//	  "meta": {...}, "success": true}], "errors": [...],
//	 "messages": [...], "success": true}
//
// against b. Unknown keys are skipped without being materialised.
func decodeResponse(data []byte, b *budget) (*response, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	resp := &response{}
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return nil, err
		}
		switch key {
		case "result":
			err = decodeResults(dec, b, resp)
		case "errors":
			err = decodeErrors(dec, resp)
		case "success":
			resp.Success, err = readBool(dec)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: data after the envelope", errBadResponse)
	}
	return resp, nil
}

func decodeResults(dec *json.Decoder, b *budget, resp *response) error {
	tok, err := dec.Token()
	if err != nil {
		return badJSON(err)
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("%w: result is not a list", errBadResponse)
	}
	for dec.More() {
		if len(resp.Results) >= maxResults {
			return fmt.Errorf("%w: more than %d statement results", errBudget, maxResults)
		}
		r, err := decodeStmt(dec, b)
		if err != nil {
			return err
		}
		resp.Results = append(resp.Results, r)
	}
	return expectDelim(dec, ']')
}

func decodeStmt(dec *json.Decoder, b *budget) (*stmtResult, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	r := &stmtResult{}
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return nil, err
		}
		switch key {
		case "results":
			err = r.decodeResults(dec, b)
		case "meta":
			err = r.decodeMeta(dec)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return nil, err
		}
	}
	return r, expectDelim(dec, '}')
}

// decodeResults reads {"columns": [...], "rows": [[...]]}, in either order;
// every row must be as wide as the column list.
func (r *stmtResult) decodeResults(dec *json.Decoder, b *budget) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	sawCols := false
	width := -1
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return err
		}
		switch key {
		case "columns":
			sawCols = true
			err = r.decodeColumns(dec, b)
		case "rows":
			width, err = r.decodeRows(dec, b, sawCols)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return err
		}
	}
	if width >= 0 && (!sawCols || width != len(r.Columns)) {
		return fmt.Errorf("%w: rows have %d values for %d columns", errBadResponse, width, len(r.Columns))
	}
	return expectDelim(dec, '}')
}

func (r *stmtResult) decodeColumns(dec *json.Decoder, b *budget) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	r.Columns = r.Columns[:0]
	for dec.More() {
		if len(r.Columns) >= maxColumns {
			return fmt.Errorf("%w: more than %d columns", errBudget, maxColumns)
		}
		tok, err := dec.Token()
		if err != nil {
			return badJSON(err)
		}
		name, ok := tok.(string)
		if !ok {
			return fmt.Errorf("%w: a column name is not a string", errBadResponse)
		}
		if err := b.take(cellCost + int64(len(name))); err != nil {
			return err
		}
		r.Columns = append(r.Columns, name)
	}
	return expectDelim(dec, ']')
}

// decodeRows reads the row arrays and returns their common width (-1 when
// there are none). When the columns came first, rows are checked against
// them as they stream.
func (r *stmtResult) decodeRows(dec *json.Decoder, b *budget, colsKnown bool) (int, error) {
	if err := expectDelim(dec, '['); err != nil {
		return -1, err
	}
	width := -1
	if colsKnown {
		width = len(r.Columns)
	}
	seen := false
	for dec.More() {
		if err := b.take(rowCost); err != nil {
			return -1, err
		}
		if err := expectDelim(dec, '['); err != nil {
			return -1, err
		}
		row := make([]any, 0, max(width, 0))
		for dec.More() {
			if len(row) >= maxColumns || width >= 0 && len(row) >= width {
				return -1, fmt.Errorf("%w: a row is wider than its columns", errBadResponse)
			}
			v, err := decodeValue(dec, b)
			if err != nil {
				return -1, err
			}
			row = append(row, v)
		}
		if err := expectDelim(dec, ']'); err != nil {
			return -1, err
		}
		if width < 0 {
			width = len(row)
		}
		if len(row) != width {
			return -1, fmt.Errorf("%w: a row has %d values for %d columns", errBadResponse, len(row), width)
		}
		r.Rows = append(r.Rows, row)
		seen = true
	}
	if !seen {
		width = -1
	}
	return width, expectDelim(dec, ']')
}

// decodeValue reads one value in D1's shapes: null, a number (an int64 when
// written as an integer that fits, else a float64), a string, or a blob as
// an array of byte values 0-255. Anything else is errBadValue.
func decodeValue(dec *json.Decoder, b *budget) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, badJSON(err)
	}
	switch v := tok.(type) {
	case nil:
		return nil, b.cell(0)
	case string:
		return v, b.cell(int64(len(v)))
	case json.Number:
		if err := b.cell(0); err != nil {
			return nil, err
		}
		return parseNumber(string(v))
	case json.Delim:
		if v != '[' {
			return nil, errBadValue
		}
		if err := b.cell(0); err != nil {
			return nil, err
		}
		blob := []byte{}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return nil, badJSON(err)
			}
			num, ok := tok.(json.Number)
			if !ok {
				return nil, errBadValue
			}
			n, err := strconv.ParseUint(string(num), 10, 8)
			if err != nil {
				return nil, errBadValue
			}
			if err := b.take(1); err != nil {
				return nil, err
			}
			blob = append(blob, byte(n))
		}
		return blob, expectDelim(dec, ']')
	}
	return nil, errBadValue
}

// parseNumber reads a JSON number as D1's JavaScript wrote it.
func parseNumber(s string) (any, error) {
	if !strings.ContainsAny(s, ".eE") {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, errBadValue
	}
	return f, nil
}

func (r *stmtResult) decodeMeta(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return badJSON(err)
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("%w: meta is not an object", errBadResponse)
	}
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return err
		}
		switch key {
		case "changes":
			r.Meta.Changes, err = readInt(dec)
			r.Meta.ChangesKnown = err == nil
		case "last_row_id":
			r.Meta.LastRowID, err = readInt(dec)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return err
		}
	}
	return expectDelim(dec, '}')
}

func decodeErrors(dec *json.Decoder, resp *response) error {
	tok, err := dec.Token()
	if err != nil {
		return badJSON(err)
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("%w: errors is not a list", errBadResponse)
	}
	for dec.More() {
		if len(resp.Errors) >= maxErrors {
			if err := skipValue(dec); err != nil {
				return err
			}
			continue
		}
		e, err := decodeError(dec)
		if err != nil {
			return err
		}
		resp.Errors = append(resp.Errors, e)
	}
	return expectDelim(dec, ']')
}

func decodeError(dec *json.Decoder) (apiError, error) {
	var e apiError
	if err := expectDelim(dec, '{'); err != nil {
		return e, err
	}
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return e, err
		}
		switch key {
		case "code":
			var n int64
			n, err = readInt(dec)
			e.Code = int(min(max(n, -1<<31), 1<<31-1))
		case "message":
			var tok json.Token
			if tok, err = dec.Token(); err != nil {
				return e, badJSON(err)
			}
			s, ok := tok.(string)
			if !ok {
				return e, fmt.Errorf("%w: an error message is not a string", errBadResponse)
			}
			e.Message = truncateText(s, maxErrorBytes)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return e, err
		}
	}
	return e, expectDelim(dec, '}')
}

// readKey reads an object key.
func readKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", badJSON(err)
	}
	k, ok := tok.(string)
	if !ok {
		return "", errBadResponse
	}
	return k, nil
}

func readBool(dec *json.Decoder) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return false, badJSON(err)
	}
	switch v := tok.(type) {
	case bool:
		return v, nil
	case nil:
		return false, nil
	}
	return false, fmt.Errorf("%w: success is not a boolean", errBadResponse)
}

// readInt reads an integer (a float is truncated; null is 0).
func readInt(dec *json.Decoder) (int64, error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, badJSON(err)
	}
	switch v := tok.(type) {
	case nil:
		return 0, nil
	case json.Number:
		n, err := parseNumber(string(v))
		if err != nil {
			return 0, err
		}
		if f, ok := n.(float64); ok {
			return int64(f), nil
		}
		return n.(int64), nil
	}
	return 0, fmt.Errorf("%w: expected a number", errBadResponse)
}

// skipValue consumes one value of any shape without keeping it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return badJSON(err)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '[', '{':
				depth++
			default:
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return badJSON(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("%w: expected %q", errBadResponse, want)
	}
	return nil
}

// badJSON marks a syntax error or a truncated body.
func badJSON(err error) error {
	if errors.Is(err, errBudget) || errors.Is(err, errBadResponse) || errors.Is(err, errBadValue) {
		return err
	}
	return fmt.Errorf("%w: %v", errBadResponse, err)
}

// truncateText cuts s to at most n bytes on a rune boundary, marking the cut
// with "…"; invalid UTF-8 is replaced.
func truncateText(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
