package keymap

import (
	"slices"
	"testing"

	"github.com/xavidop/tobira/internal/config"
)

// config cannot import this package, so it keeps its own list of names.
func TestNamesMatchConfig(t *testing.T) {
	if !slices.Equal(Names(), config.KeymapNames()) {
		t.Fatalf("Names() = %v, config.KeymapNames() = %v", Names(), config.KeymapNames())
	}
	// ByName accepts exactly Names(), not just a superset.
	for _, n := range []string{"", "x", "emacs", "Vim", "DEFAULT", "vi"} {
		if _, err := ByName(n); err == nil && !slices.Contains(Names(), n) {
			t.Errorf("ByName(%q) accepted but is not in Names()", n)
		}
	}
	for _, n := range Names() {
		if _, err := ByName(n); err != nil {
			t.Errorf("ByName(%q): %v", n, err)
		}
	}
}
