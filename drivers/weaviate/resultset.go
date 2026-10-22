package weaviate

import (
	"context"
	"time"

	"github.com/xavidop/tobira/driver"
)

// fetchFunc returns the next page of rows; done reports the last page.
type fetchFunc func(ctx context.Context) (rows []driver.Row, done bool, err error)

// pagedResultSet streams pages from fetch, at most limit rows (0 = no
// limit). It is for a single consumer.
type pagedResultSet struct {
	cols    []driver.Column
	buf     []driver.Row
	fetch   fetchFunc // nil once the source is exhausted
	limit   int
	n       int
	start   time.Time
	stats   driver.Stats
	warn    func() []string // late warnings, read at the end
	closed  bool
	stopped bool
}

func newPagedResultSet(cols []driver.Column, first []driver.Row, fetch fetchFunc, limit int, start time.Time) *pagedResultSet {
	return &pagedResultSet{cols: cols, buf: first, fetch: fetch, limit: limit, start: start, stats: driver.Stats{RowsAffected: -1}}
}

func (r *pagedResultSet) Columns() []driver.Column { return r.cols }

func (r *pagedResultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.closed || r.stopped {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, driver.Wrap(nil, Name, "query", err)
	}
	for len(r.buf) == 0 && r.fetch != nil {
		rows, done, err := r.fetch(ctx)
		if err != nil {
			r.finish()
			return nil, false, err
		}
		r.buf = rows
		if done {
			r.fetch = nil
		}
	}
	if len(r.buf) == 0 {
		r.finish()
		return nil, false, nil
	}
	if r.limit > 0 && r.n >= r.limit {
		r.stats.Truncated = true
		r.finish()
		return nil, false, nil
	}
	row := r.buf[0]
	r.buf = r.buf[1:]
	r.n++
	return row, true, nil
}

func (r *pagedResultSet) finish() {
	if r.stopped {
		return
	}
	r.stopped = true
	r.stats.Duration = time.Since(r.start)
	if r.warn != nil {
		r.stats.Warnings = append(r.stats.Warnings, r.warn()...)
	}
	r.buf, r.fetch = nil, nil
}

func (r *pagedResultSet) Stats() driver.Stats { return r.stats }

func (r *pagedResultSet) Close() error {
	if !r.closed {
		r.finish()
		r.closed = true
	}
	return nil
}
