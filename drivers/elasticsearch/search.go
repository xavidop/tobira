package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// hit is one search hit.
type hit struct {
	Index  string          `json:"_index"`
	ID     string          `json:"_id"`
	Score  *float64        `json:"_score"`
	Source json.RawMessage `json:"_source"`
	Sort   json.RawMessage `json:"sort"`
}

// searchResponse is the part of a _search response the driver reads.
type searchResponse struct {
	PitID    string `json:"pit_id"`
	TimedOut bool   `json:"timed_out"`
	Shards   struct {
		Failed int `json:"failed"`
	} `json:"_shards"`
	Hits struct {
		Hits []hit `json:"hits"`
	} `json:"hits"`
	Aggregations json.RawMessage `json:"aggregations"`
}

func (r searchResponse) warnings() []string {
	var w []string
	if r.TimedOut {
		w = append(w, "the search timed out: the hits are partial")
	}
	if r.Shards.Failed > 0 {
		w = append(w, "some shards failed: the hits are partial")
	}
	return w
}

// mappingOf fetches the merged mapping of a search target. A missing
// concrete index is ErrNotFound.
func (c *conn) mappingOf(ctx context.Context, op, target string) (*mapping, []string, error) {
	data, err := c.do(ctx, op, http.MethodGet, pathOf(target, "_mapping"), nil, nil)
	if err != nil {
		return nil, nil, err
	}
	m, indices, err := parseMappings(data)
	if err != nil {
		return nil, nil, driver.Wrap(nil, c.name(), op, err)
	}
	return m, indices, nil
}

// singleIndex reports whether target names one index or alias (no
// pattern, no list): its hits can then be edited through Source.
func singleIndex(target string, indices []string) bool {
	return len(indices) == 1 && !strings.ContainsAny(target, "*,") && target != "_all"
}

// Point-in-time keep-alives: a streaming query reads its pages at once; a
// browse waits for the user to page.
const (
	queryKeepAlive  = "1m"
	browseKeepAlive = "5m"
)

