package cosmos

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

const (
	// prefetchMax caps how many items a result reads before it fixes its
	// columns; so does maxResultBytes. Fields that only later items hold
	// have no column; the result reports how many were left out in
	// Stats.Warnings.
	prefetchMax = 1000
	// maxPageItems is the most items asked for in one page.
	maxPageItems = 1000
)

// maxResultBytes is the byte budget for items held in memory at once: a
// result's prefetch, a Browse page and Describe's sample each stop reading
// once it is reached (a variable so tests can lower it).
var maxResultBytes = 32 << 20

// budgetText spells maxResultBytes for messages.
func budgetText() string {
	if maxResultBytes%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", maxResultBytes>>20)
	}
	return fmt.Sprintf("%d bytes", maxResultBytes)
}

// Query runs one Cosmos SQL SELECT (see parseRequest) in Query.Namespace,
// or the connection's default database. Its container is the FROM name or
// the envelope's container; an envelope's partitionKey scopes it to one
// logical partition, otherwise it runs across partitions. Query.Params bind
// as @p1, @p2…. Items stream as rows: an object's fields as columns, any
// other item in one "value" column. The read-only guard applies on a
// read-only call or connection.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	req, err := parseRequest(q.Text, q.Params, q.ReadOnly || c.readOnly)
	if err != nil {
		return nil, err
	}
	db, err := c.database(op, q.Namespace)
	if err != nil {
		return nil, err
	}
	cc, err := c.container(op, db, req.container)
	if err != nil {
		return nil, err
	}
	pk := azcosmos.NewPartitionKey()
	if req.partitionKey != nil {
		if pk, err = partitionKeyFromJSON(req.partitionKey); err != nil {
			return nil, err
		}
	}
	opts := &azcosmos.QueryOptions{QueryParameters: req.params, PageSizeHint: pageHint(q.Limit)}
	var src *sourceInfo
	if isStarSelect(req.sql) {
		if props, err := c.containerProps(ctx, db, req.container, cc); err == nil {
			src = &sourceInfo{ref: driver.CollectionRef{Namespace: db, Name: req.container}, keys: keyFields(props)}
		} else if ctx.Err() != nil {
			return nil, mapError(ctx, op, err)
		}
	}
	return newDocResult(ctx, op, newPagerSource(cc.NewQueryItemsPager(req.sql, pk, opts)), q.Limit, start, src)
}

// pageHint is the page size to ask for under a row limit: one past it, so
// the first page tells whether the result was truncated.
func pageHint(limit int) int32 {
	if limit <= 0 || limit+1 > maxPageItems {
		return maxPageItems
	}
	return int32(limit + 1)
}

// isStarSelect reports whether sql selects whole items (SELECT [DISTINCT]
// [TOP n] * …), whose rows can be edited in place.
func isStarSelect(sql string) bool {
	toks, err := lex(sql)
	if err != nil {
		return false
	}
	i := 1
	if i < len(toks) && toks[i].isWord("distinct") {
		i++
	}
	if i+1 < len(toks) && toks[i].isWord("top") {
		i += 2
	}
	return i < len(toks) && toks[i].text == "*"
}

// sourceInfo marks a result's columns as editable: the collection they
// come from and its key fields (id and the partition key's).
type sourceInfo struct {
	ref  driver.CollectionRef
	keys []string
}

// pageSource yields pages of raw items.
type pageSource interface {
	// next returns the next page's items and the page's request charge;
	// done reports that no page follows.
	next(ctx context.Context) (items [][]byte, charge float64, done bool, err error)
}

// pagerSource adapts the SDK's query pager.
type pagerSource struct {
	more  func() bool
	fetch func(ctx context.Context) (azcosmos.QueryItemsResponse, error)
}

func newPagerSource(p interface {
	More() bool
	NextPage(ctx context.Context) (azcosmos.QueryItemsResponse, error)
}) *pagerSource {
	return &pagerSource{more: p.More, fetch: p.NextPage}
}

func (s *pagerSource) next(ctx context.Context) ([][]byte, float64, bool, error) {
	if !s.more() {
		return nil, 0, true, nil
	}
	page, err := s.fetch(ctx)
	if err != nil {
		return nil, 0, true, err
	}
	return page.Items, float64(page.RequestCharge), !s.more(), nil
}

