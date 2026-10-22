package mongodb

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// catalogTTL is how long completion keeps catalog lookups.
const catalogTTL = 60 * time.Second

// maxCompletions caps one completion answer.
const maxCompletions = 200

var (
	_ driver.Completer          = (*conn)(nil)
	_ driver.CatalogInvalidator = (*conn)(nil)
)

// Complete suggests, for the word at the cursor in a command document:
//
//   - collection names, in the string value of find, aggregate, count,
//     distinct, listIndexes and collStats;
//   - query operators, aggregation stages and expression operators, for a
//     word starting with "$", plus "$field" references to the command's
//     collection's fields;
//   - command names, command options and field paths (nested ones dotted)
//     of the command's collection otherwise.
//
// Collections and fields come from Collections and Describe and are cached
// for 60 s; Invalidate drops the cache.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	if req.Namespace == "" {
		req.Namespace = c.defaultDB
	}
	return complete(ctx, c.catalog, req)
}

// Invalidate drops the cached catalog so the next completion sees new
// collections and fields.
func (c *conn) Invalidate() { c.catalog.Invalidate() }

// catalogSource is what completion reads the catalog from.
type catalogSource interface {
	collections(ctx context.Context, ns string) ([]string, error)
	fields(ctx context.Context, ns, coll string) ([]string, error)
}

// connCatalog reads the catalog through the connection.
type connCatalog struct{ c *conn }

func (r connCatalog) collections(ctx context.Context, ns string) ([]string, error) {
	cols, err := r.c.Collections(ctx, driver.NamespaceRef{Name: ns})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cols))
	for i, col := range cols {
		out[i] = col.Ref.Name
	}
	return out, nil
}

func (r connCatalog) fields(ctx context.Context, ns, coll string) ([]string, error) {
	sch, err := r.c.Describe(ctx, driver.CollectionRef{Namespace: ns, Name: coll})
	if err != nil {
		return nil, err
	}
	var out []string
	var walk func(prefix string, fs []driver.Field)
	walk = func(prefix string, fs []driver.Field) {
		for _, f := range fs {
			out = append(out, prefix+f.Name)
			walk(prefix+f.Name+".", f.Fields)
		}
	}
	walk("", sch.Fields)
	return out, nil
}

// catalogCache caches a catalogSource for a TTL.
type catalogCache struct {
	src catalogSource
	ttl time.Duration

	mu    sync.Mutex
	colls map[string]cacheEntry
	flds  map[string]cacheEntry
}

type cacheEntry struct {
	names []string
	at    time.Time
}

func newCatalogCache(c *conn, ttl time.Duration) *catalogCache {
	return newCatalogCacheFrom(connCatalog{c}, ttl)
}

func newCatalogCacheFrom(src catalogSource, ttl time.Duration) *catalogCache {
	return &catalogCache{src: src, ttl: ttl, colls: map[string]cacheEntry{}, flds: map[string]cacheEntry{}}
}

// Invalidate drops every cached entry.
func (k *catalogCache) Invalidate() {
	k.mu.Lock()
	k.colls = map[string]cacheEntry{}
	k.flds = map[string]cacheEntry{}
	k.mu.Unlock()
}

func (k *catalogCache) get(m func() map[string]cacheEntry, key string, load func() ([]string, error)) ([]string, error) {
	k.mu.Lock()
	e, ok := m()[key]
	k.mu.Unlock()
	if ok && time.Since(e.at) < k.ttl {
		return e.names, nil
	}
	names, err := load() // outside the lock: Invalidate never waits on a load
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	m()[key] = cacheEntry{names: names, at: time.Now()}
	k.mu.Unlock()
	return names, nil
}

func (k *catalogCache) collections(ctx context.Context, ns string) ([]string, error) {
	return k.get(func() map[string]cacheEntry { return k.colls }, ns, func() ([]string, error) { return k.src.collections(ctx, ns) })
}

func (k *catalogCache) fields(ctx context.Context, ns, coll string) ([]string, error) {
	return k.get(func() map[string]cacheEntry { return k.flds }, ns+"\x00"+coll, func() ([]string, error) { return k.src.fields(ctx, ns, coll) })
}

var (
	collValueRe = regexp.MustCompile(`"(find|aggregate|count|distinct|listIndexes|collStats)"\s*:\s*"$`)
	targetRe    = regexp.MustCompile(`"(find|aggregate|count|distinct)"\s*:\s*"([^"\\]+)"`)
)

var commandWords = []string{
	"find", "aggregate", "count", "distinct", "listCollections", "listIndexes", "collStats", "dbStats", "explain",
	"insert", "update", "delete", "findAndModify",
}

var optionWords = []string{
	"filter", "sort", "projection", "limit", "skip", "batchSize", "hint", "comment", "collation",
	"pipeline", "cursor", "allowDiskUse", "query", "key", "nameOnly", "verbosity",
	"documents", "updates", "deletes", "q", "u", "multi", "upsert", "remove", "new", "fields",
}

var operatorWords = []string{
	// query operators
	"$eq", "$ne", "$gt", "$gte", "$lt", "$lte", "$in", "$nin", "$and", "$or", "$nor", "$not",
	"$exists", "$type", "$regex", "$options", "$expr", "$elemMatch", "$size", "$all", "$mod", "$text", "$where",
	// aggregation stages
	"$match", "$group", "$project", "$sort", "$limit", "$skip", "$unwind", "$lookup", "$addFields", "$set",
	"$unset", "$count", "$facet", "$bucket", "$sample", "$replaceRoot", "$unionWith", "$sortByCount", "$documents",
	// expression and accumulator operators
	"$sum", "$avg", "$min", "$max", "$first", "$last", "$push", "$addToSet", "$cond", "$ifNull", "$concat",
	"$toString", "$toInt", "$dateToString", "$size", "$arrayElemAt", "$filter", "$map", "$literal",
}

func isWordByte(b byte) bool {
	return b == '_' || b == '.' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// complete answers a CompletionRequest from a catalog.
func complete(ctx context.Context, cat catalogSource, req driver.CompletionRequest) ([]driver.Completion, error) {
	cur := max(0, min(req.Cursor, len(req.Text)))
	before := req.Text[:cur]
	start := cur
	for start > 0 && isWordByte(before[start-1]) {
		start--
	}
	prefix := before[start:]
	head := before[:start]
	var out []driver.Completion
	add := func(kind string, names ...string) {
		for _, n := range names {
			if len(out) >= maxCompletions {
				return
			}
			if strings.HasPrefix(strings.ToLower(n), strings.ToLower(prefix)) && !slices.ContainsFunc(out, func(c driver.Completion) bool { return c.Label == n }) {
				out = append(out, driver.Completion{Label: n, Kind: kind})
			}
		}
	}
	if collValueRe.MatchString(head) {
		names, err := cat.collections(ctx, req.Namespace)
		if err != nil {
			return nil, err
		}
		add("collection", names...)
		return out, nil
	}
	var fields []string
	if m := targetRe.FindStringSubmatch(req.Text); m != nil {
		var err error
		if fields, err = cat.fields(ctx, req.Namespace, m[2]); err != nil && driver.KindOf(err) != driver.ErrNotFound {
			return nil, err
		}
	}
	if strings.HasPrefix(prefix, "$") {
		add("keyword", operatorWords...)
		refs := make([]string, len(fields))
		for i, f := range fields {
			refs[i] = "$" + f
		}
		add("field", refs...)
		return out, nil
	}
	add("keyword", commandWords...)
	add("keyword", optionWords...)
	add("field", fields...)
	return out, nil
}
