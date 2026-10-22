package cosmos

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

const (
	// defaultPageSize is Browse's page size when BrowseOptions.Limit is 0.
	defaultPageSize = 100
	// maxBrowseFetches bounds the server pages one Browse call reads.
	maxBrowseFetches = 100
)

// Browse pages through a container with SELECT * FROM c, the filter as a
// WHERE clause and Sort as ORDER BY (several keys need a composite index on
// a real account). A page reads until it holds Limit items or
// maxResultBytes; the next page resumes from the server's continuation
// token. The cursor is that token plus how many items of its page were
// already shown (the emulator ignores the page size for ORDER BY), bound to
// this filter and sort.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	db, err := c.database(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	sql, params, err := browseSQL(opts.Filter, opts.Sort, opts.ReadOnly || c.readOnly)
	if err != nil {
		return nil, err
	}
	src := driver.CollectionRef{Namespace: db, Name: ref.Name}
	sig := browseSignature(src, sql, params)
	pos := browsePos{}
	if opts.Cursor != "" {
		if pos, err = decodeBrowseCursor(opts.Cursor, sig); err != nil {
			return nil, err
		}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageItems)
	cc, err := c.container(op, db, ref.Name)
	if err != nil {
		return nil, err
	}
	props, err := c.containerProps(ctx, db, ref.Name, cc)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	items, next, charge, err := browseItems(ctx, queryPages(cc, sql, params), pos, limit)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	cols := inferColumns(items)
	_, objects := objectsOf(items)
	index := columnIndex(cols)
	for k := range cols {
		cols[k].Source = &src
	}
	for _, key := range keyFields(props) {
		if i, ok := index[key]; ok {
			cols[i].Key = true
		}
	}
	rows := make([]driver.Row, len(items))
	omitted := 0
	for i, it := range items {
		var n int
		rows[i], n = rowOf(it, index, len(cols), !objects)
		omitted += n
	}
	st := driver.Stats{RowsAffected: -1, Duration: time.Since(start)}
	if next != nil {
		st.NextCursor = encodeBrowseCursor(sig, *next)
		st.Truncated = true
	}
	if charge > 0 {
		st.Notices = append(st.Notices, "request charge: "+strconv.FormatFloat(charge, 'f', 2, 64)+" RU")
	}
	return driver.NewSliceResultSet(cols, rows, st), nil
}

// browsePos is where a Browse page starts: the continuation token of the
// server page it begins in ("" for the first) and how many of that page's
// items earlier Browse pages already showed.
type browsePos struct {
	Token string `json:"t"`
	Skip  int    `json:"k"`
}

// pageFetch reads one server page from a continuation token ("" for the
// first page), asking for at most hint items; next is "" after the last.
type pageFetch func(ctx context.Context, token string, hint int) (items [][]byte, next string, charge float64, err error)

// queryPages fetches pages of a cross-partition query.
func queryPages(cc *azcosmos.ContainerClient, sql string, params []azcosmos.QueryParameter) pageFetch {
	return func(ctx context.Context, token string, hint int) ([][]byte, string, float64, error) {
		opts := &azcosmos.QueryOptions{QueryParameters: params, PageSizeHint: int32(min(hint, maxPageItems))}
		if token != "" {
			opts.ContinuationToken = &token
		}
		page, err := cc.NewQueryItemsPager(sql, azcosmos.NewPartitionKey(), opts).NextPage(ctx)
		if err != nil {
			return nil, "", 0, err
		}
		next := ""
		if page.ContinuationToken != nil {
			next = *page.ContinuationToken
		}
		return page.Items, next, float64(page.RequestCharge), nil
	}
}

// browseItems reads up to limit items from pos. next is where the
// following page starts, nil at the end. When a page comes back with fewer
// items than pos.Skip (the service may shrink pages under its RU and time
// budgets), the whole page counts as shown and the rest of the skip
// carries over to the next page. That assumes the server returns the same
// items in the same order from a token, which holds for an unchanged
// container; only the emulator has exercised it.
func browseItems(ctx context.Context, fetch pageFetch, pos browsePos, limit int) (items []driver.Value, next *browsePos, charge float64, err error) {
	held := 0
	for range maxBrowseFetches {
		raws, token, ch, err := fetch(ctx, pos.Token, limit-len(items)+pos.Skip)
		if err != nil {
			return nil, nil, charge, err
		}
		charge += ch
		if pos.Skip >= len(raws) && pos.Skip > 0 {
			if token == "" {
				return items, nil, charge, nil
			}
			pos = browsePos{Token: token, Skip: pos.Skip - len(raws)}
			continue
		}
		raws = raws[pos.Skip:]
		for i, raw := range raws {
			if len(items) == limit || held >= maxResultBytes {
				return items, &browsePos{Token: pos.Token, Skip: pos.Skip + i}, charge, nil
			}
			v, err := decodeValue(raw)
			if err != nil {
				return nil, nil, charge, driver.Wrap(nil, Name, "browse", fmt.Errorf("cannot decode an item: %w", err))
			}
			items = append(items, v)
			held += len(raw)
		}
		if token == "" {
			return items, nil, charge, nil
		}
		pos = browsePos{Token: token}
		if len(items) == limit || held >= maxResultBytes {
			return items, &pos, charge, nil
		}
	}
	return items, &pos, charge, nil
}

