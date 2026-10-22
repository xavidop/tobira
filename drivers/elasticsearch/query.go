package elasticsearch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/xavidop/tobira/driver"
)

// singlePageKeys are search options that do not combine with paging
// through a point in time; a request using one runs as a single page.
var singlePageKeys = []string{"from", "search_after", "pit", "collapse", "rescore", "knn", "suggest"}

// Query runs a native request; see request. The search form streams its
// hits as rows: every hit when the body has no "size" (page by page
// through a point in time), at most size hits otherwise, and never more
// than q.Limit (with Stats.Truncated set when more existed). The raw form
// returns the response: hits as rows, a JSON array as rows of its
// objects, any other object as one row of its members, plain text as
// lines.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, c.name(), op, errors.New("the query DSL has no positional parameters"))
	}
	req, err := parseRequestAs(c.name(), q.Text, q.ReadOnly || c.readOnly, c.allowSystem)
	if err != nil {
		return nil, err
	}
	if req.raw {
		return c.rawQuery(ctx, req, q.Limit)
	}
	return c.searchQuery(ctx, req, q.Limit)
}

// searchQuery runs the search form.
func (c *conn) searchQuery(ctx context.Context, req request, limit int) (driver.ResultSet, error) {
	const op = "query"
	start := time.Now()
	m, indices, err := c.mappingOf(ctx, op, req.index)
	if err != nil {
		return nil, err
	}
	body := req.body
	userSize := -1
	if raw, ok := body["size"]; ok {
		n, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
		if err != nil || n < 0 {
			return nil, driver.Wrap(driver.ErrSyntax, c.name(), op, errors.New(`"size" must be a non-negative integer`))
		}
		userSize = n
		delete(body, "size")
	}
	_, hasAggs := body["aggs"]
	if _, ok := body["aggregations"]; ok {
		hasAggs = true
	}
	want := userSize
	if limit > 0 && (want < 0 || want > limit+1) {
		want = limit + 1
	}
	single := want >= 0 && want <= c.pageSize
	for _, k := range singlePageKeys {
		if _, ok := body[k]; ok {
			single = true
		}
	}
	src := (*driver.CollectionRef)(nil)
	if singleIndex(req.index, indices) {
		src = &driver.CollectionRef{Namespace: defaultNamespace, Name: req.index}
	}
	_, filtered := body["_source"]
	opts := columnOptions{score: true, index: src == nil && (len(indices) > 1 || req.index != indices0(indices)), source: src, filtered: filtered}
	pg := &pager{c: c, op: op, path: pathOf(req.index, "_search"), body: body, pageSize: c.pageSize, want: want}
	if !single {
		if _, ok := body["track_total_hits"]; !ok {
			body["track_total_hits"] = json.RawMessage("false")
		}
		if _, ok := body["sort"]; !ok {
			body["sort"] = json.RawMessage(`["_score"]`)
		}
		if tb := c.tiebreaker(); tb != nil {
			body["sort"] = appendSort(body["sort"], tb)
		}
		pit, err := c.openPIT(ctx, op, req.index, queryKeepAlive)
		if err != nil {
			return nil, err
		}
		pg.pit, pg.keepAlive, pg.path = pit, queryKeepAlive, pathOf("_search")
	}
	hits, resp, err := pg.next(ctx)
	if err != nil {
		return nil, err
	}
	r := &hitsResult{pg: pg, page: hits, limit: limit, start: start}
	r.stats.RowsAffected = -1
	r.stats.Warnings = resp.warnings()
	if hasAggs && userSize == 0 {
		cs, row := aggregationRow(resp.Aggregations)
		r.cs, r.extraRows = cs, []driver.Row{row}
		return r, nil
	}
	if hasAggs {
		r.stats.Notices = append(r.stats.Notices, `aggregations are returned only with "size": 0`)
	}
	r.cs = newColumnSet(hits, m, opts)
	return r, nil
}

func indices0(indices []string) string {
	if len(indices) == 0 {
		return ""
	}
	return indices[0]
}

// appendSort adds keys to a request's sort, which may be one key or a
// list.
func appendSort(sort json.RawMessage, keys []any) json.RawMessage {
	var list []any
	if err := json.Unmarshal(sort, &list); err != nil {
		var one any
		if json.Unmarshal(sort, &one) == nil {
			list = []any{one}
		}
	}
	b, _ := json.Marshal(append(list, keys...))
	return b
}

// aggregationRow is the aggregations of a size-0 search as one row, a
// column per aggregation.
func aggregationRow(raw json.RawMessage) (*columnSet, driver.Row) {
	cs := &columnSet{special: map[string]int{}, fields: map[string]int{}}
	kvs, _ := orderedObject(raw)
	row := make(driver.Row, 0, len(kvs))
	for _, m := range kvs {
		cs.cols = append(cs.cols, driver.Column{Name: m.key, NativeType: "aggregation", Type: driver.TypeObject})
		row = append(row, genericValue(m.raw))
	}
	return cs, row
}

