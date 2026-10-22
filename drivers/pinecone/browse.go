package pinecone

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"

	"github.com/xavidop/tobira/driver"
)

const (
	// maxScan caps the records one unsorted Browse page examines while it
	// filters: past it, the page ends early and its cursor resumes the
	// scan.
	maxScan = 10000
	// maxSort caps the namespace size a sorted Browse reads and sorts.
	maxSort = 10000
)

// browseCursor is the position of a Browse page. An unsorted page follows
// the list pagination token: "l|<skip>|<token>", where skip counts the ids
// of that list page already shown (a filtered page may stop mid-page). A
// sorted page is an offset into the sorted namespace: "s|<a or d>|<offset>|
// <key>"; it names its key and direction, so a browse with another sort
// refuses it. The token and the key are query-escaped.
type browseCursor struct {
	sorted bool
	key    string // sorted: the field
	desc   bool   // sorted: the direction
	offset int    // sorted: rows already shown
	token  string // unsorted: the list page to read ("" = the start)
	skip   int    // unsorted: ids of that list page already examined
}

func (c browseCursor) encode() string {
	switch {
	case c.sorted:
		dir := "a"
		if c.desc {
			dir = "d"
		}
		return "s|" + dir + "|" + strconv.Itoa(c.offset) + "|" + url.QueryEscape(c.key)
	case c.token != "" || c.skip > 0:
		return "l|" + strconv.Itoa(c.skip) + "|" + url.QueryEscape(c.token)
	}
	return ""
}

func decodeCursor(s string) (browseCursor, error) {
	if s == "" {
		return browseCursor{}, nil
	}
	bad := func() (browseCursor, error) {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor"))
	}
	f := strings.SplitN(s, "|", 4)
	switch {
	case f[0] == "l" && len(f) == 3:
		skip, err := strconv.Atoi(f[1])
		tok, err2 := url.QueryUnescape(f[2])
		if err != nil || err2 != nil || skip < 0 {
			return bad()
		}
		return browseCursor{token: tok, skip: skip}, nil
	case f[0] == "s" && len(f) == 4:
		off, err := strconv.Atoi(f[2])
		key, err2 := url.QueryUnescape(f[3])
		if err != nil || err2 != nil || off < 0 || key == "" || (f[1] != "a" && f[1] != "d") {
			return bad()
		}
		return browseCursor{sorted: true, desc: f[1] == "d", offset: off, key: key}, nil
	}
	return bad()
}

// Browse returns one page of records with their vectors and metadata.
// Pinecone lists record ids in its own order and neither filters nor sorts
// a listing, so tobira does both while it reads: an unsorted page lists and
// fetches ids, keeping the records that match the filter (examining at most
// 10,000 per page); a sorted page reads the whole namespace (at most 10,000
// records, or ErrUnsupported), filters and sorts it, records without the
// field last.
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
	if strings.TrimSpace(opts.Filter.Raw) != "" {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New(`a listing takes no Pinecone filter: use field conditions, or a {"op":"query","filter":...} request`))
	}
	if err := checkConditions(opts.Filter.Conditions, "browse"); err != nil {
		return nil, err
	}
	key, desc, err := browseSort(opts.Sort)
	if err != nil {
		return nil, err
	}
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	if opts.Cursor != "" && (cur.sorted != (key != "") || (cur.sorted && (cur.key != key || cur.desc != desc))) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("the cursor belongs to a browse with another sort"))
	}
	h, info, err := c.collection(ctx, "browse", ref, true)
	if err != nil {
		return nil, err
	}
	ic := h.ns(pineconeNamespace(ref.Name))
	var (
		recs     []record
		next     browseCursor
		warnings []string
	)
	if key == "" {
		recs, next, warnings, err = c.scanPage(ctx, ic, opts.Filter.Conditions, cur, limit)
	} else {
		if n := info.namespaces[ref.Name]; n > maxSort {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "browse",
				fmt.Errorf("sorting reads the whole namespace on the client, and %q holds %d records (the cap is %d): browse unsorted, or query with a filter", ref.Name, n, maxSort))
		}
		recs, next, err = c.sortedPage(ctx, ic, opts.Filter.Conditions, key, desc, cur.offset, limit)
	}
	if err != nil {
		return nil, err
	}
	src := &driver.CollectionRef{Namespace: h.name, Name: ref.Name}
	sh := newShape(recs, shapeOptions{values: true, source: src})
	rows := make([]driver.Row, len(recs))
	for i, r := range recs {
		rows[i], _ = sh.row(r)
	}
	return driver.NewSliceResultSet(sh.columns(), rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start), NextCursor: next.encode(), Warnings: append(warnings, sh.warnings()...)}), nil
}

