package textutil

import "testing"

func TestLevenshtein(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"", "", 0}, {"abc", "", 3}, {"nme", "name", 1}, {"kitten", "sitting", 3}, {"ō", "o", 1}} {
		if got := Levenshtein(tt.a, tt.b); got != tt.want {
			t.Errorf("Levenshtein(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
