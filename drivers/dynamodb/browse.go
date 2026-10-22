package dynamodb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	maxBrowseLimit     = 10000
	// maxScanLimit caps the Limit of one Scan call.
	maxScanLimit = 1000
	keysetPrefix = "k:"
	offsetPrefix = "o:"
	// maxCursorLen bounds a cursor: room for DynamoDB's largest key
	// (2 KB partition plus 1 KB sort key) in base64 JSON.
	maxCursorLen = 16 << 10
)

var _ driver.Browser = (*conn)(nil)

// Browse pages through a table with Scan. Conditions become a
// FilterExpression (buildFilter); Filter.Raw is ErrUnsupported, since a
// DynamoDB expression cannot hold literal values: use conditions, or a
// PartiQL query.
//
// Without a Sort, pages follow the scan: the cursor is the key of the
// page's last item, used as ExclusiveStartKey, and a page reads one item
// past its size to know whether another page exists. With a Sort, Browse
// scans the whole table (at most limits.sortItems items, else
// ErrUnsupported), sorts it in memory (missing and NULL first ascending,
// ties by key) and pages by offset. The default page size is 100, capped
// at 10000; a page also ends once its items reach limits.resultBytes, with
// a next cursor and a warning. A page's columns come from all its items.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(ref.Namespace, op); err != nil {
		return nil, err
	}
	start := time.Now()
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	limit = min(limit, maxBrowseLimit)
	if strings.TrimSpace(opts.Filter.Raw) != "" {
		return nil, browseErr(driver.ErrUnsupported, errors.New("a raw filter is not supported: DynamoDB expressions take no literal values; use field conditions or a PartiQL query"))
	}
	for _, k := range opts.Sort {
		if k.Field == "" {
			return nil, browseErr(driver.ErrSyntax, errors.New("a sort key needs a field name"))
		}
	}
	meta, err := c.catalog.meta(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	f, err := buildFilter(opts.Filter.Conditions, meta)
	if err != nil {
		return nil, err
	}
	cur, err := decodeCursor(opts.Cursor, meta)
	if err != nil {
		return nil, err
	}
	sorted := len(opts.Sort) > 0
	if sorted && cur.after != nil || !sorted && cur.offset > 0 {
		return nil, browseErr(driver.ErrSyntax, errors.New("the cursor is for another sort: start from the first page"))
	}
	scanIn := func() *dynamodb.ScanInput {
		in := &dynamodb.ScanInput{TableName: aws.String(ref.Name)}
		if f.expr != "" {
			in.FilterExpression = aws.String(f.expr)
			in.ExpressionAttributeNames = f.names
			if len(f.values) > 0 {
				in.ExpressionAttributeValues = f.values
			}
		}
		return in
	}
	var (
		items  []item
		next   string
		budget bool
	)
	if sorted {
		all, err := c.scanAll(ctx, op, scanIn())
		if err != nil {
			return nil, err
		}
		sortItems(all, opts.Sort, meta)
		off := min(cur.offset, len(all))
		end := min(off+limit, len(all))
		items = all[off:end]
		if end < len(all) {
			next = offsetPrefix + strconv.Itoa(end)
		}
	} else {
		in := scanIn()
		in.ExclusiveStartKey = cur.after
		items, budget, err = c.scanPage(ctx, op, in, limit, f.expr != "")
		if err != nil {
			return nil, err
		}
		if len(items) > limit || budget {
			items = items[:min(len(items), limit)]
			if next, err = keysetCursor(keyOf(items[len(items)-1], meta), meta); err != nil {
				return nil, browseErr(nil, err)
			}
		}
	}
	ref.Namespace = namespaceName
	r, err := c.newItemResult(ctx, op, nil, items, 0, start, resultSpec{keys: meta.keys, source: &ref, whole: true})
	if err != nil {
		return nil, err
	}
	r.stats.NextCursor = next
	r.stats.Truncated = next != ""
	if budget {
		r.stats.Warnings = append(r.stats.Warnings, fmt.Sprintf(
			"the page stopped after %d items at the %s page limit; the next page continues", len(items), budgetText(c.limits.resultBytes)))
	}
	return r, nil
}

