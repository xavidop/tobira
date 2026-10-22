package firestore

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// catalogTTL is how long completion keeps the collection list.
const catalogTTL = 60 * time.Second

// catalogTimeout bounds one catalog read for completion.
const catalogTimeout = 3 * time.Second

var (
	_ driver.Completer          = (*conn)(nil)
	_ driver.CatalogInvalidator = (*conn)(nil)
)

// requestKeys are the request keys, in the order completion offers them.
var requestKeys = []string{"collection", "collectionGroup", "document", "where", "orderBy", "limit", "offset", "select", "count"}

// operators are the where operators, plus the sort directions.
var operators = []string{"==", "!=", "<", "<=", ">", ">=", "array-contains", "array-contains-any", "in", "not-in", "asc", "desc"}

// Complete suggests, for the word at the cursor in a request: root
// collection names in the value of collection, collectionGroup or
// document; request keys, where operators and sort directions anywhere
// else. Collections come from Collections and are cached for 60 s;
// Invalidate drops the cache.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	return complete(ctx, c.catalog, req)
}

// Invalidate drops the cached collection list.
func (c *conn) Invalidate() { c.catalog.Invalidate() }

// catalogSource is what completion reads collection names from.
type catalogSource interface {
	collections(ctx context.Context) ([]string, error)
}

// connCatalog reads root collections through the connection.
type connCatalog struct{ c *conn }

func (r connCatalog) collections(ctx context.Context) ([]string, error) {
	cols, err := r.c.Collections(ctx, driver.NamespaceRef{})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cols))
	for i, col := range cols {
		out[i] = col.Ref.Name
	}
	return out, nil
}

// catalogCache caches a catalogSource's collection list for catalogTTL.
// It is safe for concurrent use.
type catalogCache struct {
	src  catalogSource
	mu   sync.Mutex
	at   time.Time
	list []string
}

func newCatalogCache(src catalogSource) *catalogCache { return &catalogCache{src: src} }

func (k *catalogCache) collections(ctx context.Context) ([]string, error) {
	k.mu.Lock()
	if k.list != nil && time.Since(k.at) < catalogTTL {
		list := k.list
		k.mu.Unlock()
		return list, nil
	}
	k.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	list, err := k.src.collections(cctx)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []string{}
	}
	k.mu.Lock()
	k.list, k.at = list, time.Now()
	k.mu.Unlock()
	return list, nil
}

// Invalidate drops the cached list.
func (k *catalogCache) Invalidate() {
	k.mu.Lock()
	k.list = nil
	k.mu.Unlock()
}

// targetValue matches text ending inside the string value of a key that
// names a collection or document.
var targetValue = regexp.MustCompile(`"(collection|collectionGroup|document)"\s*:\s*"[^"]*$`)

// complete computes the suggestions for req against src.
func complete(ctx context.Context, src catalogSource, req driver.CompletionRequest) ([]driver.Completion, error) {
	cur := min(max(req.Cursor, 0), len(req.Text))
	before := req.Text[:cur]
	start := strings.LastIndexAny(before, "\"[{,: \t\n") + 1
	word := before[start:]
	var out []driver.Completion
	if targetValue.MatchString(before) {
		names, err := src.collections(ctx)
		if err != nil {
			return nil, nil // completion is best effort: no names is the answer
		}
		for _, n := range names {
			if strings.HasPrefix(n, word) {
				out = append(out, driver.Completion{Label: n, Kind: "collection"})
			}
		}
		return out, nil
	}
	for _, k := range requestKeys {
		if strings.HasPrefix(k, word) {
			out = append(out, driver.Completion{Label: k, Kind: "keyword"})
		}
	}
	if word != "" || strings.HasSuffix(before, `"`) {
		for _, o := range operators {
			if strings.HasPrefix(o, word) && !slices.ContainsFunc(out, func(c driver.Completion) bool { return c.Label == o }) {
				out = append(out, driver.Completion{Label: o, Kind: "keyword", Detail: "operator"})
			}
		}
	}
	return out, nil
}
