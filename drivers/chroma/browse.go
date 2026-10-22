package chroma

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

	"github.com/xavidop/tobira/driver"
)

// maxSort caps the records a sorted Browse reads: Chroma cannot order a
// get, so tobira sorts the matching records itself.
const maxSort = 10000

// sortPage is the get page size while a sorted Browse reads the matches.
const sortPage = 1000

// browseCursor is the position of a Browse page: an offset into Chroma's
// get order ("o|<n>"), or, when sorted, into tobira's order of the
// matching records ("s|<n>|<spec>"). A sorted cursor names its sort, so it
// is refused by a browse with another sort.
type browseCursor struct {
	sorted bool
	spec   string
	offset int
}

func (c browseCursor) encode() string {
	if c.sorted {
		return "s|" + strconv.Itoa(c.offset) + "|" + c.spec
	}
	return "o|" + strconv.Itoa(c.offset)
}

func decodeCursor(s string) (browseCursor, error) {
	bad := func() (browseCursor, error) {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor"))
	}
	if s == "" {
		return browseCursor{}, nil
	}
	f := strings.Split(s, "|")
	if len(f) < 2 {
		return bad()
	}
	n, err := strconv.Atoi(f[1])
	if err != nil || n < 0 {
		return bad()
	}
	switch {
	case f[0] == "o" && len(f) == 2:
		return browseCursor{offset: n}, nil
	case f[0] == "s" && len(f) == 3 && validSpec(f[2]):
		return browseCursor{sorted: true, spec: f[2], offset: n}, nil
	}
	return bad()
}

// sortSpec spells sort keys for a cursor: escaped field:direction items.
func sortSpec(keys []driver.SortKey) string {
	items := make([]string, len(keys))
	for i, k := range keys {
		dir := "a"
		if k.Desc {
			dir = "d"
		}
		items[i] = url.QueryEscape(k.Field) + ":" + dir
	}
	return strings.Join(items, ",")
}

func validSpec(spec string) bool {
	if spec == "" {
		return false
	}
	for _, item := range strings.Split(spec, ",") {
		field, dir, ok := strings.Cut(item, ":")
		name, err := url.QueryUnescape(field)
		if !ok || err != nil || name == "" || (dir != "a" && dir != "d") {
			return false
		}
	}
	return true
}

// matches checks that the cursor came from a browse with this sort ("" =
// unsorted).
func (c browseCursor) matches(spec string) error {
	if c.sorted == (spec != "") && c.spec == spec {
		return nil
	}
	return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("the cursor belongs to a browse with another sort"))
}

// Browse returns one page of records with their embeddings. Unsorted, it
// pages through Chroma's get order by offset. Sorted, it reads the
// matching records without embeddings (at most 10,000), sorts them
// (NULLs last in either direction, ties by id), and fetches the page's
// embeddings by id. Filters run on the server (see buildWhere).
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
	spec := ""
	if len(opts.Sort) > 0 {
		spec = sortSpec(opts.Sort)
	}
	if opts.Cursor != "" {
		if err := cur.matches(spec); err != nil {
			return nil, err
		}
	}
	w, err := buildWhere(opts.Filter)
	if err != nil {
		return nil, err
	}
	for _, k := range opts.Sort {
		if k.Field == embeddingColumn {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("cannot sort by the embedding"))
		}
	}
	info, err := c.collection(ctx, "browse", ref)
	if err != nil {
		return nil, err
	}
	var (
		recs []record
		next string
	)
	switch {
	case w.none():
	case spec == "":
		recs, next, err = c.plainPage(ctx, info, w, cur.offset, limit)
	default:
		recs, next, err = c.sortedPage(ctx, info, w, opts.Sort, spec, cur.offset, limit)
	}
	if err != nil {
		return nil, err
	}
	o := optsFor(info, []string{"documents", "metadatas", "embeddings"})
	for _, r := range recs {
		o.uris = o.uris || r.uri != nil
	}
	sh := newShape(recs, o)
	rows := make([]driver.Row, len(recs))
	for i, r := range recs {
		rows[i] = sh.row(r)
	}
	return driver.NewSliceResultSet(sh.columns(), rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start), NextCursor: next, Warnings: sh.warnings()}), nil
}

// plainPage reads limit records from offset, plus one to prove another
// page.
func (c *conn) plainPage(ctx context.Context, info *collInfo, w whereClause, offset, limit int) ([]record, string, error) {
	n := limit + 1
	resp, err := c.api.get(ctx, c.tenant, info.ref.Namespace, info.id, getRequest{
		IDs: w.ids, Where: w.where, WhereDocument: w.whereDocument, Limit: &n, Offset: &offset,
		Include: []string{"documents", "metadatas", "embeddings", "uris"},
	})
	if err != nil {
		return nil, "", c.fail(ctx, "browse", err)
	}
	recs, err := getRecords(resp)
	if err != nil {
		return nil, "", driver.Wrap(nil, Name, "browse", err)
	}
	if len(recs) > limit {
		return recs[:limit], browseCursor{offset: offset + limit}.encode(), nil
	}
	return recs, "", nil
}

