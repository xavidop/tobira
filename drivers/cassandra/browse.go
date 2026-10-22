package cassandra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	maxBrowseLimit     = 10000
	// maxCursorLen bounds a decoded cursor.
	maxCursorLen = 1 << 16
)

// maxSortRows bounds a sorted Browse: CQL sorts only by clustering columns
// inside one partition, so tobira sorts the (filtered) table itself, and
// refuses one larger than this (a variable so tests can lower it).
var maxSortRows = 10000

// Browse pages through a table or view with SELECT *. Conditions become
// WHERE relations with bound values, Filter.Raw (a CQL WHERE body) is ANDed
// after them, and any filter adds ALLOW FILTERING: browsing is exploration,
// and Cassandra refuses most filters without it (it can be slow on a large
// table; filter on the partition key to stay fast).
//
// Without Sort, a page is one server page of Limit rows and the cursor is
// the server's paging state (bound to the statement, values and sort it
// came from: a cursor replayed elsewhere is ErrSyntax), so pages follow the table's token and
// clustering order. With a Sort, which CQL cannot do across partitions,
// tobira reads the whole filtered table (at most maxSortRows rows, else
// ErrUnsupported), sorts it with NULLs last and pages by offset.
//
// Operators: eq, lt, le, gt, ge and in as =, <, <=, >, >= and IN; contains
// on a list, set or map column as CONTAINS. ne, prefix, is_null, not_null
// and contains on another column are ErrUnsupported: CQL has no such
// relations without a SASI or SAI index.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	limit = min(limit, maxBrowseLimit)
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	sorted := len(opts.Sort) > 0
	if sorted && cur.state != nil || !sorted && cur.offset > 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the cursor is for another sort: start from the first page"))
	}
	sch, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	text, args, err := buildSelect(sch, opts.Filter)
	if err != nil {
		return nil, err
	}
	tag := cursorTag(text, args, opts.Sort)
	if err := cur.check(tag); err != nil {
		return nil, err
	}
	if opts.ReadOnly || c.readOnly {
		if err := c.guard(ctx, text); err != nil {
			return nil, err
		}
	}
	source := &sch.Ref
	if sorted {
		return c.browseSorted(ctx, text, args, opts.Sort, limit, cur.offset, tag, start, source, sch.PrimaryKey)
	}
	iter := c.session.Query(text, anyParams(args)...).Idempotent(true).PageSize(limit).PageState(cur.state).Prefetch(0).IterContext(ctx)
	r, err := newRowResult(ctx, op, iter, 0, start, source, sch.PrimaryKey)
	if err != nil {
		return nil, err
	}
	// One page only: the rows of this frame, then the paging state of the
	// next one.
	r.max = iter.NumRows()
	if state := iter.PageState(); len(state) > 0 {
		r.stats.NextCursor = browseCursor{state: state, tag: tag}.encode()
		r.stats.Truncated = true
	}
	return r, nil
}

// browseSorted reads the filtered table, sorts it and returns one page.
func (c *conn) browseSorted(ctx context.Context, text string, args []param, keys []driver.SortKey, limit, offset int, tag uint64,
	start time.Time, source *driver.CollectionRef, pk []string) (driver.ResultSet, error) {
	const op = "browse"
	iter := c.session.Query(text, anyParams(args)...).Idempotent(true).PageSize(defaultPageSize).Prefetch(0).IterContext(ctx)
	rs, err := newRowResult(ctx, op, iter, maxSortRows, start, source, pk)
	if err != nil {
		return nil, err
	}
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	if st.Truncated {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf(
			"sorting needs the whole table in memory and it has more than %d rows: filter it first, or sort by the clustering columns in a CQL query on one partition", maxSortRows))
	}
	if err := sortRows(rs.cols, rows, keys); err != nil {
		return nil, err
	}
	page := rows[min(offset, len(rows)):]
	next := ""
	if len(page) > limit {
		page = page[:limit]
		next = browseCursor{offset: offset + limit, tag: tag}.encode()
	}
	out := driver.NewSliceResultSet(rs.cols, page, driver.Stats{
		RowsAffected: -1, Duration: time.Since(start), NextCursor: next, Truncated: next != "", Warnings: st.Warnings,
	})
	return out, nil
}

