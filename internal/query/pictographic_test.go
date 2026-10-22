package query

import "testing"

// Each Extended_Pictographic range has its first and last rune accepted and
// the runes just outside it refused (where those are not themselves in a range).
func TestIsPictographicTable(t *testing.T) {
	in := func(r rune) bool {
		for _, rg := range extendedPictographic {
			if r >= rg[0] && r <= rg[1] {
				return true
			}
		}
		return false
	}
	if len(extendedPictographic) < 70 {
		t.Fatalf("table has %d ranges, want the full Unicode 15 list", len(extendedPictographic))
	}
	for _, rg := range extendedPictographic {
		if !isPictographic(rg[0]) || !isPictographic(rg[1]) {
			t.Errorf("range %U-%U not accepted", rg[0], rg[1])
		}
		if r := rg[0] - 1; !in(r) && isPictographic(r) {
			t.Errorf("%U accepted but outside every range", r)
		}
		if r := rg[1] + 1; !in(r) && isPictographic(r) {
			t.Errorf("%U accepted but outside every range", r)
		}
	}
	for _, r := range []rune{0xA9, 0x203C, 0x2764, 0x1F3F3, 0x1F468, 0x1F308, 0x1F9D1, 0x1FAE0, 0x1FC00} {
		if !isPictographic(r) {
			t.Errorf("%U should be pictographic", r)
		}
	}
	for _, r := range []rune{'a', '0', 0x200D, 0x0915, 0xFE0F, 0x20E3} {
		if isPictographic(r) {
			t.Errorf("%U must not be pictographic", r)
		}
	}
}
