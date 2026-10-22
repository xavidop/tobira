package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/tui/tuitest"
)

func TestLineInputEditing(t *testing.T) {
	var l lineInput
	l.set("héllo")
	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyLeft}, {Code: tea.KeyLeft}, {Code: tea.KeyBackspace}, // hélo with the cursor before "lo"
		{Code: 'X', Text: "X"},
		{Code: tea.KeyHome}, {Code: tea.KeyDelete},
		{Code: tea.KeyEnd}, {Code: '!', Text: "!"},
	} {
		l.key(k)
	}
	if l.text != "éXlo!" {
		t.Fatalf("text = %q", l.text)
	}
	l.paste("\t1\n2")
	if l.text != "éXlo!\t1\n2" {
		t.Fatalf("paste changed the text: %q", l.text)
	}
	// A grapheme cluster is one step.
	l.set("a👍🏽")
	l.key(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if l.text != "a" {
		t.Fatalf("backspace over an emoji cluster = %q", l.text)
	}
}

func TestLineInputViewEscapesAndScrolls(t *testing.T) {
	var l lineInput
	l.set("x\x1b]52;c;AAAA\x07y\u202e" + strings.Repeat("z", 50))
	cur := lipgloss.NewStyle().Reverse(true)
	for _, w := range []int{1, 5, 12, 80} {
		v := l.view(w, cur)
		tuitest.NoInjectedControls(t, v)
		if got := ansi.StringWidth(v); got != w {
			t.Fatalf("view(%d) is %d wide: %q", w, got, ansi.Strip(v))
		}
	}
	if v := ansi.Strip(l.view(200, cur)); !strings.Contains(v, `x\x1b]52;c;AAAA\x07y\u{202E}`) {
		t.Fatalf("controls not shown escaped: %q", v)
	}
	// The cursor (at the end) is always in view.
	if v := ansi.Strip(l.view(6, cur)); !strings.HasPrefix(v, "zzzzz") {
		t.Fatalf("narrow view does not follow the cursor: %q", v)
	}
	l.key(tea.KeyPressMsg{Code: tea.KeyHome})
	if v := ansi.Strip(l.view(6, cur)); !strings.HasPrefix(v, "x") {
		t.Fatalf("home did not scroll back: %q", v)
	}
}
