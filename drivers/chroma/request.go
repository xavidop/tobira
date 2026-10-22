package chroma

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// The query ops: the only requests Query sends. Each one is a read.
const (
	opGet   = "get"
	opQuery = "query"
	opCount = "count"
)

// Bounds on the work one call may ask for.
const (
	defaultK      = 10   // query/Similar results when none is given
	maxK          = 1000 // cap on query/Similar results
	maxIDs        = 1000 // cap on the ids of one get or query
	maxQueries    = 100  // cap on query embeddings or texts in one query
	getPage       = 300  // records per /get request when a query streams
	defaultBrowse = 100  // Browse page when Limit is 0
	maxBrowse     = 1000 // cap on a Browse page
)

// request is a parsed native query.
type request struct {
	op              string
	collection      string
	ids             []string
	where           json.RawMessage
	whereDocument   json.RawMessage
	limit           *int
	offset          int
	include         []string // nil = the op's default
	queryEmbeddings [][]float32
	queryTexts      []string
	nResults        int
	warnings        []string
}

// aliases maps Chroma's snake_case spellings (the Python client's) to the
// documented camelCase keys.
var aliases = map[string]string{
	"where_document":   "whereDocument",
	"n_results":        "nResults",
	"query_embeddings": "queryEmbeddings",
	"query_texts":      "queryTexts",
}

// allowedKeys lists the members each op takes besides op and collection.
var allowedKeys = map[string][]string{
	opGet:   {"ids", "where", "whereDocument", "limit", "offset", "include"},
	opQuery: {"ids", "where", "whereDocument", "nResults", "include", "queryEmbeddings", "queryTexts"},
	opCount: {},
}

// includeValues are the record parts a get or query may include.
var includeValues = map[string]bool{"documents": true, "metadatas": true, "embeddings": true, "uris": true, "distances": true}

// parseRequest parses a native query: one JSON object naming a
// "collection", with an optional "op" (get, query or count; query when
// queryEmbeddings or queryTexts are given, get otherwise) and the op's
// members. The op set is the allow-list: every op is a read, and nothing
// else is ever sent. Unknown or duplicate members, trailing text and wrong
// types are refused before a byte is sent. With readOnly every refusal is
// ErrReadOnly, so a text the guard cannot read is never let through (fail
// closed); otherwise a write op is ErrUnsupported (Query never writes:
// edits go through the Mutator) and a malformed text ErrSyntax.
func parseRequest(text string, readOnly bool) (request, error) {
	refuse := func(kind error, format string, args ...any) (request, error) {
		msg := fmt.Sprintf(format, args...)
		if readOnly && kind != driver.ErrReadOnly {
			kind, msg = driver.ErrReadOnly, "read-only: "+msg
		}
		return request{}, driver.Wrap(kind, Name, "query", errors.New(msg))
	}
	raw, err := topLevelObject(text)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	fields := map[string]json.RawMessage{}
	for k, v := range raw {
		key := k
		if a, ok := aliases[k]; ok {
			key = a
		}
		if _, dup := fields[key]; dup {
			return refuse(driver.ErrSyntax, "%q is given twice (once as %q)", key, k)
		}
		fields[key] = v
	}
	var req request
	if rawOp, ok := fields["op"]; ok {
		if err := strict(rawOp, &req.op); err != nil {
			return refuse(driver.ErrSyntax, `"op" must be a string`)
		}
		if _, known := allowedKeys[req.op]; !known {
			if readOnly {
				return refuse(driver.ErrReadOnly, "read-only: op %q is refused; only get, query and count run", req.op)
			}
			return refuse(driver.ErrUnsupported, "op %q is not a query op: only get, query and count run (edit records with the editor)", req.op)
		}
		delete(fields, "op")
	} else {
		req.op = opGet
		if _, ok := fields["queryEmbeddings"]; ok {
			req.op = opQuery
		}
		if _, ok := fields["queryTexts"]; ok {
			req.op = opQuery
		}
	}
	rawColl, ok := fields["collection"]
	if !ok {
		return refuse(driver.ErrSyntax, `the request needs a "collection"`)
	}
	if err := strict(rawColl, &req.collection); err != nil || req.collection == "" {
		return refuse(driver.ErrSyntax, `"collection" must be a non-empty string`)
	}
	delete(fields, "collection")
	for k := range fields {
		if !slices.Contains(allowedKeys[req.op], k) {
			return refuse(driver.ErrSyntax, "unknown member %q for a %s request (it takes %s)", k, req.op, strings.Join(append([]string{"collection"}, allowedKeys[req.op]...), ", "))
		}
	}
	if err := req.decode(fields); err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	return req, nil
}

