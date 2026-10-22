package weaviate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Bounds on the work one call may ask for.
const (
	defaultK      = 10    // search and Similar results when none is given
	maxK          = 1000  // cap on Similar results
	maxResults    = 10000 // Weaviate's default QUERY_MAXIMUM_RESULTS: offset + limit
	defaultBrowse = 100   // Browse page when Limit is 0
	maxBrowse     = 1000  // cap on a Browse page
)

// classFunc adapts a function to classInfo.
type classFunc func(string) ([]string, error)

func (f classFunc) vectorizers(class string) ([]string, error) { return f(class) }

// Query runs a JSON request or a raw GraphQL read (see parseQuery). It
// never writes.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	readOnly := c.readOnly || q.ReadOnly
	p, err := parseQuery(q.Text, readOnly)
	if err != nil {
		return nil, err
	}
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("weaviate requests take no parameters: put values in the query"))
	}
	if err := checkNamespace("query", q.Namespace); err != nil {
		return nil, err
	}
	if p.gql != nil {
		return c.queryGraphQL(ctx, p.gql, q.Text, readOnly, q.Limit, start)
	}
	return c.queryRequest(ctx, p.req, readOnly, q.Limit, start)
}

// checkNamespace accepts the default namespace or none.
func checkNamespace(op, ns string) error {
	if ns == "" || ns == defaultNamespace {
		return nil
	}
	return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("namespace %q does not exist: weaviate has only %q", ns, defaultNamespace))
}

// gqlArgs writes an argument list, "" when there is none.
func gqlArgs(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return "(" + strings.Join(kept, ", ") + ")"
}

// arg writes "name: value", or "" when value is empty.
func arg(name, value string) string {
	if value == "" {
		return ""
	}
	return name + ": " + value
}

// runGraphQL sends one GraphQL query and returns its data, decoded with
// numbers kept exact. A reply with errors is an error.
func (c *conn) runGraphQL(ctx context.Context, op, query string) (map[string]any, error) {
	body, err := c.rest(ctx, op, http.MethodPost, "/graphql", map[string]string{"query": query})
	if err != nil {
		return nil, err
	}
	x, err := decodeJSONText(body)
	if err != nil {
		return nil, driver.Wrap(nil, Name, op, fmt.Errorf("unexpected GraphQL reply: %w", err))
	}
	reply, _ := x.(map[string]any)
	if errs, _ := reply["errors"].([]any); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			m, _ := e.(map[string]any)
			if s, _ := m["message"].(string); s != "" {
				msgs = append(msgs, s)
			}
		}
		if len(msgs) == 0 {
			msgs = append(msgs, "the GraphQL query failed")
		}
		return nil, graphQLError(op, msgs)
	}
	data, _ := reply["data"].(map[string]any)
	return data, nil
}

