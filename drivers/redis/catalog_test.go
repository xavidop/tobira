package redis

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func collNames(cols []driver.Collection) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Ref.Name
	}
	return out
}

// Keys without a ':' are row data (session tokens, e-mail addresses …):
// they stay in "*" and never name a collection of their own.
func TestPrefixCollectionsFoldsKeysWithoutSeparator(t *testing.T) {
	keys := []string{"session_abc123", "apikey-42", "alice@example.com", "user:1", "user:2", "doc:1", "user", ":x"}
	types := []string{"string", "string", "hash", "hash", "hash", "string", "string", "string"}
	cols := prefixCollections("0", keys, types, true)
	if got, want := collNames(cols), []string{"doc", "user"}; !slices.Equal(got, want) {
		t.Fatalf("collections = %q, want %q", got, want)
	}
	if cols[1].RowEstimate != 2 || cols[1].Type != driver.CollectionCollection {
		t.Errorf("user = %+v, want 2 hash records (the bare key user is not one)", cols[1])
	}
}

func TestPrefixCollectionsCap(t *testing.T) {
	var keys, types []string
	for i := range maxPrefixes + 37 {
		keys = append(keys, fmt.Sprintf("p%04d:x", i))
		types = append(types, "string")
	}
	cols := prefixCollections("0", keys, types, false)
	if len(cols) != maxPrefixes {
		t.Fatalf("%d collections, want the cap %d", len(cols), maxPrefixes)
	}
	if cols[0].Ref.Name != "p0000" || cols[maxPrefixes-1].Ref.Name != fmt.Sprintf("p%04d", maxPrefixes-1) {
		t.Errorf("first/last = %s/%s, want the sorted first %d", cols[0].Ref.Name, cols[maxPrefixes-1].Ref.Name, maxPrefixes)
	}
	last := cols[maxPrefixes-1].Comment
	if !strings.Contains(last, "37 more prefixes") {
		t.Errorf("last comment = %q, want a note on the 37 prefixes left out", last)
	}
	if strings.Contains(cols[0].Comment, "more prefixes") {
		t.Errorf("only the last comment carries the note: %q", cols[0].Comment)
	}

	exact := prefixCollections("0", keys[:maxPrefixes], types[:maxPrefixes], true)
	if len(exact) != maxPrefixes || strings.Contains(exact[maxPrefixes-1].Comment, "more") {
		t.Errorf("at the cap: %d collections, last comment %q", len(exact), exact[maxPrefixes-1].Comment)
	}
}
