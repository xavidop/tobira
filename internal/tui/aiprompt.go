package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

// AI prompts (row consent, execute confirmation) open when the assistant
// asks, in the middle of a turn, while the user may be typing. So they never
// take type-ahead as an answer: keys are ignored for aiAskGrace after one
// opens, only the answer keys count, and a prompt never replaces the cell
// editor, a form, a confirmation or another prompt: it waits in
// a.ai.queued until they close. Its callback keeps waiting meanwhile; a
// cancelled turn drops it.

// aiAskGrace is how long an AI prompt ignores keys after it opens.
const aiAskGrace = 400 * time.Millisecond

// isAI reports whether q is an AI turn's question.
func (q *askState) isAI() bool { return q != nil && (q.consent != nil || q.confirm != nil) }

// askConfirm offers the execute confirmation of AI turn id for stmt on
// conn; refuse takes a refusal decided when the user answers.
func (a *Model) askConfirm(id uint64, conn, title, stmt string, reply chan<- bool, refuse chan<- string) {
	a.offerAIAsk(&askState{kind: askYesNo, title: title, reason: stmt, turn: id, confirm: reply, execConn: conn, refuse: refuse})
}

// askConsent offers the row-consent prompt of AI turn id; reason says what
// produced the rows.
func (a *Model) askConsent(id uint64, title, reason string, reply chan<- assistant.Consent) {
	a.offerAIAsk(&askState{kind: askConsent, title: title, reason: reason, turn: id, consent: reply})
}

// aiAskBlocked reports whether something the user is busy with must not be
// covered by an AI prompt.
func (a *Model) aiAskBlocked() bool {
	return a.aiAskCovered() || a.ask != nil
}

// aiAskCovered reports an overlay or a focused text input that an open AI
// prompt must yield to: what is typed there must never answer it.
func (a *Model) aiAskCovered() bool {
	return a.cell != nil || a.form != nil || a.confirm != nil || a.guardSt != nil ||
		a.pal.Focused() || a.fl.Focused() || a.sb.FilterFocused()
}

// offerAIAsk opens q now, or queues it until nothing blocks it.
func (a *Model) offerAIAsk(q *askState) {
	if a.aiAskBlocked() {
		a.ai.queued = q
		return
	}
	a.ai.queued = nil
	q.opened, q.prev = a.clock(), a.focus
	a.ask = q
	a.help = false
	a.focus = focusGrid // the prompt is drawn in the grid pane
	if a.ai.ready {
		a.ai.in.Blur()
	}
}

// yieldAIAsk puts an open AI prompt back in the queue before another prompt
// takes its place; it opens again once that one closes.
func (a *Model) yieldAIAsk() {
	if q := a.ask; q.isAI() {
		a.ask = nil
		a.focus = q.prev
		a.ai.queued = q
	}
}

// syncAIAsk runs after every Update: an AI prompt yields to the cell editor,
// a form or a confirmation that opened over it, and a queued one opens once
// nothing blocks it.
func (a *Model) syncAIAsk() {
	if a.ask.isAI() && a.aiAskCovered() {
		a.yieldAIAsk()
	}
	if q := a.ai.queued; q != nil && !a.aiAskBlocked() {
		a.offerAIAsk(q)
	}
}

// aiAskKey answers an AI prompt: y, a (consent only), n or esc; ctrl+c is a
// no at any time. On the execute confirmation the arrows and PgUp/PgDn
// scroll the statement at any time. Other keys, and every key in the grace
// period, are ignored.
func (a *Model) aiAskKey(q *askState, k tea.KeyPressMsg) tea.Cmd {
	if q.confirm != nil {
		// Scrolling the statement answers nothing, so it needs no grace.
		g := a.lay.grid
		_, _, page, _ := execLayout(q, g.w, g.h)
		switch {
		case a.km.Matches(k, keymap.Down):
			execScroll(q, 1, g.w, g.h)
			return nil
		case a.km.Matches(k, keymap.Up):
			execScroll(q, -1, g.w, g.h)
			return nil
		case a.km.Matches(k, keymap.PageDown):
			execScroll(q, page, g.w, g.h)
			return nil
		case a.km.Matches(k, keymap.PageUp):
			execScroll(q, -page, g.w, g.h)
			return nil
		}
	}
	no := a.km.Matches(k, keymap.Cancel)
	if !no && a.clock().Sub(q.opened) < aiAskGrace {
		return nil
	}
	s := k.String()
	no = no || s == "n" || a.km.Matches(k, keymap.Back)
	switch {
	case q.consent != nil && s == "y":
		q.consent <- assistant.ConsentOnce
	case q.consent != nil && s == "a":
		q.consent <- assistant.ConsentSession
	case q.consent != nil && no:
		q.consent <- assistant.ConsentNo
	case q.confirm != nil && s == "y" && execUnread(q, a.lay.grid.w, a.lay.grid.h):
		return a.flash("read the whole statement first: ↓/PgDn to the end", false)
	case q.confirm != nil && s == "y":
		// A transaction may have begun while the prompt was queued behind
		// the palette.
		if why := a.txBlocksExecute(q.execConn); why != "" {
			q.refuse <- why
			a.closeAIAsk(q)
			return a.flash(why, false)
		}
		q.confirm <- true
	case q.confirm != nil && no:
		q.confirm <- false
	default:
		return nil
	}
	a.closeAIAsk(q)
	return nil
}

// closeAIAsk closes the open AI prompt q and gives the focus back.
func (a *Model) closeAIAsk(q *askState) {
	a.ask = nil
	a.focus = q.prev
	if a.focus == focusAI && a.ai.open {
		a.ai.in.Focus()
	}
}
