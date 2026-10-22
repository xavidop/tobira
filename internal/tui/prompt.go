package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/query"
)

// Prompts that stop an action until the user answers: a y/N question
// (overwrite a file, cancel an export, drop an unsaved query) or the
// three-way transaction prompt. The pending-edits confirmation is the
// guard in inline.go; quitting chains them in this order: pending edits,
// the transaction, a running export, the unsaved buffer.

type askKind int

const (
	askYesNo   askKind = iota // y runs yes; anything else keeps things as they are
	askTx                     // c commits, r rolls back (then yes); anything else cancels
	askConsent                // y once, a for the session; anything else is no
)

// askState is the open prompt, drawn in the grid pane.
type askState struct {
	kind   askKind
	title  string // escaped when drawn
	reason string
	yes    tea.Cmd
	no     string // flashed when the answer is not yes; "" flashes nothing
	// An AI turn's question (aiprompt.go): the turn it belongs to, when it
	// opened, the focus to give back, and where its answer goes (buffered
	// channels, so answering never blocks).
	turn    uint64
	opened  time.Time
	prev    focusKind
	confirm chan<- bool
	consent chan<- assistant.Consent
	// An execute confirmation's connection, and where a refusal decided at
	// the answer goes (buffered).
	execConn string
	refuse   chan<- string
	// The execute confirmation's statement box: the first line shown, and
	// whether the user has seen its last line (y waits for it).
	off     int
	seenEnd bool
}

// askYN opens a y/N prompt.
func (a *Model) askYN(title, reason string, yes tea.Cmd, no string) {
	a.yieldAIAsk()
	a.ask = &askState{kind: askYesNo, title: title, reason: reason, yes: yes, no: no}
	a.cell, a.help = nil, false
	a.dropDetail()
}

func (a *Model) askKey(k tea.KeyPressMsg) tea.Cmd {
	q := a.ask
	if q.isAI() {
		return a.aiAskKey(q, k)
	}
	a.ask = nil
	s := k.String()
	if q.kind == askTx {
		switch s {
		case "c", "C":
			if a.tx != nil && a.tx.stuck {
				return a.flash(stuckText, false)
			}
			return a.endTx("commit", q.yes)
		case "r", "R":
			return a.endTx("rollback", q.yes)
		}
		return a.flash("kept the transaction open", false)
	}
	if s == "y" || s == "Y" {
		return q.yes
	}
	if q.no != "" {
		return a.flash(q.no, false)
	}
	return nil
}

