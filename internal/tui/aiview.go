package tui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// aiView draws the panel in r: exactly r.h lines of r.w columns. The first
// line is the header, the last the input; the transcript fills the rest,
// anchored to the bottom.
func (a *Model) aiView(r rect) []string {
	if r.w < 1 || r.h < 1 {
		return nil
	}
	focused := a.focus == focusAI
	head := a.titleLine(a.aiHeader(r.w), r.w, focused)
	if r.h == 1 {
		return []string{head}
	}
	input := a.aiInputLine(r.w, focused)
	bodyH := r.h - 2
	body := a.ai.body
	// Scrolled up by a.ai.scroll lines (clamped by aiPrepare).
	end := max(len(body)-min(a.ai.scroll, len(body)), 0)
	start := max(end-bodyH, 0)
	shown := body[start:end]
	out := []string{head}
	out = append(out, fit(strings.Join(shown, "\n"), r.w, bodyH)...)
	return append(out, fit(input, r.w, 1)...)
}

// aiPrepare renders the panel's transcript for r and clamps the scroll to
// it. relayout calls it, so View only draws.
func (a *Model) aiPrepare(r rect) {
	st := a.th.Styles
	var body []string
	switch {
	case r.w < 1 || r.h < 2:
	case a.aiUnavailable() != "":
		body = wrapStyled(st.Error, a.aiUnavailable(), r.w)
	case a.aiConn() == "":
		body = wrapStyled(st.Dim, "select a connection in the sidebar to talk about it", r.w)
	default:
		body = a.aiTranscript(a.aiConn(), r.w)
	}
	a.ai.body = body
	a.ai.scroll = min(a.ai.scroll, max(len(body)-(r.h-2), 0))
}

// aiInputLine is the input, or a hint while the panel is not focused.
func (a *Model) aiInputLine(w int, focused bool) string {
	if !focused {
		return a.th.Styles.Dim.Render(query.Truncate("a close · tab focus · :ask <question>", w))
	}
	return ansi.Truncate(a.ai.in.View(), w, "")
}

// aiTranscript renders conn's conversation, w columns wide.
func (a *Model) aiTranscript(conn string, w int) []string {
	st := a.th.Styles
	c := a.ai.conv[conn]
	var lines []string
	if c == nil || len(c.entries) == 0 {
		lines = wrapStyled(st.Dim, "ask about "+conn+": ctrl+t switches between ask (propose a query) and answer (run read-only queries)", w)
	}
	if c != nil {
		last := -1
		for i, e := range c.entries {
			if e.kind == entryProposal {
				last = i
			}
		}
		for i := range c.entries {
			e := &c.entries[i]
			if i > 0 && (e.kind == entryUser || e.kind == entryNote && c.entries[i-1].kind != entryNote) {
				lines = append(lines, "")
			}
			lines = append(lines, a.aiEntryLines(e, w, i == last)...)
		}
	}
	if p := a.aiPending(conn); p != "" {
		lines = append(lines, "", st.Dim.Render(query.Truncate(p, w)))
	}
	return lines
}

// aiEntryLines draws one transcript entry. Every string here may come from
// the model or the database: it is escaped before it is styled.
func (a *Model) aiEntryLines(e *aiEntry, w int, latest bool) []string {
	st := a.th.Styles
	switch e.kind {
	case entryUser:
		return wrapStyled(st.Title, "› "+e.text, w)
	case entryModel:
		return a.markdownLines(e, w)
	case entryTool:
		return toolLines(st.Dim, e.conn, e.tc, w)
	case entryProposal:
		return a.proposalLines(e.text, w, latest)
	case entryFooter:
		return []string{st.Dim.Render(query.Truncate(query.EscapeControls(e.text), w))}
	case entryError:
		return wrapStyled(st.Error, e.text, w)
	}
	return wrapStyled(st.Dim, e.text, w)
}

// toolLines draws a tool call whole: name, connection, input and outcome,
// wrapped to w columns with the continuation lines indented under the
// marker, so an error or a long query is never cut short.
func toolLines(s lipgloss.Style, conn string, tc assistant.ToolCall, w int) []string {
	text := "▸ " + tc.Name + " on " + conn + ": " + tc.Input
	switch {
	case tc.Err != "":
		text += " → error: " + tc.Err
	case tc.Rows >= 0:
		text += fmt.Sprintf(" → %d %s", tc.Rows, plural(tc.Rows, "row", "rows"))
	}
	const indent = "  "
	lines := wrapStyled(s, text, max(w-len(indent), 1))
	for i := 1; i < len(lines); i++ {
		lines[i] = s.Render(indent) + lines[i]
	}
	return lines
}

// proposalLines draws a proposed query in a bordered block, with the keys
// that act on it under the latest one.
func (a *Model) proposalLines(q string, w int, latest bool) []string {
	st := a.th.Styles
	inner := max(w-4, 1)
	text := strings.ReplaceAll(query.EscapeControlsMultiline(q), "\t", "    ")
	border := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(a.th.Accent).
		Foreground(a.th.Fg).Padding(0, 1).Width(inner + 4)
	block := border.Render(ansi.Wrap(text, inner, ""))
	lines := strings.Split(block, "\n")
	if latest {
		lines = append(lines, st.Dim.Render(query.Truncate("ctrl+o load · f5 run · ctrl+y copy", w)))
	}
	return lines
}