// docResult is a ResultSet over query items: those already read (buf),
// then the source's later pages. Its columns come from the items read
// before the first row is returned, up to prefetchMax (or the limit). It is
// safe for a single consumer.
type docResult struct {
	op        string
	src       pageSource
	srcDone   bool
	buf       []driver.Value
	cols      []driver.Column
	index     map[string]int
	valueMode bool
	limit     int
	returned  int
	omitted   int
	window    int
	budget    bool
	charge    float64
	start     time.Time
	stats     driver.Stats
	done      bool
}

func newDocResult(ctx context.Context, op string, src pageSource, limit int, start time.Time, source *sourceInfo) (driver.ResultSet, error) {
	r := &docResult{op: op, src: src, limit: limit, start: start, stats: driver.Stats{RowsAffected: -1}}
	want := prefetchMax
	if limit > 0 && limit+1 < want {
		want = limit + 1
	}
	// The columns come from the first items that fit in maxResultBytes (at
	// least one): a page arrives whole, so the budget is counted per item.
	held, window := 0, 0
	for !r.srcDone && len(r.buf) < want && !r.budget {
		sizes, err := r.fill(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range sizes {
			if held += n; held > maxResultBytes && window > 0 {
				r.budget = true
				break
			}
			window++
		}
	}
	window = min(window, want)
	if limit > 0 {
		window = min(window, limit)
	}
	shown := r.buf[:window]
	r.window = len(shown)
	r.cols = inferColumns(shown)
	_, objects := objectsOf(shown)
	r.valueMode = !objects
	r.index = columnIndex(r.cols)
	if source != nil && !r.valueMode {
		for k := range r.cols {
			ref := source.ref
			r.cols[k].Source = &ref
		}
		for _, key := range source.keys {
			if i, ok := r.index[key]; ok {
				r.cols[i].Key = true
			}
		}
	}
	return r, nil
}

// fill reads one page into buf and returns the size in bytes of each item.
func (r *docResult) fill(ctx context.Context) ([]int, error) {
	items, charge, done, err := r.src.next(ctx)
	r.srcDone = done
	r.charge += charge
	if err != nil {
		return nil, mapError(ctx, r.op, err)
	}
	sizes := make([]int, 0, len(items))
	for _, raw := range items {
		v, err := decodeValue(raw)
		if err != nil {
			return nil, driver.Wrap(nil, Name, r.op, fmt.Errorf("cannot decode an item: %w", err))
		}
		r.buf = append(r.buf, v)
		sizes = append(sizes, len(raw))
	}
	return sizes, nil
}

func (r *docResult) Columns() []driver.Column { return r.cols }

func (r *docResult) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, mapError(ctx, r.op, err)
	}
	if r.limit > 0 && r.returned >= r.limit {
		for len(r.buf) == 0 && !r.srcDone {
			if _, err := r.fill(ctx); err != nil {
				r.finish()
				return nil, false, err
			}
		}
		r.stats.Truncated = len(r.buf) > 0
		r.finish()
		return nil, false, nil
	}
	for len(r.buf) == 0 {
		if r.srcDone {
			r.finish()
			return nil, false, nil
		}
		if _, err := r.fill(ctx); err != nil {
			r.finish()
			return nil, false, err
		}
	}
	item := r.buf[0]
	r.buf = r.buf[1:]
	row, omitted := rowOf(item, r.index, len(r.cols), r.valueMode)
	r.omitted += omitted
	r.returned++
	return row, true, nil
}

// finish ends the result and fixes the stats.
func (r *docResult) finish() {
	if r.done {
		return
	}
	r.done = true
	r.buf = nil
	r.stats.Duration = time.Since(r.start)
	if r.charge > 0 {
		r.stats.Notices = append(r.stats.Notices, "request charge: "+strconv.FormatFloat(r.charge, 'f', 2, 64)+" RU")
	}
	if r.omitted > 0 {
		why := ""
		if r.budget {
			why = " (the " + budgetText() + " read-ahead limit)"
		}
		r.stats.Warnings = append(r.stats.Warnings, fmt.Sprintf(
			"%d fields of later items have no column: columns come from the first %d items%s; select the fields you need to see them",
			r.omitted, r.window, why))
	}
}

func (r *docResult) Stats() driver.Stats {
	st := r.stats
	if !r.done {
		st.Duration = time.Since(r.start)
	}
	return st
}

// Close ends the result early. It is idempotent.
func (r *docResult) Close() error {
	r.finish()
	return nil
}
