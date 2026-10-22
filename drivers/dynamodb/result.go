package dynamodb

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

type item = map[string]types.AttributeValue

// pager reads the next page of items; more is false after the last page.
type pager func(ctx context.Context) (items []item, more bool, err error)

// itemResult is a ResultSet over DynamoDB items. Its columns come from the
// first items read ahead (up to limits.prefetch, or Limit, and
// limits.resultBytes); later pages are read as Next needs them. An
// attribute of a later item outside the columns is left out and counted in
// a warning.
type itemResult struct {
	c        *conn
	op       string
	next     pager // nil once the last page is read
	buf      []item
	cols     []driver.Column
	index    map[string]int
	limit    int
	returned int
	omitted  int
	window   int
	budget   bool
	start    time.Time
	stats    driver.Stats
	done     bool
}

// resultSpec says how to label a result's columns.
type resultSpec struct {
	keys       []string              // key attributes, first and flagged Key
	projection []string              // the SELECT's plain names, or nil
	source     *driver.CollectionRef // the table the rows can be edited in, or nil
	whole      bool                  // columns come from every given item (a Browse page)
}

func (c *conn) newItemResult(ctx context.Context, op string, next pager, buf []item, limit int, start time.Time, spec resultSpec) (*itemResult, error) {
	r := &itemResult{c: c, op: op, next: next, buf: buf, limit: limit, start: start, stats: driver.Stats{RowsAffected: -1}}
	want := c.limits.prefetch
	if limit > 0 && limit+1 < want {
		want = limit + 1
	}
	held := 0
	for _, it := range r.buf {
		held += itemSize(it)
	}
	for r.next != nil && len(r.buf) < want {
		if held >= c.limits.resultBytes {
			r.budget = true
			break
		}
		items, err := r.page(ctx)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			held += itemSize(it)
		}
		r.buf = append(r.buf, items...)
	}
	shown := r.buf
	if !spec.whole {
		shown = shown[:min(len(shown), want)]
	}
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	r.window = len(shown)
	r.cols = inferColumns(shown, spec.keys, spec.projection)
	r.index = make(map[string]int, len(r.cols))
	for i, col := range r.cols {
		r.index[col.Name] = i
		if spec.source != nil {
			r.cols[i].Source = spec.source
			r.cols[i].Key = slices.Contains(spec.keys, col.Name)
		}
	}
	return r, nil
}

// page reads one page and drops the pager after the last one.
func (r *itemResult) page(ctx context.Context) ([]item, error) {
	items, more, err := r.next(ctx)
	if err != nil {
		return nil, err
	}
	if !more {
		r.next = nil
	}
	return items, nil
}

// fill reads pages until the buffer holds an item or no page is left
// (a filtered page may be empty).
func (r *itemResult) fill(ctx context.Context) error {
	for len(r.buf) == 0 && r.next != nil {
		items, err := r.page(ctx)
		if err != nil {
			return err
		}
		r.buf = items
	}
	return nil
}

func (r *itemResult) Columns() []driver.Column { return r.cols }

func (r *itemResult) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, mapError(ctx, r.op, err)
	}
	if err := r.fill(ctx); err != nil {
		r.finish()
		return nil, false, err
	}
	if r.limit > 0 && r.returned >= r.limit {
		r.stats.Truncated = len(r.buf) > 0
		r.finish()
		return nil, false, nil
	}
	if len(r.buf) == 0 {
		r.finish()
		return nil, false, nil
	}
	it := r.buf[0]
	r.buf[0] = nil
	r.buf = r.buf[1:]
	row := make(driver.Row, len(r.cols))
	for i, col := range r.cols {
		row[i] = valueOf(it[col.Name])
	}
	for k := range it {
		if _, ok := r.index[k]; !ok {
			r.omitted++
		}
	}
	r.returned++
	return row, true, nil
}

func (r *itemResult) finish() {
	if r.done {
		return
	}
	r.done = true
	r.buf, r.next = nil, nil
	r.stats.Duration = time.Since(r.start)
	if r.omitted > 0 {
		why := ""
		if r.budget {
			why = " (the " + budgetText(r.c.limits.resultBytes) + " read-ahead limit)"
		}
		r.stats.Warnings = append(r.stats.Warnings, fmt.Sprintf(
			"%d attributes of later items have no column: columns come from the first %d items%s; list the attributes in the SELECT to see them",
			r.omitted, r.window, why))
	}
}

func (r *itemResult) Stats() driver.Stats {
	st := r.stats
	if !r.done {
		st.Duration = time.Since(r.start)
	}
	return st
}

// Close stops reading pages. It is idempotent.
func (r *itemResult) Close() error {
	r.finish()
	return nil
}