// fieldPath renders a field name as a property path on the alias c, with
// dots separating nested properties: addr.city is c["addr"]["city"].
func fieldPath(field string) (string, error) {
	if field == "" {
		return "", driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("a filter or sort names no field"))
	}
	var b strings.Builder
	b.WriteString("c")
	for seg := range strings.SplitSeq(field, ".") {
		if seg == "" {
			return "", driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("field %q has an empty path segment", field))
		}
		q, _ := json.Marshal(seg)
		b.WriteString("[")
		b.Write(q)
		b.WriteString("]")
	}
	return b.String(), nil
}

// whereClause renders a filter's conditions (ANDed, values as @w1, @w2…
// parameters) and its raw fragment (wrapped in parentheses, checked by
// checkRawFilter) as a WHERE body, "" when the filter is empty.
func whereClause(f driver.Filter, readOnly bool) (string, []azcosmos.QueryParameter, error) {
	var parts []string
	var params []azcosmos.QueryParameter
	bind := func(v driver.Value) (string, error) {
		raw, err := rawJSON(v)
		if err != nil {
			return "", driver.Wrap(driver.ErrUnsupported, Name, "browse", err)
		}
		name := "@w" + strconv.Itoa(len(params)+1)
		params = append(params, azcosmos.QueryParameter{Name: name, Value: raw})
		return name, nil
	}
	for _, cond := range f.Conditions {
		path, err := fieldPath(cond.Field)
		if err != nil {
			return "", nil, err
		}
		isNull := "(NOT IS_DEFINED(" + path + ") OR IS_NULL(" + path + "))"
		var part string
		switch cond.Op {
		case driver.OpIsNull:
			part = isNull
		case driver.OpNotNull:
			part = "(IS_DEFINED(" + path + ") AND NOT IS_NULL(" + path + "))"
		case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
			if cond.Op == driver.OpEq && cond.Value.IsNull() {
				part = isNull
				break
			}
			p, err := bind(cond.Value)
			if err != nil {
				return "", nil, err
			}
			part = path + " " + comparison[cond.Op] + " " + p
		case driver.OpContains, driver.OpPrefix:
			if cond.Value.Type() != driver.TypeString {
				return "", nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("%s compares text: give a string", cond.Op))
			}
			p, err := bind(cond.Value)
			if err != nil {
				return "", nil, err
			}
			fn := "CONTAINS"
			if cond.Op == driver.OpPrefix {
				fn = "STARTSWITH"
			}
			part = fn + "(" + path + ", " + p + ")"
		case driver.OpIn:
			if cond.Value.Type() != driver.TypeList {
				return "", nil, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("in needs a list of values"))
			}
			p, err := bind(cond.Value)
			if err != nil {
				return "", nil, err
			}
			part = "ARRAY_CONTAINS(" + p + ", " + path + ")"
		default:
			return "", nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("operator %q is not supported", cond.Op))
		}
		parts = append(parts, part)
	}
	if strings.TrimSpace(f.Raw) != "" {
		if err := checkRawFilter(f.Raw, readOnly); err != nil {
			return "", nil, err
		}
		parts = append(parts, "("+f.Raw+")")
	}
	return strings.Join(parts, " AND "), params, nil
}

var comparison = map[driver.Op]string{
	driver.OpEq: "=", driver.OpNe: "!=", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">=",
}

// browseSQL builds Browse's query.
func browseSQL(f driver.Filter, sort []driver.SortKey, readOnly bool) (string, []azcosmos.QueryParameter, error) {
	where, params, err := whereClause(f, readOnly)
	if err != nil {
		return "", nil, err
	}
	sql := "SELECT * FROM c"
	if where != "" {
		sql += " WHERE " + where
	}
	for i, k := range sort {
		path, err := fieldPath(k.Field)
		if err != nil {
			return "", nil, err
		}
		if i == 0 {
			sql += " ORDER BY "
		} else {
			sql += ", "
		}
		dir := " ASC"
		if k.Desc {
			dir = " DESC"
		}
		sql += path + dir
	}
	return sql, params, nil
}

// browseSignature ties a cursor to one container, query and parameters.
func browseSignature(ref driver.CollectionRef, sql string, params []azcosmos.QueryParameter) string {
	h := sha256.New()
	for _, part := range []string{ref.Namespace, ref.Name, sql} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{':'})
		h.Write([]byte(part))
	}
	for _, p := range params {
		h.Write([]byte{0})
		h.Write([]byte(p.Name))
		if raw, ok := p.Value.(json.RawMessage); ok {
			h.Write(raw)
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

type cursorBody struct {
	Sig string    `json:"s"`
	Pos browsePos `json:"p"`
}

// encodeBrowseCursor packs a page position with its query's signature.
func encodeBrowseCursor(sig string, pos browsePos) string {
	b, _ := json.Marshal(cursorBody{Sig: sig, Pos: pos})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeBrowseCursor unpacks a cursor, which must belong to this query.
func decodeBrowseCursor(cur, sig string) (browsePos, error) {
	bad := func(msg string) (browsePos, error) {
		return browsePos{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New(msg))
	}
	raw, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return bad("the cursor is not one this driver issued")
	}
	var body cursorBody
	if err := json.Unmarshal(raw, &body); err != nil || body.Sig == "" || body.Pos.Skip < 0 || body.Pos.Token == "" && body.Pos.Skip == 0 {
		return bad("the cursor is not one this driver issued")
	}
	if body.Sig != sig {
		return bad("the cursor belongs to another filter or sort: start from the first page")
	}
	return body.Pos, nil
}