// sortRows sorts rows (with columns cols) stably by keys, NULLs last in
// either direction.
func sortRows(cols []driver.Column, rows []driver.Row, keys []driver.SortKey) error {
	idx := make([]int, len(keys))
	for i, k := range keys {
		idx[i] = slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == k.Field })
		if idx[i] < 0 {
			return driver.Wrap(driver.ErrNotFound, Name, "browse", fmt.Errorf("no column %q to sort by", k.Field))
		}
	}
	slices.SortStableFunc(rows, func(a, b driver.Row) int {
		for i, k := range keys {
			x, y := a[idx[i]], b[idx[i]]
			switch {
			case x.IsNull() && y.IsNull():
				continue
			case x.IsNull():
				return 1
			case y.IsNull():
				return -1
			}
			c := compareValues(x, y)
			if k.Desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
	return nil
}

// compareValues orders two non-NULL values of one column: numbers
// numerically and exactly, times as instants, bools false first, text and
// bytes bytewise, anything else by its display text.
func compareValues(x, y driver.Value) int {
	if rx, ok := numeric(x); ok {
		if ry, ok := numeric(y); ok {
			return rx.Cmp(ry)
		}
	}
	if tx, ok := x.AsTime(); ok {
		if ty, ok := y.AsTime(); ok {
			return tx.Compare(ty)
		}
	}
	if bx, ok := x.AsBool(); ok {
		if by, ok := y.AsBool(); ok {
			switch {
			case bx == by:
				return 0
			case by:
				return -1
			}
			return 1
		}
	}
	if bx, ok := x.AsBytes(); ok {
		if by, ok := y.AsBytes(); ok {
			return bytes.Compare(bx, by)
		}
	}
	return strings.Compare(x.String(), y.String())
}

// numeric returns an Int, Float or Decimal exactly.
func numeric(v driver.Value) (*big.Rat, bool) {
	switch v.Type() {
	case driver.TypeInt:
		n, _ := v.AsInt()
		return new(big.Rat).SetInt64(n), true
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(f), true
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return new(big.Rat).SetString(s)
	}
	return nil, false
}

// anyParams turns bound values into gocql arguments.
func anyParams(ps []param) []any {
	out := make([]any, len(ps))
	for i, p := range ps {
		out[i] = p
	}
	return out
}

// relations maps the comparison operators to CQL.
var relations = map[driver.Op]string{
	driver.OpEq: "=", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">=",
}

// buildSelect builds the Browse statement for sch and f, with its bound
// values. A condition on a column the table lacks is ErrNotFound.
func buildSelect(sch *driver.CollectionSchema, f driver.Filter) (string, []param, error) {
	var (
		where []string
		args  []param
	)
	for _, cond := range f.Conditions {
		i := slices.IndexFunc(sch.Fields, func(fd driver.Field) bool { return fd.Name == cond.Field })
		if i < 0 {
			return "", nil, driver.Wrap(driver.ErrNotFound, Name, "browse", fmt.Errorf("no column %q", cond.Field))
		}
		field := sch.Fields[i]
		col := quoteIdent(cond.Field)
		switch cond.Op {
		case driver.OpIn:
			list, ok := cond.Value.AsList()
			if !ok {
				return "", nil, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("in on %q needs a list", cond.Field))
			}
			marks := make([]string, len(list))
			for j, v := range list {
				marks[j] = "?"
				args = append(args, param{v})
			}
			where = append(where, col+" IN ("+strings.Join(marks, ", ")+")")
		case driver.OpContains:
			if field.Type != driver.TypeList && field.Type != driver.TypeObject {
				return "", nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf(
					"contains on %q (%s): CQL matches substrings only with a SASI or SAI index; contains works on list, set and map columns", cond.Field, field.NativeType))
			}
			where = append(where, col+" CONTAINS ?")
			args = append(args, param{cond.Value})
		default:
			rel, ok := relations[cond.Op]
			if !ok {
				return "", nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("filter operator %q: CQL has no such relation", cond.Op))
			}
			where = append(where, col+" "+rel+" ?")
			args = append(args, param{cond.Value})
		}
	}
	if raw := strings.TrimSpace(f.Raw); raw != "" {
		if err := checkRawFilter(raw); err != nil {
			return "", nil, err
		}
		where = append(where, raw+"\n")
	}
	text := "SELECT * FROM " + quoteIdent(sch.Ref.Namespace) + "." + quoteIdent(sch.Ref.Name)
	if len(where) > 0 {
		text += " WHERE " + strings.Join(where, " AND ") + " ALLOW FILTERING"
	}
	return text, args, nil
}

