package cosmos

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

// sqlKeywords are the Cosmos SQL keywords offered by completion.
var sqlKeywords = []string{
	"AND", "ARRAY", "AS", "ASC", "BETWEEN", "BY", "DESC", "DISTINCT", "ESCAPE", "EXISTS", "FALSE", "FROM",
	"GROUP", "IN", "IS", "JOIN", "LIKE", "LIMIT", "NOT", "NULL", "OFFSET", "OR", "ORDER", "SELECT", "TOP",
	"TRUE", "UNDEFINED", "VALUE", "WHERE",
}

// sqlFunctions are the Cosmos SQL built-in functions offered by completion.
var sqlFunctions = []string{
	"ABS", "ACOS", "ARRAY_CONCAT", "ARRAY_CONTAINS", "ARRAY_LENGTH", "ARRAY_SLICE", "ASIN", "ATAN", "AVG",
	"CEILING", "CONCAT", "CONTAINS", "COS", "COUNT", "DateTimeAdd", "DateTimeDiff", "DateTimePart",
	"DocumentId", "ENDSWITH", "EXP", "FLOOR", "FullTextContains", "FullTextScore", "GetCurrentDateTime",
	"GetCurrentTimestamp", "INDEX_OF", "IS_ARRAY", "IS_BOOL", "IS_DEFINED", "IS_NULL", "IS_NUMBER",
	"IS_OBJECT", "IS_PRIMITIVE", "IS_STRING", "LEFT", "LENGTH", "LOG", "LOWER", "LTRIM", "MAX", "MIN",
	"POWER", "RRF", "RegexMatch", "REPLACE", "REPLICATE", "REVERSE", "RIGHT", "ROUND", "RTRIM",
	"SetIntersect", "SetUnion", "SIGN", "SQRT", "ST_DISTANCE", "ST_INTERSECTS", "ST_ISVALID", "ST_WITHIN",
	"STARTSWITH", "StringEquals", "StringToArray", "StringToBoolean", "StringToNumber", "StringToObject",
	"SUBSTRING", "SUM", "ToString", "TRIM", "TRUNC", "UPPER", "VectorDistance",
}

// Complete suggests, for the word at the cursor:
//
//   - container names after FROM (and FROM x IN), and in an envelope's
//     "container" value;
//   - field paths of the query's container after "alias.";
//   - keywords and built-in functions otherwise.
//
// Containers and fields come from Collections and Describe and are cached
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

// Invalidate drops the cached catalog and container properties.
func (c *conn) Invalidate() {
	c.catalog.Invalidate()
	c.props.clear()
}

// catalogSource is what completion reads the catalog from.
type catalogSource interface {
	containers(ctx context.Context, db string) ([]string, error)
	fields(ctx context.Context, db, container string) ([]string, error)
}

// connCatalog reads the catalog through the connection.
type connCatalog struct{ c *conn }

func (r connCatalog) containers(ctx context.Context, db string) ([]string, error) {
	cols, err := r.c.Collections(ctx, driver.NamespaceRef{Name: db})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cols))
	for i, col := range cols {
		out[i] = col.Ref.Name
	}
	return out, nil
}

func (r connCatalog) fields(ctx context.Context, db, container string) ([]string, error) {
	sch, err := r.c.Describe(ctx, driver.CollectionRef{Namespace: db, Name: container})
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

// catalogCache memoises a catalogSource for a TTL.
type catalogCache struct {
	src catalogSource
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]cacheEntry
}

type cacheEntry struct {
	names []string
	at    time.Time
}

func newCatalogCache(src catalogSource, ttl time.Duration) *catalogCache {
	return &catalogCache{src: src, ttl: ttl, m: map[string]cacheEntry{}}
}

func (k *catalogCache) get(key string, load func() ([]string, error)) ([]string, error) {
	k.mu.Lock()
	e, ok := k.m[key]
	k.mu.Unlock()
	if ok && time.Since(e.at) < k.ttl {
		return e.names, nil
	}
	names, err := load()
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.m[key] = cacheEntry{names: names, at: time.Now()}
	k.mu.Unlock()
	return names, nil
}

func (k *catalogCache) containers(ctx context.Context, db string) ([]string, error) {
	return k.get("c\x00"+db, func() ([]string, error) { return k.src.containers(ctx, db) })
}