func (a *Model) askView(w, h int) string {
	q := a.ask
	st := a.th.Styles
	if q.confirm != nil {
		return a.execConfirmView(q, w, h)
	}
	hint := "y confirms · any other key cancels"
	switch q.kind {
	case askTx:
		hint = "c commits · r rolls back · any other key cancels"
	case askConsent:
		hint = "[y]es once · [a]lways this session · [n]o"
	}
	lines := []string{st.Title.Render(ansi.Wrap(query.EscapeControls(q.title), w, ""))}
	if q.reason != "" {
		lines = append(lines, st.Dim.Render(ansi.Wrap(query.EscapeControls(q.reason), w, "")))
	}
	lines = append(lines, "", st.Dim.Render(ansi.Wrap(hint, w, "")))
	return lipgloss.NewStyle().MaxWidth(w).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// The execute confirmation shows the whole statement: it is the one gate
// between a model-written write and the database, so no part of it may be
// hidden. The statement sits in a box under the question, scrolled with
// the arrows and PgUp/PgDn; when it does not fit, a line under the box
// says how much is left, and y is refused until the last line has been
// shown. The hint line is always drawn.

const (
	execHint    = "y runs it · n or esc refuses"
	execBoxEdge = "│ "
)

// execLayout lays the execute confirmation q out in w×h: the question's
// lines that fit, every line of the statement wrapped to the box, and how
// many statement lines the box shows (at least one). Two rows are kept for
// the line under the box and the hint. When the whole question does not
// fit, its end goes first: the lines up to the one naming the connection
// are kept. whole reports that everything y depends on can be drawn: those
// lines, and every statement line within the box width (a pane narrower
// than the box edge plus a wide rune cannot draw it).
func execLayout(q *askState, w, h int) (head, body []string, box int, whole bool) {
	w = max(w, 1)
	wrap := func(s string, width int) []string { return strings.Split(ansi.Wrap(s, width, ""), "\n") }
	fits := func(lines []string, width int) bool {
		for _, l := range lines {
			if ansi.StringWidth(l) > width {
				return false
			}
		}
		return true
	}
	edge := ansi.StringWidth(execBoxEdge)
	bw := max(w-edge, 1)
	text := strings.ReplaceAll(query.EscapeControlsMultiline(strings.TrimRight(q.reason, "\n")), "\t", "    ")
	for _, line := range strings.Split(text, "\n") {
		body = append(body, wrap(line, bw)...)
	}
	avail := h - 2
	budget := max(avail-1, 0)
	head = wrap(query.EscapeControls(q.title), w)
	key := len(head) // lines that must be drawn: up to the connection's
	if ask := "run this statement on " + q.execConn + "?"; q.execConn != "" && strings.HasPrefix(q.title, ask) {
		keep := wrap(query.EscapeControls(ask), w)
		key = len(keep)
		if len(head) > budget {
			// The question up to the connection whole, then what else fits.
			rest := wrap(query.EscapeControls(strings.TrimSpace(strings.TrimPrefix(q.title, ask))), w)
			head = append(keep, rest...)
		}
	}
	whole = w >= edge+2 && key <= budget && fits(head[:min(key, len(head))], w) && fits(body, bw)
	head = head[:min(len(head), budget)]
	return head, body, max(avail-len(head), 1), whole
}

// execMaxOff is the last first line of q's statement box in w×h; 0 when the
// statement fits.
func execMaxOff(q *askState, w, h int) int {
	_, body, box, _ := execLayout(q, w, h)
	return max(len(body)-box, 0)
}

// execScroll moves q's statement box by d lines in w×h, and records when
// its last line is on screen.
func execScroll(q *askState, d, w, h int) {
	maxOff := execMaxOff(q, w, h)
	q.off = min(max(q.off+d, 0), maxOff)
	if q.off == maxOff {
		q.seenEnd = true
	}
}

// execUnread reports whether y must wait: part of q's statement has not
// been shown yet, or the pane cannot draw it whole (too short for a
// statement line under the question up to the connection, or too narrow
// for its runes). It fails closed.
func execUnread(q *askState, w, h int) bool {
	if _, _, _, whole := execLayout(q, w, h); h < 3 || !whole {
		return true
	}
	if q.seenEnd {
		return false
	}
	if execMaxOff(q, w, h) == 0 {
		q.seenEnd = true
		return false
	}
	return true
}

func (a *Model) execConfirmView(q *askState, w, h int) string {
	st := a.th.Styles
	head, body, box, _ := execLayout(q, w, h)
	maxOff := max(len(body)-box, 0)
	off := min(q.off, maxOff)
	lines := make([]string, 0, h)
	for _, l := range head {
		lines = append(lines, st.Title.Render(l))
	}
	for _, l := range body[off:min(off+box, len(body))] {
		lines = append(lines, st.Dim.Render(execBoxEdge)+l)
	}
	more := ""
	switch rest := len(body) - off - box; {
	case rest == 1:
		more = "… 1 more line — ↓/PgDn to read"
	case rest > 1:
		more = fmt.Sprintf("… %d more lines — ↓/PgDn to read", rest)
	case maxOff > 0:
		more = "end of the statement — ↑/PgUp scrolls back"
	}
	lines = append(lines, st.Dim.Render(ansi.Truncate(more, w, "…")), st.Dim.Render(ansi.Truncate(execHint, w, "…")))
	return lipgloss.NewStyle().MaxWidth(w).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

func (a *Model) askPaneTitle() string {
	switch {
	case a.ask.kind == askTx:
		return "open transaction"
	case a.ask.kind == askConsent:
		return "send rows to the AI"
	case a.ask.confirm != nil:
		return "AI execute"
	}
	return "confirm"
}

// ---- quitting ---------------------------------------------------------------

// quitMsg is a quit on its way through the prompt chain: the flags record
// the questions already answered yes.
type quitMsg struct{ exportOK, bufferOK bool }

// quit quits, after asking about everything that would be lost.
func (a *Model) quit() tea.Cmd { return a.quitWith(quitMsg{}) }

func (a *Model) quitWith(q quitMsg) tea.Cmd {
	if a.apply != nil && a.apply.cancelled && a.stuck() {
		return a.quitStuck()
	}
	if a.txEnding() {
		if a.txStuck() {
			return a.quitStuckTx()
		}
		return a.flash(a.waitTxText(), false)
	}
	if cmd, held := a.guard("quitting drops them", resend(q)); held {
		return cmd
	}
	if cmd, held := a.txGuard("quitting ends it", resend(q), true); held {
		return cmd
	}
	if a.export != nil && !q.exportOK {
		next := q
		next.exportOK = true
		a.askYN("an export is running; quit and cancel it? y/N", "the partial file is removed", resend(next), "")
		return nil
	}
	if !q.bufferOK {
		if conn, ok := a.unsavedBuffer(); ok {
			next := q
			next.bufferOK = true
			a.askYN("discard the unsaved query? y/N", "the editor buffer of "+conn+" was changed since it last ran",
				resend(next), "")
			return nil
		}
	}
	return a.leave()
}

// unsavedBuffer reports a connection whose editor buffer holds text that
// was not run as it is: non-empty and different from the buffer at its last
// run (the empty buffer when nothing ran).
func (a *Model) unsavedBuffer() (string, bool) {
	changed := func(conn, text string) bool {
		return strings.TrimSpace(text) != "" && text != a.runBuf[conn]
	}
	if a.edConn != "" && changed(a.edConn, a.ed.Value()) {
		return a.edConn, true
	}
	for _, conn := range slices.Sorted(maps.Keys(a.buffers)) {
		if conn != a.edConn && changed(conn, a.buffers[conn].text) {
			return conn, true
		}
	}
	return "", false
}

// leave quits now: it cancels what is in flight and records an editor run
// it cancelled in the history (bounded by recordTimeout) before the program
// exits, so the history keeps every run.
func (a *Model) leave() tea.Cmd {
	record, _ := a.cancelRun()
	a.shutdown()
	if record == nil {
		return tea.Quit
	}
	return func() tea.Msg {
		record() // bounded; its outcome is only logged
		return tea.Quit()
	}
}

// recordRunAtExit records an editor run still in flight when the program
// stopped without going through leave (its context was cancelled). It runs
// after the program has exited, never on the Update goroutine.
func (a *Model) recordRunAtExit() {
	if record, ok := a.cancelRun(); ok && record != nil {
		record()
	}
}
