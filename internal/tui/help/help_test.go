package help

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/palette"
	"github.com/xavidop/tobira/internal/tui/theme"
)

func TestHelpListsAllBindings(t *testing.T) {
	for _, km := range []keymap.Map{keymap.Default(), keymap.Vim()} {
		t.Run(km.Name, func(t *testing.T) {
			got := ansi.Strip(View(km, theme.Dark(), 120, 100))
			if !strings.Contains(got, "Keys — "+km.Name) {
				t.Errorf("missing title in %q", got)
			}
			for _, title := range keymap.GroupTitles() {
				if !strings.Contains(got, title) {
					t.Errorf("missing group %q", title)
				}
			}
			for _, grp := range km.Help() {
				for _, b := range grp {
					h := b.Help()
					if !strings.Contains(got, h.Key) || !strings.Contains(got, h.Desc) {
						t.Errorf("missing %q / %q", h.Key, h.Desc)
					}
				}
			}
		})
	}
}

func TestHelpFitsAndScrolls(t *testing.T) {
	km, th := keymap.Vim(), theme.Dark()
	for _, sz := range [][2]int{{-1, -1}, {0, 0}, {1, 1}, {5, 3}, {20, 5}, {40, 10}, {80, 24}, {200, 80}} {
		for off := -2; off < 100; off += 7 {
			v := ansi.Strip(ViewAt(km, th, sz[0], sz[1], off))
			lines := strings.Split(v, "\n")
			if sz[1] > 0 && len(lines) > sz[1] {
				t.Fatalf("%v off %d: %d lines", sz, off, len(lines))
			}
			for _, l := range lines {
				if query.DisplayWidth(l) > max(sz[0], 0) {
					t.Fatalf("%v: line too wide %q", sz, l)
				}
			}
		}
	}
	top := ansi.Strip(ViewAt(km, th, 40, 8, 0))
	scrolled := ansi.Strip(ViewAt(km, th, 40, 8, 5))
	if top == scrolled {
		t.Fatal("offset had no effect")
	}
	if !strings.Contains(scrolled, "Keys — vim") {
		t.Fatalf("title should stay pinned: %q", scrolled)
	}
}

func TestMaxOffset(t *testing.T) {
	if n := MaxOffset(keymap.Default(), 40, 8); n <= 0 {
		t.Fatalf("expected scrollable, got %d", n)
	}
	if n := MaxOffset(keymap.Default(), 200, 100); n != 0 {
		t.Fatalf("expected fits, got %d", n)
	}
}

// TestHelpListsPaletteCommands: the overlay lists the palette commands with
// their usage, :tx, :readonly and :export included.
func TestHelpListsPaletteCommands(t *testing.T) {
	got := ansi.Strip(View(keymap.Default(), theme.Dark(), 200, 100))
	if !strings.Contains(got, "Commands") {
		t.Fatalf("no Commands group in %q", got)
	}
	for _, c := range palette.Commands() {
		if !strings.Contains(got, c.Usage) || !strings.Contains(got, c.Desc) {
			t.Errorf("missing %q / %q", c.Usage, c.Desc)
		}
	}
	if strings.Contains(got, "arrives in M3") {
		t.Error("help still mentions an M3 placeholder")
	}
}
