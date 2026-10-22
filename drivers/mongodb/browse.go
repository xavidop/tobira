package mongodb

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	maxBrowseLimit     = 10000
	keysetPrefix       = "k:"
	offsetPrefix       = "o:"
	// maxCursorLen bounds a decoded cursor: room for a large composite _id.
	maxCursorLen = 1 << 20
)

var objectIDHex = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)

var _ driver.Browser = (*conn)(nil)

// Browse pages through a collection or view with find. Conditions become a
// filter document, Filter.Raw (a JSON filter document) is ANDed with them,
// and the page is read in one go, so its columns are the union of its
// documents' fields.
//
// Without Sort, pages use an _id keyset: {$expr: {$gt: ["$_id", last]}}
// sorted by _id, which compares across BSON types in MongoDB's total order
// and so stays correct when _id types are mixed. With a Sort, pages use
// skip, with _id as the last sort key so pages do not overlap. The default
// page size is 100, capped at 10000; a page also ends once its documents
// reach maxResultBytes, with a next cursor and a warning.
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
	if (opts.ReadOnly || c.readOnly) && strings.TrimSpace(opts.Filter.Raw) != "" {
		if err := checkFilterReadOnly(opts.Filter.Raw); err != nil {
			return nil, err
		}
	}
	filter, err := buildFilter(opts.Filter)
	if err != nil {
		return nil, err
	}
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	keyset := len(opts.Sort) == 0
	if keyset && cur.offset > 0 || !keyset && cur.after != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the cursor is for another sort: start from the first page"))
	}
	sort := bson.D{}
	for _, k := range opts.Sort {
		if err := checkField(k.Field); err != nil {
			return nil, err
		}
		dir := 1
		if k.Desc {
			dir = -1
		}
		sort = append(sort, bson.E{Key: k.Field, Value: dir})
	}
	if _, ok := lookup(sort, "_id"); !ok {
		sort = append(sort, bson.E{Key: "_id", Value: 1})
	}
	if cur.after != nil {
		after := bson.D{{Key: "$expr", Value: bson.D{{Key: "$gt", Value: bson.A{"$_id", bson.D{{Key: "$literal", Value: *cur.after}}}}}}}
		filter = andFilters(filter, after)
	}
	db := c.database(ref.Namespace)
	ref.Namespace = db.Name()
	fo := options.Find().SetSort(sort).SetLimit(int64(limit) + 1)
	if cur.offset > 0 {
		fo.SetSkip(int64(cur.offset))
	}
	mc, err := db.Collection(ref.Name).Find(ctx, filter, fo)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer closeCursor(mc)
	var docs []bson.Raw
	held, budget := 0, false
	for mc.Next(ctx) {
		docs = append(docs, append(bson.Raw(nil), mc.Current...))
		if held += len(mc.Current); held >= maxResultBytes && len(docs) <= limit {
			budget = true // end the page early; the next page resumes after it
			break
		}
	}
	if err := mc.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	next := ""
	if len(docs) > limit || budget {
		docs = docs[:min(len(docs), limit)]
		if keyset {
			if next, err = keysetCursor(docs[len(docs)-1].Lookup("_id")); err != nil {
				return nil, driver.Wrap(nil, Name, op, err)
			}
		} else {
			next = offsetCursor(cur.offset + len(docs))
		}
	}
	rs, err := newDocResult(ctx, op, nil, docs, 0, start, -1, &ref)
	if err != nil {
		return nil, err
	}
	r := rs.(*docResult)
	r.stats.NextCursor = next
	r.stats.Truncated = next != ""
	if budget {
		r.stats.Warnings = append(r.stats.Warnings, fmt.Sprintf(
			"the page stopped after %d documents at the %s page limit; the next page continues", len(docs), budgetText()))
	}
	return r, nil
}

// browseCursor is a decoded Browse cursor: the last _id of a keyset page,
// or the offset of a sorted page.
type browseCursor struct {
	after  *bson.RawValue
	offset int
}

// keysetCursor encodes the last _id of a page: "k:" and the base64url of
// the BSON document {k: id}.
func keysetCursor(id bson.RawValue) (string, error) {
	b, err := bson.Marshal(bson.D{{Key: "k", Value: id}})
	if err != nil {
		return "", err
	}
	return keysetPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func offsetCursor(n int) string { return offsetPrefix + strconv.Itoa(n) }

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
		n, err := strconv.Atoi(s[len(offsetPrefix):])
		if err != nil || n < 0 {
			return bad("bad offset")
		}
		return browseCursor{offset: n}, nil
	case strings.HasPrefix(s, keysetPrefix):
		b, err := base64.RawURLEncoding.DecodeString(s[len(keysetPrefix):])
		if err != nil || bson.Raw(b).Validate() != nil {
			return bad("bad key")
		}
		v, err := bson.Raw(b).LookupErr("k")
		if err != nil {
			return bad("bad key")
		}
		return browseCursor{after: &v}, nil
	}
	return bad("unknown form")
}

// checkField refuses an empty field name or one starting with "$", which
// would be read as an operator.
func checkField(f string) error {
	if f == "" || strings.HasPrefix(f, "$") {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("invalid field name %q", f))
	}
	return nil
}

