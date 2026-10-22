package firestore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	maxBrowseLimit     = 1000
	cursorPrefix       = "d:"
	maxCursorLen       = 2048
	// prefixEnd follows every character Firestore sorts in a string
	// prefix, the documented idiom for a prefix range.
	prefixEnd = ""
)

var _ driver.Browser = (*conn)(nil)

// Browse pages through a collection (a root collection or a subcollection
// path) with a query: conditions become where filters, Filter.Raw (a JSON
// list of [field, op, value] triples, as in a request's where) is ANDed
// with them, and Sort becomes orderBy; Firestore breaks ties by document
// id. Without a Sort, pages come in document-id order (after the field of
// an inequality filter, which Firestore sorts by first). Firestore leaves out of a sorted query every document that
// lacks a sort field. The default page size is 100, capped at 1000; a
// page also ends once its documents reach maxResultBytes. The cursor
// names the page's last document: the next page starts after a fresh read
// of it, so a deleted last document is ErrNotFound (start again from the
// first page).
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	if err := c.checkNamespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	coll, err := c.collection(op, ref.Name)
	if err != nil {
		return nil, err
	}
	ref.Namespace = c.dsn.database
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	limit = min(limit, maxBrowseLimit)
	var where []whereClause
	for _, cond := range opts.Filter.Conditions {
		cl, err := conditionClauses(cond)
		if err != nil {
			return nil, err
		}
		where = append(where, cl...)
	}
	raw, err := parseRawFilter(opts.Filter.Raw)
	if err != nil {
		return nil, err
	}
	where = append(where, raw...)
	sig := pageSignature(ref.Name, opts)
	after, err := decodeCursor(opts.Cursor, sig)
	if err != nil {
		return nil, err
	}
	q, err := c.applyWhere(op, coll.Query, coll, where)
	if err != nil {
		return nil, err
	}
	for i, k := range opts.Sort {
		path, err := browseField(k.Field)
		if err != nil {
			return nil, err
		}
		if len(path) == 1 && path[0] == docIDField {
			if i != len(opts.Sort)-1 {
				return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("_id must be the last sort key: it is unique"))
			}
			q = q.OrderByPath(fs.FieldPath{docIDField}, direction(k.Desc))
			break
		}
		q = q.OrderByPath(fs.FieldPath(path), direction(k.Desc))
	}
	// Firestore orders ties (and an unsorted query) by document id, after
	// any field an inequality filter names; the cursor's StartAfter adds
	// the same keys.
	if after != "" {
		snap, err := coll.Doc(after).Get(ctx)
		if err != nil {
			err = c.fail(ctx, op, err)
			if errors.Is(err, driver.ErrNotFound) {
				return nil, driver.Wrap(driver.ErrNotFound, Name, op, errors.New("the last document of the previous page is gone: start again from the first page"))
			}
			return nil, err
		}
		q = q.StartAfter(snap)
	}
	it := q.Limit(limit + 1).Documents(ctx)
	defer it.Stop()
	r := &docResult{c: c, op: op, it: it}
	var rows []docRow
	held, budget := 0, false
	for len(rows) <= limit {
		d, ok, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		rows = append(rows, d)
		if held += rowSize(d); held >= maxResultBytes && len(rows) <= limit {
			budget = true // end the page early; the next page resumes after it
			break
		}
	}
	next := ""
	if len(rows) > limit || budget {
		rows = rows[:min(len(rows), limit)]
		next = encodeCursor(sig, rows[len(rows)-1].id)
	}
	rs, err := newDocResult(ctx, c, op, nil, rows, 0, start, &ref, false)
	if err != nil {
		return nil, err
	}
	res := rs.(*docResult)
	res.stats.NextCursor = next
	res.stats.Truncated = next != ""
	if budget {
		res.stats.Warnings = append(res.stats.Warnings, fmt.Sprintf(
			"the page stopped after %d documents at the %s page limit; the next page continues", len(rows), budgetText()))
	}
	return res, nil
}

// browseField is the field path a Browse column names: the column is one
// top-level field, dots included; _id is the document id and _id_field a
// stored field named _id. A name holding a backquote is a field path, as
// in a request: "`address`.city" is nested, "`a.b`" the field a.b.
func browseField(name string) ([]string, error) {
	switch {
	case name == "":
		return nil, syntaxErr("browse", errors.New("empty field name"))
	case strings.Contains(name, "`"):
		p, err := fieldPath(name)
		if err != nil {
			return nil, syntaxErr("browse", err)
		}
		return p, nil
	case name == idColumn:
		return []string{docIDField}, nil
	}
	return []string{storedName(name)}, nil
}

