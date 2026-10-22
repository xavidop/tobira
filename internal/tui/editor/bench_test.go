package editor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// benchEditor is a 5k-line buffer with the cursor in the middle.
func benchEditor() Model {
	m := New(theme.Dark(), keymap.Default()).Resize(120, 20)
	m.SetDialect("postgresql")
	var b strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&b, "select id, name, 'row %d' as label from people where id = %d; -- note %d\n", i, i, i)
	}
	m.SetValue(b.String())
	m.SetCursor(len(b.String()) / 2)
	return m
}

func BenchmarkEditorView(b *testing.B) {
	m := benchEditor()
	_ = m.View(120, 20, true) // the first frame lexes
	b.ReportAllocs()
	for b.Loop() {
		_ = m.View(120, 20, true)
	}
}
