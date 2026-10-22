package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xavidop/tobira/driver"
)

// prefetchMax caps how many documents a result reads before it fixes its
// columns; so does maxResultBytes. Fields that only later documents hold have no column; the result
// reports how many were left out in Stats.Warnings.
const prefetchMax = 1000

// maxResultBytes is the byte budget for documents held in memory at once:
// a result's prefetch, a Browse page and Describe's sample each stop
// reading once it is reached (a variable so tests can lower it).
var maxResultBytes = 32 << 20

// budgetText spells maxResultBytes for messages: "32 MiB", or bytes.
func budgetText() string {
	if maxResultBytes%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", maxResultBytes>>20)
	}
	return fmt.Sprintf("%d bytes", maxResultBytes)
}

// cursorCloseTimeout bounds closing a server cursor.
const cursorCloseTimeout = 5 * time.Second

// Query runs one database command document (see parseCommand) in
// Query.Namespace, or the connection's default database. find, aggregate,
// listCollections and listIndexes stream their cursor's documents as rows;
// distinct returns one row per value; any other command returns its reply
// as one row (without ok, $clusterTime and operationTime). Query.Params are
// not supported: put values in the document. On a read-only call (or
// connection) the command must pass the read-only guard.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("MongoDB commands take no parameters: put the values in the command document"))
	}
	cmd, err := parseCommand(q.Text)
	if err != nil {
		return nil, err
	}
	if cmd, err = prepareCommand(cmd, q.ReadOnly || c.readOnly); err != nil {
		return nil, err
	}
	db := c.database(q.Namespace)
	var source *driver.CollectionRef
	if cmd[0].Key == "find" && commandTarget(cmd) != "" {
		source = &driver.CollectionRef{Namespace: db.Name(), Name: commandTarget(cmd)}
	}
	return c.run(ctx, op, db, cmd, q.Limit, start, source)
}

// run sends a prepared command and wraps its answer as a ResultSet.
func (c *conn) run(ctx context.Context, op string, db *mongo.Database, cmd bson.D, limit int, start time.Time, source *driver.CollectionRef) (driver.ResultSet, error) {
	name := cmd[0].Key
	if explain, _ := lookup(cmd, "explain"); cursorCommands[name] && explain != true {
		cur, err := db.RunCommandCursor(ctx, cmd)
		if err != nil {
			return nil, mapError(ctx, op, err)
		}
		return newDocResult(ctx, op, cur, nil, limit, start, -1, source)
	}
	reply, err := db.RunCommand(ctx, cmd).Raw()
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if err := replyWriteError(op, reply); err != nil {
		return nil, err
	}
	docs, err := replyDocs(name, cmd, reply)
	if err != nil {
		return nil, driver.Wrap(nil, Name, op, err)
	}
	return newDocResult(ctx, op, nil, docs, limit, start, rowsAffected(name, reply), nil)
}

// replyWriteError turns the first write error (or write concern error) a
// write command reports in an ok reply into an error.
func replyWriteError(op string, reply bson.Raw) error {
	var first bson.RawValue
	if arr, ok := reply.Lookup("writeErrors").ArrayOK(); ok {
		if vals, _ := arr.Values(); len(vals) > 0 {
			first = vals[0]
		}
	}
	if first.IsZero() {
		if wce := reply.Lookup("writeConcernError"); !wce.IsZero() {
			first = wce
		}
	}
	doc, ok := first.DocumentOK()
	if !ok {
		return nil
	}
	code, _ := doc.Lookup("code").AsInt64OK()
	msg, _ := doc.Lookup("errmsg").StringValueOK()
	return &driver.Error{Kind: kindForCode(int(code)), Driver: Name, Op: op, Code: fmt.Sprint(code), Err: errors.New(msg)}
}

// replyFields are reply fields that describe the call, not its result.
var replyFields = map[string]bool{"ok": true, "$clusterTime": true, "operationTime": true}