// browseSort returns the field to order by ("" = list order).
func browseSort(keys []driver.SortKey) (string, bool, error) {
	switch {
	case len(keys) == 0:
		return "", false, nil
	case len(keys) > 1:
		return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("pinecone browses order by one field at a time"))
	case keys[0].Field == valuesColumn || keys[0].Field == sparseColumn || keys[0].Field == "":
		return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("cannot order by %q", keys[0].Field))
	}
	return keys[0].Field, keys[0].Desc, nil
}

// scanPage reads one unsorted page: list pages from the cursor, fetched
// and filtered, until limit records match, the namespace ends, or maxScan
// records were examined.
func (c *conn) scanPage(ctx context.Context, ic *pinecone.IndexConnection, conds []driver.Condition, cur browseCursor, limit int) ([]record, browseCursor, []string, error) {
	var out []record
	token, skip, scanned := cur.token, cur.skip, 0
	for {
		n := listPage
		if len(conds) == 0 {
			n = skip + limit - len(out) // without a filter every id is a row
		}
		ids, next, err := c.listPage(ctx, "browse", ic, "", n, token)
		if err != nil {
			return nil, browseCursor{}, nil, err
		}
		if skip > len(ids) {
			skip = len(ids)
		}
		page := ids[skip:]
		recs, err := c.fetch(ctx, "browse", ic, page)
		if err != nil {
			return nil, browseCursor{}, nil, err
		}
		byID := make(map[string]record, len(recs))
		for _, r := range recs {
			byID[r.id] = r
		}
		for i, id := range page {
			r, ok := byID[id]
			if !ok {
				continue // deleted between the list and the fetch
			}
			match, err := matchAll(conds, r)
			if err != nil {
				return nil, browseCursor{}, nil, err
			}
			if !match {
				continue
			}
			out = append(out, r)
			if len(out) == limit {
				switch {
				case skip+i+1 < len(ids):
					return out, browseCursor{token: token, skip: skip + i + 1}, nil, nil
				case next != "":
					return out, browseCursor{token: next}, nil, nil
				}
				return out, browseCursor{}, nil, nil
			}
		}
		scanned += len(page)
		if next == "" || len(ids) == 0 {
			return out, browseCursor{}, nil, nil
		}
		token, skip = next, 0
		if scanned >= maxScan {
			return out, browseCursor{token: token}, []string{fmt.Sprintf("examined %d records without filling the page: the next page continues the scan", scanned)}, nil
		}
	}
}

// sortedPage reads the namespace (at most maxSort records), filters and
// sorts it, and returns limit records from offset.
func (c *conn) sortedPage(ctx context.Context, ic *pinecone.IndexConnection, conds []driver.Condition, key string, desc bool, offset, limit int) ([]record, browseCursor, error) {
	var all []record
	token := ""
	for {
		ids, next, err := c.listPage(ctx, "browse", ic, "", listPage, token)
		if err != nil {
			return nil, browseCursor{}, err
		}
		recs, err := c.fetch(ctx, "browse", ic, ids)
		if err != nil {
			return nil, browseCursor{}, err
		}
		for _, r := range recs {
			match, err := matchAll(conds, r)
			if err != nil {
				return nil, browseCursor{}, err
			}
			if match {
				all = append(all, r)
			}
		}
		if len(all) > maxSort {
			return nil, browseCursor{}, driver.Wrap(driver.ErrUnsupported, Name, "browse",
				fmt.Errorf("sorting reads the whole namespace on the client, and it holds over %d records: browse unsorted, or query with a filter", maxSort))
		}
		if next == "" || len(ids) == 0 {
			break
		}
		token = next
	}
	sortRecords(all, key, desc)
	if offset > len(all) {
		offset = len(all)
	}
	end := min(offset+limit, len(all))
	var next browseCursor
	if end < len(all) {
		next = browseCursor{sorted: true, key: key, desc: desc, offset: end}
	}
	return all[offset:end], next, nil
}
