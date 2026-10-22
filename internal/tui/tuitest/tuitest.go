// Package tuitest holds assertions shared by the view packages' tests.
package tuitest

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

// NoInjectedControls fails unless every ESC in raw begins a CSI SGR sequence
// (ESC [ params m, the styling a view emits itself) and raw has no BEL, no
// C1 control (as U+0080-U+009F or as a stray 0x80-0x9F byte) and no C0
// control besides newline. Unlike a check on ansi.Strip output, it sees
// sequences that came from database text.
func NoInjectedControls(t *testing.T, raw string) {
	t.Helper()
	if err := check(raw); err != nil {
		t.Fatalf("%v in %q", err, raw)
	}
}

func check(raw string) error {
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRuneInString(raw[i:])
		switch {
		case r == utf8.RuneError && size == 1 && raw[i] >= 0x80 && raw[i] <= 0x9f:
			return fmt.Errorf("raw C1 byte %#x at %d", raw[i], i)
		case r >= 0x80 && r <= 0x9f:
			return fmt.Errorf("C1 control %U at %d", r, i)
		case r == 0x1b:
			j := i + 1
			if j >= len(raw) || raw[j] != '[' {
				return fmt.Errorf("non-CSI escape at byte %d", i)
			}
			for j++; j < len(raw) && (raw[j] >= '0' && raw[j] <= '9' || raw[j] == ';' || raw[j] == ':'); j++ {
			}
			if j >= len(raw) || raw[j] != 'm' {
				return fmt.Errorf("CSI sequence that is not SGR at byte %d", i)
			}
			i = j + 1
			continue
		case r == '\n':
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("control byte %#x at %d", r, i)
		}
		i += size
	}
	return nil
}
