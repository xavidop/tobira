package milvus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowse = 100   // Browse page when Limit is 0
	maxBrowse     = 1000  // cap on a Browse page
	maxSortScan   = 10000 // entities a sorted Browse may read
)

// browseCursor is the position of a Browse page. Unsorted pages follow the
// primary key ("k|i:<int>" or "k|s:<text>"): the next page starts after
// it. Sorted pages, which tobira sorts itself, skip the rows already shown
// ("s|<a or d>|<n>|<column>"); such a cursor names its column and
// direction, so it is refused by a browse with another sort. Texts are
// query-escaped.
type browseCursor struct {
	sorted bool
	field  string
	desc   bool
	offset int
	after  driver.Value // keyset: the last key shown (NULL = start)
}

func (c browseCursor) encode() string {
	switch {
	case c.sorted:
		dir := "a"
		if c.desc {
			dir = "d"
		}
		return "s|" + dir + "|" + strconv.Itoa(c.offset) + "|" + url.QueryEscape(c.field)
	case c.after.Type() == driver.TypeInt:
		n, _ := c.after.AsInt()
		return "k|i:" + strconv.FormatInt(n, 10)
	case c.after.Type() == driver.TypeString:
		s, _ := c.after.AsText()
		return "k|s:" + url.QueryEscape(s)
	}
	return ""
}

func decodeCursor(s string) (browseCursor, error) {
	bad := func() (browseCursor, error) {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor"))
	}
	if s == "" {
		return browseCursor{}, nil
	}
	f := strings.Split(s, "|")
	switch {
	case f[0] == "k" && len(f) == 2:
		switch {
		case strings.HasPrefix(f[1], "i:"):
			n, err := strconv.ParseInt(f[1][2:], 10, 64)
			if err != nil {
				return bad()
			}
			return browseCursor{after: driver.Int(n)}, nil
		case strings.HasPrefix(f[1], "s:"):
			v, err := url.QueryUnescape(f[1][2:])
			if err != nil {
				return bad()
			}
			return browseCursor{after: driver.String(v)}, nil
		}
	case f[0] == "s" && len(f) == 4:
		n, err := strconv.Atoi(f[2])
		field, ferr := url.QueryUnescape(f[3])
		if (f[1] != "a" && f[1] != "d") || err != nil || n < 0 || n > maxSortScan || ferr != nil || field == "" {
			return bad()
		}
		return browseCursor{sorted: true, field: field, desc: f[1] == "d", offset: n}, nil
	}
	return bad()
}

// matches checks that the cursor came from a browse with this sort ("" =
// key order).
func (c browseCursor) matches(field string, desc bool) error {
	if c.sorted == (field != "") && (!c.sorted || (c.field == field && c.desc == desc)) {
		return nil
	}
	return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("the cursor belongs to a browse with another sort"))
}

// browseSort returns the column to sort by ("" = primary-key order, which
// Milvus serves itself).
func browseSort(keys []driver.SortKey, s *collSchema) (string, bool, error) {
	switch {
	case len(keys) == 0:
		return "", false, nil
	case len(keys) > 1:
		return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("milvus browsing sorts by one field at a time"))
	}
	k := keys[0]
	if f, ok := s.field(k.Field); ok {
		switch {
		case f.pk && !k.Desc:
			return "", false, nil
		case f.isVector():
			return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("cannot sort by vector %q", k.Field))
		}
		return k.Field, k.Desc, nil
	}
	if !s.dynamic {
		return "", false, driver.Wrap(driver.ErrNotFound, Name, "browse", fmt.Errorf("collection %s has no field %q", s.name, k.Field))
	}
	return k.Field, k.Desc, nil
}

