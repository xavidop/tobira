package cassandra

import (
	"context"
	"fmt"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
)

// defaultPageSize is how many rows a query fetches per round trip.
const defaultPageSize = 1000

// Query runs one CQL statement in Query.Namespace (the keyspace), or the
// DSN's keyspace. Query.Params bind to ? or :name placeholders, each
// encoded for the type the server reports for it. Rows stream page by page
// (1000 rows, or Limit+1); a statement that returns no rows (a write, DDL)
// gives an empty result. On a read-only call (or connection) the statement
// must pass the read-only guard first.
//
// A Namespace other than the DSN's keyspace needs native protocol 5
// (Cassandra 4.0 and later), which carries the keyspace with the query;
// on an older protocol it is ErrUnsupported: qualify the table names.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	if q.ReadOnly || c.readOnly {
		if err := c.guard(ctx, q.Text); err != nil {
			return nil, err
		}
	}
	gq := c.session.Query(q.Text, params(q.Params)...)
	if q.Namespace != "" && q.Namespace != c.set.keyspace {
		if c.proto < 5 {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf(
				"running in keyspace %q needs native protocol 5 (this connection speaks %d): qualify the table names", q.Namespace, c.proto))
		}
		gq = gq.SetKeyspace(q.Namespace)
	}
	pageSize := defaultPageSize
	if q.Limit > 0 && q.Limit < pageSize {
		pageSize = q.Limit + 1
	}
	iter := gq.PageSize(pageSize).IterContext(ctx)
	rs, err := newRowResult(ctx, op, iter, q.Limit, start, nil, nil)
	if err == nil && changesSchema(q.Text) {
		c.Invalidate()
	}
	return rs, err
}

// changesSchema reports whether text is a CREATE, ALTER or DROP
// statement, after which cached schema answers are stale.
func changesSchema(text string) bool {
	toks, _ := lexCQL(text)
	if len(toks) == 0 || toks[0].kind != tWord {
		return false
	}
	switch toks[0].low {
	case "create", "alter", "drop":
		return true
	}
	return false
}

// rowResult is a ResultSet over a gocql iterator. Rows are decoded as
// they are read; pages are fetched as the reader reaches them. It is safe
// for a single consumer.
type rowResult struct {
	op       string
	iter     *gocql.Iter
	cols     []driver.Column
	cells    []any // one per column, or one per element of a tuple column
	widths   []int // the cells of each column: 0 for a plain column
	limit    int   // 0 = none
	max      int   // rows of the current page only (Browse); -1 = all pages
	returned int
	start    time.Time
	stats    driver.Stats
	done     bool
}

// newRowResult wraps iter. A query that fails comes back from gocql on the
// first page, so it is reported here rather than from Next. source and
// keys mark the columns of a Browse result.
func newRowResult(ctx context.Context, op string, iter *gocql.Iter, limit int, start time.Time, source *driver.CollectionRef, keys []string) (*rowResult, error) {
	infos := iter.Columns()
	if iter.NumRows() == 0 && len(infos) == 0 {
		if err := iter.Close(); err != nil {
			return nil, mapError(ctx, op, err)
		}
	}
	r := &rowResult{op: op, iter: iter, limit: limit, max: -1, start: start, stats: driver.Stats{RowsAffected: -1}}
	r.cols = make([]driver.Column, len(infos))
	r.widths = make([]int, len(infos))
	for i, ci := range infos {
		r.cols[i] = driver.Column{Name: ci.Name, NativeType: nativeName(ci.TypeInfo), Type: columnType(ci.TypeInfo)}
		if source != nil {
			r.cols[i].Source = source
			for _, k := range keys {
				if k == ci.Name {
					r.cols[i].Key = true
				}
			}
		}
		// gocql scans a tuple column into one destination per element.
		if tt, ok := ci.TypeInfo.(gocql.TupleTypeInfo); ok {
			r.widths[i] = len(tt.Elems)
			for range tt.Elems {
				r.cells = append(r.cells, &cell{})
			}
			continue
		}
		r.cells = append(r.cells, &cell{})
	}
	r.stats.Warnings = iter.Warnings()
	return r, nil
}

// tupleValue assembles a tuple column from its element cells. gocql hands
// a NULL tuple over as NULL elements, so a tuple whose elements are all
// NULL reads as NULL.
func tupleValue(cells []any) driver.Value {
	out := make([]driver.Value, len(cells))
	allNull := true
	for i, c := range cells {
		out[i] = c.(*cell).v
		allNull = allNull && out[i].IsNull()
	}
	if allNull {
		return driver.Null()
	}
	return driver.List(out...)
}

func (r *rowResult) Columns() []driver.Column { return r.cols }

func (r *rowResult) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.finish()
		return nil, false, mapError(ctx, r.op, err)
	}
	if r.max >= 0 && r.returned >= r.max {
		r.finish()
		return nil, false, nil
	}
	if !r.iter.Scan(r.cells...) {
		err := r.iter.Close()
		r.finish()
		if err != nil {
			return nil, false, mapError(ctx, r.op, err)
		}
		return nil, false, nil
	}
	if r.limit > 0 && r.returned >= r.limit {
		r.stats.Truncated = true // the extra row proves there were more
		r.finish()
		return nil, false, nil
	}
	row := make(driver.Row, len(r.cols))
	k := 0
	for i, w := range r.widths {
		if w == 0 {
			row[i] = r.cells[k].(*cell).v
			k++
			continue
		}
		row[i] = tupleValue(r.cells[k : k+w])
		k += w
	}
	r.returned++
	return row, true, nil
}

// finish ends the result and fixes the stats.
func (r *rowResult) finish() {
	if r.done {
		return
	}
	r.done = true
	_ = r.iter.Close()
	r.stats.Duration = time.Since(r.start)
}

func (r *rowResult) Stats() driver.Stats {
	st := r.stats
	if !r.done {
		st.Duration = time.Since(r.start)
	}
	return st
}

// Close stops reading; no further page is fetched. It is idempotent.
func (r *rowResult) Close() error {
	r.finish()
	return nil
}
