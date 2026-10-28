package github

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// catalogTTL is how long completion keeps the repository names.
const catalogTTL = 60 * time.Second

// maxCompletions caps one completion answer.
const maxCompletions = 200

// nameCache keeps the repository names for completion.
type nameCache struct {
	c     *conn
	mu    sync.Mutex
	names []string
	at    time.Time
}

func newNameCache(c *conn) *nameCache { return &nameCache{c: c} }

// Invalidate drops the cached names.
func (k *nameCache) Invalidate() {
	k.mu.Lock()
	k.names, k.at = nil, time.Time{}
	k.mu.Unlock()
}

// repositories lists the repository names, cached for catalogTTL. A
// listing that fails (no token, say) gives none.
func (k *nameCache) repositories(ctx context.Context) []string {
	k.mu.Lock()
	names, at := k.names, k.at
	k.mu.Unlock()
	if names != nil && time.Since(at) < catalogTTL {
		return names
	}
	nss, err := k.c.Namespaces(ctx)
	if err != nil {
		return nil
	}
	names = make([]string, len(nss))
	for i, ns := range nss {
		names[i] = ns.Name
	}
	k.mu.Lock()
	k.names, k.at = names, time.Now()
	k.mu.Unlock()
	return names
}

// Invalidate drops the cached repository names.
func (c *conn) Invalidate() { c.catalog.Invalidate() }

// Complete suggests, for the word at the cursor of a query:
//
//   - op names after "op":", collection names after "collection":",
//     repository names after "repo":", search types after "type":",
//     methods after "method":" and sort fields after "sort":";
//   - otherwise the members the op takes, with the collection's own
//     parameters for a list, and the collection's columns.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	return complete(ctx, c.catalog, req), nil
}

var (
	valueOfRe = regexp.MustCompile(`"(op|collection|repo|type|method|sort|order|state|direction)"\s*:\s*"$`)
	opRe      = regexp.MustCompile(`"op"\s*:\s*"([a-z_]+)"`)
	collRe    = regexp.MustCompile(`"collection"\s*:\s*"([a-z_]+)"`)
	keyPosRe  = regexp.MustCompile(`[{,]\s*"?$`)
)

func isWordByte(b byte) bool {
	return b == '_' || b == '/' || b == '-' || b == '.' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// complete answers a CompletionRequest.
func complete(ctx context.Context, names *nameCache, req driver.CompletionRequest) []driver.Completion {
	cur := max(0, min(req.Cursor, len(req.Text)))
	before := req.Text[:cur]
	start := cur
	for start > 0 && isWordByte(before[start-1]) {
		start--
	}
	prefix := before[start:]
	head := before[:start]
	var out []driver.Completion
	add := func(kind string, words ...string) {
		for _, w := range words {
			if len(out) >= maxCompletions {
				return
			}
			if strings.HasPrefix(strings.ToLower(w), strings.ToLower(prefix)) && !slices.ContainsFunc(out, func(c driver.Completion) bool { return c.Label == w }) {
				out = append(out, driver.Completion{Label: w, Kind: kind})
			}
		}
	}
	op := opList
	if m := opRe.FindStringSubmatch(req.Text); m != nil {
		op = m[1]
	}
	var col *collection
	if m := collRe.FindStringSubmatch(req.Text); m != nil {
		col = collectionByName(m[1])
	}
	if m := valueOfRe.FindStringSubmatch(head); m != nil {
		switch m[1] {
		case "op":
			add("keyword", sortedKeys(opKeys)...)
		case "collection":
			add("collection", collectionNames()...)
		case "repo":
			add("namespace", names.repositories(ctx)...)
		case "type":
			add("keyword", searchTypes...)
		case "method":
			add("keyword", restMethods...)
		case "sort":
			if op == opSearch {
				add("keyword", "created", "updated", "comments", "reactions", "interactions", "stars", "forks", "author-date", "committer-date")
			} else if col != nil {
				add("field", sortedValues(col.sorts)...)
			}
		case "order", "direction":
			add("keyword", "asc", "desc")
		case "state":
			add("keyword", "open", "closed", "all")
		}
		return out
	}
	if keyPosRe.MatchString(head) || head == "" {
		add("keyword", "op")
		add("keyword", opKeys[op]...)
		if op == opList && col != nil {
			add("keyword", col.params...)
		}
		if op == opGet && col != nil {
			add("field", col.keyCol)
		}
		return out
	}
	if col != nil {
		for _, c := range col.columns {
			add("field", c.name)
		}
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