// openPIT opens a point in time on target.
func (c *conn) openPIT(ctx context.Context, op, target, keepAlive string) (string, error) {
	params := url.Values{"keep_alive": {keepAlive}}
	p := pathOf(target, "_pit")
	if c.fl == flavorOpenSearch {
		p = pathOf(target, "_search", "point_in_time")
	}
	data, err := c.do(ctx, op, http.MethodPost, p, params, nil)
	if err != nil {
		return "", err
	}
	var r struct {
		ID    string `json:"id"`
		PitID string `json:"pit_id"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.ID+r.PitID == "" {
		return "", driver.Wrap(nil, c.name(), op, errors.New("the server returned no point-in-time id"))
	}
	c.trackPIT("", r.ID+r.PitID)
	return r.ID + r.PitID, nil
}

// trackPIT records that the connection holds point in time id instead of
// old (either may be empty), so Close can release what is still open.
func (c *conn) trackPIT(old, id string) {
	c.pitMu.Lock()
	defer c.pitMu.Unlock()
	if old != "" {
		delete(c.pits, old)
	}
	if id != "" {
		if c.pits == nil {
			c.pits = map[string]struct{}{}
		}
		c.pits[id] = struct{}{}
	}
}

// closeTimeout bounds releasing points in time, one by one or all on
// Close.
const closeTimeout = 5 * time.Second

// closePIT releases a point in time, best effort: an unclosed one expires
// after its keep-alive anyway. It runs on its own short context so a
// cancelled call still releases it.
func (c *conn) closePIT(id string) {
	if id == "" {
		return
	}
	c.trackPIT(id, "")
	if c.closed.Load() {
		return // Close released it
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	c.releasePIT(ctx, id)
}

// releasePIT sends the request that closes a point in time.
func (c *conn) releasePIT(ctx context.Context, id string) {
	var body []byte
	p := pathOf("_pit")
	if c.fl == flavorOpenSearch {
		body, _ = json.Marshal(map[string][]string{"pit_id": {id}})
		p = pathOf("_search", "point_in_time")
	} else {
		body, _ = json.Marshal(map[string]string{"id": id})
	}
	_, _ = c.send(ctx, "close", http.MethodDelete, p, nil, body, "application/json")
}

// releaseAllPITs closes every point in time the connection still holds,
// best effort, within closeTimeout in all.
func (c *conn) releaseAllPITs() {
	c.pitMu.Lock()
	ids := make([]string, 0, len(c.pits))
	for id := range c.pits {
		ids = append(ids, id)
	}
	c.pits = nil
	c.pitMu.Unlock()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	for _, id := range ids {
		c.releasePIT(ctx, id)
	}
}

// pitBody is the "pit" member of a search on a point in time.
func pitBody(id, keepAlive string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"id": id, "keep_alive": keepAlive})
	return b
}

// tiebreaker is the sort key that makes search_after positions unique on
// a point in time: Elasticsearch adds _shard_doc implicitly; OpenSearch
// has no _shard_doc and sorts by _id instead (which loads _id field data).
func (c *conn) tiebreaker() []any {
	if c.fl == flavorOpenSearch {
		return []any{map[string]any{"_id": "asc"}}
	}
	return nil
}

// indexOrder is the sort of a browse without sort keys: index order on
// Elasticsearch, _id order on OpenSearch.
func (c *conn) indexOrder() []any {
	if c.fl == flavorOpenSearch {
		return c.tiebreaker()
	}
	return []any{map[string]any{"_shard_doc": "asc"}}
}

// defaultSearchSize is the server's own default size, used for a single
// page whose request names none (one with "from", say).
const defaultSearchSize = 10

// pager fetches the pages of one search through a point in time with
// search_after, or a single page without one.
type pager struct {
	c         *conn
	op        string
	path      reqPath
	body      map[string]json.RawMessage
	pit       string
	keepAlive string
	pageSize  int
	want      int // hits to fetch in all; -1 = every hit
	fetched   int
	after     json.RawMessage
	done      bool
}

// next fetches the next page; done is set once no page follows. The point
// in time is closed when done.
func (p *pager) next(ctx context.Context) ([]hit, searchResponse, error) {
	var size int
	switch {
	case p.pit == "" && p.want < 0:
		size = defaultSearchSize // one page, as the server would
	case p.pit == "":
		size = p.want
	case p.want >= 0:
		size = min(p.pageSize, p.want-p.fetched)
	default:
		size = p.pageSize
	}
	body := make(map[string]json.RawMessage, len(p.body)+3)
	for k, v := range p.body {
		body[k] = v
	}
	body["size"], _ = json.Marshal(size)
	if p.pit != "" {
		body["pit"] = pitBody(p.pit, p.keepAlive)
	}
	if p.after != nil {
		body["search_after"] = p.after
	}
	data, _ := json.Marshal(body)
	raw, err := p.c.do(ctx, p.op, http.MethodPost, p.path, nil, data)
	if err != nil {
		p.finish()
		return nil, searchResponse{}, err
	}
	var resp searchResponse
	if err := readJSON(raw, &resp); err != nil {
		p.finish()
		return nil, searchResponse{}, driver.Wrap(nil, p.c.name(), p.op, errors.New("unexpected search response"))
	}
	if resp.PitID != "" && resp.PitID != p.pit {
		p.c.trackPIT(p.pit, resp.PitID)
		p.pit = resp.PitID
	}
	hits := resp.Hits.Hits
	p.fetched += len(hits)
	if len(hits) < size || (p.want >= 0 && p.fetched >= p.want) || p.pit == "" {
		p.finish()
	} else {
		p.after = hits[len(hits)-1].Sort
		if len(p.after) == 0 {
			p.finish()
		}
	}
	return hits, resp, nil
}

// finish marks the pager done and releases its point in time.
func (p *pager) finish() {
	p.done = true
	if p.pit != "" {
		p.c.closePIT(p.pit)
		p.pit = ""
	}
}

// columnSet is the columns of a hits result and how each one is filled.
type columnSet struct {
	cols    []driver.Column
	m       *mapping
	special map[string]int // _id, _index, _score, _distance -> column
	fields  map[string]int // source key -> column
}

// columnOptions picks the metadata columns of a hits result.
type columnOptions struct {
	score, index, distance bool
	source                 *driver.CollectionRef
	// filtered: the request filtered _source, so only fields seen on the
	// first page become columns.
	filtered bool
}

// newColumnSet builds the columns from the first page: _id, then _index,
// _distance and _score as asked, then the source fields in the order the
// first hits hold them, then the remaining mapped fields (not when the
// request filtered _source). Fields first seen on a later page are not
// shown.
func newColumnSet(hits []hit, m *mapping, o columnOptions) *columnSet {
	cs := &columnSet{m: m, special: map[string]int{}, fields: map[string]int{}}
	add := func(col driver.Column) int {
		cs.cols = append(cs.cols, col)
		return len(cs.cols) - 1
	}
	cs.special["_id"] = add(driver.Column{Name: "_id", NativeType: "_id", Type: driver.TypeString, Source: o.source, Key: o.source != nil})
	if o.index {
		cs.special["_index"] = add(driver.Column{Name: "_index", NativeType: "_index", Type: driver.TypeString})
	}
	if o.distance {
		cs.special["_distance"] = add(driver.Column{Name: "_distance", NativeType: "distance", Type: driver.TypeFloat})
	}
	if o.score {
		cs.special["_score"] = add(driver.Column{Name: "_score", NativeType: "_score", Type: driver.TypeFloat})
	}
	addField := func(key string) {
		if _, ok := cs.fields[key]; ok {
			return
		}
		if _, ok := cs.special[key]; ok {
			return // a source field named like a metadata column stays hidden
		}
		f := m.lookup(key)
		typ := fieldType(f)
		if f == nil {
			typ = driver.TypeUnknown
		}
		cs.fields[key] = add(driver.Column{Name: key, NativeType: nativeType(f), Type: typ, Source: o.source})
	}
	for _, h := range hits {
		kvs, err := orderedObject(h.Source)
		if err != nil {
			continue
		}
		for _, m := range kvs {
			addField(m.key)
		}
	}
	if !o.filtered && m != nil {
		for _, name := range m.order {
			addField(name)
		}
	}
	return cs
}

// row converts one hit.
func (cs *columnSet) row(h hit, distance func(hit) driver.Value) driver.Row {
	row := make(driver.Row, len(cs.cols))
	row[cs.special["_id"]] = driver.String(h.ID)
	if i, ok := cs.special["_index"]; ok {
		row[i] = driver.String(h.Index)
	}
	if i, ok := cs.special["_score"]; ok && h.Score != nil {
		row[i] = driver.Float(*h.Score)
	}
	if i, ok := cs.special["_distance"]; ok && distance != nil {
		row[i] = distance(h)
	}
	kvs, _ := orderedObject(h.Source)
	for _, m := range kvs {
		if i, ok := cs.fields[m.key]; ok {
			row[i] = convertValue(m.raw, cs.m.lookup(m.key))
		}
	}
	return row
}

// hitsResult streams the hits of a pager as rows, up to limit rows (0 = no
// limit), closing the point in time when it ends or is closed early.
type hitsResult struct {
	cs        *columnSet
	pg        *pager
	page      []hit
	pos       int
	limit     int
	returned  int
	distance  func(hit) driver.Value
	start     time.Time
	stats     driver.Stats
	finished  bool
	closed    bool
	extraRows []driver.Row // rows that are not hits (aggregations)
}

func (r *hitsResult) Columns() []driver.Column { return r.cs.cols }

func (r *hitsResult) Next(ctx context.Context) (driver.Row, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if r.closed || r.finished {
		return nil, false, nil
	}
	if len(r.extraRows) > 0 {
		row := r.extraRows[0]
		r.extraRows = r.extraRows[1:]
		return row, true, nil
	}
	for r.pos >= len(r.page) {
		if r.pg == nil || r.pg.done {
			r.end()
			return nil, false, nil
		}
		hits, resp, err := r.pg.next(ctx)
		if err != nil {
			r.end()
			return nil, false, err
		}
		r.stats.Warnings = append(r.stats.Warnings, resp.warnings()...)
		r.page, r.pos = hits, 0
	}
	if r.limit > 0 && r.returned >= r.limit {
		r.stats.Truncated = true
		r.end()
		return nil, false, nil
	}
	h := r.page[r.pos]
	r.pos++
	r.returned++
	return r.cs.row(h, r.distance), true, nil
}

func (r *hitsResult) end() {
	if r.finished {
		return
	}
	r.finished = true
	r.stats.Duration = time.Since(r.start)
	if r.pg != nil {
		r.pg.finish()
	}
}

func (r *hitsResult) Stats() driver.Stats { return r.stats }

func (r *hitsResult) Close() error {
	if !r.closed {
		r.closed = true
		r.end()
	}
	return nil
}