// browseCursor is a decoded Browse cursor: the server's paging state, or
// the offset of a sorted page, with the tag of the statement it belongs
// to (cursorTag).
type browseCursor struct {
	state  []byte
	offset int
	tag    uint64
}

const (
	statePrefix  = "p:"
	offsetPrefix = "o:"
	tagLen       = 8
)

// encode spells the cursor: "p:" and the base64url of the tag (8 bytes,
// big-endian) followed by the paging state, or "o:", the tag in hex, ":"
// and the offset. The first page's cursor is "".
func (b browseCursor) encode() string {
	switch {
	case len(b.state) > 0:
		return statePrefix + base64.RawURLEncoding.EncodeToString(append(binary.BigEndian.AppendUint64(nil, b.tag), b.state...))
	case b.offset > 0:
		return offsetPrefix + fmt.Sprintf("%016x:%d", b.tag, b.offset)
	}
	return ""
}

// check refuses a cursor made for another statement, values or sort: the
// server would read its paging state against the wrong query.
func (b browseCursor) check(tag uint64) error {
	if (len(b.state) > 0 || b.offset > 0) && b.tag != tag {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("the cursor is for another table, filter or sort: start from the first page"))
	}
	return nil
}

// cursorTag identifies a Browse statement: its text, its bound values
// (type and value) and its sort.
func cursorTag(text string, args []param, sort []driver.SortKey) uint64 {
	h := sha256.New()
	h.Write([]byte(text))
	for _, a := range args {
		js, _ := a.v.MarshalJSON()
		_, _ = fmt.Fprintf(h, "\x00%d:%s", a.v.Type(), js) // a hash never fails to write
	}
	for _, k := range sort {
		_, _ = fmt.Fprintf(h, "\x01%s:%t", k.Field, k.Desc)
	}
	return binary.BigEndian.Uint64(h.Sum(nil))
}

func decodeCursor(s string) (browseCursor, error) {
	bad := func(msg string) (browseCursor, error) {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor: "+msg))
	}
	switch {
	case s == "":
		return browseCursor{}, nil
	case len(s) > maxCursorLen:
		return bad("too long")
	case strings.HasPrefix(s, offsetPrefix):
		hexTag, num, ok := strings.Cut(s[len(offsetPrefix):], ":")
		if !ok || len(hexTag) != 2*tagLen {
			return bad("bad offset")
		}
		tag, err := strconv.ParseUint(hexTag, 16, 64)
		if err != nil {
			return bad("bad offset")
		}
		n, err := strconv.Atoi(num)
		if err != nil || n < 0 {
			return bad("bad offset")
		}
		return browseCursor{offset: n, tag: tag}, nil
	case strings.HasPrefix(s, statePrefix):
		b, err := base64.RawURLEncoding.DecodeString(s[len(statePrefix):])
		if err != nil || len(b) <= tagLen {
			return bad("bad paging state")
		}
		return browseCursor{tag: binary.BigEndian.Uint64(b), state: b[tagLen:]}, nil
	}
	return bad("unknown form")
}