// Browse returns one page of entities with every field. Unsorted, it
// follows the primary key, which Milvus orders itself. Sorted by any other
// field (or the key descending), tobira reads the matching entities, at
// most 10,000 of them, and sorts them: NULLs come last whichever the
// direction, and ties keep key order.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "browse"); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowse
	}
	limit = min(limit, maxBrowse)
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	s, err := c.getSchema(ctx, "browse", c.db(ref.Namespace), ref.Name)
	if err != nil {
		return nil, err
	}
	readOnly := c.readOnly || opts.ReadOnly
	params := exprParams{}
	filter, err := buildFilter(opts.Filter, s, readOnly, params)
	if err != nil {
		return nil, err
	}
	sortCol, desc, err := browseSort(opts.Sort, s)
	if err != nil {
		return nil, err
	}
	if opts.Cursor != "" {
		if err := cur.matches(sortCol, desc); err != nil {
			return nil, err
		}
	}
	sp := scanSpec{s: s, op: "browse", filter: filter, params: params, consistency: commonpb.ConsistencyLevel_Strong, readOnly: readOnly}
	if sortCol == "" {
		recs, err := c.scanPage(ctx, sp, cur.after, 0, limit+1)
		if err != nil {
			return nil, err
		}
		var next browseCursor
		if len(recs) > limit {
			recs = recs[:limit]
			next.after = recs[limit-1].values[s.pkField().name]
		}
		sh := newShape(s, staticFields(s, nil), "", recs)
		return driver.NewSliceResultSet(sh.columns(recs), sh.rows(recs, nil), driver.Stats{RowsAffected: -1, Duration: time.Since(start), NextCursor: next.encode()}), nil
	}
	recs, err := c.scanAll(ctx, sp)
	if err != nil {
		return nil, err
	}
	sh := newShape(s, staticFields(s, nil), "", recs)
	cols := sh.columns(recs)
	rows := sh.rows(recs, nil)
	idx := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == sortCol })
	if idx >= 0 { // else no entity has the key: key order stays
		sortRows(rows, idx, 0, desc)
	}
	from := min(cur.offset, len(rows))
	to := min(from+limit, len(rows))
	var next browseCursor
	if to < len(rows) {
		next = browseCursor{sorted: true, field: sortCol, desc: desc, offset: to}
	}
	return driver.NewSliceResultSet(cols, rows[from:to], driver.Stats{RowsAffected: -1, Duration: time.Since(start), NextCursor: next.encode()}), nil
}

// scanAll reads every entity the scan matches, in key order, refusing to
// read more than maxSortScan.
func (c *conn) scanAll(ctx context.Context, sp scanSpec) ([]record, error) {
	var all []record
	after := driver.Null()
	pk := sp.s.pkField().name
	for {
		recs, err := c.scanPage(ctx, sp, after, 0, pageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
		if len(all) > maxSortScan {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("sorting reads at most %d entities and more match: add a filter, or browse in key order", maxSortScan))
		}
		if len(recs) < pageSize {
			return all, nil
		}
		after = recs[len(recs)-1].values[pk]
	}
}

// sortRows orders rows by column idx: values of one kind by value (in the
// direction asked), kinds in a fixed order (numbers, text, booleans,
// times, others), NULLs last; ties by column key, ascending.
func sortRows(rows []driver.Row, idx, key int, desc bool) {
	rank := func(v driver.Value) int {
		switch v.Type() {
		case driver.TypeInt, driver.TypeFloat, driver.TypeDecimal:
			return 0
		case driver.TypeString:
			return 1
		case driver.TypeBool:
			return 2
		case driver.TypeTime:
			return 3
		case driver.TypeNull:
			return 5
		}
		return 4
	}
	slices.SortStableFunc(rows, func(a, b driver.Row) int {
		va, vb := a[idx], b[idx]
		ra, rb := rank(va), rank(vb)
		if ra != rb {
			return ra - rb
		}
		c := compareValues(va, vb)
		if desc {
			c = -c
		}
		if c != 0 {
			return c
		}
		return compareValues(a[key], b[key])
	})
}

// compareValues orders two values of the same rank.
func compareValues(a, b driver.Value) int {
	switch {
	case a.IsNull() || b.IsNull():
		return 0
	case a.Type() == driver.TypeString || b.Type() == driver.TypeString:
		sa, _ := a.AsText()
		sb, _ := b.AsText()
		return strings.Compare(sa, sb)
	case a.Type() == driver.TypeBool:
		ba, _ := a.AsBool()
		bb, _ := b.AsBool()
		switch {
		case ba == bb:
			return 0
		case !ba:
			return -1
		}
		return 1
	case a.Type() == driver.TypeTime:
		ta, _ := a.AsTime()
		tb, _ := b.AsTime()
		return ta.Compare(tb)
	case a.Type() == driver.TypeInt && b.Type() == driver.TypeInt:
		ia, _ := a.AsInt()
		ib, _ := b.AsInt()
		return cmp.Compare(ia, ib)
	}
	fa, oka := a.AsFloat()
	fb, okb := b.AsFloat()
	if oka && okb {
		return cmp.Compare(fa, fb)
	}
	return strings.Compare(a.String(), b.String())
}