// replyDocs returns the rows of a single-document reply: one per value for
// distinct, else the reply itself without replyFields.
func replyDocs(name string, cmd bson.D, reply bson.Raw) ([]bson.Raw, error) {
	if name == "distinct" {
		key, _ := lookup(cmd, "key")
		col, _ := key.(string)
		if col == "" {
			col = "value"
		}
		vals, err := reply.Lookup("values").Array().Values()
		if err != nil {
			return nil, err
		}
		out := make([]bson.Raw, 0, len(vals))
		for _, v := range vals {
			b, err := bson.Marshal(bson.D{{Key: col, Value: v}})
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		return out, nil
	}
	elems, err := reply.Elements()
	if err != nil {
		return nil, err
	}
	d := make(bson.D, 0, len(elems))
	for _, e := range elems {
		if !replyFields[e.Key()] {
			d = append(d, bson.E{Key: e.Key(), Value: e.Value()})
		}
	}
	b, err := bson.Marshal(d)
	if err != nil {
		return nil, err
	}
	return []bson.Raw{b}, nil
}

// rowsAffected reads n from a write command's reply; -1 for anything else.
func rowsAffected(name string, reply bson.Raw) int64 {
	switch name {
	case "insert", "update", "delete":
		if n, ok := reply.Lookup("n").AsInt64OK(); ok {
			return n
		}
	case "findAndModify", "findandmodify":
		if n, ok := reply.Lookup("lastErrorObject", "n").AsInt64OK(); ok {
			return n
		}
	}
	return -1
}

// docResult is a ResultSet over documents: those already read (buf), then
// the rest of a server cursor (cur, nil when there is none). Its columns
// come from the documents read before the first row is returned, up to
// prefetchMax (or the limit). It is safe for a single consumer.
type docResult struct {
	op       string
	cur      *mongo.Cursor
	buf      []bson.Raw
	cols     []driver.Column
	index    map[string]int
	limit    int
	returned int
	omitted  int
	window   int  // documents the columns come from
	budget   bool // the prefetch stopped at maxResultBytes
	start    time.Time
	stats    driver.Stats
	done     bool
}

func newDocResult(ctx context.Context, op string, cur *mongo.Cursor, buf []bson.Raw, limit int, start time.Time, affected int64, source *driver.CollectionRef) (driver.ResultSet, error) {
	r := &docResult{op: op, cur: cur, buf: buf, limit: limit, start: start, stats: driver.Stats{RowsAffected: affected}}
	want := prefetchMax
	if limit > 0 && limit+1 < want {
		want = limit + 1
	}
	held := 0
	for _, d := range r.buf {
		held += len(d)
	}
	for r.cur != nil && len(r.buf) < want {
		if held >= maxResultBytes {
			r.budget = true
			break
		}
		if !r.cur.Next(ctx) {
			err := r.cur.Err()
			r.closeCursor()
			if err != nil {
				return nil, mapError(ctx, op, err)
			}
			break
		}
		r.buf = append(r.buf, append(bson.Raw(nil), r.cur.Current...))
		held += len(r.cur.Current)
	}
	shown := r.buf
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	r.window = len(shown)
	r.cols = inferColumns(shown)
	r.index = columnIndex(r.cols)
	if i, ok := r.index["_id"]; ok && source != nil {
		for k := range r.cols {
			r.cols[k].Source = source
		}
		r.cols[i].Key = true
	}
	return r, nil
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
		more := len(r.buf) > 0 || (r.cur != nil && r.cur.Next(ctx))
		r.stats.Truncated = more
		r.finish()
		return nil, false, nil
	}
	var doc bson.Raw
	switch {
	case len(r.buf) > 0:
		doc, r.buf = r.buf[0], r.buf[1:]
	case r.cur != nil:
		if !r.cur.Next(ctx) {
			err := r.cur.Err()
			r.finish()
			if err != nil {
				return nil, false, mapError(ctx, r.op, err)
			}
			return nil, false, nil
		}
		doc = r.cur.Current
	default:
		r.finish()
		return nil, false, nil
	}
	row, omitted := rowOf(doc, r.index, len(r.cols))
	r.omitted += omitted
	r.returned++
	return row, true, nil
}

// finish ends the result: it closes the cursor and fixes the stats.
func (r *docResult) finish() {
	if r.done {
		return
	}
	r.done = true
	r.buf = nil
	r.closeCursor()
	r.stats.Duration = time.Since(r.start)
	if r.omitted > 0 {
		why := ""
		if r.budget {
			why = " (the " + budgetText() + " read-ahead limit)"
		}
		r.stats.Warnings = append(r.stats.Warnings, fmt.Sprintf(
			"%d fields of later documents have no column: columns come from the first %d documents%s; add a projection to see them",
			r.omitted, r.window, why))
	}
}

func (r *docResult) closeCursor() {
	if r.cur == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cursorCloseTimeout)
	defer cancel()
	_ = r.cur.Close(ctx)
	r.cur = nil
}

func (r *docResult) Stats() driver.Stats {
	st := r.stats
	if !r.done {
		st.Duration = time.Since(r.start)
	}
	return st
}

// Close releases the server cursor early. It is idempotent.
func (r *docResult) Close() error {
	r.finish()
	return nil
}