// wrapStyled escapes text, wraps it to w columns and styles each line.
func wrapStyled(s lipgloss.Style, text string, w int) []string {
	text = strings.ReplaceAll(query.EscapeControlsMultiline(text), "\t", "    ")
	lines := strings.Split(ansi.Wrap(text, max(w, 1), ""), "\n")
	for i, l := range lines {
		lines[i] = s.Render(l)
	}
	return lines
}

// markdownLines renders a model's markdown with glamour, cached per width
// and theme. The text is escaped first, so its control characters show as
// text, and glamour's output keeps only colour and style sequences: no
// hyperlink or other sequence a model could steer reaches the terminal.
func (a *Model) markdownLines(e *aiEntry, w int) []string {
	if e.md != nil && e.mdW == w && e.mdTheme == a.th.Name {
		return e.md
	}
	src := strings.ReplaceAll(query.EscapeControlsMultiline(e.text), "\t", "    ")
	// goldmark decodes character references (&#27; → ESC) after any escape
	// done here, so no & reaches it: each one travels as a private-use
	// placeholder and comes back after rendering. Entities show literally.
	src = strings.ReplaceAll(src, "&", ampHold)
	out, err := renderMarkdown(src, a.th, w)
	if err != nil {
		e.md = wrapStyled(a.th.Styles.Cell, e.text, w)
	} else {
		// Escaping widens what glamour wrapped: wrap again to w.
		clean := strings.ReplaceAll(sgrOnly(out), ampHold, "&")
		e.md = strings.Split(ansi.Wrap(strings.Trim(clean, "\n"), max(w, 1), ""), "\n")
		for i, l := range e.md {
			e.md[i] = strings.TrimRight(l, " ")
		}
	}
	e.mdW, e.mdTheme = w, a.th.Name
	return e.md
}

// ampHold stands in for "&" while markdown renders (U+E000, private use).
const ampHold = "\uE000"

// renderMarkdown renders md for a w-column panel in theme th's palette.
func renderMarkdown(md string, th theme.Theme, w int) (string, error) {
	r, err := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle(th)), glamour.WithWordWrap(max(w, 1)))
	if err != nil {
		return "", err
	}
	return r.Render(md)
}

// markdownStyle is glamour's dark or light style recoloured from the tobira
// palette, without the document margin (the panel is narrow).
func markdownStyle(th theme.Theme) gansi.StyleConfig {
	s := styles.DarkStyleConfig
	if th.Name == theme.Light().Name {
		s = styles.LightStyleConfig
	}
	fg, accent, muted, danger := hexColor(th.Fg), hexColor(th.Accent), hexColor(th.Muted), hexColor(th.Danger)
	if th.Name == theme.Light().Name {
		accent = hexColor(th.OK) // the light accent is too pale as text
	}
	zero := uint(0)
	s.Document.Margin = &zero
	s.Document.BlockPrefix, s.Document.BlockSuffix = "", ""
	s.Document.Color = &fg
	s.Heading.Color = &accent
	s.H1.Color, s.H1.BackgroundColor = &accent, nil
	s.H1.Prefix, s.H1.Suffix = "# ", ""
	s.H6.Color = &muted
	s.Link.Color, s.LinkText.Color = &muted, &accent
	s.Code.Color, s.Code.BackgroundColor = &accent, nil
	s.CodeBlock.Margin = &zero
	s.CodeBlock.Color = &muted
	s.HorizontalRule.Color = &muted
	s.BlockQuote.Color = &muted
	s.Image.Color, s.ImageText.Color = &danger, &muted
	return s
}

func hexColor(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", r>>8, g>>8, b>>8)
}

// sgrOnly keeps only the SGR (colour and style) sequences of glamour's
// output and escapes all the text between them with query.EscapeControls,
// line by line. Escaping the markdown first is not enough: glamour decodes
// character references (&#x202E;, &#x9d;) after it, so bidi, C1 and
// invalid UTF-8 could come back. OSC (hyperlinks, titles), other CSI
// sequences and lone escapes are dropped.
func sgrOnly(s string) string {
	var sb, seg strings.Builder
	flush := func() {
		lines := strings.Split(seg.String(), "\n")
		for i, l := range lines {
			lines[i] = query.EscapeControls(l)
		}
		sb.WriteString(strings.Join(lines, "\n"))
		seg.Reset()
	}
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			seg.WriteByte(s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			break // a trailing lone ESC
		}
		switch s[i+1] {
		case '[': // CSI: keep it only when it is SGR
			j := i + 2
			for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
				j++
			}
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
				j++
			}
			if j >= len(s) {
				i++ // unterminated: drop the ESC, keep the rest as text
				continue
			}
			if s[j] == 'm' && onlySGRParams(s[i+2:j]) {
				flush()
				sb.WriteString(s[i : j+1])
			}
			i = j + 1
		case ']', 'P', '_', '^', 'X': // string sequences end at BEL or ST
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !isST(s, j) {
				j++
			}
			switch {
			case j >= len(s):
				i += 2 // unterminated: drop the introducer, keep the text
			case s[j] == 0x07:
				i = j + 1
			default:
				i = j + 2
			}
		default:
			i++ // a lone ESC: drop it alone (it may start the next sequence)
		}
	}
	flush()
	return sb.String()
}

// isST reports whether the string terminator ESC \ starts at s[j].
func isST(s string, j int) bool { return s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' }

func onlySGRParams(p string) bool {
	for i := 0; i < len(p); i++ {
		if (p[i] < '0' || p[i] > '9') && p[i] != ';' && p[i] != ':' {
			return false
		}
	}
	return true
}
