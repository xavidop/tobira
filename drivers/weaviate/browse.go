package weaviate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// browseCursor is the position of a Browse page: after an object id ("a:"
// + UUID) for an unfiltered, unsorted browse, which reads the collection
// in id order through Weaviate's cursor API; or an offset ("o:" + n) for a
// filtered or sorted one, which Weaviate pages with offset and limit (up
// to its maximum of 10000 results).
type browseCursor struct {
	after  string
	offset int
}

func (c browseCursor) encode() string {
	switch {
	case c.after != "":
		return "a:" + c.after
	case c.offset > 0:
		return "o:" + strconv.Itoa(c.offset)
	}
	return ""
}

func decodeCursor(s string) (browseCursor, error) {
	bad := func() (browseCursor, error) {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor"))
	}
	if s == "" {
		return browseCursor{}, nil
	}
	kind, val, ok := strings.Cut(s, ":")
	if !ok {
		return bad()
	}
	switch kind {
	case "a":
		if !isUUID(val) {
			return bad()
		}
		return browseCursor{after: strings.ToLower(val)}, nil
	case "o":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 || !digits.MatchString(val) {
			return bad()
		}
		return browseCursor{offset: n}, nil
	}
	return bad()
}

// sortable lists the element types Weaviate sorts by.
var sortable = map[string]bool{"text": true, "string": true, "uuid": true, "int": true, "number": true, "boolean": true, "date": true}

// browseSort writes the sort argument of a sorted browse: the keys, then
// the id ascending to break ties, so offset pages never overlap. "" when
// there are no keys.
func browseSort(keys []driver.SortKey, s *collSchema) (string, error) {
	if len(keys) == 0 {
		return "", nil
	}
	dir := func(desc bool) string {
		if desc {
			return "desc"
		}
		return "asc"
	}
	parts := make([]string, 0, len(keys)+1)
	byID := false
	for _, k := range keys {
		path := k.Field
		switch k.Field {
		case idColumn:
			path, byID = "_id", true
		default:
			if _, ok := s.vectorByColumn(k.Field); ok {
				return "", driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("cannot sort by vector %q", k.Field))
			}
			p, ok := s.prop(k.Field)
			if !ok {
				return "", driver.Wrap(driver.ErrNotFound, Name, "browse", fmt.Errorf("collection %s has no property %q", s.class, k.Field))
			}
			t, arr := baseType(p.dataType)
			if arr || !sortable[t] || isReference(p.dataType) {
				return "", driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("cannot sort by %s property %q", nativeType(p.dataType), k.Field))
			}
		}
		parts = append(parts, fmt.Sprintf(`{path: [%q], order: %s}`, path, dir(k.Desc)))
	}
	if !byID {
		parts = append(parts, `{path: ["_id"], order: asc}`)
	}
	return "[" + strings.Join(parts, ", ") + "]", nil
}

// Browse returns one page of objects with their properties and vectors.
// Unfiltered and unsorted, it follows the cursor API in id order. Filtered
// or sorted (by properties or the id), it pages by offset, the id breaking
// ties; Weaviate serves at most 10000 results that way. A NULL sorts
// first ascending and last descending.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	start := time.Now()
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowse
	}
	limit = min(limit, maxBrowse)
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	s, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	where, none, err := buildWhere(opts.Filter, s)
	if err != nil {
		return nil, err
	}
	sort, err := browseSort(opts.Sort, s)
	if err != nil {
		return nil, err
	}
	spec := columnSpec{props: s.readable(), vectors: true}
	cols := s.columns(spec)
	st := driver.Stats{RowsAffected: -1}
	if none {
		st.Duration = time.Since(start)
		return driver.NewSliceResultSet(cols, nil, st), nil
	}
	byCursor := where == "" && sort == ""
	if byCursor && cur.offset > 0 || !byCursor && cur.after != "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the cursor belongs to a browse with another filter or sort"))
	}
	var args string
	if byCursor {
		args = gqlArgs(arg("limit", strconv.Itoa(limit+1)), afterArg(cur.after))
	} else {
		if cur.offset+limit+1 > maxResults {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("weaviate serves at most %d filtered or sorted results: narrow the filter", maxResults))
		}
		if sort == "" {
			sort = `[{path: ["_id"], order: asc}]`
		}
		args = gqlArgs(arg("where", where), arg("sort", sort), arg("limit", strconv.Itoa(limit+1)), offsetArg(cur.offset))
	}
	objs, err := c.getObjects(ctx, op, s, args, s.selectionOf(spec.props, true, nil))
	if err != nil {
		return nil, err
	}
	rows := make([]driver.Row, 0, len(objs))
	var uuids []string
	for _, o := range objs {
		r, uuid := s.row(spec, o)
		rows = append(rows, r)
		uuids = append(uuids, uuid)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		if byCursor {
			st.NextCursor = browseCursor{after: strings.ToLower(uuids[limit-1])}.encode()
		} else {
			st.NextCursor = browseCursor{offset: cur.offset + limit}.encode()
		}
	}
	cols[0].Type = idType(rows)
	st.Duration = time.Since(start)
	return driver.NewSliceResultSet(cols, rows, st), nil
}