// conditionClauses translates a Browse condition on a column (see
// browseField):
//
//   - eq, ne, lt, le, gt, ge and in as ==, !=, <, <=, >, >= and in;
//   - is_null as == null and not_null as != null;
//   - prefix on a String as a range: >= p and < p + U+F8FF.
//
// contains is ErrUnsupported: Firestore has no substring match. Firestore
// leaves documents that lack the field out of !=, not-in and every range.
func conditionClauses(cond driver.Condition) ([]whereClause, error) {
	const op = "browse"
	path, err := browseField(cond.Field)
	if err != nil {
		return nil, err
	}
	isID := len(path) == 1 && path[0] == docIDField
	value := func() (any, error) {
		v, err := toFirestore(cond.Value)
		if err != nil {
			return nil, err
		}
		if isID {
			if _, ok := v.(string); !ok {
				return nil, syntaxErr(op, errors.New("an _id filter takes a document id (a string)"))
			}
		}
		return v, nil
	}
	one := func(fop string) ([]whereClause, error) {
		v, err := value()
		if err != nil {
			return nil, err
		}
		return []whereClause{{path: path, op: fop, value: v}}, nil
	}
	switch cond.Op {
	case driver.OpEq:
		return one("==")
	case driver.OpNe:
		return one("!=")
	case driver.OpLt:
		return one("<")
	case driver.OpLe:
		return one("<=")
	case driver.OpGt:
		return one(">")
	case driver.OpGe:
		return one(">=")
	case driver.OpIsNull:
		return []whereClause{{path: path, op: "==", value: nil}}, nil
	case driver.OpNotNull:
		return []whereClause{{path: path, op: "!=", value: nil}}, nil
	case driver.OpIn:
		list, ok := cond.Value.AsList()
		if !ok {
			return nil, syntaxErr(op, fmt.Errorf("in on %q needs a list", cond.Field))
		}
		vals := make([]any, len(list))
		for i, e := range list {
			v, err := toFirestore(e)
			if err != nil {
				return nil, err
			}
			if _, ok := v.(string); isID && !ok {
				return nil, syntaxErr(op, errors.New("an _id filter takes document ids (strings)"))
			}
			vals[i] = v
		}
		return []whereClause{{path: path, op: "in", value: vals}}, nil
	case driver.OpPrefix:
		s, ok := cond.Value.AsText()
		if !ok || cond.Value.Type() != driver.TypeString {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("prefix on %q needs a string: Firestore matches prefixes of strings only", cond.Field))
		}
		return []whereClause{{path: path, op: ">=", value: s}, {path: path, op: "<", value: s + prefixEnd}}, nil
	case driver.OpContains:
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("contains: Firestore has no substring match (use prefix, or array-contains in a request)"))
	}
	return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("filter operator %q", cond.Op))
}

// parseRawFilter reads Filter.Raw: a JSON list of [field, op, value]
// triples, as a request's where takes them. It can only filter, so it
// passes on a read-only call.
func parseRawFilter(raw string) ([]whereClause, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), new([]json.RawMessage)); err != nil {
		return nil, syntaxErr("browse", errors.New(`the raw filter is a JSON list of [field, op, value] triples, such as [["status","==","paid"]]`))
	}
	w, err := parseWhere(json.RawMessage(raw))
	if err != nil {
		return nil, syntaxErr("browse", err)
	}
	return w, nil
}

// pageSignature names what a page belongs to: the collection path, the
// sort and a digest of the filter, so a cursor is used only with its own.
func pageSignature(path string, opts driver.BrowseOptions) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%q\n", path)
	for _, k := range opts.Sort {
		_, _ = fmt.Fprintf(h, "sort %q %v\n", k.Field, k.Desc)
	}
	for _, c := range opts.Filter.Conditions {
		_, _ = fmt.Fprintf(h, "cond %q %q %s %q\n", c.Field, c.Op, c.Value.Type(), c.Value.String())
	}
	_, _ = fmt.Fprintf(h, "raw %q\n", opts.Filter.Raw)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:16])
}

// pageCursor is a Browse cursor: the sort it belongs to and the id of the
// page's last document.
type pageCursor struct {
	Sort string `json:"s"`
	ID   string `json:"id"`
}

func encodeCursor(sig, id string) string {
	b, _ := json.Marshal(pageCursor{Sort: sig, ID: id})
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor returns the cursor's document id ("" for the first page).
func decodeCursor(s, sig string) (string, error) {
	bad := func(msg string) (string, error) {
		return "", syntaxErr("browse", errors.New("invalid cursor: "+msg))
	}
	switch {
	case s == "":
		return "", nil
	case len(s) > maxCursorLen:
		return bad("too long")
	case !strings.HasPrefix(s, cursorPrefix):
		return bad("unknown form")
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(cursorPrefix):])
	if err != nil {
		return bad("bad encoding")
	}
	var pc pageCursor
	if err := json.Unmarshal(b, &pc); err != nil || pc.ID == "" || strings.Contains(pc.ID, "/") {
		return bad("bad document id")
	}
	if pc.Sort != sig {
		return bad("the cursor is for another collection, sort or filter: start from the first page")
	}
	return pc.ID, nil
}
