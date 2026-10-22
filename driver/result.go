package driver

import (
	"context"
	"slices"
	"time"
)

// Query is a native query plus host-enforced options.
type Query struct {
	Text      string  // native language: SQL, Mongo JSON, Redis command…
	Params    []Value // positional parameters, if the dialect supports them
	Namespace string  // default namespace to run in (search_path, Mongo db, Redis db); "" = connection default
	Limit     int     // max rows to return; 0 = no limit. Driver stops early and sets Stats.Truncated.
	ReadOnly  bool    // refuse/abort anything mutating, returning ErrReadOnly
}

// ResultSet is a forward-only cursor over query results.
type ResultSet interface {
	Columns() []Column
	Next(ctx context.Context) (Row, bool, error) // (row, true, nil) | (nil, false, nil) at end | (nil, false, err)
	Stats() Stats                                // final once Next returned false
	Close() error                                // idempotent; releases the cursor early
}

// MultiResultSet is optionally implemented when one query yields several result
// sets (multi-statement SQL). NextResultSet advances; Columns/Next then refer to it.
type MultiResultSet interface {
	ResultSet
	NextResultSet(ctx context.Context) (bool, error)
}

// Row is aligned with ResultSet.Columns().
type Row []Value

// Column describes one result column.
type Column struct {
	Name       string
	NativeType string
	Type       Type
	Source     *CollectionRef // originating collection, when known (enables inline edit)
	Key        bool           // part of Source's primary key
}

// Stats summarises a finished (or finishing) result.
type Stats struct {
	RowsAffected int64 // -1 = not applicable
	Duration     time.Duration
	Truncated    bool   // Query.Limit / BrowseOptions.Limit reached and more rows existed
	NextCursor   string // Browse only; "" = last page
	Warnings     []string
	Notices      []string // server messages (RAISE NOTICE…)
}

// sliceResultSet is an in-memory ResultSet for a single consumer.
type sliceResultSet struct {
	cols   []Column
	rows   []Row
	stats  Stats
	pos    int
	closed bool
}

// NewSliceResultSet returns a ResultSet over rows. The rows slice header is
// copied, so callers may reuse their slice. It is safe for a single consumer.
func NewSliceResultSet(cols []Column, rows []Row, stats Stats) ResultSet {
	return &sliceResultSet{cols: cols, rows: slices.Clone(rows), stats: stats}
}

func (s *sliceResultSet) Columns() []Column { return s.cols }

func (s *sliceResultSet) Next(ctx context.Context) (Row, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.closed || s.pos >= len(s.rows) {
		return nil, false, nil
	}
	r := s.rows[s.pos]
	s.pos++
	return r, true, nil
}

func (s *sliceResultSet) Stats() Stats { return s.stats }

func (s *sliceResultSet) Close() error {
	s.closed = true
	return nil
}

// Collect drains rs into a slice and always closes it. When max > 0 and more
// rows exist, it reads exactly one extra row to decide truncation, then stops.
// On error it returns the rows collected so far. The returned Stats is
// rs.Stats() with Truncated set if rs reported it or Collect cut the stream.
func Collect(ctx context.Context, rs ResultSet, max int) ([]Row, Stats, error) {
	var rows []Row
	truncated := false
	var err error
	for {
		var (
			row Row
			ok  bool
		)
		row, ok, err = rs.Next(ctx)
		if err != nil || !ok {
			break
		}
		if max > 0 && len(rows) >= max {
			truncated = true
			break
		}
		rows = append(rows, row)
	}
	st := rs.Stats()
	st.Truncated = st.Truncated || truncated
	if cerr := rs.Close(); err == nil {
		err = cerr
	}
	return rows, st, err
}