func (k *catalogCache) fields(ctx context.Context, db, container string) ([]string, error) {
	return k.get("f\x00"+db+"\x00"+container, func() ([]string, error) { return k.src.fields(ctx, db, container) })
}

// Invalidate drops every cached lookup.
func (k *catalogCache) Invalidate() {
	k.mu.Lock()
	clear(k.m)
	k.mu.Unlock()
}

var envelopeContainer = regexp.MustCompile(`"container"\s*:\s*"([^"\\]*)$`)

func complete(ctx context.Context, src catalogSource, req driver.CompletionRequest) ([]driver.Completion, error) {
	if req.Cursor < 0 || req.Cursor > len(req.Text) {
		return nil, nil
	}
	prefix := req.Text[:req.Cursor]
	if strings.HasPrefix(strings.TrimSpace(req.Text), "{") {
		m := envelopeContainer.FindStringSubmatch(prefix)
		if m == nil {
			return nil, nil
		}
		return containerCompletions(ctx, src, req.Namespace, m[1])
	}
	toks, err := lex(prefix)
	if err != nil {
		return nil, nil // inside a string literal, or text the lexer refuses
	}
	word := ""
	if n := len(toks); n > 0 && toks[n-1].kind == tokIdent && strings.HasSuffix(prefix, toks[n-1].text) {
		word, toks = toks[n-1].text, toks[:n-1]
	}
	prev := func(back int) token {
		if i := len(toks) - back; i >= 0 {
			return toks[i]
		}
		return token{}
	}
	switch {
	case prev(1).isWord("from") || prev(1).isWord("in") && prev(3).isWord("from"):
		return containerCompletions(ctx, src, req.Namespace, word)
	case prev(1).kind == tokPunct && prev(1).text == ".":
		return fieldCompletions(ctx, src, req, toks, word)
	}
	if word == "" {
		return nil, nil
	}
	var out []driver.Completion
	for _, k := range sqlKeywords {
		if hasFoldPrefix(k, word) {
			out = append(out, driver.Completion{Label: k, Kind: "keyword"})
		}
	}
	for _, f := range sqlFunctions {
		if hasFoldPrefix(f, word) {
			out = append(out, driver.Completion{Label: f, Kind: "function", Insert: f + "("})
		}
	}
	return capCompletions(out), nil
}

func hasFoldPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func capCompletions(out []driver.Completion) []driver.Completion {
	if len(out) > maxCompletions {
		return out[:maxCompletions]
	}
	return out
}

func containerCompletions(ctx context.Context, src catalogSource, db, word string) ([]driver.Completion, error) {
	if db == "" {
		return nil, nil
	}
	names, err := src.containers(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []driver.Completion
	for _, n := range names {
		if hasFoldPrefix(n, word) {
			out = append(out, driver.Completion{Label: n, Kind: "collection", Detail: "container"})
		}
	}
	return capCompletions(out), nil
}

// fieldCompletions offers the next path segment after alias.a.b. for the
// query's container.
func fieldCompletions(ctx context.Context, src catalogSource, req driver.CompletionRequest, toks []token, word string) ([]driver.Completion, error) {
	// Walk back over "ident ." pairs to the alias: alias.a.b. → path [a b].
	var path []string
	i := len(toks) - 1
	for i >= 1 && toks[i].text == "." && toks[i-1].kind == tokIdent {
		path = append([]string{toks[i-1].text}, path...)
		i -= 2
	}
	if len(path) == 0 || req.Namespace == "" {
		return nil, nil
	}
	path = path[1:] // drop the alias
	all, err := lex(req.Text)
	if err != nil {
		all = toks
	}
	container, err := fromContainer(all)
	if err != nil {
		return nil, nil
	}
	fields, err := src.fields(ctx, req.Namespace, container)
	if err != nil {
		return nil, err
	}
	base := ""
	if len(path) > 0 {
		base = strings.Join(path, ".") + "."
	}
	var out []driver.Completion
	for _, f := range fields {
		rest, ok := strings.CutPrefix(f, base)
		if !ok || strings.Contains(rest, ".") || !hasFoldPrefix(rest, word) {
			continue
		}
		if !slices.ContainsFunc(out, func(c driver.Completion) bool { return c.Label == rest }) {
			out = append(out, driver.Completion{Label: rest, Kind: "field"})
		}
	}
	return capCompletions(out), nil
}
