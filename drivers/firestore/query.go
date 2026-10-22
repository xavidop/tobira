package firestore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/api/iterator"

	"github.com/xavidop/tobira/driver"
)

// prefetchMax caps how many documents a result reads before it fixes its
// columns; so does maxResultBytes. Fields only later documents hold have
// no column; the result reports how many were left out in Stats.Warnings.
const prefetchMax = 1000

// maxResultBytes is the byte budget for documents held in memory at once:
// a result's prefetch, a Browse page and Describe's sample each stop
// reading once it is reached (a variable so tests can lower it). Sizes are
// estimated from the decoded values.
var maxResultBytes = 32 << 20

// budgetText spells maxResultBytes for messages.
func budgetText() string {
	if maxResultBytes%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", maxResultBytes>>20)
	}
	return fmt.Sprintf("%d bytes", maxResultBytes)
}

// checkNamespace accepts the connection's database ("" means it): a
// connection is bound to one database.
func (c *conn) checkNamespace(op, ns string) error {
	if ns == "" || ns == c.dsn.database {
		return nil
	}
	return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("database %q: this connection is to database %q; open another connection for it", ns, c.dsn.database))
}

// collection returns the collection at a path, checked.
func (c *conn) collection(op, path string) (*fs.CollectionRef, error) {
	if err := checkPath(path, false); err != nil {
		return nil, syntaxErr(op, err)
	}
	return c.client.Collection(path), nil
}

// Query runs one JSON request (see parseQuery): a document read, a
// collection or collection-group query, or its count. Query.Params are not
// supported: put values in the request. Rows are documents: _id (the key),
// _path for collection-group queries, then the documents' fields.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("requests take no parameters: put the values in the request"))
	}
	spec, err := parseQuery(q.Text, q.ReadOnly || c.readOnly)
	if err != nil {
		return nil, err
	}
	if err := c.checkNamespace(op, q.Namespace); err != nil {
		return nil, err
	}
	if spec.document != "" {
		return c.getDocument(ctx, op, spec.document, start)
	}
	var (
		base   fs.Query
		coll   *fs.CollectionRef
		source *driver.CollectionRef
	)
	if spec.collection != "" {
		coll = c.client.Collection(spec.collection)
		base = coll.Query
		source = &driver.CollectionRef{Namespace: c.dsn.database, Name: spec.collection}
	} else {
		base = c.client.CollectionGroup(spec.collectionGroup).Query
	}
	fq, err := c.applyWhere(op, base, coll, spec.where)
	if err != nil {
		return nil, err
	}
	for _, k := range spec.orderBy {
		fq = fq.OrderByPath(fs.FieldPath(k.path), direction(k.desc))
	}
	if spec.offset > 0 {
		fq = fq.Offset(spec.offset)
	}
	if spec.hasSelect {
		paths := make([]fs.FieldPath, len(spec.selectPaths))
		for i, p := range spec.selectPaths {
			paths[i] = fs.FieldPath(p)
		}
		fq = fq.SelectPaths(paths...)
	}
	limit := 0
	if spec.hasLimit {
		limit = spec.limit
	}
	if spec.count {
		if limit > 0 {
			fq = fq.Limit(limit)
		}
		return c.count(ctx, op, fq, start)
	}
	// Read one document past Query.Limit to know whether it truncated.
	if q.Limit > 0 && (limit == 0 || limit > q.Limit+1) {
		limit = q.Limit + 1
	}
	if limit > 0 {
		fq = fq.Limit(limit)
	}
	it := fq.Documents(ctx)
	return newDocResult(ctx, c, op, it, nil, q.Limit, start, source, spec.collectionGroup != "")
}

func direction(desc bool) fs.Direction {
	if desc {
		return fs.Desc
	}
	return fs.Asc
}

// applyWhere adds filters to q. Reference literals are resolved, and a
// document-id filter's string values become references: an id within
// coll, or a document path (the only form a collection-group query
// takes).
func (c *conn) applyWhere(op string, q fs.Query, coll *fs.CollectionRef, where []whereClause) (fs.Query, error) {
	for _, w := range where {
		v := resolveRefs(c.client, w.value)
		if len(w.path) == 1 && w.path[0] == docIDField {
			var err error
			if v, err = c.docIDValue(op, coll, v); err != nil {
				return q, err
			}
		}
		q = q.WherePath(fs.FieldPath(w.path), w.op, v)
	}
	return q, nil
}

