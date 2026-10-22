package elasticsearch

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Browse page sizes.
const (
	defaultBrowse = 100
	maxBrowse     = 1000
)

// Browse pages through an index with search_after on a point in time, so
// pages stay consistent while documents change. The cursor carries the
// point in time and the last row's sort values; the point in time is
// closed on the last page, and expires five minutes after the last page
// read otherwise. Filters and sorts translate as buildQuery and buildSort
// describe; without a sort, rows come in index order (in _id order on
// OpenSearch).
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	if !validTarget(ref.Name) || strings.ContainsAny(ref.Name, "*,") {
		return nil, c.errorf(driver.ErrNotFound, op, "no index %q", ref.Name)
	}
	readOnly := opts.ReadOnly || c.readOnly
	if readOnly && !c.allowSystem && systemTarget(ref.Name) {
		return nil, errRefused(c.name(), true, driver.ErrReadOnly, "%s", systemReason(ref.Name))
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowse
	}
	limit = min(limit, maxBrowse)
	var cur browseCursor
	if opts.Cursor != "" {
		var err error
		if cur, err = decodeCursorAs(c.name(), opts.Cursor); err != nil {
			return nil, err
		}
	}
	m, indices, err := c.mappingOf(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	query, err := buildQueryAs(c.name(), op, opts.Filter, m, readOnly, c.allowSystem)
	if err != nil {
		return nil, err
	}
	sort, err := buildSortAs(c.name(), opts.Sort, m)
	if err != nil {
		return nil, err
	}
	if len(sort) == 0 {
		sort = c.indexOrder()
	} else {
		sort = append(sort, c.tiebreaker()...)
	}
	pit := cur.PIT
	if pit == "" {
		if pit, err = c.openPIT(ctx, op, ref.Name, browseKeepAlive); err != nil {
			return nil, err
		}
	}
	body := map[string]any{
		"size":             limit + 1,
		"query":            query,
		"sort":             sort,
		"pit":              pitBody(pit, browseKeepAlive),
		"track_total_hits": false,
	}
	if cur.After != nil {
		body["search_after"] = cur.After
	}
	data, err := json.Marshal(body)
	if err != nil {
		c.closePIT(pit)
		return nil, c.errorf(driver.ErrSyntax, op, "%v", err)
	}
	raw, err := c.do(ctx, op, "POST", pathOf("_search"), nil, data)
	if err != nil {
		if opts.Cursor == "" {
			c.closePIT(pit)
		}
		return nil, err
	}
	var resp searchResponse
	if err := readJSON(raw, &resp); err != nil {
		c.closePIT(pit)
		return nil, c.errorf(nil, op, "unexpected search response")
	}
	if resp.PitID != "" && resp.PitID != pit {
		c.trackPIT(pit, resp.PitID)
		pit = resp.PitID
	}
	hits := resp.Hits.Hits
	st := driver.Stats{RowsAffected: -1, Warnings: resp.warnings()}
	if len(hits) > limit && len(hits[limit-1].Sort) > 0 {
		next, err := browseCursor{PIT: pit, After: hits[limit-1].Sort}.encode()
		if err != nil {
			c.closePIT(pit)
			return nil, c.errorf(nil, op, "%v", err)
		}
		st.NextCursor = next
		hits = hits[:limit]
	} else {
		c.closePIT(pit)
		hits = hits[:min(len(hits), limit)]
	}
	src := &driver.CollectionRef{Namespace: defaultNamespace, Name: ref.Name}
	if !singleIndex(ref.Name, indices) {
		src = nil
	}
	cs := newColumnSet(hits, m, columnOptions{source: src, index: src == nil})
	rows := make([]driver.Row, len(hits))
	for i, h := range hits {
		rows[i] = cs.row(h, nil)
	}
	st.Duration = time.Since(start)
	return driver.NewSliceResultSet(cs.cols, rows, st), nil
}
