package neo4j

import (
	"context"
	"errors"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/xavidop/tobira/driver"
)

// Query runs one Cypher statement in Query.Namespace (the database), or
// the DSN's. Query.Params bind to $1, $2 … in order. Rows stream as the
// server sends them; a statement that returns no rows (a write, a schema
// change) gives an empty result whose Stats count what it changed. On a
// read-only call (or connection) the statement runs in a read-mode
// transaction, in which the server refuses any write.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	ps, err := params(q.Params)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	return c.run(ctx, op, q.Namespace, q.ReadOnly, q.Text, ps, q.Limit, nil, nil)
}

// run opens a session on db, runs cypher in it and returns the streaming
// result. source and keys mark the columns of a Browse result.
func (c *conn) run(ctx context.Context, op, db string, readOnly bool, cypher string, ps map[string]any,
	limit int, source *driver.CollectionRef, keys []string) (*rowResult, error) {
	start := time.Now()
	sess := c.session(ctx, db, readOnly)
	var cfgs []func(*neo4j.TransactionConfig)
	if c.set.timeout > 0 {
		cfgs = append(cfgs, neo4j.WithTxTimeout(c.set.timeout))
	}
	res, err := sess.Run(ctx, cypher, ps, cfgs...)
	if err != nil {
		_ = sess.Close(ctx)
		return nil, mapError(ctx, op, err)
	}
	names, err := res.Keys()
	if err != nil {
		_ = sess.Close(ctx)
		return nil, mapError(ctx, op, err)
	}
	r := &rowResult{op: op, sess: sess, res: res, limit: limit, start: start, stats: driver.Stats{RowsAffected: -1}}
	r.cols = make([]driver.Column, len(names))
	for i, n := range names {
		r.cols[i] = driver.Column{Name: n, Type: driver.TypeUnknown, Source: source}
		for _, k := range keys {
			if k == n {
				r.cols[i].Key = true
			}
		}
	}
	// The first record types the columns: Cypher results carry no types
	// of their own.
	var first *neo4j.Record
	if res.PeekRecord(ctx, &first) {
		for i, v := range first.Values {
			if i < len(r.cols) {
				r.cols[i].NativeType = nativeTypeOf(v)
				r.cols[i].Type = decode(v).Type()
			}
		}
	} else if err := res.Err(); err != nil {
		_ = sess.Close(ctx)
		return nil, mapError(ctx, op, err)
	}
	for i := range r.cols {
		if r.cols[i].Type == driver.TypeNull {
			r.cols[i].Type = driver.TypeUnknown
		}
	}
	return r, nil
}

// rowResult is a ResultSet over a streaming Neo4j result and the session
// that owns it. It is safe for a single consumer.
type rowResult struct {
	op       string
	sess     neo4j.SessionWithContext
	res      neo4j.ResultWithContext
	cols     []driver.Column
	limit    int // 0 = none
	returned int
	start    time.Time
	stats    driver.Stats
	// cursor, when set, is the next page's cursor to report when the
	// result was cut by limit (Browse).
	cursor func() string
	done   bool
}

func (r *rowResult) Columns() []driver.Column { return r.cols }

func (r *rowResult) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.done {
		return nil, false, nil
	}
	if r.limit > 0 && r.returned >= r.limit {
		if r.res.Peek(ctx) {
			r.stats.Truncated = true
			if r.cursor != nil {
				r.stats.NextCursor = r.cursor()
			}
		}
		r.finish(ctx)
		return nil, false, nil
	}
	var rec *neo4j.Record
	if !r.res.NextRecord(ctx, &rec) {
		err := r.res.Err()
		r.finish(ctx)
		if err != nil {
			return nil, false, mapError(ctx, r.op, err)
		}
		return nil, false, nil
	}
	row := make(driver.Row, len(r.cols))
	for i := range r.cols {
		if i < len(rec.Values) {
			row[i] = decode(rec.Values[i])
		}
	}
	r.returned++
	return row, true, nil
}

// finish ends the result: the summary is read when every record was
// (so the counters and notices are known), the session closes either
// way. A cut result is not consumed: the server stops sending.
func (r *rowResult) finish(ctx context.Context) {
	if r.done {
		return
	}
	r.done = true
	if !r.stats.Truncated && ctx.Err() == nil {
		if sum, err := r.res.Consume(ctx); err == nil && sum != nil {
			r.stats.RowsAffected = affected(sum.Counters())
			for _, n := range sum.Notifications() {
				r.stats.Notices = append(r.stats.Notices, n.Title()+": "+n.Description())
			}
		}
	}
	_ = r.sess.Close(context.WithoutCancel(ctx))
	r.stats.Duration = time.Since(r.start)
}

// affected sums what a write changed, or -1 for a read.
func affected(c neo4j.Counters) int64 {
	if c == nil || !c.ContainsUpdates() {
		return -1
	}
	return int64(c.NodesCreated() + c.NodesDeleted() + c.RelationshipsCreated() + c.RelationshipsDeleted() + c.PropertiesSet())
}

func (r *rowResult) Stats() driver.Stats { return r.stats }

func (r *rowResult) Close() error {
	if !r.done {
		r.stats.Truncated = r.stats.Truncated || r.res.Peek(context.Background())
	}
	r.finish(context.Background())
	return nil
}

// errNoRows is the failure of a query that was to return one row.
var errNoRows = errors.New("the query returned no rows")

// one runs cypher and returns its single record's values; a query that
// returns none is errNoRows, under a *driver.Error.
func (c *conn) one(ctx context.Context, op, db string, cypher string, ps map[string]any) ([]any, error) {
	rows, err := c.all(ctx, op, db, cypher, ps)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, driver.Wrap(nil, Name, op, errNoRows)
	}
	return rows[0], nil
}

// all runs cypher and returns every record's values.
func (c *conn) all(ctx context.Context, op, db string, cypher string, ps map[string]any) ([][]any, error) {
	sess := c.session(ctx, db, true)
	defer func() { _ = sess.Close(context.WithoutCancel(ctx)) }()
	res, err := sess.Run(ctx, cypher, ps)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	out := make([][]any, len(recs))
	for i, r := range recs {
		out[i] = r.Values
	}
	return out, nil
}
