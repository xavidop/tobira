package influxdb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/flight"

	"github.com/xavidop/tobira/driver"
)

// Query runs one statement in the connection's language (SQL unless the
// DSN says language=influxql). Every statement passes the read-only guard
// first, whatever Query.ReadOnly says: the driver never writes.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	params, err := queryParams(q.Params)
	if err != nil {
		return nil, err
	}
	return c.run(ctx, "query", q.Text, c.set.lang, q.Namespace, params, q.Limit)
}

// run checks text against the allow-list and starts it on database ns (the
// DSN's database when ""). limit > 0 stops after limit rows, reading one
// more to set Stats.Truncated.
func (c *conn) run(ctx context.Context, op, text string, lang language, ns string, params map[string]any, limit int) (*resultSet, error) {
	if err := checkStatement(text, lang); err != nil {
		return nil, err
	}
	db := ns
	if db == "" {
		db = c.set.database
	}
	if db == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("no database: name one in the DSN path or pick a namespace"))
	}
	qt := influxdb3.SQL
	if lang == langInfluxQL {
		qt = influxdb3.InfluxQL
	}
	start := time.Now()
	qctx, cancel := context.WithCancel(ctx)
	it, err := c.client.QueryWithParameters(qctx, text, params, influxdb3.WithDatabase(db), influxdb3.WithQueryType(qt))
	if err != nil {
		cancel()
		return nil, c.scrub(mapError(ctx, op, err))
	}
	reader := it.Raw()
	if reader == nil { // never with this client version; fail rather than guess
		cancel()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errors.New("no Flight reader"))
	}
	return &resultSet{
		conn: c, op: op, reader: reader, cancel: cancel, limit: limit, start: start,
		cols: columnsOf(reader.Schema()),
	}, nil
}

// columnsOf describes a result's columns. NativeType is InfluxDB's column
// role ("tag", "timestamp", "field::float") when the server reports one,
// else the Arrow type.
func columnsOf(s *arrow.Schema) []driver.Column {
	cols := make([]driver.Column, s.NumFields())
	for i, f := range s.Fields() {
		native := f.Type.String()
		if v, ok := f.Metadata.GetValue("iox::column::type"); ok {
			native = strings.TrimPrefix(v, "iox::column_type::")
		}
		cols[i] = driver.Column{Name: f.Name, NativeType: native, Type: columnType(f.Type)}
	}
	return cols
}

// resultSet streams the record batches of one Flight query.
type resultSet struct {
	conn   *conn
	op     string
	cancel context.CancelFunc // ends the Flight stream
	cols   []driver.Column
	limit  int
	start  time.Time

	// nextCursor is reported as Stats.NextCursor when the limit was hit
	// (Browse).
	nextCursor string

	mu        sync.Mutex
	reader    *flight.Reader
	rec       arrow.RecordBatch // current batch, owned by reader
	idx       int               // next row in rec
	count     int
	done      bool
	truncated bool
	dur       time.Duration
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

func (r *resultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, driver.Wrap(nil, Name, r.op, err)
	}
	for r.rec == nil || int64(r.idx) >= r.rec.NumRows() {
		stop := context.AfterFunc(ctx, r.cancel) // a cancelled Next ends the stream
		ok := r.reader.Next()
		stop()
		if !ok {
			err := r.reader.Err()
			r.finish()
			if err != nil {
				return nil, false, r.conn.scrub(mapError(ctx, r.op, err))
			}
			return nil, false, nil
		}
		r.rec, r.idx = r.reader.RecordBatch(), 0
	}
	if r.limit > 0 && r.count >= r.limit {
		r.truncated = true
		r.finish()
		return nil, false, nil
	}
	row := make(driver.Row, r.rec.NumCols())
	for j := range row {
		row[j] = valueAt(r.rec.Column(j), r.idx)
	}
	r.idx++
	r.count++
	return row, true, nil
}

// finish ends the stream and releases the reader. r.mu is held.
func (r *resultSet) finish() {
	if r.done {
		return
	}
	r.done = true
	r.dur = time.Since(r.start)
	r.rec = nil
	r.cancel()
	r.reader.Release()
}

func (r *resultSet) Stats() driver.Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := driver.Stats{RowsAffected: -1, Duration: r.dur, Truncated: r.truncated}
	if !r.done {
		st.Duration = time.Since(r.start)
	}
	if r.truncated {
		st.NextCursor = r.nextCursor
	}
	return st
}

// Close ends the stream early. It is idempotent and safe to call while
// another goroutine is in Next: the stream is cancelled first, so that
// Next returns.
func (r *resultSet) Close() error {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finish()
	return nil
}