// docIDValue turns a document-id filter value into references.
func (c *conn) docIDValue(op string, coll *fs.CollectionRef, v any) (any, error) {
	switch x := v.(type) {
	case *fs.DocumentRef:
		return x, nil
	case string:
		if !strings.Contains(x, "/") && coll != nil && x != "" {
			return coll.Doc(x), nil
		}
		if err := checkPath(x, true); err != nil {
			return nil, syntaxErr(op, fmt.Errorf("an _id filter takes document ids (or document paths in a collection group): %w", err))
		}
		return c.client.Doc(x), nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := c.docIDValue(op, coll, e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return nil, syntaxErr(op, errors.New("an _id filter takes document ids (strings)"))
}

// getDocument reads one document; a missing one is ErrNotFound.
func (c *conn) getDocument(ctx context.Context, op, path string, start time.Time) (driver.ResultSet, error) {
	ref := c.client.Doc(path)
	snap, err := ref.Get(ctx)
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	row := rowFromSnapshot(snap)
	source := &driver.CollectionRef{Namespace: c.dsn.database, Name: relativePath(ref.Parent.Path)}
	return newDocResult(ctx, c, op, nil, []docRow{row}, 0, start, source, false)
}

// count runs a count aggregation: one row with column count.
func (c *conn) count(ctx context.Context, op string, q fs.Query, start time.Time) (driver.ResultSet, error) {
	res, err := q.NewAggregationQuery().WithCount("count").Get(ctx)
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	var n int64
	switch v := res["count"].(type) {
	case *firestorepb.Value:
		n = v.GetIntegerValue()
	case int64:
		n = v
	}
	cols := []driver.Column{{Name: "count", NativeType: "integer", Type: driver.TypeInt}}
	return driver.NewSliceResultSet(cols, []driver.Row{{driver.Int(n)}}, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
}

// rowFromSnapshot converts a document. A document the SDK cannot decode
// (it refuses some stored shapes, such as a reference to a collection)
// keeps its id and is marked bad.
func rowFromSnapshot(snap *fs.DocumentSnapshot) docRow {
	var m map[string]any
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("decode: %v", p)
			}
		}()
		return snap.DataTo(&m)
	}()
	if err != nil {
		return docRow{id: snap.Ref.ID, path: relativePath(snap.Ref.Path), bad: true}
	}
	return rowFromMap(snap.Ref.ID, relativePath(snap.Ref.Path), m)
}

// rowFromMap converts decoded fields, sorted by name. A stored field named
// _id becomes _id_field (and a stored _id_field is then hidden).
func rowFromMap(id, path string, m map[string]any) docRow {
	r := docRow{id: id, path: path}
	if v, ok := m[idColumn]; ok {
		r.renamed = true
		_, r.hidden = m[idFieldColumn]
		m = maps.Clone(m)
		delete(m, idColumn)
		m[idFieldColumn] = v
	}
	keys := slices.Sorted(maps.Keys(m))
	r.fields = make(driver.Object, len(keys))
	r.natives = make([]string, len(keys))
	for i, k := range keys {
		r.fields[i] = driver.Member{Key: k, Value: valueOf(m[k])}
		r.natives[i] = nativeType(m[k])
	}
	return r
}

// sizeOf estimates the memory a value holds.
func sizeOf(v driver.Value) int {
	switch v.Type() {
	case driver.TypeString, driver.TypeDecimal:
		s, _ := v.AsText()
		return 16 + len(s)
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return 24 + len(b)
	case driver.TypeVector:
		f, _ := v.AsVector()
		return 24 + 4*len(f)
	case driver.TypeList:
		list, _ := v.AsList()
		n := 24
		for _, e := range list {
			n += sizeOf(e)
		}
		return n
	case driver.TypeObject:
		o, _ := v.AsObject()
		return objectSize(o)
	case driver.TypeUnknown:
		return 16 + len(v.String())
	}
	return 16
}

func objectSize(o driver.Object) int {
	n := 24
	for _, m := range o {
		n += 16 + len(m.Key) + sizeOf(m.Value)
	}
	return n
}

func rowSize(d docRow) int { return 32 + len(d.id) + len(d.path) + objectSize(d.fields) }

// docResult is a ResultSet over documents: those already read (buf), then
// the rest of a query (it, nil when there is none). Its columns come from
// the documents read before the first row is returned, up to prefetchMax
// (or the limit). It is safe for a single consumer.
type docResult struct {
	c        *conn
	op       string
	it       *fs.DocumentIterator
	buf      []docRow
	cols     []driver.Column
	index    map[string]int
	withPath bool
	limit    int
	returned int
	omitted  int
	bad      int
	renamed  int
	hidden   int
	window   int  // documents the columns come from
	budget   bool // the prefetch stopped at maxResultBytes
	start    time.Time
	stats    driver.Stats
	done     bool
}

