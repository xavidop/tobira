// Package statusbar renders the one-line bar at the bottom of the screen:
// breadcrumb, row position and load time, read-only badge, connection status
// dot and key hints.
package statusbar

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// flashFor is how long a flash message replaces the middle section; an error
// flash stays longer (flashErrFor) unless a key press dismisses it first.
const (
	flashFor    = 3 * time.Second
	flashErrFor = 10 * time.Second
)

// Options configures the bar.
type Options struct {
	// Plain swaps the emoji lock and the dot glyph for ASCII (RO and *).
	Plain bool
}

// FlashExpiredMsg is delivered flashFor after a Flash; only the newest
// flash's message clears it.
type FlashExpiredMsg struct{ ID int }

// Pane is the pane that has the focus. The key hints follow it, so the bar
// always shows the keys that act where the user is.
type Pane int

// The panes; the zero value is the grid.
const (
	PaneGrid Pane = iota
	PaneSidebar
	PaneEditor
	PaneAI
)

// Model is the status bar.
type Model struct {
	th   theme.Theme
	km   keymap.Map
	opts Options

	conn, ns, coll string

	pos, loaded int
	estimate    int64
	took        time.Duration

	readOnly bool
	partial  bool
	status   session.Status
	pane     Pane // the focused pane, which picks the key hints
	ai       bool // an assistant is configured, so the ask hint is worth showing
	pending  int  // inline edits not yet applied
	tx       bool // a transaction (:tx) is open
	txStuck  bool // ... and only a rollback may reach it (TX!)
	activity string

	flash    string
	flashErr bool
	flashID  int
	flashing bool
}

// New returns an empty bar.
func New(th theme.Theme, km keymap.Map, opts Options) Model {
	return Model{th: th, km: km, opts: opts}
}

// SetContext sets the breadcrumb.
func (m Model) SetContext(conn, ns, coll string) Model {
	m.conn, m.ns, m.coll = conn, ns, coll
	return m
}

// SetStats sets the row position (1-based; 0 when none), the number of rows
// loaded, the driver's row estimate (0 when unknown) and the last load time.
func (m Model) SetStats(pos, loaded int, estimate int64, took time.Duration) Model {
	m.pos, m.loaded, m.estimate, m.took = pos, loaded, estimate, took
	return m
}

// SetPartial says the total is not exact: an estimate is shown as ~N and,
// without one, the loaded count as N+. Without it the numbers are exact.
func (m Model) SetPartial(partial bool) Model { m.partial = partial; return m }

// SetReadOnly shows or hides the read-only badge.
func (m Model) SetReadOnly(ro bool) Model { m.readOnly = ro; return m }

// SetPending shows "N pending" next to the badge; 0 hides it.
func (m Model) SetPending(n int) Model { m.pending = n; return m }

// SetTx shows or hides the TX badge of an open transaction.
func (m Model) SetTx(open bool) Model { m.tx = open; return m }

// SetTxStuck draws the TX badge as TX! in the danger colour.
func (m Model) SetTxStuck(stuck bool) Model { m.txStuck = stuck; return m }

// SetActivity shows text, such as an export's progress, in place of the
// stats until it is cleared with "". A flash still shows over it.
func (m Model) SetActivity(text string) Model { m.activity = text; return m }

// SetPane switches the key hints to the focused pane's.
func (m Model) SetPane(p Pane) Model { m.pane = p; return m }

// SetAI says whether an assistant is configured; the grid's ask hint is
// shown only then.
func (m Model) SetAI(configured bool) Model { m.ai = configured; return m }

// SetStatus sets the connection status dot.
func (m Model) SetStatus(s session.Status) Model { m.status = s; return m }

// SetTheme swaps the theme.
func (m Model) SetTheme(th theme.Theme) Model { m.th = th; return m }

// SetPlain switches the ASCII-only glyphs on or off.
func (m Model) SetPlain(plain bool) Model { m.opts.Plain = plain; return m }

// SetKeymap swaps the keymap used for hints.
func (m Model) SetKeymap(km keymap.Map) Model { m.km = km; return m }

// Flash replaces the middle section with text for three seconds, or ten for
// an error (see DismissError). The returned command delivers the
// FlashExpiredMsg.
func (m Model) Flash(text string, isErr bool) (Model, tea.Cmd) {
	m.flashID++
	m.flash, m.flashErr, m.flashing = text, isErr, true
	id := m.flashID
	d := flashFor
	if isErr {
		d = flashErrFor
	}
	return m, tea.Tick(d, func(time.Time) tea.Msg { return FlashExpiredMsg{ID: id} })
}

// DismissError clears an error flash; the parent calls it on a key press. A
// plain flash is left to expire on its own.
func (m Model) DismissError() Model {
	if m.flashing && m.flashErr {
		m.flashing, m.flash, m.flashErr = false, "", false
	}
	return m
}