// decode reads the op's members, already checked against allowedKeys.
func (r *request) decode(f map[string]json.RawMessage) error {
	if v, ok := f["ids"]; ok {
		if err := strict(v, &r.ids); err != nil || len(r.ids) == 0 {
			return errors.New(`"ids" must be a non-empty array of strings`)
		}
		for _, id := range r.ids {
			if id == "" {
				return errors.New(`"ids" must not hold an empty id`)
			}
		}
		if len(r.ids) > maxIDs {
			return fmt.Errorf(`"ids" holds at most %d ids`, maxIDs)
		}
	}
	for key, dst := range map[string]*json.RawMessage{"where": &r.where, "whereDocument": &r.whereDocument} {
		if v, ok := f[key]; ok {
			if t := bytes.TrimSpace(v); len(t) == 0 || t[0] != '{' {
				return fmt.Errorf("%q must be a JSON object", key)
			}
			*dst = v
		}
	}
	if v, ok := f["limit"]; ok {
		n, err := wholeNumber(v)
		if err != nil {
			return fmt.Errorf(`"limit" %v`, err)
		}
		r.limit = &n
	}
	if v, ok := f["offset"]; ok {
		n, err := wholeNumber(v)
		if err != nil {
			return fmt.Errorf(`"offset" %v`, err)
		}
		r.offset = n
	}
	if v, ok := f["include"]; ok {
		if err := strict(v, &r.include); err != nil || r.include == nil {
			return errors.New(`"include" must be an array of strings`)
		}
		for _, s := range r.include {
			if !includeValues[s] || (s == "distances" && r.op != opQuery) {
				return fmt.Errorf("cannot include %q in a %s request (documents, metadatas, embeddings, uris%s)", s, r.op, map[bool]string{true: ", distances"}[r.op == opQuery])
			}
		}
	}
	if r.op != opQuery {
		return nil
	}
	_, hasE := f["queryEmbeddings"]
	_, hasT := f["queryTexts"]
	switch {
	case hasE && hasT:
		return errors.New(`give "queryEmbeddings" or "queryTexts", not both`)
	case hasE:
		var es [][]float64
		if err := strict(f["queryEmbeddings"], &es); err != nil || len(es) == 0 {
			return errors.New(`"queryEmbeddings" must be a non-empty array of arrays of numbers`)
		}
		if len(es) > maxQueries {
			return fmt.Errorf("a query takes at most %d embeddings", maxQueries)
		}
		for _, e := range es {
			if len(e) == 0 {
				return errors.New(`"queryEmbeddings" holds an empty embedding`)
			}
			v := make([]float32, len(e))
			for i, x := range e {
				if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) > math.MaxFloat32 {
					return errors.New(`"queryEmbeddings" components must be finite float32 numbers`)
				}
				v[i] = float32(x)
			}
			r.queryEmbeddings = append(r.queryEmbeddings, v)
		}
	case hasT:
		if err := strict(f["queryTexts"], &r.queryTexts); err != nil || len(r.queryTexts) == 0 {
			return errors.New(`"queryTexts" must be a non-empty array of strings`)
		}
		if len(r.queryTexts) > maxQueries {
			return fmt.Errorf("a query takes at most %d texts", maxQueries)
		}
	default:
		return errors.New(`a query request needs "queryEmbeddings" or "queryTexts"`)
	}
	r.nResults = defaultK
	if v, ok := f["nResults"]; ok {
		n, err := wholeNumber(v)
		if err != nil {
			return fmt.Errorf(`"nResults" %v`, err)
		}
		switch {
		case n > maxK:
			r.warnings = append(r.warnings, fmt.Sprintf("nResults %d is above the driver's cap: returning at most %d records per query", n, maxK))
			r.nResults = maxK
		case n > 0:
			r.nResults = n
		}
	}
	return nil
}

// strict decodes raw into dst refusing null for strings and
// type mismatches.
func strict(raw json.RawMessage, dst any) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return errors.New("null")
	}
	return json.Unmarshal(raw, dst)
}

// wholeNumber reads a non-negative integer that fits an int32 (Chroma's
// limit and offset type).
func wholeNumber(raw json.RawMessage) (int, error) {
	var n json.Number
	if t := bytes.TrimSpace(raw); len(t) == 0 || (t[0] != '-' && (t[0] < '0' || t[0] > '9')) {
		return 0, errors.New("must be a whole number") // json.Number would accept "2"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, errors.New("must be a whole number")
	}
	i, err := n.Int64()
	if err != nil || i < 0 || i > math.MaxInt32 {
		return 0, errors.New("must be a whole number, 0 or more")
	}
	return int(i), nil
}

// topLevelObject decodes text as exactly one JSON object and returns its
// members. A duplicate key is an error: encoding/json would keep the last
// one, so {"op":"get","op":"delete"} must not be read as either.
func topLevelObject(text string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	tok, err := dec.Token()
	if err != nil {
		return nil, errors.New("the request must be a JSON object")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the request must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, _ := tok.(string)
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the request must be one JSON object, with nothing after it")
	}
	return fields, nil
}
