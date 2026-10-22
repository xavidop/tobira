package redis

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Every key listing is a bounded, cancellable SCAN; the driver never sends
// KEYS. These bounds apply to the driver's own scans (Collections, Describe,
// Browse); a user's SCAN in Query is a single call.
const (
	scanCount       = 1000            // SCAN COUNT hint per call
	maxScanCalls    = 200             // SCAN calls per operation (about 200k keys examined)
	scanTimeBudget  = 5 * time.Second // wall-clock budget per operation
	catalogSample   = 10000           // keys sampled by Collections
	describeSample  = 100             // keys sampled by Describe and Browse for the record schema
	maxSortKeys     = 10000           // keys a sorted Browse may load
	previewItems    = 100             // elements in a value preview
	previewBytes    = 4096            // bytes of a string value preview
	maxJSONPreview  = 64 * 1024       // bytes of a JSON value preview
	maxRecordFields = 100             // distinct hash fields before a prefix stops being a record collection
	rowChunk        = 100             // keys fetched per pipeline
	maxBrowseLimit  = 10000           // largest Browse page
	defaultLimit    = 100             // Browse page when Limit is 0
	watchBuffer     = 64              // events buffered by Watch
	nsCap           = 1024            // logical databases listed at most
)

// scanBudget bounds one operation's SCAN calls by count and time.
type scanBudget struct {
	calls    int
	deadline time.Time
}

func newScanBudget(d time.Duration) *scanBudget {
	return &scanBudget{deadline: time.Now().Add(d)}
}

// spent reports whether another SCAN call would exceed the budget.
func (b *scanBudget) spent() bool {
	return b.calls >= maxScanCalls || time.Now().After(b.deadline)
}

// scanOnce runs one SCAN with the collection's MATCH pattern and returns
// the keys that belong to the collection.
func scanOnce(ctx context.Context, cl *goredis.Client, b *scanBudget, coll string, cursor uint64) ([]string, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	b.calls++
	return scanBatch(ctx, cl, coll, cursor)
}

// scanBatch runs one SCAN call with the collection's MATCH pattern and
// returns the keys that belong to the collection.
func scanBatch(ctx context.Context, cl *goredis.Client, coll string, cursor uint64) ([]string, uint64, error) {
	keys, next, err := cl.Scan(ctx, cursor, matchPattern(coll), scanCount).Result()
	if err != nil {
		return nil, 0, err
	}
	out := keys[:0]
	for _, k := range keys {
		if inCollection(coll, k) {
			out = append(out, k)
		}
	}
	return out, next, nil
}

// sampleKeys returns up to max keys of a collection, and whether the scan
// covered the whole keyspace (so the keys are all of them when complete
// and fewer than max).
func sampleKeys(ctx context.Context, cl *goredis.Client, coll string, max int, budget time.Duration) ([]string, bool, error) {
	b := newScanBudget(budget)
	seen := map[string]bool{}
	var out []string
	var cursor uint64
	for {
		keys, next, err := scanOnce(ctx, cl, b, coll, cursor)
		if err != nil {
			return nil, false, err
		}
		for _, k := range keys {
			if seen[k] {
				continue
			}
			if len(out) >= max {
				return out, false, nil
			}
			seen[k] = true
			out = append(out, k)
		}
		if next == 0 {
			return out, true, nil
		}
		if b.spent() {
			return out, false, nil
		}
		cursor = next
	}
}
