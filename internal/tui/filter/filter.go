package filter

import (
	"errors"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

const prompt = "filter> "

// AppliedMsg is emitted when the user presses enter on valid input. An empty
// input yields the zero Filter, which clears the filter.
type AppliedMsg struct {
	Filter driver.Filter
	Text   string
}

// CancelledMsg is emitted on esc.
type CancelledMsg struct{}

// Model is the filter bar. While focused it swallows every key.
type Model struct {
	th   theme.Theme
	km   keymap.Map
	in   textinput.Model
	cols []driver.Column
	err  *ParseError
	w    int
}

// New returns a closed filter bar.
func New(th theme.Theme, km keymap.Map) Model {
	in := textinput.New()
	in.Prompt = prompt
	// A steady cursor: blinking would keep a timer command alive.
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	return Model{th: th, km: km, in: in}
}

// Resize sets the bar's width.
func (m Model) Resize(w int) Model {
	m.w = w
	m.in.SetWidth(max(w-len(prompt)-1, 1))
	return m
}

// SetTheme restyles the bar in place, keeping its text and focus.
func (m Model) SetTheme(th theme.Theme) Model { m.th = th; return m }

// SetKeymap swaps the key bindings in place.
func (m Model) SetKeymap(km keymap.Map) Model { m.km = km; return m }

// Open focuses the bar for a collection with columns cols, prefilled with
// the text of the filter that is currently applied.
func (m Model) Open(cols []driver.Column, current string) Model {
	m.cols, m.err = cols, nil
	m.in.SetValue(current)
	m.in.CursorEnd()
	m.in.Focus()
	return m
}

// Focused reports whether the bar is open.
func (m Model) Focused() bool { return m.in.Focused() }

// Value is the text in the input.
func (m Model) Value() string { return m.in.Value() }

// Update handles a key while the bar is open.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if !m.in.Focused() {
		return m, nil
	}
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		return m, cmd
	}
	switch {
	case m.km.Matches(k, keymap.Back):
		m.in.Blur()
		m.err = nil
		return m, func() tea.Msg { return CancelledMsg{} }
	case m.km.Matches(k, keymap.Open):
		text := m.in.Value()
		f, err := Parse(text, m.cols)
		if err != nil {
			var pe *ParseError
			if !errors.As(err, &pe) {
				pe = &ParseError{Pos: len(text), Msg: "invalid filter"}
			}
			m.err = pe
			return m, nil
		}
		m.in.Blur()
		m.err = nil
		return m, func() tea.Msg { return AppliedMsg{Filter: f, Text: text} }
	}
	before := m.in.Value()
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(k)
	if m.in.Value() != before {
		m.err = nil
	}
	return m, cmd
}

// View renders the input line and, after a failed parse, a second line with
// a caret under the offending position. It is never wider than width.
func (m Model) View(width int) string {
	if width < 1 {
		return ""
	}
	line := m.in.View()
	if m.err == nil {
		return ansi.Truncate(line, width, "")
	}
	text := m.in.Value()
	pos := min(max(m.err.Pos, 0), len(text))
	for pos > 0 && pos < len(text) && !isRuneStart(text[pos]) {
		pos--
	}
	col := len(prompt) + query.DisplayWidth(query.EscapeControls(text[:pos]))
	msg := strings.Repeat(" ", col) + "^ " + query.EscapeControls(m.err.Msg)
	return ansi.Truncate(line, width, "") + "\n" + m.th.Styles.Error.Render(query.Truncate(msg, width))
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
