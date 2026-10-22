package dynamodb

import (
	"slices"
	"testing"
)

func TestReadyShards(t *testing.T) {
	s := func(start, parent string) *shardState {
		it := "it"
		return &shardState{iter: &it, start: start, parent: parent}
	}
	shards := map[string]*shardState{
		"c":  s("300", "a"), // child of a, which is still open
		"a":  s("100", ""),
		"b":  s("20", ""),
		"d":  s("1000", "gone"), // parent not tracked: ready
		"cc": s("400", "c"),     // grandchild: waits for c
	}
	got := readyShards(shards)
	if want := []string{"b", "a", "d"}; !slices.Equal(got, want) {
		t.Errorf("ready %v, want %v (by start sequence, children after their parents)", got, want)
	}
	delete(shards, "a")
	if got := readyShards(shards); !slices.Equal(got, []string{"b", "c", "d"}) {
		t.Errorf("after a closes: %v", got)
	}
}