// rawQuery runs the raw form and shapes its response.
func (c *conn) rawQuery(ctx context.Context, req request, limit int) (driver.ResultSet, error) {
	const op = "query"
	start := time.Now()
	ct := "application/json"
	if req.ndjson {
		ct = "application/x-ndjson"
	}
	data, err := c.doType(ctx, op, req.method, rawPath(req.path), req.params, req.rawBody, ct)
	if err != nil {
		return nil, err
	}
	cols, rows, affected := shapeResponse(data)
	st := driver.Stats{RowsAffected: affected}
	if limit > 0 && len(rows) > limit {
		rows, st.Truncated = rows[:limit], true
	}
	st.Duration = time.Since(start)
	return driver.NewSliceResultSet(cols, rows, st), nil
}

// shapeResponse turns a raw response into rows; see Query. affected is
// the count of a by-query or bulk write, or -1.
func shapeResponse(data []byte) ([]driver.Column, []driver.Row, int64) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil, -1
	}
	if !json.Valid(data) {
		cols := []driver.Column{{Name: "line", Type: driver.TypeString}}
		var rows []driver.Row
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64*1024), maxResponse)
		for sc.Scan() {
			rows = append(rows, driver.Row{driver.String(sc.Text())})
		}
		return cols, rows, -1
	}
	switch data[0] {
	case '[':
		var items []json.RawMessage
		_ = json.Unmarshal(data, &items)
		return objectRows(items)
	case '{':
		var resp searchResponse
		var probe struct {
			Hits *struct {
				Hits []json.RawMessage `json:"hits"`
			} `json:"hits"`
		}
		if json.Unmarshal(data, &probe) == nil && probe.Hits != nil && probe.Hits.Hits != nil && readJSON(data, &resp) == nil {
			cs := newColumnSet(resp.Hits.Hits, nil, columnOptions{score: true, index: true})
			rows := make([]driver.Row, len(resp.Hits.Hits))
			for i, h := range resp.Hits.Hits {
				rows[i] = cs.row(h, nil)
			}
			return cs.cols, rows, -1
		}
		cols, rows, _ := objectRows([]json.RawMessage{data})
		return cols, rows, writeCount(data)
	}
	return []driver.Column{{Name: "value", Type: driver.TypeUnknown}}, []driver.Row{{genericValue(data)}}, -1
}

// writeCount sums the counts a by-query or bulk-style response reports.
func writeCount(data []byte) int64 {
	var r struct {
		Deleted *int64 `json:"deleted"`
		Updated *int64 `json:"updated"`
		Created *int64 `json:"created"`
	}
	if json.Unmarshal(data, &r) != nil || (r.Deleted == nil && r.Updated == nil && r.Created == nil) {
		return -1
	}
	var n int64
	for _, p := range []*int64{r.Deleted, r.Updated, r.Created} {
		if p != nil {
			n += *p
		}
	}
	return n
}

// objectRows turns JSON objects into rows, a column per key in order of
// first appearance, typed by the first non-null value (TypeUnknown when
// values differ). Non-object items fill a "value" column.
func objectRows(items []json.RawMessage) ([]driver.Column, []driver.Row, int64) {
	var cols []driver.Column
	idx := map[string]int{}
	var parsed [][]kv
	for _, it := range items {
		kvs, err := orderedObject(it)
		if err != nil {
			kvs = []kv{{"value", it}}
		}
		parsed = append(parsed, kvs)
		for _, m := range kvs {
			if _, ok := idx[m.key]; !ok {
				idx[m.key] = len(cols)
				cols = append(cols, driver.Column{Name: m.key, Type: driver.TypeNull})
			}
		}
	}
	rows := make([]driver.Row, len(parsed))
	for r, kvs := range parsed {
		row := make(driver.Row, len(cols))
		for _, m := range kvs {
			i := idx[m.key]
			v := genericValue(m.raw)
			row[i] = v
			switch t := cols[i].Type; {
			case v.IsNull():
			case t == driver.TypeNull:
				cols[i].Type = v.Type()
			case t != v.Type():
				cols[i].Type = driver.TypeUnknown
			}
		}
		rows[r] = row
	}
	for i := range cols {
		if cols[i].Type == driver.TypeNull {
			cols[i].Type = driver.TypeUnknown
		}
	}
	return cols, rows, -1
}

// errorf builds an error of kind for op.
func (c *conn) errorf(kind error, op, format string, args ...any) error {
	return driver.Wrap(kind, c.name(), op, fmt.Errorf(format, args...))
}
