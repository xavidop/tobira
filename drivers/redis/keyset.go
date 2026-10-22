package redis

import (
	"cmp"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/xavidop/tobira/driver"
)

// scanFunc runs one SCAN call from cursor and returns the keys of the
// collection it found (possibly repeated, in any order) and the next
// cursor; 0 ends the scan.
type scanFunc func(ctx context.Context, cursor uint64) ([]string, uint64, error)

// keysetPage fills one unsorted Browse page, keyset-paged by key name. SCAN
// promises only that a full scan returns every key at least once: between
// pages a rehash can reorder keys, repeat them or move them to another
// cursor, so a page cannot resume at a SCAN cursor. Instead each page runs
// a full scan (cursor 0 back to 0) and keeps the smallest keys strictly
// greater than cur.after (smallestAfter), fetches their rows (rows, aligned
// with the keys, nil for a key that vanished or failed the conditions) and
// returns them in key order. When conditions reject keys, it scans again
// from the last key examined until the page is full or the keys run out.
//
// keep is the key-only filter (Filter.Raw). Every page costs a full scan of
// the database, bounded by maxScanCalls and scanTimeBudget: when the first
// scan of a page cannot finish within them the page is ErrUnsupported, never
// a page built from part of the keyspace; when a later one cannot, the page
// ends early with a warning, at a key every key below which was examined.
func keysetPage(ctx context.Context, scan scanFunc, cur browseCursor, limit int,
	keep func(context.Context, []string) ([]string, error),
	rows func(context.Context, []string) ([]driver.Row, error),
) ([]driver.Row, string, []string, error) {
	budget := newScanBudget(scanTimeBudget)
	after, hasAfter := cur.after, cur.hasAfter
	var page []driver.Row
	for {
		keys, more, err := smallestAfter(ctx, scan, budget, after, hasAfter, limit+1, keep)
		if err != nil {
			if errBudget(err) && len(page) > 0 {
				next, err := browseCursor{after: after, hasAfter: true}.encode()
				return page, next, []string{fmt.Sprintf("scan budget reached after %d SCAN calls: the page is short; continue with the next page", budget.calls)}, err
			}
			return nil, "", nil, err
		}
		for i := 0; i < len(keys); i += rowChunk {
			chunk := keys[i:min(i+rowChunk, len(keys))]
			got, err := rows(ctx, chunk)
			if err != nil {
				return nil, "", nil, err
			}
			for j, r := range got {
				if r == nil {
					continue
				}
				if len(page) == limit { // a further row exists: continue after the last one returned
					next, err := browseCursor{after: keys[i+j-1], hasAfter: true}.encode()
					return page, next, nil, err
				}
				page = append(page, r)
			}
		}
		if !more {
			return page, "", nil, nil
		}
		after, hasAfter = keys[len(keys)-1], true
		if len(page) == limit {
			next, err := browseCursor{after: after, hasAfter: true}.encode()
			return page, next, nil, err
		}
	}
}

// errScanBudget marks a full scan the budget cut short.
type errScanBudget struct{ calls int }

func (e errScanBudget) Error() string {
	return fmt.Sprintf("browsing needs a full SCAN of the database per page, and it did not finish within %d SCAN calls or %s (stopped after %d SCAN calls); narrow the browse with a filter pattern, or read keys with a SCAN query",
		maxScanCalls, scanTimeBudget, e.calls)
}

func errBudget(err error) bool {
	var e errScanBudget
	return errors.As(err, &e)
}

// smallestAfter runs a full scan (cursor 0 back to 0) and returns, sorted,
// the k smallest distinct keys strictly greater than after (any key when
// !hasAfter) that keep passes, and whether more such keys exist. It holds
// at most k keys (a max-heap plus a set of its members, so a repeated key
// is counted once). A scan the budget cuts short is ErrUnsupported
// (wrapping errScanBudget).
func smallestAfter(ctx context.Context, scan scanFunc, budget *scanBudget, after string, hasAfter bool, k int,
	keep func(context.Context, []string) ([]string, error),
) ([]string, bool, error) {
	h := &keyHeap{}
	in := map[string]bool{}
	more := false
	var cursor uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if budget.spent() {
			return nil, false, driver.Wrap(driver.ErrUnsupported, Name, "browse", errScanBudget{calls: budget.calls})
		}
		budget.calls++
		batch, next, err := scan(ctx, cursor)
		if err != nil {
			return nil, false, err
		}
		if batch, err = keep(ctx, batch); err != nil {
			return nil, false, err
		}
		for _, key := range batch {
			if hasAfter && key <= after || in[key] {
				continue
			}
			if h.Len() < k {
				heap.Push(h, key)
				in[key] = true
				continue
			}
			more = true
			if key < (*h)[0] {
				delete(in, (*h)[0])
				(*h)[0] = key
				heap.Fix(h, 0)
				in[key] = true
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	keys := []string(*h)
	slices.Sort(keys)
	return keys, more, nil
}

// keyHeap is a max-heap of keys in byte order.
type keyHeap []string

func (h keyHeap) Len() int           { return len(h) }
func (h keyHeap) Less(i, j int) bool { return cmp.Less(h[j], h[i]) }
func (h keyHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *keyHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *keyHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
