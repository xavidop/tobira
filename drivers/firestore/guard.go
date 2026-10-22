package firestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	fs "cloud.google.com/go/firestore"

	"github.com/xavidop/tobira/driver"
)

// docIDField is Firestore's name for the document id in a field path; a
// request names it "_id" (or "__name__").
const docIDField = fs.DocumentID

// readKeys is the allow-list of request keys. Each one only shapes a read:
// a request holding any other key is refused, so the format cannot carry a
// write however it is spelled.
var readKeys = map[string]bool{
	"document": true, "collection": true, "collectionGroup": true,
	"where": true, "orderBy": true, "limit": true, "offset": true, "select": true, "count": true,
}

// writeVerbs are keys that name a write. On a writable call they get the
// hint that edits go through the editor; they are refused like any other
// unknown key.
var writeVerbs = map[string]bool{
	"set": true, "update": true, "delete": true, "create": true, "add": true, "insert": true,
	"batch": true, "commit": true, "transaction": true, "writes": true, "data": true,
}

// whereOps are the filter operators a request may use.
var whereOps = map[string]bool{
	"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true,
	"array-contains": true, "array-contains-any": true, "in": true, "not-in": true,
}

// listOps take a list value.
var listOps = map[string]bool{"in": true, "not-in": true, "array-contains-any": true}

// querySpec is a parsed request.
type querySpec struct {
	document        string // a document path: read that one document
	collection      string // a collection path
	collectionGroup string // a collection id: every collection with that id
	where           []whereClause
	orderBy         []orderKey
	limit           int
	hasLimit        bool
	offset          int
	selectPaths     [][]string
	hasSelect       bool
	count           bool
}

// whereClause is one filter: a field path, an operator and a value in the
// SDK's form (docPath for a reference literal).
type whereClause struct {
	path  []string
	op    string
	value any
}

// orderKey is one sort key.
type orderKey struct {
	path []string
	desc bool
}

const writeHint = "Firestore queries only read: edit documents in the grid editor (the driver's Mutator) instead"

// parseQuery parses a request and is the read-only guard. A request is
// exactly one JSON object whose keys are all in readKeys, each at most
// once. Text that is not one object, trailing data, a repeated key or any
// other key is refused before anything is sent: as ErrReadOnly on a
// read-only call (the guard fails closed), and otherwise as ErrUnsupported
// for a write verb or ErrSyntax. A bad value under a known key is
// ErrSyntax either way: the request is then known to be a read.
func parseQuery(text string, readOnly bool) (querySpec, error) {
	shape := func(err error) (querySpec, error) {
		if readOnly {
			return querySpec{}, driver.Wrap(driver.ErrReadOnly, Name, "query", fmt.Errorf("refused on a read-only call: %w", err))
		}
		return querySpec{}, driver.Wrap(driver.ErrSyntax, Name, "query", err)
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return shape(errors.New(`a query is one JSON object, such as {"collection":"orders","limit":10}`))
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return shape(fmt.Errorf("invalid JSON: %w", err))
		}
		k, _ := kt.(string)
		if _, dup := fields[k]; dup {
			return shape(fmt.Errorf("key %q appears twice", k))
		}
		if !readKeys[k] {
			if !readOnly && writeVerbs[strings.ToLower(k)] {
				return querySpec{}, driver.Wrap(driver.ErrUnsupported, Name, "query", fmt.Errorf("%q: %s", k, writeHint))
			}
			return shape(fmt.Errorf("unknown key %q (known: collection, collectionGroup, document, where, orderBy, limit, offset, select, count)", k))
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return shape(fmt.Errorf("invalid JSON: %w", err))
		}
		fields[k] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return shape(errors.New("invalid JSON: the object is not closed"))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return shape(errors.New("trailing data after the query object"))
	}
	spec, err := specOf(fields)
	if err != nil {
		return querySpec{}, syntaxErr("query", err)
	}
	return spec, nil
}

// specOf reads the values of a request's keys.
func specOf(f map[string]json.RawMessage) (querySpec, error) {
	var s querySpec
	targets := 0
	for _, k := range []string{"document", "collection", "collectionGroup"} {
		raw, ok := f[k]
		if !ok {
			continue
		}
		targets++
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return s, fmt.Errorf("%s must be a string", k)
		}
		switch k {
		case "document":
			if err := checkPath(v, true); err != nil {
				return s, err
			}
			s.document = v
		case "collection":
			if err := checkPath(v, false); err != nil {
				return s, err
			}
			s.collection = v
		case "collectionGroup":
			if v == "" || strings.Contains(v, "/") {
				return s, errors.New("collectionGroup must be a collection id, without /")
			}
			s.collectionGroup = v
		}
	}
	if targets != 1 {
		return s, errors.New("name exactly one of collection, collectionGroup or document")
	}
	if s.document != "" && len(f) > 1 {
		return s, errors.New("a document read takes no other key")
	}
	if raw, ok := f["where"]; ok {
		var err error
		if s.where, err = parseWhere(raw); err != nil {
			return s, err
		}
	}
	if raw, ok := f["orderBy"]; ok {
		var err error
		if s.orderBy, err = parseOrderBy(raw); err != nil {
			return s, err
		}
	}
	if raw, ok := f["limit"]; ok {
		n, err := nonNegative(raw, "limit")
		if err != nil || n == 0 {
			return s, errors.New("limit must be a positive integer")
		}
		s.limit, s.hasLimit = n, true
	}
	if raw, ok := f["offset"]; ok {
		n, err := nonNegative(raw, "offset")
		if err != nil {
			return s, err
		}
		s.offset = n
	}
	if raw, ok := f["select"]; ok {
		var paths []string
		if err := json.Unmarshal(raw, &paths); err != nil || paths == nil {
			return s, errors.New("select must be a list of field paths")
		}
		s.hasSelect = true
		for _, p := range paths {
			fp, err := fieldPath(p)
			if err != nil {
				return s, err
			}
			s.selectPaths = append(s.selectPaths, fp)
		}
	}
	if raw, ok := f["count"]; ok {
		if err := json.Unmarshal(raw, &s.count); err != nil {
			return s, errors.New("count must be true or false")
		}
		if s.count && s.hasSelect {
			return s, errors.New("count takes no select")
		}
	}
	return s, nil
}

