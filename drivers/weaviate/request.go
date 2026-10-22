package weaviate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// The JSON request format:
//
//	{"collection": "Article",          required
//	 "fields": ["title", "n"],         properties to return (default: all)
//	 "where": {…},                     a Weaviate where filter, as in REST
//	 "sort": [{"path": ["n"], "order": "desc"}],
//	 "limit": 10, "offset": 0,
//	 "after": "<uuid>",                cursor: objects after this id
//	 "vectors": true,                  also return the vectors
//	 "nearVector" | "nearObject" | "nearText" | "bm25" | "hybrid": {…}}
//
// Values are written as in Weaviate's GraphQL arguments, in JSON. The
// request is translated to one GraphQL Get; it only reads.

// searchOps are the search operators a request may use (at most one).
var searchOps = []string{"nearVector", "nearObject", "nearText", "bm25", "hybrid"}

// jsonRequest is a parsed JSON request. where, sortRaw and searchArg are
// GraphQL literals, rendered and validated.
type jsonRequest struct {
	collection string
	fields     []string
	where      string
	sort       string
	sortRaw    json.RawMessage
	limit      *int
	offset     int
	after      string
	vectors    bool
	search     string
	searchArg  string
}

// parsedQuery is a JSON request or a raw GraphQL read.
type parsedQuery struct {
	req *jsonRequest
	gql *gqlDoc
}

// parseQuery parses a query text. With readOnly every refusal is
// ErrReadOnly, so a text the guard cannot read is never let through (fail
// closed); otherwise a write is ErrUnsupported (Query never writes: edit
// objects with the editor) and a malformed text ErrSyntax.
func parseQuery(text string, readOnly bool) (parsedQuery, error) {
	refuse := func(kind error, err error) (parsedQuery, error) {
		msg := err.Error()
		if readOnly {
			kind, msg = driver.ErrReadOnly, "read-only: "+msg
		}
		return parsedQuery{}, driver.Wrap(kind, Name, "query", errors.New(msg))
	}
	if !utf8.ValidString(text) {
		// Decoding would replace the bad bytes with U+FFFD: the query
		// would silently match other text.
		return refuse(driver.ErrSyntax, errors.New("the query is not valid UTF-8"))
	}
	trimmed := strings.TrimLeft(text, " \t\r\n\ufeff")
	switch {
	case trimmed == "":
		return refuse(driver.ErrSyntax, errors.New(`the query is empty: give a JSON request ({"collection": …}) or a GraphQL { Get { … } }`))
	case trimmed[0] == '[':
		return refuse(driver.ErrSyntax, errors.New("the request must be one JSON object"))
	case trimmed[0] == '{' && jsonObjectStart(trimmed):
		req, err := parseRequest(trimmed)
		if err != nil {
			return refuse(driver.ErrSyntax, err)
		}
		return parsedQuery{req: req}, nil
	}
	doc, err := parseGraphQL(text)
	if err != nil {
		var r *gqlRefusal
		if errors.As(err, &r) && r.mutation {
			return refuse(driver.ErrUnsupported, err)
		}
		return refuse(driver.ErrSyntax, err)
	}
	return parsedQuery{gql: doc}, nil
}

// jsonObjectStart reports whether s (starting with '{') opens a JSON
// object: its first member key is a string, or it is empty.
func jsonObjectStart(s string) bool {
	rest := strings.TrimLeft(s[1:], " \t\r\n")
	return rest == "" || rest[0] == '"' || rest[0] == '}'
}