// Update clears the flash when its own expiry arrives.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if e, ok := msg.(FlashExpiredMsg); ok && e.ID == m.flashID {
		m.flashing, m.flash = false, ""
	}
	return m, nil
}

func (m Model) breadcrumb() string {
	var parts []string
	for _, p := range []string{m.conn, m.ns, m.coll} {
		if p != "" {
			parts = append(parts, query.EscapeControls(p))
		}
	}
	return strings.Join(parts, " › ")
}

func (m Model) middle() (string, bool) {
	if m.flashing {
		return query.EscapeControls(m.flash), m.flashErr
	}
	if m.activity != "" {
		return query.EscapeControls(m.activity), false
	}
	var parts []string
	total := ""
	switch {
	case m.estimate > 0 && m.partial:
		total = "~" + thousands(m.estimate)
	case m.estimate > 0:
		total = thousands(m.estimate)
	case m.loaded > 0 && m.partial:
		total = thousands(int64(m.loaded)) + "+"
	case m.loaded > 0:
		total = thousands(int64(m.loaded))
	}
	switch {
	case m.pos > 0 && total != "":
		parts = append(parts, fmt.Sprintf("row %s of %s", thousands(int64(m.pos)), total))
	case total != "":
		parts = append(parts, total+" rows")
	}
	if m.took > 0 {
		parts = append(parts, duration(m.took))
	}
	return strings.Join(parts, "  "), false
}

func duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func thousands(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var sb strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			sb.WriteByte(',')
		}
		sb.WriteRune(r)
	}
	if neg {
		return "-" + sb.String()
	}
	return sb.String()
}

// hint is one key hint: the key of an action and a short name for what it
// does here.
type hint struct {
	a    keymap.Action
	name string
	// prefer is the key shown when the action has it: f5 runs in every
	// terminal, ctrl+enter only in those with the kitty keyboard protocol.
	prefer string
}

// The hints of each pane, most important first: when the bar is narrow the
// last ones drop out, so the keys a newcomer needs most stay visible.
var (
	sidebarHints = []hint{
		{keymap.Help, "help", ""}, {keymap.Open, "open", ""}, {keymap.SidebarFilter, "filter", ""},
		{keymap.ConnAdd, "add", ""}, {keymap.Palette, "cmd", ""}, {keymap.Quit, "quit", ""},
	}
	gridHints = []hint{
		{keymap.Help, "help", ""}, {keymap.Open, "detail", ""}, {keymap.Back, "sidebar", ""},
		{keymap.Filter, "filter", ""}, {keymap.Sort, "sort", ""}, {keymap.Edit, "edit", ""}, {keymap.EditorToggle, "query", ""},
		{keymap.Palette, "cmd", ""}, {keymap.Quit, "quit", ""}, {keymap.CopyCell, "copy", ""},
		{keymap.Ask, "ask", ""},
	}
	editorHints = []hint{
		{keymap.RunStatement, "run", "f5"}, {keymap.RunBuffer, "buffer", "shift+f5"},
		{keymap.Complete, "complete", ""}, {keymap.HistSearch, "history", ""},
		{keymap.Back, "grid", ""}, {keymap.EditorToggle, "close", ""},
	}
	aiHints = []hint{
		{keymap.Open, "send", ""}, {keymap.LoadProposal, "load", ""}, {keymap.CopyProposal, "copy", ""},
		{keymap.Back, "grid", ""},
	}
)

// shownHint is a hint resolved against the keymap: the key and the name,
// rendered apart so the key can stand out.
type shownHint struct{ key, name string }

func (h shownHint) plain() string { return h.key + " " + h.name }

// hints returns the focused pane's key hints, most important first.
func (m Model) hints() []shownHint {
	list := gridHints
	switch m.pane {
	case PaneSidebar:
		list = sidebarHints
	case PaneEditor:
		list = editorHints
	case PaneAI:
		list = aiHints
	}
	var out []shownHint
	for _, h := range list {
		if h.a == keymap.Ask && !m.ai {
			continue
		}
		keys := m.km.Binding(h.a).Keys()
		if len(keys) == 0 {
			continue
		}
		k := keys[0]
		for _, c := range keys {
			if c == h.prefer {
				k = c
			}
		}
		out = append(out, shownHint{k, h.name})
	}
	return out
}

// joinHints renders the first n hints, keys in the accent colour.
func (m Model) joinHints(hs []shownHint, n int) string {
	st := m.th.Styles.StatusBar
	var sb strings.Builder
	for i, h := range hs[:n] {
		if i > 0 {
			sb.WriteString(st.Render("  "))
		}
		sb.WriteString(st.Foreground(m.th.Accent).Render(h.key) + st.Render(" "+h.name))
	}
	return sb.String()
}

// plainHints is the first n hints as plain text, for measuring.
func plainHints(hs []shownHint, n int) string {
	parts := make([]string, n)
	for i, h := range hs[:n] {
		parts[i] = h.plain()
	}
	return strings.Join(parts, "  ")
}

