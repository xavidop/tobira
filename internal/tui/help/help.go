// Package help renders the help overlay: the key bindings of the active
// keymap, then the palette commands.
package help

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/palette"
	"github.com/xavidop/tobira/internal/tui/theme"
)

const (
	colGap     = 3
	headerRows = 2 // title and a blank line
)

// block is one rendered group: a title line followed by "key  desc" lines.
type block struct {
	title string
	lines []string
	width int
}

func blocks(km keymap.Map) []block {
	titles := keymap.GroupTitles()
	var out []block
	for i, grp := range km.Help() {
		b := block{title: titles[i], width: query.DisplayWidth(titles[i])}
		keyW := 0
		var keys, descs []string
		for _, bind := range grp {
			h := bind.Help()
			if h.Key == "" && h.Desc == "" {
				continue
			}
			k := query.EscapeControls(h.Key)
			keys = append(keys, k)
			descs = append(descs, query.EscapeControls(h.Desc))
			keyW = max(keyW, query.DisplayWidth(k))
		}
		for j := range keys {
			l := keys[j] + strings.Repeat(" ", keyW-query.DisplayWidth(keys[j])+2) + descs[j]
			b.lines = append(b.lines, l)
			b.width = max(b.width, query.DisplayWidth(l))
		}
		out = append(out, b)
	}
	return append(out, commandBlock())
}

// commandBlock lists the palette commands, usage then description.
func commandBlock() block {
	b := block{title: "Commands", width: query.DisplayWidth("Commands")}
	for _, c := range palette.Commands() {
		l := query.EscapeControls(c.Usage) + "  " + query.EscapeControls(c.Desc)
		b.lines = append(b.lines, l)
		b.width = max(b.width, query.DisplayWidth(l))
	}
	return b
}

// layout places the blocks into as many columns as fit width and returns
// the body lines, with group titles styled.
func layout(km keymap.Map, th theme.Theme, width int) (body []string) {
	bs := blocks(km)
	colW := 0
	for _, b := range bs {
		colW = max(colW, b.width)
	}
	ncols := max(min(len(bs), (width+colGap)/(colW+colGap)), 1)
	cols := make([][]string, ncols)
	titles := make([][]bool, ncols)
	for i, b := range bs {
		c := i % ncols
		if len(cols[c]) > 0 {
			cols[c] = append(cols[c], "")
			titles[c] = append(titles[c], false)
		}
		cols[c] = append(cols[c], b.title)
		titles[c] = append(titles[c], true)
		for _, l := range b.lines {
			cols[c] = append(cols[c], l)
			titles[c] = append(titles[c], false)
		}
	}
	rows := 0
	for _, c := range cols {
		rows = max(rows, len(c))
	}
	for r := range rows {
		var sb strings.Builder
		for c := range cols {
			cell := ""
			if r < len(cols[c]) {
				cell = cols[c][r]
			}
			if c > 0 {
				sb.WriteString(strings.Repeat(" ", colGap))
			}
			pad := ""
			if c < ncols-1 {
				pad = strings.Repeat(" ", colW-query.DisplayWidth(cell))
			}
			if r < len(titles[c]) && titles[c][r] {
				cell = th.Styles.Title.Render(cell)
			}
			sb.WriteString(cell + pad)
		}
		body = append(body, sb.String())
	}
	return body
}

// MaxOffset is the largest useful scroll offset for the overlay at this size.
func MaxOffset(km keymap.Map, width, height int) int {
	body := layout(km, theme.Dark(), width)
	return max(len(body)-max(height-headerRows, 1), 0)
}

// View renders the overlay scrolled to the top.
func View(km keymap.Map, th theme.Theme, width, height int) string {
	return ViewAt(km, th, width, height, 0)
}

// ViewAt renders the overlay with the body scrolled by offset lines. The
// title stays pinned. The result is at most height lines of at most width
// columns.
func ViewAt(km keymap.Map, th theme.Theme, width, height, offset int) string {
	if width < 1 || height < 1 {
		return ""
	}
	title := query.Truncate("Keys — "+query.EscapeControls(km.Name), width)
	out := []string{th.Styles.Title.Render(title)}
	if height > 1 {
		out = append(out, "")
	}
	room := height - len(out)
	if room > 0 {
		body := layout(km, th, width)
		offset = min(max(offset, 0), max(len(body)-room, 0))
		end := min(offset+room, len(body))
		for _, l := range body[offset:end] {
			out = append(out, ansi.Truncate(l, width, ""))
		}
	}
	return strings.Join(out, "\n")
}