// parseRequest parses a JSON request. Unknown keys, duplicate keys and
// anything after the object are refused.
func parseRequest(text string) (*jsonRequest, error) {
	fields, order, err := topLevelObject(text)
	if err != nil {
		return nil, err
	}
	r := &jsonRequest{}
	for _, k := range order {
		raw := fields[k]
		switch k {
		case "collection":
			if err := json.Unmarshal(raw, &r.collection); err != nil {
				return nil, errors.New(`"collection" must be a string`)
			}
			if !namePattern.MatchString(r.collection) {
				return nil, fmt.Errorf("%q is not a valid collection name", r.collection)
			}
		case "fields":
			if err := json.Unmarshal(raw, &r.fields); err != nil || r.fields == nil {
				return nil, errors.New(`"fields" must be a list of property names`)
			}
			for _, f := range r.fields {
				if !namePattern.MatchString(f) {
					return nil, fmt.Errorf("%q is not a valid property name", f)
				}
			}
		case "where":
			if r.where, err = renderLiteral(raw); err != nil {
				return nil, fmt.Errorf(`"where": %w`, err)
			}
			if !strings.HasPrefix(r.where, "{") {
				return nil, errors.New(`"where" must be an object`)
			}
		case "sort":
			if r.sort, err = renderLiteral(raw); err != nil {
				return nil, fmt.Errorf(`"sort": %w`, err)
			}
			if !strings.HasPrefix(r.sort, "{") && !strings.HasPrefix(r.sort, "[") {
				return nil, errors.New(`"sort" must be an object or a list`)
			}
			r.sortRaw = raw
		case "limit", "offset":
			n, err := nonNegative(raw)
			if err != nil {
				return nil, fmt.Errorf("%q %w", k, err)
			}
			if k == "limit" {
				r.limit = &n
			} else {
				r.offset = n
			}
		case "after":
			if err := json.Unmarshal(raw, &r.after); err != nil || !isUUID(r.after) {
				return nil, errors.New(`"after" must be an object id (a UUID)`)
			}
			r.after = strings.ToLower(r.after)
		case "vectors":
			if err := json.Unmarshal(raw, &r.vectors); err != nil {
				return nil, errors.New(`"vectors" must be true or false`)
			}
		case "nearVector", "nearObject", "nearText", "bm25", "hybrid":
			if r.search != "" {
				return nil, fmt.Errorf("give one search operator, not %s and %s", r.search, k)
			}
			r.search = k
			if r.searchArg, err = renderLiteral(raw); err != nil {
				return nil, fmt.Errorf("%q: %w", k, err)
			}
			if !strings.HasPrefix(r.searchArg, "{") {
				return nil, fmt.Errorf("%q must be an object", k)
			}
		default:
			return nil, fmt.Errorf("unknown key %q: a request takes collection, fields, where, sort, limit, offset, after, vectors and one of %s", k, strings.Join(searchOps, ", "))
		}
	}
	if r.collection == "" {
		return nil, errors.New(`the request needs a "collection"`)
	}
	if r.after != "" && (r.where != "" || r.sort != "" || r.offset > 0 || r.search != "") {
		return nil, errors.New(`"after" pages through a whole collection: it cannot be combined with where, sort, offset or a search`)
	}
	if r.search != "" && (r.sort != "") {
		return nil, fmt.Errorf("%s results are ranked: they cannot be sorted", r.search)
	}
	return r, nil
}

// nonNegative reads a JSON integer >= 0.
func nonNegative(raw json.RawMessage) (int, error) {
	n, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	if err != nil || n < 0 {
		return 0, errors.New("must be a non-negative integer")
	}
	return n, nil
}

// topLevelObject decodes text as exactly one JSON object and returns its
// members and their order. A duplicate key is an error: encoding/json
// would keep the last one, so {"collection":"A","collection":"B"} must not
// be read as either.
func topLevelObject(text string) (map[string]json.RawMessage, []string, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, errors.New("the request must be a JSON object")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("the request must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, _ := tok.(string)
		if _, dup := fields[key]; dup {
			return nil, nil, fmt.Errorf("duplicate key %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, fmt.Errorf("invalid JSON: %w", err)
		}
		fields[key] = raw
		order = append(order, key)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("the request must be one JSON object, with nothing after it")
	}
	return fields, order, nil
}

// enumKeys hold GraphQL enum values: their JSON strings are written bare.
var enumKeys = map[string]bool{"operator": true, "order": true, "fusionType": true}

// pathElement is a where/sort path element: a name, or len(name) for the
// length of a property.
var pathElement = regexp.MustCompile(`^(len\()?[_A-Za-z][_0-9A-Za-z]*\)?$`)

// renderLiteral writes a JSON value as a GraphQL input literal: object keys
// become names (validated), strings stay JSON-quoted (a valid GraphQL
// string), and the values of operator, order and fusionType become enum
// names (validated). Nothing in the input can close the literal early.
func renderLiteral(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", errors.New("the value is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if err := renderValue(dec, &b, ""); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errors.New("trailing data after the value")
	}
	return b.String(), nil
}

func renderValue(dec *json.Decoder, b *strings.Builder, key string) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if enumKeys[key] {
		s, ok := tok.(string)
		if !ok || !namePattern.MatchString(s) {
			return fmt.Errorf("%s must be a name, such as Equal or desc", key)
		}
		b.WriteString(s)
		return nil
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			b.WriteByte('{')
			seen := map[string]bool{}
			for first := true; dec.More(); first = false {
				kt, err := dec.Token()
				if err != nil {
					return fmt.Errorf("invalid JSON: %w", err)
				}
				k, _ := kt.(string)
				if !namePattern.MatchString(k) {
					return fmt.Errorf("%q is not a valid argument name", k)
				}
				if seen[k] {
					return fmt.Errorf("duplicate key %q", k)
				}
				seen[k] = true
				if !first {
					b.WriteString(", ")
				}
				b.WriteString(k + ": ")
				if err := renderValue(dec, b, k); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			b.WriteByte('}')
		case '[':
			b.WriteByte('[')
			for first := true; dec.More(); first = false {
				if !first {
					b.WriteString(", ")
				}
				elemKey := ""
				if key == "path" {
					elemKey = "path"
				}
				if err := renderValue(dec, b, elemKey); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			b.WriteByte(']')
		default:
			return fmt.Errorf("unexpected %q", t)
		}
	case string:
		if key == "path" && !pathElement.MatchString(t) {
			return fmt.Errorf("%q is not a valid path element", t)
		}
		q, _ := json.Marshal(t)
		b.Write(q)
	case json.Number:
		b.WriteString(t.String())
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	}
	return nil
}
