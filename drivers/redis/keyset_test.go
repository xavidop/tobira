package redis

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// rehashScanner is a SCAN stand-in that keeps SCAN's only promise, that a
// full scan (cursor 0 back to 0) returns every key at least once, and
// nothing more: every full scan visits the keys in a new order, in batches
// of new sizes, with some keys returned twice, as a dict that rehashes
// between pages does.
type rehashScanner struct {
	keys    []string
	rnd     *rand.Rand
	batches [][]string
	calls   int
}

func (s *rehashScanner) scan(_ context.Context, cursor uint64) ([]string, uint64, error) {
	s.calls++
	if cursor == 0 {
		order := slices.Clone(s.keys)
		s.rnd.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for range len(order) / 8 { // duplicates, anywhere in the scan
			order = slices.Insert(order, s.rnd.IntN(len(order)+1), s.keys[s.rnd.IntN(len(s.keys))])
		}
		s.batches = nil
		for len(order) > 0 {
			n := min(s.rnd.IntN(40), len(order)) // some batches are empty
			s.batches = append(s.batches, order[:n])
			order = order[n:]
		}
		if len(s.batches) == 0 {
			s.batches = [][]string{nil}
		}
	}
	if cursor >= uint64(len(s.batches)) {
		return nil, 0, fmt.Errorf("unknown cursor %d", cursor)
	}
	next := cursor + 1
	if next == uint64(len(s.batches)) {
		next = 0
	}
	return slices.Clone(s.batches[cursor]), next, nil
}

func keepAll(_ context.Context, keys []string) ([]string, error) { return keys, nil }

// rowsFor makes a row per key; drop says which keys a condition rejects.
func rowsFor(drop func(string) bool) func(context.Context, []string) ([]driver.Row, error) {
	return func(_ context.Context, keys []string) ([]driver.Row, error) {
		out := make([]driver.Row, len(keys))
		for i, k := range keys {
			if !drop(k) {
				out[i] = driver.Row{driver.String(k)}
			}
		}
		return out, nil
	}
}

// Paging through a keyspace that reorders and repeats keys on every scan
// returns each key exactly once, in key order, whatever the page size, and
// with conditions that reject some keys.
func TestKeysetPageRehash(t *testing.T) {
	var keys []string
	for i := range 537 {
		keys = append(keys, fmt.Sprintf("p:%d", i))
	}
	keys = append(keys, "p:", "p:\x00bin", "p:\xffhigh", "p:a:b")
	for _, limit := range []int{1, 7, 100, 536, 541, 1000} {
		for _, dropEvery := range []int{0, 3} {
			drop := func(k string) bool { return dropEvery > 0 && len(k)%dropEvery == 0 }
			var want []string
			for _, k := range keys {
				if !drop(k) {
					want = append(want, k)
				}
			}
			slices.Sort(want)
			sc := &rehashScanner{keys: keys, rnd: rand.New(rand.NewPCG(uint64(limit), uint64(dropEvery)))}
			var got []string
			cur := browseCursor{}
			for page := 0; ; page++ {
				if page > len(keys)+2 {
					t.Fatalf("limit %d drop %d: no end after %d pages", limit, dropEvery, page)
				}
				rows, next, warns, err := keysetPage(context.Background(), sc.scan, cur, limit, keepAll, rowsFor(drop))
				if err != nil || len(warns) != 0 {
					t.Fatalf("limit %d drop %d page %d: %v %v", limit, dropEvery, page, err, warns)
				}
				if len(rows) > limit {
					t.Fatalf("limit %d: page of %d rows", limit, len(rows))
				}
				for _, r := range rows {
					got = append(got, r[0].String())
				}
				if next == "" {
					break
				}
				if cur, err = parseCursor(next); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("limit %d drop %d: got %d keys, want %d in key order (first diff at %d)",
					limit, dropEvery, len(got), len(want), firstDiff(got, want))
			}
		}
	}
}

func firstDiff(a, b []string) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// A scan that never finishes within the budget is an error on the first
// page, never a page built from part of the keyspace.
func TestKeysetPageBudget(t *testing.T) {
	endless := func(_ context.Context, cursor uint64) ([]string, uint64, error) {
		return []string{fmt.Sprintf("k:%d", cursor)}, cursor + 1, nil
	}
	_, _, _, err := keysetPage(context.Background(), endless, browseCursor{}, 10, keepAll, rowsFor(func(string) bool { return false }))
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "SCAN calls") {
		t.Errorf("endless scan: %v, want ErrUnsupported naming the SCAN budget", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sc := &rehashScanner{keys: []string{"a", "b"}, rnd: rand.New(rand.NewPCG(1, 2))}
	if _, _, _, err := keysetPage(ctx, sc.scan, browseCursor{}, 10, keepAll, rowsFor(func(string) bool { return false })); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: %v, want context.Canceled", err)
	}
}

// The key cursor round-trips any key bytes, and refuses oversized or
// malformed tokens.
func TestKeyCursor(t *testing.T) {
	for _, k := range []string{"", "a", "user:1", "x\x00y\xff:z", strings.Repeat("k", maxCursorKey)} {
		c := browseCursor{after: k, hasAfter: true}
		s, err := c.encode()
		if err != nil {
			t.Fatalf("encode %q: %v", k, err)
		}
		got, err := parseCursor(s)
		if err != nil || got != c {
			t.Errorf("parseCursor(encode(%q)) = %+v, %v", k, got, err)
		}
	}
	if _, err := (browseCursor{after: strings.Repeat("k", maxCursorKey+1), hasAfter: true}).encode(); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("encode of an oversized key: %v, want ErrUnsupported", err)
	}
	for _, bad := range []string{"k:!!", "k:" + strings.Repeat("A", 2*maxCursorKey), "s:1:2"} {
		if _, err := parseCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseCursor(%.20q) = %v, want ErrSyntax", bad, err)
		}
	}
}
