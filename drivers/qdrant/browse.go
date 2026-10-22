package qdrant

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc/codes"

	"github.com/xavidop/tobira/driver"
)

// browseCursor is the position of a Browse page. Unsorted pages follow the
// scroll's point-id offset ("o|<id>"). Sorted pages run in two phases:
// first the points ordered by the sort key, skipping the rows already shown
// ("s|<a or d>|<n>|<key>"), then the points without a value for it, which
// Qdrant's order_by leaves out ("n|<a or d>|<key>|<id>", the id empty at
// the start). A sorted cursor names its key and direction, so it is refused
// by a browse with another sort. Keys and ids are query-escaped.
type browseCursor struct {
	sorted bool
	key    string      // sorted: the payload key
	desc   bool        // sorted: the direction
	skip   uint64      // sorted phase: rows already returned
	nulls  bool        // sorted: in the no-value phase
	offset *pb.PointId // scroll offset (unsorted, or the no-value phase)
}

func (c browseCursor) dir() string {
	if c.desc {
		return "d"
	}
	return "a"
}

func (c browseCursor) encode() string {
	esc := url.QueryEscape
	switch {
	case c.sorted && c.nulls:
		return "n|" + c.dir() + "|" + esc(c.key) + "|" + esc(idString(c.offset))
	case c.sorted:
		return "s|" + c.dir() + "|" + strconv.FormatUint(c.skip, 10) + "|" + esc(c.key)
	case c.offset != nil:
		return "o|" + esc(idString(c.offset))
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
	field := func(i int) (string, bool) {
		v, err := url.QueryUnescape(f[i])
		return v, err == nil
	}
	dir := func(v string) (bool, bool) { return v == "d", v == "a" || v == "d" }
	switch {
	case f[0] == "o" && len(f) == 2:
		v, ok := field(1)
		id, okID := parseIDString(v)
		if !ok || !okID {
			return bad()
		}
		return browseCursor{offset: id}, nil
	case f[0] == "s" && len(f) == 4:
		desc, okDir := dir(f[1])
		n, err := strconv.ParseUint(f[2], 10, 64)
		key, okKey := field(3)
		if !okDir || err != nil || !okKey || key == "" {
			return bad()
		}
		return browseCursor{sorted: true, key: key, desc: desc, skip: n}, nil
	case f[0] == "n" && len(f) == 4:
		desc, okDir := dir(f[1])
		key, okKey := field(2)
		idText, okID := field(3)
		if !okDir || !okKey || key == "" || !okID {
			return bad()
		}
		c := browseCursor{sorted: true, key: key, desc: desc, nulls: true}
		if idText != "" {
			id, ok := parseIDString(idText)
			if !ok {
				return bad()
			}
			c.offset = id
		}
		return c, nil
	}
	return bad()
}

// matches checks that the cursor came from a browse with this sort ("" =
// unsorted).
func (c browseCursor) matches(key string, desc bool) error {
	if c.sorted == (key != "") && (!c.sorted || (c.key == key && c.desc == desc)) {
		return nil
	}
	return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("the cursor belongs to a browse with another sort"))
}

// Browse returns one page of points with their vectors and payload.
// Unsorted, it scrolls in point-id order. Sorted (one payload field, or the
// id ascending), it uses Qdrant's order_by, which needs a range payload
// index on the field (integer, float or datetime); points without a value
// for the field come last, in point-id order, whichever the direction.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "browse"); err != nil {
		return nil, err
	}
	if err := checkNamespace("browse", ref.Namespace); err != nil {
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
	schema, err := c.getSchema(ctx, "browse", ref.Name)
	if err != nil {
		return nil, err
	}
	filter, err := buildFilter(opts.Filter, schema)
	if err != nil {
		return nil, err
	}
	sortKey, desc, err := browseSort(opts.Sort, schema)
	if err != nil {
		return nil, err
	}
	if opts.Cursor != "" {
		if err := cur.matches(sortKey, desc); err != nil {
			return nil, err
		}
	}
	var (
		pts  []point
		next browseCursor
	)
	if sortKey == "" {
		pts, next.offset, err = c.scrollPage(ctx, ref.Name, filter, cur.offset, uint32(limit))
	} else {
		cur.sorted, cur.key, cur.desc = true, sortKey, desc
		pts, next, err = c.sortedPage(ctx, ref.Name, filter, cur, limit)
	}
	if err != nil {
		return nil, err
	}
	sh := newShape(schema, pts, true, "", nil)
	rows := make([]driver.Row, len(pts))
	for i, p := range pts {
		rows[i], _ = sh.row(p)
	}
	return driver.NewSliceResultSet(sh.columns(pts), rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start), NextCursor: next.encode()}), nil
}

