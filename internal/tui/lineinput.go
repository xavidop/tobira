package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/query"
)

// lineInput is the cell editor's one-line text input. It holds the raw text,
// tabs and newlines included, so a value is never changed by being edited,
// and draws it escaped, scrolled to keep the cursor in view. The cursor is a
// byte offset on a grapheme cluster boundary.
type lineInput struct {
	text string
	cur  int
}

func (l *lineInput) set(s string) { l.text, l.cur = s, len(s) }

// clusters splits s into grapheme clusters.
func clusters(s string) []string {
	var out []string
	for s != "" {
		c, _ := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		if c == "" {
			c = s[:1]
		}
		out = append(out, c)
		s = s[len(c):]
	}
	return out
}

// prevBoundary and nextBoundary are the cluster boundaries around the cursor.
func (l *lineInput) prevBoundary() int {
	b := 0
	for _, c := range clusters(l.text[:l.cur]) {
		if b+len(c) >= l.cur {
			break
		}
		b += len(c)
	}
	return b
}

func (l *lineInput) nextBoundary() int {
	if l.cur >= len(l.text) {
		return l.cur
	}
	c, _ := ansi.FirstGraphemeCluster(l.text[l.cur:], ansi.GraphemeWidth)
	return l.cur + max(len(c), 1)
}

// key edits the text; it reports whether the key was one it handles.
func (l *lineInput) key(k tea.KeyPressMsg) bool {
	switch k.String() {
	case "left":
		l.cur = l.prevBoundary()
	case "right":
		l.cur = l.nextBoundary()
	case "home", "ctrl+a":
		l.cur = 0
	case "end":
		l.cur = len(l.text)
	case "backspace":
		if l.cur > 0 {
			p := l.prevBoundary()
			l.text, l.cur = l.text[:p]+l.text[l.cur:], p
		}
	case "delete":
		n := l.nextBoundary()
		l.text = l.text[:l.cur] + l.text[n:]
	default:
		if k.Text == "" || k.Mod&(tea.ModCtrl|tea.ModAlt) != 0 {
			return false
		}
		l.paste(k.Text)
	}
	return true
}

// paste inserts s at the cursor as is.
func (l *lineInput) paste(s string) {
	l.text = l.text[:l.cur] + s + l.text[l.cur:]
	l.cur += len(s)
}

// view draws the text escaped in exactly w columns, the cursor cell in cur.
func (l lineInput) view(w int, cur lipgloss.Style) string {
	if w < 1 {
		return ""
	}
	type cell struct {
		text string
		w    int
	}
	var cells []cell
	at := -1 // the cell the cursor is on
	off := 0
	for _, c := range clusters(l.text) {
		if off == l.cur {
			at = len(cells)
		}
		e := query.EscapeControls(c)
		cells = append(cells, cell{e, query.DisplayWidth(e)})
		off += len(c)
	}
	if at < 0 {
		at = len(cells)
		cells = append(cells, cell{" ", 1})
	}
	// Scroll so the cursor cell ends inside the view.
	start, used := at, cells[at].w
	for start > 0 && used+cells[start-1].w <= w {
		start--
		used += cells[start].w
	}
	var sb strings.Builder
	width := 0
	for i := start; i < len(cells); i++ {
		c := cells[i]
		if width+c.w > w {
			break
		}
		if i == at {
			sb.WriteString(cur.Render(c.text))
		} else {
			sb.WriteString(c.text)
		}
		width += c.w
	}
	if width < w {
		sb.WriteString(strings.Repeat(" ", w-width))
	}
	return sb.String()
}
