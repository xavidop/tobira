package chroma

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/xavidop/tobira/driver"
)

// whereClause is a driver.Filter in Chroma's terms: an id selection, a
// metadata where clause and a document clause.
type whereClause struct {
	ids           []string
	idsSet        bool // ids restricts the records (possibly to none)
	where         any  // nil = none
	whereDocument any  // nil = none
}

// none reports an id selection that matches nothing.
func (w whereClause) none() bool { return w.idsSet && len(w.ids) == 0 }

// metadataOps maps the comparison ops to Chroma's where operators.
var metadataOps = map[driver.Op]string{
	driver.OpEq: "$eq", driver.OpNe: "$ne", driver.OpLt: "$lt", driver.OpLe: "$lte",
	driver.OpGt: "$gt", driver.OpGe: "$gte", driver.OpIn: "$in",
}

// buildWhere translates a filter, on the server's side throughout:
//   - id: eq and in select ids (several id conditions intersect);
//   - document: contains ($contains) and prefix (an anchored $regex);
//   - a metadata column: eq, ne, lt, le, gt, ge and in;
//   - Raw: a Chroma where object, ANDed with the conditions.
//
// Everything else is ErrUnsupported: Chroma has no substring match on
// metadata and no test for a missing key.
func buildWhere(f driver.Filter) (whereClause, error) {
	var (
		w     whereClause
		where []any
		docs  []any
	)
	refuse := func(format string, args ...any) (whereClause, error) {
		return whereClause{}, driver.Wrap(driver.ErrUnsupported, Name, "filter", fmt.Errorf(format, args...))
	}
	for _, c := range f.Conditions {
		switch c.Field {
		case "":
			return refuse("a condition needs a field")
		case idColumn:
			var ids []string
			switch c.Op {
			case driver.OpEq:
				id, err := idText(c.Value)
				if err != nil {
					return whereClause{}, err
				}
				ids = []string{id}
			case driver.OpIn:
				l, ok := c.Value.AsList()
				if !ok {
					return refuse("in needs a list")
				}
				for _, v := range l {
					id, err := idText(v)
					if err != nil {
						return whereClause{}, err
					}
					ids = append(ids, id)
				}
			default:
				return refuse("chroma selects records by id with eq or in only, not %s", c.Op)
			}
			if len(ids) > maxIDs {
				return refuse("an id filter takes at most %d ids", maxIDs)
			}
			if w.idsSet {
				ids = slices.DeleteFunc(ids, func(id string) bool { return !slices.Contains(w.ids, id) })
			}
			w.ids, w.idsSet = ids, true
		case documentColumn:
			s, ok := c.Value.AsText()
			if !ok || c.Value.Type() != driver.TypeString {
				return refuse("a document condition needs a string")
			}
			switch c.Op {
			case driver.OpContains:
				docs = append(docs, map[string]any{"$contains": s})
			case driver.OpPrefix:
				docs = append(docs, map[string]any{"$regex": "^" + regexp.QuoteMeta(s)})
			default:
				return refuse("chroma matches documents with contains or prefix only, not %s", c.Op)
			}
		case embeddingColumn:
			return refuse("cannot filter on the embedding: use similarity search")
		default:
			op, ok := metadataOps[c.Op]
			if !ok {
				return refuse("chroma cannot filter metadata with %s (use a raw where filter, such as {\"tags\": {\"$contains\": \"x\"}} for an array)", c.Op)
			}
			var val any
			if c.Op == driver.OpIn {
				l, ok := c.Value.AsList()
				if !ok || len(l) == 0 {
					return refuse("in needs a non-empty list")
				}
				vs := make([]any, len(l))
				for i, v := range l {
					x, err := filterValue(v)
					if err != nil {
						return whereClause{}, err
					}
					vs[i] = x
				}
				val = vs
			} else {
				x, err := filterValue(c.Value)
				if err != nil {
					return whereClause{}, err
				}
				val = x
			}
			where = append(where, map[string]any{metaKey(c.Field): map[string]any{op: val}})
		}
	}
	if f.Raw != "" {
		raw, err := rawWhere(f.Raw)
		if err != nil {
			return whereClause{}, err
		}
		if raw != nil {
			where = append(where, raw)
		}
	}
	w.where = and(where)
	w.whereDocument = and(docs)
	return w, nil
}

// and combines clauses: none is nil, one is itself, more are an $and.
func and(cs []any) any {
	switch len(cs) {
	case 0:
		return nil
	case 1:
		return cs[0]
	}
	return map[string]any{"$and": cs}
}

// filterValue converts a comparison value: a scalar metadata value.
func filterValue(v driver.Value) (any, error) {
	if v.IsNull() {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "filter", errors.New("chroma cannot compare with NULL"))
	}
	x, err := encodeMeta(v)
	if err != nil {
		return nil, err
	}
	if _, isList := x.([]any); isList {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "filter", errors.New("compare with one value, or use in"))
	}
	return x, nil
}

// rawWhere reads Filter.Raw: exactly one JSON object ({} is none).
func rawWhere(s string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "filter", errors.New("a raw filter is a chroma where object, such as {\"age\": {\"$gt\": 30}}"))
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "filter", errors.New("a raw filter is one JSON object, with nothing after it"))
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}
