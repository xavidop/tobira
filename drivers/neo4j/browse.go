package neo4j

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	maxBrowseLimit     = 10000
)

// browseCursor is a Browse cursor: the offset of the next page and a tag of
// the statement it belongs to, so a cursor is never replayed on another
// filter or sort.
type browseCursor struct {
	Offset int    `json:"o"`
	Tag    uint64 `json:"t"`
}

func (c browseCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (browseCursor, error) {
	if s == "" {
		return browseCursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) > 256 {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor"))
	}
	var c browseCursor
	if err := json.Unmarshal(b, &c); err != nil || c.Offset < 0 {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor"))
	}
	return c, nil
}

// cursorTag fingerprints a page's statement and arguments.
func cursorTag(cypher string, args map[string]any) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(cypher))
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range args {
			if !yield(k) {
				return
			}
		}
	})
	for _, k := range keys {
		if k == "skip" || k == "limit" {
			continue
		}
		_, _ = fmt.Fprintf(h, "\x00%s=%v", k, args[k])
	}
	return h.Sum64()
}

// Browse pages through the nodes of a label with SKIP and LIMIT. Each row
// is the node's element id (_id) and its properties, as Describe lists
// them; a node without a property has NULL there. Conditions become
// predicates on the properties with bound values, Filter.Raw (a Cypher
// WHERE body over the node variable n) is ANDed after them, and pages are
// ordered by the sort keys, or by element id. Every operator is
// supported: contains and prefix compare the property's text. The
// cursor is an offset, bound to the filter and sort it came from.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	limit = min(limit, maxBrowseLimit)
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	sch, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range sch.Fields {
		if f.Name != idColumn {
			names = append(names, f.Name)
		}
	}
	known := func(field string) error {
		if field != idColumn && !slices.Contains(names, field) {
			return driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("unknown property %q on label %s", field, ref.Name))
		}
		return nil
	}
	args := map[string]any{}
	bind := func(v any) string {
		k := "p" + strconv.Itoa(len(args))
		args[k] = v
		return "$" + k
	}
	var where []string
	for _, cond := range opts.Filter.Conditions {
		if err := known(cond.Field); err != nil {
			return nil, err
		}
		pred, err := condition("n", cond, bind)
		if err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, err)
		}
		where = append(where, pred)
	}
	if raw := strings.TrimSpace(opts.Filter.Raw); raw != "" {
		where = append(where, "("+raw+")")
	}
	var order []string
	for _, k := range opts.Sort {
		if err := known(k.Field); err != nil {
			return nil, err
		}
		expr := "n." + quote(k.Field)
		if k.Field == idColumn {
			expr = "elementId(n)"
		}
		if k.Desc {
			expr += " DESC"
		}
		order = append(order, expr)
	}
	if len(order) == 0 {
		order = []string{"elementId(n)"}
	}
	var sb strings.Builder
	sb.WriteString("MATCH (n:" + quote(ref.Name) + ")")
	if len(where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	sb.WriteString(" RETURN elementId(n) AS " + quote(idColumn))
	for _, p := range names {
		sb.WriteString(", n." + quote(p) + " AS " + quote(p))
	}
	sb.WriteString(" ORDER BY " + strings.Join(order, ", ") + " SKIP $skip LIMIT $limit")
	cypher := sb.String()
	tag := cursorTag(cypher, args)
	if opts.Cursor != "" && cur.Tag != tag {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the cursor is for another filter or sort: start from the first page"))
	}
	args["skip"], args["limit"] = cur.Offset, limit+1
	r, err := c.run(ctx, op, ref.Namespace, true, cypher, args, limit, &ref, []string{idColumn})
	if err != nil {
		return nil, err
	}
	next := browseCursor{Offset: cur.Offset + limit, Tag: tag}
	r.cursor = next.encode
	return r, nil
}