// scanPage scans from in.ExclusiveStartKey until it holds limit + 1 items,
// the table ends, or the items reach limits.resultBytes (budget). A
// filtered scan asks for more items per call, since the filter drops some
// after they are read.
func (c *conn) scanPage(ctx context.Context, op string, in *dynamodb.ScanInput, limit int, filtered bool) (items []item, budget bool, err error) {
	held := 0
	for {
		ask := limit + 1 - len(items)
		if filtered {
			ask *= 4
		}
		in.Limit = aws.Int32(int32(min(ask, maxScanLimit)))
		out, err := c.db.Scan(ctx, in)
		if err != nil {
			return nil, false, c.fail(ctx, op, err)
		}
		for _, it := range out.Items {
			items = append(items, it)
			if len(items) > limit {
				return items, false, nil
			}
			if held += itemSize(it); held >= c.limits.resultBytes {
				return items, true, nil
			}
		}
		if out.LastEvaluatedKey == nil {
			return items, false, nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

// scanAll reads a whole (filtered) table for a sorted Browse: at most
// limits.sortItems items and limits.resultBytes, else ErrUnsupported.
func (c *conn) scanAll(ctx context.Context, op string, in *dynamodb.ScanInput) ([]item, error) {
	var items []item
	held := 0
	tooBig := func() error {
		return browseErr(driver.ErrUnsupported, fmt.Errorf(
			"sorting reads the whole table into memory, at most %d items and %s: filter it first, or browse unsorted",
			c.limits.sortItems, budgetText(c.limits.resultBytes)))
	}
	for {
		out, err := c.db.Scan(ctx, in)
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		for _, it := range out.Items {
			items = append(items, it)
			held += itemSize(it)
		}
		if len(items) > c.limits.sortItems || held > c.limits.resultBytes {
			return nil, tooBig()
		}
		if out.LastEvaluatedKey == nil {
			return items, nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

func keyOf(it item, meta *tableMeta) item {
	k := make(item, len(meta.keys))
	for _, n := range meta.keys {
		k[n] = it[n]
	}
	return k
}

// sortItems orders items by the sort keys (compareAV, reversed for Desc),
// then by the table's key attributes ascending, so offset pages are
// stable.
func sortItems(items []item, keys []driver.SortKey, meta *tableMeta) {
	slices.SortStableFunc(items, func(a, b item) int {
		for _, k := range keys {
			if c := compareAV(a[k.Field], b[k.Field]); c != 0 {
				if k.Desc {
					return -c
				}
				return c
			}
		}
		for _, k := range meta.keys {
			if c := compareAV(a[k], b[k]); c != 0 {
				return c
			}
		}
		return 0
	})
}

// typeRank orders values of different types: missing and NULL, then BOOL,
// N, S, B, and everything else.
func typeRank(av types.AttributeValue) int {
	switch av.(type) {
	case nil, *types.AttributeValueMemberNULL:
		return 0
	case *types.AttributeValueMemberBOOL:
		return 1
	case *types.AttributeValueMemberN:
		return 2
	case *types.AttributeValueMemberS:
		return 3
	case *types.AttributeValueMemberB:
		return 4
	}
	return 5
}

// compareAV is a total order over attribute values: by typeRank, then
// numbers by value, strings and binaries bytewise, false before true, and
// sets, lists and maps by their display text.
func compareAV(a, b types.AttributeValue) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		return ra - rb
	}
	switch x := a.(type) {
	case nil, *types.AttributeValueMemberNULL:
		return 0
	case *types.AttributeValueMemberBOOL:
		y := b.(*types.AttributeValueMemberBOOL)
		switch {
		case x.Value == y.Value:
			return 0
		case !x.Value:
			return -1
		}
		return 1
	case *types.AttributeValueMemberN:
		return compareNumbers(x.Value, b.(*types.AttributeValueMemberN).Value)
	case *types.AttributeValueMemberS:
		return strings.Compare(x.Value, b.(*types.AttributeValueMemberS).Value)
	case *types.AttributeValueMemberB:
		return bytes.Compare(x.Value, b.(*types.AttributeValueMemberB).Value)
	}
	return strings.Compare(valueOf(a).String(), valueOf(b).String())
}

// browseCursor is a decoded Browse cursor: the key of the last item of an
// unsorted page, or the offset of a sorted one.
type browseCursor struct {
	after  item
	offset int
}

// keysetCursor encodes a key: "k:" and the base64url of a JSON object
// mapping each key attribute to {"S": …}, {"N": …} or {"B": base64}.
func keysetCursor(key item, meta *tableMeta) (string, error) {
	enc := make(map[string]map[string]string, len(key))
	for _, n := range meta.keys {
		switch v := key[n].(type) {
		case *types.AttributeValueMemberS:
			enc[n] = map[string]string{"S": v.Value}
		case *types.AttributeValueMemberN:
			enc[n] = map[string]string{"N": v.Value}
		case *types.AttributeValueMemberB:
			enc[n] = map[string]string{"B": base64.StdEncoding.EncodeToString(v.Value)}
		default:
			return "", fmt.Errorf("key attribute %q has no S, N or B value", n)
		}
	}
	b, err := json.Marshal(enc)
	if err != nil {
		return "", err
	}
	return keysetPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor reads a cursor. A key cursor must name exactly the table's
// key attributes, each with its declared type.
func decodeCursor(s string, meta *tableMeta) (browseCursor, error) {
	bad := func(msg string) (browseCursor, error) {
		return browseCursor{}, browseErr(driver.ErrSyntax, errors.New("invalid cursor: "+msg))
	}
	switch {
	case s == "":
		return browseCursor{}, nil
	case len(s) > maxCursorLen:
		return bad("too long")
	case strings.HasPrefix(s, offsetPrefix):
		n, err := strconv.Atoi(s[len(offsetPrefix):])
		if err != nil || n < 0 {
			return bad("bad offset")
		}
		return browseCursor{offset: n}, nil
	case strings.HasPrefix(s, keysetPrefix):
		raw, err := base64.RawURLEncoding.DecodeString(s[len(keysetPrefix):])
		if err != nil {
			return bad("bad key")
		}
		var enc map[string]map[string]string
		if err := json.Unmarshal(raw, &enc); err != nil || len(enc) != len(meta.keys) {
			return bad("bad key")
		}
		key := make(item, len(enc))
		for _, n := range meta.keys {
			v, ok := enc[n]
			if !ok || len(v) != 1 {
				return bad("the key is not this table's")
			}
			text, ok := v[meta.keyTypes[n]]
			if !ok {
				return bad("the key is not this table's")
			}
			switch meta.keyTypes[n] {
			case "S":
				key[n] = &types.AttributeValueMemberS{Value: text}
			case "N":
				key[n] = &types.AttributeValueMemberN{Value: text}
			case "B":
				b, err := base64.StdEncoding.DecodeString(text)
				if err != nil {
					return bad("bad key")
				}
				key[n] = &types.AttributeValueMemberB{Value: b}
			default:
				return bad("the key is not this table's")
			}
		}
		return browseCursor{after: key}, nil
	}
	return bad("unknown form")
}
