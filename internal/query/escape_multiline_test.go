package query_test

import (
	"testing"

	"github.com/xavidop/tobira/internal/query"
)

func TestEscapeControlsMultilineKeepsLines(t *testing.T) {
	got := query.EscapeControlsMultiline("a\nb\tc\x1b[31m\x07\u202e")
	want := "a\nb\tc\\x1b[31m\\x07\\u{202E}"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if query.EscapeControlsMultiline("plain\ntext") != "plain\ntext" {
		t.Error("clean text changed")
	}
}

func TestEscapeControlsMultilineHostileInput(t *testing.T) {
	cases := map[string]string{
		"a\rb":          "a\\x0db",
		"x\xffy":        "x\\x{FF}y",
		"\u202etxt":     "\\u{202E}txt",
		"\x1b]0;t\x07z": "\\x1b]0;t\\x07z",
		"\x9b31m":       "\\x{9B}31m",
	}
	for in, want := range cases {
		if got := query.EscapeControlsMultiline(in); got != want {
			t.Errorf("EscapeControlsMultiline(%q) = %q, want %q", in, got, want)
		}
	}
}