// parseFilter reads a raw filter: one JSON (extended JSON) object.
func parseFilter(raw string) (bson.D, error) {
	d, err := parseCommandDoc(raw, "browse", true)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// buildFilter translates a Filter to a filter document. Conditions:
//
//   - eq, ne, lt, le, gt, ge, in as $eq (a plain match), $ne, $lt, $lte,
//     $gt, $gte, $in; a 24-hex-digit String compared by eq, ne or in also
//     matches the ObjectId it spells;
//   - is_null as {f: null} (null or missing), not_null as {f: {$ne: null}};
//   - contains and prefix as a regular expression match on the value
//     converted to a string ($regexMatch on $convert), so numbers match by
//     their digits; the text is matched literally.
//
// Several conditions and Raw are ANDed. A field name that is empty or
// starts with "$" is ErrSyntax; another operator is ErrUnsupported.
func buildFilter(f driver.Filter) (bson.D, error) {
	var parts []bson.D
	for _, c := range f.Conditions {
		p, err := conditionDoc(c)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	if strings.TrimSpace(f.Raw) != "" {
		raw, err := parseFilter(f.Raw)
		if err != nil {
			return nil, err
		}
		parts = append(parts, raw)
	}
	return andFilters(parts...), nil
}

func andFilters(parts ...bson.D) bson.D {
	var nonEmpty []bson.D
	for _, p := range parts {
		if len(p) > 0 {
			nonEmpty = append(nonEmpty, p)
		}
	}
	switch len(nonEmpty) {
	case 0:
		return bson.D{}
	case 1:
		return nonEmpty[0]
	}
	arr := make(bson.A, len(nonEmpty))
	for i, p := range nonEmpty {
		arr[i] = p
	}
	return bson.D{{Key: "$and", Value: arr}}
}

var comparisonOps = map[driver.Op]string{
	driver.OpNe: "$ne", driver.OpLt: "$lt", driver.OpLe: "$lte", driver.OpGt: "$gt", driver.OpGe: "$gte",
}

func conditionDoc(c driver.Condition) (bson.D, error) {
	if err := checkField(c.Field); err != nil {
		return nil, err
	}
	field := func(v any) (bson.D, error) { return bson.D{{Key: c.Field, Value: v}}, nil }
	switch c.Op {
	case driver.OpIsNull:
		return field(nil)
	case driver.OpNotNull:
		return field(bson.D{{Key: "$ne", Value: nil}})
	case driver.OpEq:
		if alts, ok := hexAlternatives(c.Value); ok {
			return field(bson.D{{Key: "$in", Value: alts}})
		}
		v, err := toBSON(c.Value)
		if err != nil {
			return nil, err
		}
		return field(v)
	case driver.OpIn:
		list, ok := c.Value.AsList()
		if !ok {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("in on %q needs a list", c.Field))
		}
		var arr bson.A
		for _, e := range list {
			if alts, ok := hexAlternatives(e); ok {
				arr = append(arr, alts...)
				continue
			}
			v, err := toBSON(e)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if arr == nil {
			arr = bson.A{}
		}
		return field(bson.D{{Key: "$in", Value: arr}})
	case driver.OpContains, driver.OpPrefix:
		text := c.Value.String()
		if s, ok := c.Value.AsText(); ok {
			text = s
		}
		re := regexp.QuoteMeta(text)
		if c.Op == driver.OpPrefix {
			re = "^" + re
		}
		conv := bson.D{{Key: "$convert", Value: bson.D{
			{Key: "input", Value: "$" + c.Field}, {Key: "to", Value: "string"},
			{Key: "onError", Value: nil}, {Key: "onNull", Value: nil},
		}}}
		return bson.D{{Key: "$expr", Value: bson.D{{Key: "$regexMatch", Value: bson.D{{Key: "input", Value: conv}, {Key: "regex", Value: re}}}}}}, nil
	}
	if mop, ok := comparisonOps[c.Op]; ok {
		if c.Op == driver.OpNe {
			if alts, ok := hexAlternatives(c.Value); ok {
				return field(bson.D{{Key: "$nin", Value: alts}})
			}
		}
		v, err := toBSON(c.Value)
		if err != nil {
			return nil, err
		}
		return field(bson.D{{Key: mop, Value: v}})
	}
	return nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("filter operator %q", c.Op))
}

// hexAlternatives returns [ObjectId, string] for a String of 24 hex digits,
// so a filter typed as text matches an ObjectId as well.
func hexAlternatives(v driver.Value) (bson.A, bool) {
	s, ok := v.AsText()
	if !ok || v.Type() != driver.TypeString || !objectIDHex.MatchString(s) {
		return nil, false
	}
	oid, err := bson.ObjectIDFromHex(s)
	if err != nil {
		return nil, false
	}
	return bson.A{oid, s}, true
}

// checkFilterReadOnly applies the $out/$merge scan to a Browse raw filter.
func checkFilterReadOnly(raw string) error {
	f, err := parseFilter(raw)
	if err != nil {
		return err
	}
	return checkNoWriteStage(f)
}