func nonNegative(raw json.RawMessage, key string) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	i, err := n.Int64()
	if err != nil || i < 0 || i > 1<<31-1 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return int(i), nil
}

// fieldPath parses a field path: dotted segments ("address.city"), each
// either plain or backquoted, so a name holding dots or other characters
// is one segment ("`a.b`.c"); inside backquotes a backslash escapes the
// next character. "_id" and "__name__" name the document id, and
// "_id_field" a stored field named _id.
func fieldPath(p string) ([]string, error) {
	switch p {
	case idColumn, docIDField:
		return []string{docIDField}, nil
	case idFieldColumn:
		return []string{idColumn}, nil
	}
	bad := fmt.Errorf("invalid field path %q (dotted names; backquote a name holding dots: `a.b`)", p)
	var segs []string
	for i := 0; ; {
		if i >= len(p) {
			return nil, bad // an empty path or segment
		}
		var seg string
		if p[i] == '`' {
			var b strings.Builder
			j, closed := i+1, false
			for j < len(p) {
				ch := p[j]
				if ch == '\\' {
					if j+1 >= len(p) {
						return nil, bad
					}
					b.WriteByte(p[j+1])
					j += 2
					continue
				}
				j++
				if ch == '`' {
					closed = true
					break
				}
				b.WriteByte(ch)
			}
			if !closed || b.Len() == 0 {
				return nil, bad
			}
			seg, i = b.String(), j
		} else {
			j := i
			for j < len(p) && p[j] != '.' {
				if p[j] == '`' {
					return nil, bad
				}
				j++
			}
			seg, i = p[i:j], j
			if seg == "" {
				return nil, bad
			}
		}
		segs = append(segs, seg)
		if i == len(p) {
			return segs, nil
		}
		if p[i] != '.' {
			return nil, bad
		}
		i++
	}
}

// parseWhere reads [[field, op, value], …].
func parseWhere(raw json.RawMessage) ([]whereClause, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, errors.New(`where must be a list of [field, op, value] triples, such as [["status","==","paid"]]`)
	}
	out := make([]whereClause, 0, len(items))
	for _, it := range items {
		var triple []json.RawMessage
		if err := json.Unmarshal(it, &triple); err != nil || len(triple) != 3 {
			return nil, errors.New("each where entry is a [field, op, value] triple")
		}
		var field, op string
		if json.Unmarshal(triple[0], &field) != nil || json.Unmarshal(triple[1], &op) != nil {
			return nil, errors.New("a where entry's field and op are strings")
		}
		path, err := fieldPath(field)
		if err != nil {
			return nil, err
		}
		if !whereOps[op] {
			return nil, fmt.Errorf("unknown operator %q (known: ==, !=, <, <=, >, >=, array-contains, array-contains-any, in, not-in)", op)
		}
		v, err := decodeJSON(triple[2], true)
		if err != nil {
			return nil, fmt.Errorf("where %s value: %w", field, err)
		}
		if _, isList := v.([]any); listOps[op] && !isList {
			return nil, fmt.Errorf("%s takes a list", op)
		}
		out = append(out, whereClause{path: path, op: op, value: v})
	}
	return out, nil
}

// parseOrderBy reads ["field", ["field", "desc"], …].
func parseOrderBy(raw json.RawMessage) ([]orderKey, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, errors.New(`orderBy must be a list of fields or [field, "asc"|"desc"] pairs`)
	}
	out := make([]orderKey, 0, len(items))
	for _, it := range items {
		var field, dir string
		var pair []string
		switch {
		case json.Unmarshal(it, &field) == nil:
		case json.Unmarshal(it, &pair) == nil && len(pair) == 2:
			field, dir = pair[0], pair[1]
		default:
			return nil, errors.New(`each orderBy entry is a field or a [field, "asc"|"desc"] pair`)
		}
		if dir != "" && dir != "asc" && dir != "desc" {
			return nil, fmt.Errorf("unknown direction %q (asc or desc)", dir)
		}
		path, err := fieldPath(field)
		if err != nil {
			return nil, err
		}
		out = append(out, orderKey{path: path, desc: dir == "desc"})
	}
	return out, nil
}