// resultObjects extracts data[op][key] as a list of objects.
func resultObjects(data map[string]any, op, key string) []map[string]any {
	inner, _ := data[op].(map[string]any)
	items, _ := inner[key].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// getObjects runs one Get on a collection.
func (c *conn) getObjects(ctx context.Context, op string, s *collSchema, args, sel string) ([]map[string]any, error) {
	data, err := c.runGraphQL(ctx, op, "{ Get { "+s.class+args+" { "+sel+" } } }")
	if err != nil {
		return nil, err
	}
	return resultObjects(data, "Get", s.class), nil
}

// queryRequest runs a JSON request: one ranked search, a cursor stream
// over the whole collection, or offset pages of a filtered or sorted read.
func (c *conn) queryRequest(ctx context.Context, r *jsonRequest, readOnly bool, limit int, start time.Time) (driver.ResultSet, error) {
	const op = "query"
	s, err := c.getSchema(ctx, op, r.collection)
	if err != nil {
		return nil, err
	}
	if readOnly && (r.search == "nearText" || r.search == "hybrid") {
		if remote := remoteVectorizer(s.vectorizerNames()); remote != "" {
			return nil, driver.Wrap(driver.ErrReadOnly, Name, op, fmt.Errorf("read-only: %s on %s calls the %s vectorizer, a remote service; search by vector instead (Similar with ai.embedder)", r.search, s.class, remote))
		}
	}
	props, err := s.fieldProps(op, r.fields)
	if err != nil {
		return nil, err
	}
	spec := columnSpec{props: props, vectors: r.vectors}
	var extra []string
	switch r.search {
	case "nearVector", "nearObject", "nearText":
		spec.extra, extra = distanceColumn, []string{"distance"}
	case "bm25", "hybrid":
		spec.extra, extra = scoreColumn, []string{"score"}
	}
	sel := s.selectionOf(props, r.vectors, extra)
	cols := s.columns(spec)
	toRows := func(objs []map[string]any) ([]driver.Row, string) {
		rows := make([]driver.Row, len(objs))
		last := ""
		for i, o := range objs {
			rows[i], last = s.row(spec, o)
		}
		return rows, last
	}
	// want is how many rows a page may ask for: at most n, and no more
	// than the caller's limit plus one (which proves truncation).
	fetched := 0
	want := func(n int) int {
		if limit > 0 {
			n = min(n, limit+1-fetched)
		}
		return max(n, 1)
	}
	var warnings []string

	if r.search != "" {
		k := defaultK
		if r.limit != nil {
			k = *r.limit
		}
		if k > maxResults-r.offset {
			k = max(maxResults-r.offset, 0)
			warnings = append(warnings, fmt.Sprintf("the limit is above Weaviate's maximum of %d results: returning at most %d", maxResults, k))
		}
		var objs []map[string]any
		if k > 0 {
			args := gqlArgs(arg(r.search, r.searchArg), arg("where", r.where), arg("limit", fmt.Sprint(want(k))), offsetArg(r.offset))
			if objs, err = c.getObjects(ctx, op, s, args, sel); err != nil {
				return nil, err
			}
		}
		rows, _ := toRows(objs)
		cols[0].Type = idType(rows)
		rs := newPagedResultSet(cols, rows, nil, limit, start)
		rs.stats.Warnings = warnings
		return rs, nil
	}

	// stream: the whole collection in id order, through the cursor API.
	stream := r.where == "" && r.sort == "" && r.offset == 0
	total := -1 // the request's own cap; -1 = none
	if r.limit != nil {
		total = *r.limit
	}
	sortArg := r.sort
	if !stream && r.sort != "" && total < 0 {
		// Several offset pages: break ties by id so pages never overlap.
		sortArg = withIDTiebreak(r.sortRaw, r.sort)
	}
	after, offset := r.after, r.offset
	page := func(ctx context.Context) ([]driver.Row, bool, error) {
		n := c.pageSize
		if total >= 0 {
			n = min(n, total-fetched)
		}
		if !stream {
			if offset >= maxResults {
				warnings = append(warnings, fmt.Sprintf("stopped after %d objects, Weaviate's maximum for a filtered or sorted read: narrow the filter, or page with limit and offset", maxResults))
				return nil, true, nil
			}
			n = min(n, maxResults-offset)
		}
		n = want(n)
		var args string
		if stream {
			args = gqlArgs(arg("limit", fmt.Sprint(n)), afterArg(after))
		} else {
			args = gqlArgs(arg("where", r.where), arg("sort", sortArg), arg("limit", fmt.Sprint(n)), offsetArg(offset))
		}
		objs, err := c.getObjects(ctx, op, s, args, sel)
		if err != nil {
			return nil, false, err
		}
		rows, last := toRows(objs)
		fetched += len(rows)
		after, offset = last, offset+len(rows)
		done := len(rows) < n || total >= 0 && fetched >= total || limit > 0 && fetched > limit
		return rows, done, nil
	}
	if total == 0 {
		return newPagedResultSet(cols, nil, nil, limit, start), nil
	}
	first, done, err := page(ctx)
	if err != nil {
		return nil, err
	}
	cols[0].Type = idType(first)
	var fetch fetchFunc
	if !done {
		fetch = page
	}
	rs := newPagedResultSet(cols, first, fetch, limit, start)
	rs.warn = func() []string { return warnings }
	return rs, nil
}

func offsetArg(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("offset: %d", n)
}

func afterArg(uuid string) string {
	if uuid == "" {
		return ""
	}
	return fmt.Sprintf("after: %q", uuid)
}

// withIDTiebreak appends {path: ["_id"], order: asc} to a rendered sort
// unless it already sorts by id.
func withIDTiebreak(raw json.RawMessage, rendered string) string {
	if strings.Contains(string(raw), `"_id"`) {
		return rendered
	}
	const tie = `{path: ["_id"], order: asc}`
	if strings.HasPrefix(rendered, "[") {
		if rendered == "[]" {
			return "[" + tie + "]"
		}
		return rendered[:len(rendered)-1] + ", " + tie + "]"
	}
	return "[" + rendered + ", " + tie + "]"
}

// queryGraphQL runs a raw GraphQL read, after the guard.
func (c *conn) queryGraphQL(ctx context.Context, doc *gqlDoc, text string, readOnly bool, limit int, start time.Time) (driver.ResultSet, error) {
	const op = "query"
	s, err := c.getSchema(ctx, op, doc.class.name)
	if err != nil {
		return nil, err
	}
	if err := guardGraphQL(doc, readOnly, classFunc(func(string) ([]string, error) { return s.vectorizerNames(), nil })); err != nil {
		return nil, err
	}
	data, err := c.runGraphQL(ctx, op, text)
	if err != nil {
		return nil, err
	}
	objs := resultObjects(data, doc.op, doc.class.key())
	layout := rawLayout(doc, s)
	cols := make([]driver.Column, len(layout))
	for i, l := range layout {
		cols[i] = l.col
	}
	rows := make([]driver.Row, len(objs))
	for i, o := range objs {
		r := make(driver.Row, len(layout))
		for j, l := range layout {
			r[j] = l.value(o)
		}
		rows[i] = r
	}
	for j, l := range layout {
		if l.col.Name == idColumn && l.col.Key {
			cols[j].Type = idTypeAt(rows, j)
		}
	}
	return newPagedResultSet(cols, rows, nil, limit, start), nil
}

// rawColumn is one column of a raw GraphQL result and how to read it.
type rawColumn struct {
	col   driver.Column
	value func(obj map[string]any) driver.Value
}

// rawLayout lays out a raw result by its selection: one column per
// selected field, in order, with _additional's members spread out (id as
// "id", distance as "_distance", score as "_score", the others with a "_"
// prefix). Properties are typed by the schema.
func rawLayout(doc *gqlDoc, s *collSchema) []rawColumn {
	ref := &driver.CollectionRef{Namespace: defaultNamespace, Name: s.class}
	var out []rawColumn
	for _, f := range doc.class.sel {
		if f.name == "" {
			continue
		}
		key := f.key()
		if doc.op == "Get" && f.name == "_additional" {
			for _, a := range f.sel {
				if a.name == "" {
					continue
				}
				out = append(out, additionalColumn(key, a, ref))
			}
			continue
		}
		p, isProp := s.prop(f.name)
		if doc.op == "Get" && isProp {
			col := driver.Column{Name: key, NativeType: nativeType(p.dataType), Type: typeOf(p.dataType)}
			if f.alias == "" && !isReference(p.dataType) {
				col.Source = ref
			}
			out = append(out, rawColumn{col: col, value: func(o map[string]any) driver.Value { return decodeProp(p.dataType, p.nested, o[key]) }})
			continue
		}
		out = append(out, rawColumn{col: driver.Column{Name: key, Type: driver.TypeUnknown}, value: func(o map[string]any) driver.Value { return generic(o[key]) }})
	}
	return out
}

// additionalColumn reads one member of _additional.
func additionalColumn(addKey string, a *gqlField, ref *driver.CollectionRef) rawColumn {
	key := a.key()
	get := func(o map[string]any) any {
		add, _ := o[addKey].(map[string]any)
		return add[key]
	}
	switch a.name {
	case "id":
		name := idColumn
		if a.alias != "" {
			name = "_" + a.alias
		}
		return rawColumn{col: driver.Column{Name: name, NativeType: "uuid", Type: driver.TypeString, Source: ref, Key: a.alias == ""}, value: func(o map[string]any) driver.Value {
			s, ok := get(o).(string)
			if !ok {
				return driver.Null()
			}
			return idValue(s)
		}}
	case "distance", "certainty", "score":
		return rawColumn{col: driver.Column{Name: "_" + key, NativeType: "number", Type: driver.TypeFloat}, value: func(o map[string]any) driver.Value { return numberValue(get(o), nil) }}
	case "creationTimeUnix", "lastUpdateTimeUnix":
		return rawColumn{col: driver.Column{Name: "_" + key, NativeType: "unix ms", Type: driver.TypeInt}, value: func(o map[string]any) driver.Value {
			if s, ok := get(o).(string); ok && digits.MatchString(s) {
				return generic(json.Number(s))
			}
			return generic(get(o))
		}}
	case "vector":
		return rawColumn{col: driver.Column{Name: "_" + key, NativeType: "vector", Type: driver.TypeVector}, value: func(o map[string]any) driver.Value { return vectorValue(get(o)) }}
	case "vectors":
		return rawColumn{col: driver.Column{Name: "_" + key, Type: driver.TypeObject}, value: func(o map[string]any) driver.Value {
			m, _ := get(o).(map[string]any)
			if m == nil {
				return driver.Null()
			}
			var members []driver.Member
			for _, sub := range a.sel {
				members = append(members, driver.Member{Key: sub.key(), Value: vectorValue(m[sub.key()])})
			}
			return driver.ObjectValue(members...)
		}}
	}
	return rawColumn{col: driver.Column{Name: "_" + key, Type: driver.TypeUnknown}, value: func(o map[string]any) driver.Value { return generic(get(o)) }}
}

// idTypeAt is idType for column j.
func idTypeAt(rows []driver.Row, j int) driver.Type {
	col := make([]driver.Row, len(rows))
	for i, r := range rows {
		col[i] = driver.Row{r[j]}
	}
	return idType(col)
}