func (m Model) dotColor() lipgloss.Style {
	st := m.th.Styles.StatusBar
	switch m.status {
	case session.Ready:
		return st.Foreground(m.th.OK)
	case session.Connecting:
		return st.Foreground(m.th.Warn)
	case session.Failed:
		return st.Foreground(m.th.Danger)
	}
	return st.Foreground(m.th.Muted)
}

// truncLeft keeps the tail of s within w columns, marking the cut with "…".
func truncLeft(s string, w int) string {
	if w < 1 {
		return ""
	}
	if query.DisplayWidth(s) <= w {
		return s
	}
	type cluster struct {
		text string
		w    int
	}
	var cs []cluster
	for rest := s; rest != ""; {
		c, cw := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if c == "" {
			break
		}
		cs = append(cs, cluster{c, cw})
		rest = rest[len(c):]
	}
	used := 1
	i := len(cs)
	for i > 0 {
		if used+cs[i-1].w > w {
			break
		}
		used += cs[i-1].w
		i--
	}
	var sb strings.Builder
	sb.WriteString("…")
	for _, c := range cs[i:] {
		sb.WriteString(c.text)
	}
	return sb.String()
}

// View renders the bar on one line, never wider than width.
func (m Model) View(width int) string {
	if width < 1 {
		return ""
	}
	st := m.th.Styles.StatusBar
	w := query.DisplayWidth

	left := m.breadcrumb()
	mid, midErr := m.middle()
	var badge string
	if m.readOnly {
		badge = "🔒"
		if m.opts.Plain {
			badge = "RO"
		}
	}
	dot := "●"
	if m.opts.Plain {
		dot = "*"
	}
	core := dot
	if badge != "" {
		core = badge + " " + dot
	}
	txText := "TX"
	if m.txStuck {
		txText = "TX!"
	}
	if m.tx {
		core = txText + " " + core
	}
	pending := ""
	if m.pending > 0 {
		pending = fmt.Sprintf("%d pending", m.pending)
		core = pending + "  " + core
	}
	hints := m.hints()

	// A plain flash or the stats take up to half the bar. An error may use
	// everything but the badge area and a little room for the breadcrumb
	// (hints drop out first, below).
	const minLeft = 8
	midMax := max(width/2, 10)
	if midErr {
		midMax = max(width-w(core)-1-2-min(w(left), minLeft), 10)
	}
	mid = query.Truncate(mid, midMax)

	// Choose the richest layout that leaves the breadcrumb some room. The
	// first baseHints hints (help and what enter does here) are kept even if
	// the breadcrumb must be cut to minLeft; the rest only join while the
	// whole breadcrumb still fits.
	const baseHints = 2
	var rightHints string
	shown := 0
	showMid := mid != ""
	chosen := false
	for _, withMid := range []bool{showMid, false} {
		for n := len(hints); n >= 0; n-- {
			h := plainHints(hints, n)
			right := core
			if h != "" {
				right = h + "  " + core
			}
			used := w(right) + 1
			if withMid {
				used += w(mid) + 2
			}
			need := w(left)
			if n <= baseHints {
				need = min(w(left), minLeft)
			}
			if width-used >= need {
				rightHints, shown, showMid, chosen = h, n, withMid, true
				break
			}
		}
		if chosen {
			break
		}
	}
	if !chosen {
		rightHints, shown, showMid = "", 0, false
	}
	right := core
	if rightHints != "" {
		right = rightHints + "  " + core
	}
	if w(right) >= width {
		// Too narrow for anything but the dot.
		return m.dotColor().Render(dot) + st.Render(strings.Repeat(" ", width-w(dot)))
	}

	avail := width - w(right) - 1
	if showMid {
		avail -= w(mid) + 2
	}
	left = truncLeft(left, avail)

	var sb strings.Builder
	used := 0
	sb.WriteString(st.Render(left))
	used += w(left)
	if showMid {
		gap := 2
		if used == 0 {
			gap = 0
		}
		sb.WriteString(st.Render(strings.Repeat(" ", gap)))
		ms := st
		if midErr {
			ms = m.th.Styles.StatusBar.Foreground(m.th.Danger)
		}
		sb.WriteString(ms.Render(mid))
		used += gap + w(mid)
	}
	pad := max(width-used-w(right), 1)
	sb.WriteString(st.Render(strings.Repeat(" ", pad)))
	if shown > 0 {
		sb.WriteString(m.joinHints(hints, shown) + st.Render("  "))
	}
	if pending != "" {
		sb.WriteString(st.Foreground(m.th.Warn).Render(pending) + st.Render("  "))
	}
	if m.tx {
		c := m.th.Accent
		if m.txStuck {
			c = m.th.Danger
		}
		sb.WriteString(st.Foreground(c).Render(txText) + st.Render(" "))
	}
	if badge != "" {
		sb.WriteString(st.Render(badge + " "))
	}
	sb.WriteString(m.dotColor().Render(dot))
	return sb.String()
}