// sortedPage reads every matching record without embeddings, sorts them
// and returns the page from offset with its embeddings.
func (c *conn) sortedPage(ctx context.Context, info *collInfo, w whereClause, keys []driver.SortKey, spec string, offset, limit int) ([]record, string, error) {
	var all []record
	for {
		n, off := sortPage, len(all)
		resp, err := c.api.get(ctx, c.tenant, info.ref.Namespace, info.id, getRequest{
			IDs: w.ids, Where: w.where, WhereDocument: w.whereDocument, Limit: &n, Offset: &off,
			Include: []string{"documents", "metadatas", "uris"},
		})
		if err != nil {
			return nil, "", c.fail(ctx, "browse", err)
		}
		recs, err := getRecords(resp)
		if err != nil {
			return nil, "", driver.Wrap(nil, Name, "browse", err)
		}
		all = append(all, recs...)
		if len(all) > maxSort {
			return nil, "", driver.Wrap(driver.ErrUnsupported, Name, "browse",
				fmt.Errorf("chroma cannot sort: tobira sorts at most %d records itself; filter the collection first", maxSort))
		}
		if len(recs) < n {
			break
		}
	}
	if err := sortRecords(all, keys); err != nil {
		return nil, "", err
	}
	if offset >= len(all) {
		return nil, "", nil
	}
	page := all[offset:min(offset+limit, len(all))]
	next := ""
	if offset+limit < len(all) {
		next = browseCursor{sorted: true, spec: spec, offset: offset + limit}.encode()
	}
	ids := make([]string, len(page))
	for i, r := range page {
		ids[i] = r.id
	}
	resp, err := c.api.get(ctx, c.tenant, info.ref.Namespace, info.id, getRequest{IDs: ids, Include: []string{"embeddings"}})
	if err != nil {
		return nil, "", c.fail(ctx, "browse", err)
	}
	embs := map[string][]float32{}
	for i, id := range resp.IDs {
		if i < len(resp.Embeddings) && resp.Embeddings[i] != nil {
			embs[id] = numbersToVector(resp.Embeddings[i])
		}
	}
	for i := range page {
		page[i].embedding = embs[page[i].id]
	}
	return page, next, nil
}

// fieldValue is a record's value for a column.
func fieldValue(r record, field string) driver.Value {
	switch field {
	case idColumn:
		return idValue(r.id)
	case documentColumn:
		return strOrNull(r.document)
	case uriColumn:
		return strOrNull(r.uri)
	}
	return metaValue(r.metadata[metaKey(field)])
}

// sortRecords orders records by keys: NULLs last in either direction,
// then by id ascending.
func sortRecords(recs []record, keys []driver.SortKey) error {
	for _, k := range keys {
		if k.Field == embeddingColumn {
			return driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("cannot sort by the embedding"))
		}
	}
	slices.SortStableFunc(recs, func(a, b record) int {
		for _, k := range keys {
			va, vb := fieldValue(a, k.Field), fieldValue(b, k.Field)
			switch {
			case va.IsNull() && vb.IsNull():
				continue
			case va.IsNull():
				return 1
			case vb.IsNull():
				return -1
			}
			if c := compareValues(va, vb); c != 0 {
				if k.Desc {
					return -c
				}
				return c
			}
		}
		return compareValues(idValue(a.id), idValue(b.id))
	})
	return nil
}

// typeRank orders values of different kinds: numbers, then bools, then
// text, then anything else.
func typeRank(v driver.Value) int {
	switch v.Type() {
	case driver.TypeInt, driver.TypeFloat:
		return 0
	case driver.TypeBool:
		return 1
	case driver.TypeString:
		return 2
	}
	return 3
}

func compareValues(a, b driver.Value) int {
	if ra, rb := typeRank(a), typeRank(b); ra != rb {
		return cmp.Compare(ra, rb)
	}
	switch typeRank(a) {
	case 0:
		if a.Type() == driver.TypeInt && b.Type() == driver.TypeInt {
			x, _ := a.AsInt()
			y, _ := b.AsInt()
			return cmp.Compare(x, y)
		}
		x, _ := a.AsFloat()
		y, _ := b.AsFloat()
		return cmp.Compare(x, y)
	case 1:
		x, _ := a.AsBool()
		y, _ := b.AsBool()
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	case 2:
		x, _ := a.AsText()
		y, _ := b.AsText()
		return strings.Compare(x, y)
	}
	return strings.Compare(a.String(), b.String())
}
