package redis

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// Browse pages through a collection with SCAN MATCH. Rows are _key, type,
// ttl and a bounded value preview, plus the hash fields of a record
// collection. Conditions are evaluated on those columns client side, and
// Filter.Raw is a Redis glob the key must also match. An unsorted Browse
// returns rows in key-name (byte) order, keyset-paged: each page runs a
// full SCAN, bounded by maxScanCalls and scanTimeBudget, and is
// ErrUnsupported when that scan cannot finish (see keysetPage). A sorted
// Browse loads at most maxSortKeys keys and is refused with ErrUnsupported
// beyond that.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "browse"); err != nil {
		return nil, err
	}
	cur, err := parseCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	if cur.sorted != (len(opts.Sort) > 0) && opts.Cursor != "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("the cursor belongs to a browse with a different sort"))
	}
	for _, cond := range opts.Filter.Conditions { // surface a bad operator even on an empty page
		if _, err := matchCondition(driver.Null(), cond); err != nil {
			return nil, err
		}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxBrowseLimit)
	cl, _, release, err := c.client("browse", ref.Namespace)
	if err != nil {
		return nil, err
	}
	defer release()
	rec, _, _, err := sampleRecords(ctx, cl, ref.Name)
	if err != nil {
		return nil, mapError(ctx, "browse", err)
	}
	cols := columnsFor(ref, collectionFields(rec), primaryKey(rec))
	b := &browser{cl: cl, ref: ref, rec: rec, cols: cols, filter: opts.Filter}
	var page []driver.Row
	var next string
	var warnings []string
	if len(opts.Sort) > 0 {
		page, next, err = b.sorted(ctx, opts.Sort, cur.offset, limit)
	} else {
		page, next, warnings, err = b.unsorted(ctx, cur, limit)
	}
	if err != nil {
		return nil, mapError(ctx, "browse", err)
	}
	return driver.NewSliceResultSet(cols, page, driver.Stats{
		RowsAffected: -1, Duration: time.Since(start), Truncated: next != "", NextCursor: next, Warnings: warnings,
	}), nil
}

type browser struct {
	cl     *goredis.Client
	ref    driver.CollectionRef
	rec    *recordSchema
	cols   []driver.Column
	filter driver.Filter
}

// keyFilter applies Filter.Raw, the cheap key-only filter.
func (b *browser) keyFilter(ctx context.Context, keys []string) ([]string, error) {
	if b.filter.Raw == "" {
		return keys, nil
	}
	out := keys[:0]
	for _, k := range keys {
		ok, err := globMatch(ctx, b.filter.Raw, k)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, k)
		}
	}
	return out, nil
}

// rows fetches rows for keys and applies the conditions; the result is
// aligned with keys (nil where a key vanished or did not match).
func (b *browser) rows(ctx context.Context, keys []string) ([]driver.Row, error) {
	rows, err := fetchRows(ctx, b.cl, keys, b.rec)
	if err != nil {
		return nil, err
	}
	for i, r := range rows {
		if r == nil {
			continue
		}
		ok, err := matchConditions(b.cols, r, b.filter.Conditions)
		if err != nil {
			return nil, err
		}
		if !ok {
			rows[i] = nil
		}
	}
	return rows, nil
}

// unsorted fills one page, keyset-paged by key name (see keysetPage).
func (b *browser) unsorted(ctx context.Context, cur browseCursor, limit int) ([]driver.Row, string, []string, error) {
	scan := func(ctx context.Context, cursor uint64) ([]string, uint64, error) {
		return scanBatch(ctx, b.cl, b.ref.Name, cursor)
	}
	return keysetPage(ctx, scan, cur, limit, b.keyFilter, b.rows)
}

// sorted loads every matching row (at most maxSortKeys keys, within the
// scan budget), sorts them and returns the page at offset.
func (b *browser) sorted(ctx context.Context, keys []driver.SortKey, offset, limit int) ([]driver.Row, string, error) {
	budget := newScanBudget(scanTimeBudget)
	seen := map[string]bool{}
	var all []string
	var scan uint64
	tooMany := driver.Wrap(driver.ErrUnsupported, Name, "browse",
		fmt.Errorf("sorting needs a full scan, and %s has more than %d keys or takes longer than %s to scan; browse unsorted or narrow it with a filter pattern", b.ref.Name, maxSortKeys, scanTimeBudget))
	for {
		batch, next, err := scanOnce(ctx, b.cl, budget, b.ref.Name, scan)
		if err != nil {
			return nil, "", err
		}
		batch, err = b.keyFilter(ctx, batch)
		if err != nil {
			return nil, "", err
		}
		for _, k := range batch {
			if !seen[k] {
				seen[k] = true
				all = append(all, k)
			}
		}
		if len(all) > maxSortKeys {
			return nil, "", tooMany
		}
		if next == 0 {
			break
		}
		if budget.spent() {
			return nil, "", tooMany
		}
		scan = next
	}
	var rows []driver.Row
	for i := 0; i < len(all); i += rowChunk {
		chunk, err := b.rows(ctx, all[i:min(i+rowChunk, len(all))])
		if err != nil {
			return nil, "", err
		}
		for _, r := range chunk {
			if r != nil {
				rows = append(rows, r)
			}
		}
	}
	idx := make([]int, len(keys))
	for i, k := range keys {
		idx[i] = slices.IndexFunc(b.cols, func(c driver.Column) bool { return c.Name == k.Field })
	}
	slices.SortStableFunc(rows, func(x, y driver.Row) int {
		for i, k := range keys {
			if idx[i] < 0 {
				continue
			}
			a, c := x[idx[i]], y[idx[i]]
			n := compareForSort(a, c)
			if k.Desc && !a.IsNull() && !c.IsNull() {
				n = -n
			}
			if n != 0 {
				return n
			}
		}
		return compareValues(x[0], y[0]) // _key: a stable order across pages
	})
	if offset >= len(rows) {
		return nil, "", nil
	}
	end := min(offset+limit, len(rows))
	next := ""
	if end < len(rows) {
		next, _ = browseCursor{sorted: true, offset: end}.encode() // a sorted cursor always encodes
	}
	return rows[offset:end], next, nil
}
