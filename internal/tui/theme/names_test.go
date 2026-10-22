package theme

import (
	"slices"
	"testing"

	"github.com/xavidop/tobira/internal/config"
)

// config cannot import this package, so it keeps its own list of names.
func TestNamesMatchConfig(t *testing.T) {
	if !slices.Equal(Names(), config.ThemeNames()) {
		t.Fatalf("Names() = %v, config.ThemeNames() = %v", Names(), config.ThemeNames())
	}
	// ByName accepts exactly Names(), not just a superset.
	for _, n := range []string{"", "x", "dark", "light", "AUTO", "tobira", "tobira-darker"} {
		if _, err := ByName(n, true); err == nil && !slices.Contains(Names(), n) {
			t.Errorf("ByName(%q) accepted but is not in Names()", n)
		}
	}
	for _, n := range Names() {
		if _, err := ByName(n, true); err != nil {
			t.Errorf("ByName(%q): %v", n, err)
		}
	}
}
