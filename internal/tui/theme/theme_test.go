package theme

import (
	"image/color"
	"math"
	"testing"

	"charm.land/lipgloss/v2"
)

func hex(c color.Color) [4]uint32 {
	r, g, b, a := c.RGBA()
	return [4]uint32{r, g, b, a}
}

func TestThemesHavePalette(t *testing.T) {
	d := Dark()
	want := map[string]struct {
		got color.Color
		hex string
	}{
		"Bg":     {d.Bg, "#0B1220"},
		"Fg":     {d.Fg, "#E8ECF4"},
		"Accent": {d.Accent, "#5EEAD4"},
		"Muted":  {d.Muted, "#94A3B8"},
	}
	for name, w := range want {
		if hex(w.got) != hex(lipgloss.Color(w.hex)) {
			t.Errorf("Dark %s = %v, want %s", name, hex(w.got), w.hex)
		}
	}
	if d.Name != "tobira-dark" {
		t.Errorf("Dark name = %q", d.Name)
	}
	l := Light()
	if l.Name != "tobira-light" {
		t.Errorf("Light name = %q", l.Name)
	}
	if hex(l.Accent) != hex(d.Accent) {
		t.Errorf("Light accent %v != Dark accent %v", hex(l.Accent), hex(d.Accent))
	}
	if hex(l.Bg) == hex(d.Bg) {
		t.Error("Light and Dark share a background")
	}
}

func TestByNameAuto(t *testing.T) {
	tests := []struct {
		name string
		dark bool
		want string
		err  bool
	}{
		{"auto", true, "tobira-dark", false},
		{"auto", false, "tobira-light", false},
		{"tobira-dark", false, "tobira-dark", false},
		{"tobira-light", true, "tobira-light", false},
		{"nope", true, "", true},
		{"", true, "", true},
	}
	for _, tt := range tests {
		got, err := ByName(tt.name, tt.dark)
		if (err != nil) != tt.err {
			t.Fatalf("ByName(%q,%v) err = %v", tt.name, tt.dark, err)
		}
		if !tt.err && got.Name != tt.want {
			t.Errorf("ByName(%q,%v) = %s, want %s", tt.name, tt.dark, got.Name, tt.want)
		}
	}
}

func TestNullStyleIsMutedItalic(t *testing.T) {
	for _, th := range []Theme{Dark(), Light()} {
		s := th.Styles.Null
		if !s.GetItalic() {
			t.Errorf("%s: Null style not italic", th.Name)
		}
		if hex(s.GetForeground()) != hex(th.Muted) {
			t.Errorf("%s: Null foreground is not Muted", th.Name)
		}
	}
}

func lum(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	ch := func(v uint32) float64 {
		x := float64(v) / 65535
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(r) + 0.7152*ch(g) + 0.0722*ch(b)
}

func contrast(a, b color.Color) float64 {
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestStyleContrast(t *testing.T) {
	for _, th := range []Theme{Dark(), Light()} {
		styles := map[string]lipgloss.Style{
			"Title": th.Styles.Title, "Header": th.Styles.Header, "Cell": th.Styles.Cell,
			"CellCursor": th.Styles.CellCursor, "StatusBar": th.Styles.StatusBar,
		}
		for name, s := range styles {
			bg := s.GetBackground()
			if _, unset := bg.(lipgloss.NoColor); unset {
				bg = th.Bg
			}
			if r := contrast(s.GetForeground(), bg); r < 4.5 {
				t.Errorf("%s %s contrast %.2f < 4.5", th.Name, name, r)
			}
		}
	}
}
