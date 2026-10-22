package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const (
	// sidebarW is the sidebar's content width when it sits beside the grid.
	sidebarW = 28
	// narrowBelow is the width under which only one pane is shown at a time.
	narrowBelow = 60
	// shortBelow is the height under which a focused editor takes the whole
	// main pane.
	shortBelow = 16
	// minEditorH is the fewest editor text lines drawn when there is room.
	minEditorH = 5
	// aiSideMin is the width from which the AI panel sits beside the panes
	// (leaving them at least narrowBelow); below it the panel is a
	// full-pane overlay.
	aiSideMin = 120
	// aiMinW is the side panel's least width; it takes about 40% otherwise.
	aiMinW = 40
)

// aiPlace is where the AI panel goes.
type aiPlace int

const (
	aiNone    aiPlace = iota
	aiSide            // a column on the right of the panes
	aiOverlay         // over the whole body
)

// aiPanelW is the side panel's width on a w-column screen.
func aiPanelW(w int) int { return max(aiMinW, w*2/5) }

type focusKind int

const (
	focusSidebar focusKind = iota
	focusGrid
	focusEditor
	focusAI
)

// rect is a region of the screen in absolute cells.
type rect struct{ x, y, w, h int }

// layout is where everything goes for one window size and UI state. The model
// computes it once per Update (relayout) and both resizes the components and
// draws from it, so the sizes handed to Resize and to View cannot diverge.
//
// From the top: an optional title row, the panes, the palette or filter
// lines, and the status bar. At narrow widths only the focused pane is drawn,
// full width. The main (grid) column stacks, from the top, the result tab
// bar, the grid of the active tab, and the query editor: a title line over
// its text, about 40% of the column.
type layout struct {
	w, h int

	bodyH  int  // lines above the input and the bar
	titles bool // a title row heads each pane
	// sidebar and grid are the content rectangles, below the title row. At
	// narrow widths both are the full-width rectangle and showSidebar /
	// showGrid say which one is drawn.
	sidebar, grid rect
	showSidebar   bool
	showGrid      bool
	borderX       int // x of the divider column; -1 when none

	// col is the whole main column below its title row; tabs, grid, edTitle
	// and editor split it from the top. A part not drawn has h 0.
	col, tabs, edTitle, editor rect
	// edFooter is the editor's last line while it shows the history search
	// prompt; the text gets one line less.
	edFooter rect
	// chart is the chart pane (:chart) under the grid, its title line
	// included; h 0 when closed or when there is no room.
	chart rect

	// ai is the AI panel's rectangle (w 0 when closed): beside the panes,
	// or over the whole body when aiOverlay.
	ai        rect
	aiOverlay bool

	inputY, inputH int
	barY           int
}

// colOpts says what the main column holds besides the grid.
type colOpts struct {
	tabs   bool // a result tab bar heads the grid
	editor bool // the query editor is open
	footer bool // the editor shows a footer line (the history search)
	ai     aiPlace
	chart  bool // the chart pane is open
}

// computeLayout lays out a w x h screen with inputH lines of palette or
// filter. overlay is whether a detail, help or connection view replaces the
// grid; it hides the tab bar and the editor too.
func computeLayout(w, h, inputH int, focus focusKind, overlay bool, o colOpts) layout {
	paneW := w
	if o.ai == aiSide {
		paneW = w - aiPanelW(w) - 1 // the panel and its divider
	}
	l := computePanes(paneW, h, inputH, focus, overlay)
	l.w = w
	if l.w >= 1 && l.h >= 1 {
		switch o.ai {
		case aiSide:
			l.ai = rect{paneW + 1, 0, w - paneW - 1, l.bodyH}
		case aiOverlay:
			l.ai = rect{0, 0, w, l.bodyH}
			l.aiOverlay = true
		}
	}
	l.col = l.grid
	if overlay || l.w < 1 || l.h < 1 {
		return l
	}
	col := l.grid
	if o.editor {
		edH := max(minEditorH+1, col.h*2/5) // the title line and the text
		if h < shortBelow && focus == focusEditor {
			edH = col.h
		}
		edH = min(edH, col.h)
		if edH > 0 {
			l.edTitle = rect{col.x, col.y + col.h - edH, col.w, 1}
			l.editor = rect{col.x, l.edTitle.y + 1, col.w, edH - 1}
			if o.footer && l.editor.h >= 2 {
				l.editor.h--
				l.edFooter = rect{col.x, l.editor.y + l.editor.h, col.w, 1}
			}
		}
		col.h -= edH
	}
	if ch := min(chartH, col.h/2); o.chart && ch >= 3 {
		l.chart = rect{col.x, col.y + col.h - ch, col.w, ch}
		col.h -= ch
	}
	if o.tabs && col.h >= 2 {
		l.tabs = rect{col.x, col.y, col.w, 1}
		col.y++
		col.h--
	}
	l.grid = col
	return l
}

func computePanes(w, h, inputH int, focus focusKind, overlay bool) layout {
	l := layout{w: w, h: h, borderX: -1}
	if w < 1 || h < 1 {
		return l
	}
	l.barY = h - 1
	l.inputH = max(0, min(inputH, h-1))
	l.inputY = l.barY - l.inputH
	l.bodyH = l.inputY
	l.titles = l.bodyH >= 3
	top := 0
	if l.titles {
		top = 1
	}
	ch := l.bodyH - top
	if w >= narrowBelow {
		l.sidebar = rect{0, top, sidebarW, ch}
		l.borderX = sidebarW
		l.grid = rect{sidebarW + 1, top, w - sidebarW - 1, ch}
		l.showSidebar, l.showGrid = true, true
		return l
	}
	full := rect{0, top, w, ch}
	l.sidebar, l.grid = full, full
	if focus == focusSidebar && !overlay {
		l.showSidebar = true
	} else {
		l.showGrid = true
	}
	return l
}

// fit returns s as exactly h lines of exactly w columns: extra lines and
// columns are cut, missing ones padded with spaces.
func fit(s string, w, h int) []string {
	if h < 1 {
		return nil
	}
	var in []string
	if s != "" {
		in = strings.Split(s, "\n")
	}
	out := make([]string, h)
	for i := range out {
		var line string
		if i < len(in) {
			line = in[i]
		}
		sw := ansi.StringWidth(line)
		if sw > w {
			line = ansi.Truncate(line, w, "")
			sw = ansi.StringWidth(line)
		}
		if sw < w {
			line += strings.Repeat(" ", w-sw)
		}
		out[i] = line
	}
	return out
}