func newDocResult(ctx context.Context, c *conn, op string, it *fs.DocumentIterator, buf []docRow, limit int, start time.Time, source *driver.CollectionRef, withPath bool) (driver.ResultSet, error) {
	r := &docResult{c: c, op: op, it: it, buf: buf, limit: limit, start: start, withPath: withPath, stats: driver.Stats{RowsAffected: -1}}
	want := prefetchMax
	if limit > 0 && limit+1 < want {
		want = limit + 1
	}
	held := 0
	for _, d := range r.buf {
		held += rowSize(d)
	}
	for r.it != nil && len(r.buf) < want {
		if held >= maxResultBytes {
			r.budget = true
			break
		}
		d, ok, err := r.read(ctx)
		if err != nil {
			r.stop()
			return nil, err
		}
		if !ok {
			break
		}
		r.buf = append(r.buf, d)
		held += rowSize(d)
	}
	shown := r.buf
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	r.window = len(shown)
	r.cols = inferColumns(shown, withPath)
	r.index = columnIndex(r.cols)
	if source != nil {
		for k := range r.cols {
			r.cols[k].Source = source
		}
	}
	return r, nil
}

// read returns the iterator's next document; ok is false at the end, when
// the iterator is stopped.
func (r *docResult) read(ctx context.Context) (docRow, bool, error) {
	snap, err := r.it.Next()
	if errors.Is(err, iterator.Done) {
		r.stop()
		return docRow{}, false, nil
	}
	if err != nil {
		return docRow{}, false, r.c.fail(ctx, r.op, err)
	}
	return rowFromSnapshot(snap), true, nil
}

func (r *docResult) Columns() []driver.Column { return r.cols }

func (r *docResult) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.finish()
		return nil, false, mapError(ctx, r.op, err)
	}
	if r.limit > 0 && r.returned >= r.limit {
		more := len(r.buf) > 0
		if !more && r.it != nil {
			_, ok, err := r.read(ctx)
			if err != nil {
				r.finish()
				return nil, false, err
			}
			more = ok
		}
		r.stats.Truncated = more
		r.finish()
		return nil, false, nil
	}
	var d docRow
	switch {
	case len(r.buf) > 0:
		d, r.buf = r.buf[0], r.buf[1:]
	case r.it != nil:
		var ok bool
		var err error
		if d, ok, err = r.read(ctx); err != nil || !ok {
			r.finish()
			return nil, false, err
		}
	default:
		r.finish()
		return nil, false, nil
	}
	if d.bad {
		r.bad++
	}
	if d.renamed {
		r.renamed++
	}
	if d.hidden {
		r.hidden++
	}
	row, omitted := rowOf(d, r.index, len(r.cols), r.withPath)
	r.omitted += omitted
	r.returned++
	return row, true, nil
}

func (r *docResult) stop() {
	if r.it != nil {
		r.it.Stop()
		r.it = nil
	}
}

// finish ends the result: it stops the query and fixes the stats.
func (r *docResult) finish() {
	if r.done {
		return
	}
	r.done = true
	r.buf = nil
	r.stop()
	r.stats.Duration = time.Since(r.start)
	if r.omitted > 0 {
		why := ""
		if r.budget {
			why = " (the " + budgetText() + " read-ahead limit)"
		}
		r.stats.Warnings = append(r.stats.Warnings, fmt.Sprintf(
			"%d fields of later documents have no column: columns come from the first %d documents%s; add a select to see them",
			r.omitted, r.window, why))
	}
	if r.renamed > 0 {
		w := fmt.Sprintf("%d documents hold a field named _id: it is shown as _id_field (_id is the document id)", r.renamed)
		if r.hidden > 0 {
			w += fmt.Sprintf("; %d of them also hold a field named _id_field, which is not shown", r.hidden)
		}
		r.stats.Warnings = append(r.stats.Warnings, w)
	}
	if r.bad > 0 {
		r.stats.Warnings = append(r.stats.Warnings, fmt.Sprintf(
			"%d documents hold values the Firestore client cannot decode: only their _id is shown", r.bad))
	}
}

func (r *docResult) Stats() driver.Stats {
	st := r.stats
	if !r.done {
		st.Duration = time.Since(r.start)
	}
	return st
}

// Close stops the query early. It is idempotent.
func (r *docResult) Close() error {
	r.finish()
	return nil
}