// browseSort returns the payload key to order by ("" = point-id order).
func browseSort(keys []driver.SortKey, s *collSchema) (string, bool, error) {
	switch {
	case len(keys) == 0:
		return "", false, nil
	case len(keys) > 1:
		return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("qdrant orders by one field at a time"))
	case keys[0].Field == idColumn && !keys[0].Desc:
		return "", false, nil
	case keys[0].Field == idColumn:
		return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("qdrant scrolls point ids in ascending order only"))
	}
	if _, ok := s.vectorByColumn(keys[0].Field); ok {
		return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("cannot order by vector %q", keys[0].Field))
	}
	return s.payloadKey(keys[0].Field), keys[0].Desc, nil
}

// scrollPage reads up to limit points from offset; next is nil on the last
// page.
func (c *conn) scrollPage(ctx context.Context, coll string, filter *pb.Filter, offset *pb.PointId, limit uint32) ([]point, *pb.PointId, error) {
	resp, err := c.client.GetPointsClient().Scroll(ctx, &pb.ScrollPoints{
		CollectionName: coll,
		Filter:         filter,
		Offset:         offset,
		Limit:          &limit,
		WithPayload:    pb.NewWithPayload(true),
		WithVectors:    pb.NewWithVectors(true),
	})
	if err != nil {
		return nil, nil, mapError(ctx, "browse", err)
	}
	return retrievedPoints(resp.GetResult()), resp.GetNextPageOffset(), nil
}

// sortedPage reads one sorted page: ordered points first, then, once they
// run out, the points without a value for key.
func (c *conn) sortedPage(ctx context.Context, coll string, filter *pb.Filter, cur browseCursor, limit int) ([]point, browseCursor, error) {
	key := cur.key
	phase := func(nulls bool, skip uint64, offset *pb.PointId) browseCursor {
		return browseCursor{sorted: true, key: key, desc: cur.desc, nulls: nulls, skip: skip, offset: offset}
	}
	var pts []point
	if !cur.nulls {
		dir := pb.Direction_Asc
		if cur.desc {
			dir = pb.Direction_Desc
		}
		n := uint64(limit) + 1 // one extra proves another ordered page
		resp, err := c.client.GetPointsClient().Query(ctx, &pb.QueryPoints{
			CollectionName: coll,
			Query:          pb.NewQueryOrderBy(&pb.OrderBy{Key: key, Direction: &dir}),
			Filter:         filter,
			Offset:         &cur.skip,
			Limit:          &n,
			WithPayload:    pb.NewWithPayload(true),
			WithVectors:    pb.NewWithVectors(true),
		})
		if err != nil {
			return nil, browseCursor{}, sortError(ctx, key, err)
		}
		pts = scoredPoints(resp.GetResult())
		if len(pts) > limit {
			return pts[:limit], phase(false, cur.skip+uint64(limit), nil), nil
		}
		cur = phase(true, 0, nil)
	}
	// The no-value phase. With the page already full, peek one point so
	// the last page never ends with an empty one after it.
	room := uint32(max(limit-len(pts), 1))
	nf := &pb.Filter{Must: []*pb.Condition{pb.NewIsEmpty(key)}}
	if filter != nil {
		nf.Must = append(nf.Must, pb.NewFilterAsCondition(filter))
	}
	more, next, err := c.scrollPage(ctx, coll, nf, cur.offset, room)
	if err != nil {
		return nil, browseCursor{}, err
	}
	if len(pts) == limit {
		if len(more) == 0 {
			return pts, browseCursor{}, nil
		}
		return pts, phase(true, 0, nil), nil
	}
	pts = append(pts, more...)
	if next == nil {
		return pts, browseCursor{}, nil
	}
	return pts, phase(true, 0, next), nil
}

// sortError explains the error Qdrant gives for order_by on a field
// without a range index.
func sortError(ctx context.Context, key string, err error) error {
	err = mapError(ctx, "browse", err)
	// Qdrant has no dedicated code for it: an InvalidArgument whose text
	// names the missing range index (checked against the pinned server by
	// TestBrowseSortedPaging).
	var de *driver.Error
	if errors.As(err, &de) && de.Code == codes.InvalidArgument.String() && strings.Contains(de.Err.Error(), "No range index") {
		return driver.Wrap(driver.ErrUnsupported, Name, "browse",
			fmt.Errorf("sorting by %q needs a range payload index (integer, float or datetime) on it", key))
	}
	return err
}
