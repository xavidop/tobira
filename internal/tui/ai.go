package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// The AI panel: a conversation with the assistant about the current
// connection. Each turn is a cancellable command; the assistant's callbacks
// (tool calls, row consent, execute confirmation) reach the Update loop as
// messages over a channel, in order, and a callback that needs an answer
// blocks on its own goroutine until the user gives one or the turn is
// cancelled. Nothing the model proposes runs without a keypress, and a
// proposal runs through the editor's own path (read-only, Tx, history).

// notConfiguredText is shown when there is no ai: section.
const notConfiguredText = "AI is not configured — add an ai: section (see docs/ai)"

type aiMode int

const (
	aiAsk    aiMode = iota // propose a query
	aiAnswer               // answer with the read-only tools
)

func (m aiMode) String() string {
	if m == aiAnswer {
		return "answer"
	}
	return "ask"
}

type aiEntryKind int

const (
	entryUser     aiEntryKind = iota
	entryModel                // markdown from the model
	entryTool                 // a tool call of an answer turn
	entryProposal             // a proposed query
	entryFooter               // model, tokens and latency of a turn
	entryError
	entryNote
)

// aiEntry is one block of a transcript. Text is raw (model text is
// untrusted): it is escaped when drawn.
type aiEntry struct {
	kind aiEntryKind
	text string
	// A tool call (entryTool) and the connection it ran on.
	tc   assistant.ToolCall
	conn string
	// The markdown rendered for width w with theme th, cached.
	md      []string
	mdW     int
	mdTheme string
}

// aiConv is one connection's conversation in this session, kept in memory
// only.
type aiConv struct {
	entries []aiEntry
	// history is what the next turn sends: user questions and model final
	// text, never tool results or rows.
	history []assistant.Turn
	// sess keeps row consent across the conversation's turns; nil until the
	// first turn, and dropped by a reload so consent never survives one.
	sess assistant.Session
	gen  uint64 // the connection's config generation the conversation began on
	exec bool   // :ai execute on
	// proposal is the most recent proposed query; "" for none.
	proposal string
}

func (c *aiConv) add(kind aiEntryKind, text string) {
	c.entries = append(c.entries, aiEntry{kind: kind, text: text})
}

// aiTurn is the turn in flight.
type aiTurn struct {
	id   uint64
	conn string
	// conns is every connection the turn has touched: its scope's, and any
	// a tool call, a row prompt or an execute confirmation named. The
	// assistant binds tools to the scope, so it is conn alone in practice.
	conns  map[string]bool
	cancel context.CancelFunc
	done   chan struct{} // closed when the assistant call has returned
}

// aiState is the panel.
type aiState struct {
	open   bool
	ready  bool // in is set up
	mode   aiMode
	in     textinput.Model
	scroll int // transcript lines scrolled up from the bottom
	conv   map[string]*aiConv
	turn   *aiTurn
	seq    uint64
	queued *askState
	body   []string // the transcript as drawn, from aiPrepare
}

// aiEvent travels from a turn's goroutine to the Update loop.
type aiEvent interface{ turnID() uint64 }

type (
	aiToolEvent struct {
		id uint64
		tc assistant.ToolCall
	}
	aiConsentEvent struct {
		id    uint64
		req   assistant.ConsentRequest
		reply chan<- assistant.Consent // buffered: a send never blocks
	}
	aiConfirmEvent struct {
		id     uint64
		conn   string
		stmt   string
		reply  chan<- bool   // buffered
		refuse chan<- string // buffered: a refusal without asking, and why
	}
	aiDoneEvent struct {
		id  uint64
		res aiResult
		err error
	}
)

func (e aiToolEvent) turnID() uint64    { return e.id }
func (e aiConsentEvent) turnID() uint64 { return e.id }
func (e aiConfirmEvent) turnID() uint64 { return e.id }
func (e aiDoneEvent) turnID() uint64    { return e.id }

// aiEventMsg delivers one event and carries the channel to listen on next.
type aiEventMsg struct {
	ev aiEvent
	ch <-chan aiEvent
}

// aiResult is what a turn produced.
type aiResult struct {
	// question is the user turn recorded in the history.
	question string
	markdown string
	query    string // a proposal ("" for none)
	usage    assistant.Usage
}

// listenAI waits for the next event of a turn. A closed channel (the turn
// is over) ends the chain.
func listenAI(ch <-chan aiEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return aiEventMsg{ev: ev, ch: ch}
	}
}

// newAIInput is the panel's single-line input.
func newAIInput() textinput.Model {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = "ask about this connection…"
	st := in.Styles()
	st.Cursor.Blink = false // a steady cursor: blinking keeps a timer alive
	in.SetStyles(st)
	return in
}

// ---- opening, focus and keys -------------------------------------------------

// aiVisible reports whether the panel is drawn: beside the panes, or, below
// the side-panel width, as an overlay while it has the focus.
func (a *Model) aiVisible() bool {
	if !a.ai.open {
		return false
	}
	if a.w >= aiSideMin {
		return true
	}
	return a.focus == focusAI && a.ask == nil
}

// openAI opens the panel and gives it the focus.
func (a *Model) openAI() {
	if !a.ai.ready {
		a.ai.in, a.ai.ready = newAIInput(), true
	}
	a.ai.open = true
	a.focus = focusAI
	a.ai.in.Focus()
	a.help = false
	a.dropDetail()
}

// toggleAI is the a key: open a closed panel, focus a hidden one, close a
// visible one.
func (a *Model) toggleAI() {
	if a.ai.open && a.aiVisible() && a.focus != focusAI {
		a.ai.open = false
		return
	}
	a.openAI()
}

// aiKey handles a key while the panel has the focus. The input owns every
// key but these, so no bare-letter binding fires while typing: the
// proposal keys (ctrl+o loads, ctrl+enter/F5 loads and runs, ctrl+y
// copies) all take a modifier or a function key, and enter only sends a
// question.
func (a *Model) aiKey(k tea.KeyPressMsg) tea.Cmd {
	c := a.aiConvFor(a.aiConn())
	nav := k.Text == "" // scrolling never takes a letter (vim binds j and k)
	switch {
	case a.km.Matches(k, keymap.Back):
		a.focus = focusGrid
		a.ai.in.Blur()
		return nil
	case a.km.Matches(k, keymap.FocusNext):
		a.ai.mode = 1 - a.ai.mode
		return nil
	case nav && a.km.Matches(k, keymap.PageUp):
		a.ai.scroll += max(a.lay.ai.h-4, 1)
		return nil
	case nav && a.km.Matches(k, keymap.PageDown):
		a.ai.scroll = max(a.ai.scroll-max(a.lay.ai.h-4, 1), 0)
		return nil
	case nav && a.km.Matches(k, keymap.Up):
		a.ai.scroll++
		return nil
	case nav && a.km.Matches(k, keymap.Down):
		a.ai.scroll = max(a.ai.scroll-1, 0)
		return nil
	case a.km.Matches(k, keymap.LoadProposal), a.km.Matches(k, keymap.RunStatement), a.km.Matches(k, keymap.CopyProposal):
		if c == nil || c.proposal == "" {
			return a.flash("no proposed query yet: ask for one first", false)
		}
		if a.km.Matches(k, keymap.CopyProposal) {
			return tea.Batch(tea.SetClipboard(c.proposal), a.flash("copied the proposed query", false))
		}
		return a.offerProposal(c.proposal, a.km.Matches(k, keymap.RunStatement))
	case a.km.Matches(k, keymap.Open):
		q := strings.TrimSpace(a.ai.in.Value())
		if q == "" {
			return nil // never acts on a proposal
		}
		a.ai.in.SetValue("")
		return a.aiSend(q, a.ai.mode)
	}
	var cmd tea.Cmd
	a.ai.in, cmd = a.ai.in.Update(k)
	return cmd
}

// aiCommand runs :ask and :ask!: open the panel and, with a question, send it.
func (a *Model) aiCommand(c palette.AskCmd) tea.Cmd {
	a.openAI()
	if c.Answer {
		a.ai.mode = aiAnswer
	} else if c.Question != "" {
		a.ai.mode = aiAsk
	}
	if c.Question == "" {
		return nil
	}
	return a.aiSend(c.Question, a.ai.mode)
}

// proposalMsg loads a proposal once the user agreed to replace the editor
// buffer.
type proposalMsg struct {
	q   string
	run bool
}

// offerProposal loads q into the editor (and with run runs it), asking
// first when that would replace unsaved text that differs from q: the
// editor has no undo.
func (a *Model) offerProposal(q string, run bool) tea.Cmd {
	conn := a.aiConn()
	if conn == "" || conn != a.edConn {
		return a.flash("the proposal belongs to another connection", false)
	}
	if buf := a.ed.Value(); buf != q && strings.TrimSpace(buf) != "" && buf != a.runBuf[conn] {
		a.askYN("replace the editor buffer? y/N", "the editor buffer of "+conn+" has unsaved changes",
			resend(proposalMsg{q: q, run: run}), "kept the editor buffer")
		return nil
	}
	return a.loadProposal(q, run)
}

// loadProposal puts q into the editor of its connection, replacing the
// buffer, and with run runs it as the editor would (F5).
func (a *Model) loadProposal(q string, run bool) tea.Cmd {
	conn := a.aiConn()
	if conn == "" || conn != a.edConn {
		return a.flash("the proposal belongs to another connection", false)
	}
	a.closePopup()
	a.ed.SetValue(q)
	a.ed.SetCursor(len(q))
	a.edOpen = true
	if !run {
		a.focus = focusEditor
		a.ai.in.Blur()
		return a.flash("loaded the proposed query into the editor", false)
	}
	return a.runBuffer()
}

// ---- scope and conversations ---------------------------------------------------

// aiConn is the connection the panel talks about: the editor's.
func (a *Model) aiConn() string {
	conn := a.editorConn()
	if conn == "" || !hasConn(a.mgr, conn) {
		return ""
	}
	return conn
}

// aiScope is conn plus the open collection when it belongs to conn.
func (a *Model) aiScope(conn string) assistant.Scope {
	s := assistant.Scope{Conn: conn}
	if a.cur.set && a.cur.conn == conn {
		s.Namespace, s.Collection = a.cur.ref.Namespace, a.cur.ref.Name
	}
	return s
}

// aiConvFor returns conn's conversation, creating it; nil for no connection.
func (a *Model) aiConvFor(conn string) *aiConv {
	if conn == "" {
		return nil
	}
	if a.ai.conv == nil {
		a.ai.conv = map[string]*aiConv{}
	}
	c := a.ai.conv[conn]
	if c == nil {
		c = &aiConv{gen: a.mgr.ConfigGeneration(conn)}
		a.ai.conv[conn] = c
	}
	return c
}

// aiSession is conv's session, made on first use: one per conversation, so
// row consent spans its turns.
func (a *Model) aiSession(c *aiConv) assistant.Session {
	if c.sess == nil {
		if s, ok := a.asst.(assistant.Sessioner); ok {
			c.sess = s.NewSession()
		} else {
			c.sess = a.asst
		}
	}
	return c.sess
}

// aiReloaded runs after a :reload (all) or a connection write: a
// conversation whose connection was repointed is cleared, with its session,
// and one whose connection was removed is dropped. A :reload also drops
// every other session, so row consent never survives one; a write keeps the
// sessions of the connections it left alone.
func (a *Model) aiReloaded(all bool) {
	for conn, c := range a.ai.conv {
		if all {
			c.sess = nil
		}
		gen := a.mgr.ConfigGeneration(conn)
		switch {
		case gen == 0:
			delete(a.ai.conv, conn)
		case gen != c.gen:
			*c = aiConv{gen: gen}
			c.add(entryNote, "transcript cleared: "+conn+" was reconfigured")
		}
	}
}

// ---- turns ------------------------------------------------------------------------

// aiUnavailable is why no turn can run, "" when one can.
func (a *Model) aiUnavailable() string {
	if a.asst != nil {
		return ""
	}
	if a.aiErr == nil || errors.Is(a.aiErr, assistant.ErrNotConfigured) {
		return notConfiguredText
	}
	return "AI is unavailable: " + aiErrText(a.aiErr)
}

// aiSend sends question in mode for the panel's connection.
func (a *Model) aiSend(question string, mode aiMode) tea.Cmd {
	if why := a.aiUnavailable(); why != "" {
		return a.flash(why, true)
	}
	conn := a.aiConn()
	if conn == "" {
		return a.flash("select a connection in the sidebar first", false)
	}
	c := a.aiConvFor(conn)
	c.add(entryUser, question)
	scope, hist := a.aiScope(conn), slices.Clone(c.history)
	if mode == aiAsk {
		return a.startTurn(conn, c, func(ctx context.Context, s assistant.Session, ev assistant.Events) (aiResult, error) {
			r, err := s.Ask(ctx, assistant.AskRequest{Scope: scope, Question: question, History: hist}, ev)
			return aiResult{question: question, markdown: r.Explanation, query: r.Query, usage: r.Usage}, err
		})
	}
	exec := c.exec && !a.mgr.State(conn).ReadOnly
	return a.startTurn(conn, c, func(ctx context.Context, s assistant.Session, ev assistant.Events) (aiResult, error) {
		r, err := s.Answer(ctx, assistant.AnswerRequest{Scope: scope, Question: question, History: hist, AllowExecute: exec}, ev)
		return aiResult{question: question, markdown: r.Markdown, usage: r.Usage}, err
	})
}

// fixWithAI sends the failed run of tab t to the assistant: the statement,
// the error as shown and the original driver error.
func (a *Model) fixWithAI(t *resultTab) tea.Cmd {
	a.openAI()
	if why := a.aiUnavailable(); why != "" {
		return a.flash(why, true)
	}
	if t.conn != a.aiConn() {
		return a.flash("switch to "+t.conn+" to fix its run", false)
	}
	c := a.aiConvFor(t.conn)
	stmt, shown, cause := t.stmt, t.errText, t.err
	question := "fix: " + stmt
	c.add(entryUser, question)
	scope := a.aiScope(t.conn)
	return a.startTurn(t.conn, c, func(ctx context.Context, s assistant.Session, ev assistant.Events) (aiResult, error) {
		r, err := s.FixError(ctx, assistant.FixErrorRequest{Scope: scope, Query: stmt, Error: shown, Err: cause}, ev)
		return aiResult{question: question, markdown: r.Explanation, query: r.Query, usage: r.Usage}, err
	})
}

// explainResults sends the active result tab's collected rows.
func (a *Model) explainResults() tea.Cmd {
	t := a.activeTab()
	if t == nil || t.isText || t.state != tabDone {
		return a.flash("open a result tab first", false)
	}
	a.openAI()
	if why := a.aiUnavailable(); why != "" {
		return a.flash(why, true)
	}
	if t.conn != a.aiConn() {
		return a.flash("switch to "+t.conn+" to explain its results", false)
	}
	c := a.aiConvFor(t.conn)
	req := assistant.ExplainResultsRequest{Scope: a.aiScope(t.conn), Query: t.stmt, Columns: t.cols, Rows: t.result, Truncated: t.truncated}
	question := "explain the results of: " + t.stmt
	c.add(entryUser, question)
	return a.startTurn(t.conn, c, func(ctx context.Context, s assistant.Session, ev assistant.Events) (aiResult, error) {
		r, err := s.ExplainResults(ctx, req, ev)
		return aiResult{question: question, markdown: r.Markdown, usage: r.Usage}, err
	})
}

// startTurn runs fn in a command with its own cancellable context. A turn
// already running is superseded: cancelled, its late result dropped.
func (a *Model) startTurn(conn string, c *aiConv, fn func(context.Context, assistant.Session, assistant.Events) (aiResult, error)) tea.Cmd {
	a.cancelTurn("cancelled: a newer question was sent")
	a.ai.seq++
	id := a.ai.seq
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.ai.turn = &aiTurn{id: id, conn: conn, conns: map[string]bool{conn: true}, cancel: cancel, done: done}
	a.ai.scroll = 0
	sess := a.aiSession(c)
	ch := make(chan aiEvent)
	send := func(e aiEvent) bool {
		select {
		case ch <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	ev := assistant.Events{
		ToolCall: func(tc assistant.ToolCall) { send(aiToolEvent{id: id, tc: tc}) },
		ConsentRows: func(ctx context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
			reply := make(chan assistant.Consent, 1)
			if !send(aiConsentEvent{id: id, req: req, reply: reply}) {
				return assistant.ConsentNo, ctx.Err()
			}
			select {
			case r := <-reply:
				return r, nil
			case <-ctx.Done():
				return assistant.ConsentNo, ctx.Err()
			}
		},
		ConfirmExecute: func(ctx context.Context, conn, stmt string) (bool, error) {
			reply, refuse := make(chan bool, 1), make(chan string, 1)
			if !send(aiConfirmEvent{id: id, conn: conn, stmt: stmt, reply: reply, refuse: refuse}) {
				return false, ctx.Err()
			}
			select {
			case r := <-reply:
				return r, nil
			case why := <-refuse:
				return false, &assistant.ExecuteRefusal{Reason: why}
			case <-ctx.Done():
				return false, ctx.Err()
			}
		},
	}
	resolve := a.resolveDriver
	work := func() tea.Msg {
		var res aiResult
		var err error
		if resolve != nil {
			err = resolve(ctx, conn)
		}
		if err == nil {
			res, err = fn(ctx, sess, ev)
		}
		close(done) // the call has returned: a waiting config write may go on
		send(aiDoneEvent{id: id, res: res, err: err})
		close(ch)
		return nil
	}
	return tea.Batch(work, listenAI(ch))
}

// cancelTurn cancels the turn in flight, if any, notes why in its
// conversation and closes a prompt it had open. It reports whether a turn
// was running.
func (a *Model) cancelTurn(note string) bool {
	t := a.ai.turn
	if t == nil {
		return false
	}
	t.cancel()
	a.ai.turn = nil
	// Its callback returns through the cancelled context.
	if a.ask.isAI() && a.ask.turn == t.id {
		a.closeAIAsk(a.ask)
	}
	if q := a.ai.queued; q != nil && q.turn == t.id {
		a.ai.queued = nil
	}
	if c := a.ai.conv[t.conn]; c != nil && note != "" {
		c.add(entryNote, note)
	}
	return true
}

// stopAIOn cancels the turn that touched conn ("" for any) before a write
// or reload that may close the connection, and makes the write wait for it.
func (a *Model) stopAIOn(conn string) {
	t := a.ai.turn
	if t == nil || conn != "" && !t.conns[conn] {
		return
	}
	a.cancelTurn("cancelled: the connection is being changed")
	a.stopping = append(stillRunning(a.stopping), t.done)
}

// syncAI drops the turn when the scope's connection changed under it: the
// open transaction's or the open collection's. With neither, the sidebar
// cursor is only browsing and the turn goes on.
func (a *Model) syncAI() {
	t := a.ai.turn
	if t == nil {
		return
	}
	pinned := ""
	switch {
	case a.tx != nil:
		pinned = a.tx.conn
	case a.cur.set:
		pinned = a.cur.conn
	}
	if pinned != "" && pinned != t.conn {
		a.cancelTurn("cancelled: switched to another connection")
	}
}

// aiEvent handles one event of a turn and keeps listening on its channel.
func (a *Model) aiEvent(msg aiEventMsg) tea.Cmd {
	next := listenAI(msg.ch)
	t := a.ai.turn
	if t == nil || msg.ev.turnID() != t.id {
		// A stale turn: answer what it waits for with no.
		switch e := msg.ev.(type) {
		case aiConsentEvent:
			e.reply <- assistant.ConsentNo
		case aiConfirmEvent:
			e.reply <- false
		}
		return next
	}
	c := a.aiConvFor(t.conn)
	switch e := msg.ev.(type) {
	case aiToolEvent:
		t.touch(e.tc.Conn)
		c.entries = append(c.entries, aiEntry{kind: entryTool, tc: e.tc, conn: t.conn})
	case aiConsentEvent:
		t.touch(e.req.Conn)
		model := ""
		if a.asst != nil {
			model = a.asst.Model()
		}
		a.askConsent(t.id, consentTitle(e.req, model), e.req.What, e.reply)
	case aiConfirmEvent:
		t.touch(e.conn)
		if a.mgr.State(e.conn).ReadOnly {
			e.reply <- false
			c.add(entryNote, "execute refused: "+e.conn+" is read-only")
			break
		}
		if !c.exec { // :ai execute off since the turn started
			e.reply <- false
			c.add(entryNote, "execute refused: AI execute is off")
			break
		}
		if why := a.txBlocksExecute(e.conn); why != "" {
			e.refuse <- why
			c.add(entryNote, "execute refused: "+why)
			return tea.Batch(next, a.flash(why, false))
		}
		a.askConfirm(t.id, e.conn, "run this statement on "+e.conn+"? it may change data. y/N", e.stmt, e.reply, e.refuse)
	case aiDoneEvent:
		a.ai.turn = nil
		t.cancel()
		a.aiDone(t, c, e)
	}
	return next
}

// consentTitle is the row-consent question: how many rows, from which
// connection, to which model.
func consentTitle(req assistant.ConsentRequest, model string) string {
	return fmt.Sprintf("send %s from %s to %s?", assistant.RowsText(req.Rows), req.Conn, model)
}

// txBlocksExecute says why an AI execute on conn may not run now, "" when
// it may: a transaction is open there, and execute runs outside it, so its
// write would commit at once while the user expects :tx rollback to undo it.
func (a *Model) txBlocksExecute(conn string) string {
	if a.tx != nil && a.tx.conn == conn {
		return "a transaction is open on " + conn + "; commit or roll back first"
	}
	return ""
}

// touch records that the turn used conn ("" is none).
func (t *aiTurn) touch(conn string) {
	if conn != "" {
		t.conns[conn] = true
	}
}

// aiDone puts a finished turn into its conversation.
func (a *Model) aiDone(t *aiTurn, c *aiConv, e aiDoneEvent) {
	a.ai.scroll = 0
	if e.err != nil {
		var te *assistant.TurnError
		if errors.As(e.err, &te) && te.Usage.Model != "" {
			defer c.add(entryFooter, usageLine(te.Usage))
		}
		c.add(entryError, a.mgr.Redact(t.conn, aiErrText(e.err)))
		return
	}
	r := e.res
	model := r.markdown
	if r.markdown != "" {
		c.add(entryModel, r.markdown)
	}
	if r.query != "" {
		c.add(entryProposal, r.query)
		c.proposal = r.query
		model = strings.TrimSpace(model + "\n\n" + fence(r.query))
	}
	if r.usage.Model != "" {
		c.add(entryFooter, usageLine(r.usage))
	}
	if model != "" { // an empty answer is no context for the next turn
		c.history = append(c.history, assistant.Turn{Role: "user", Text: r.question}, assistant.Turn{Role: "model", Text: model})
	}
}

// fence is q as a markdown code block, for the history.
func fence(q string) string {
	n := 3
	for run := 0; run < len(q); {
		if q[run] != '`' {
			run++
			continue
		}
		j := run
		for j < len(q) && q[j] == '`' {
			j++
		}
		n = max(n, j-run+1)
		run = j
	}
	f := strings.Repeat("`", n)
	return f + "sql\n" + q + "\n" + f
}

// aiExecute is :ai execute on|off for the panel's connection.
func (a *Model) aiExecute(on bool) tea.Cmd {
	conn := a.aiConn()
	if conn == "" {
		return a.flash("select a connection in the sidebar first", false)
	}
	if on && a.mgr.State(conn).ReadOnly {
		return a.flash("🔒 "+conn+" is read-only; AI execute stays off", false)
	}
	a.aiConvFor(conn).exec = on
	if on {
		return a.flash("AI execute on for "+conn+": every statement asks first", false)
	}
	return a.flash("AI execute off for "+conn, false)
}

// usageLine is a turn footer: model, tokens and latency.
func usageLine(u assistant.Usage) string {
	return u.Model + " · " + tokens(u.InputTokens) + " in / " + tokens(u.OutputTokens) + " out · " + duration(u.Latency)
}

func tokens(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// aiErrText is the message for a failed turn or setup; err is already
// redacted by the assistant.
func aiErrText(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, assistant.ErrNotConfigured):
		return notConfiguredText
	case errors.Is(err, assistant.ErrAuth):
		return "the AI provider rejected the credentials; check ai.api_key (" + msg + ")"
	case errors.Is(err, assistant.ErrRateLimit):
		return "the AI provider's rate limit was reached; wait a moment and try again (" + msg + ")"
	case errors.Is(err, assistant.ErrUnavailable):
		return "the AI provider cannot be reached (" + msg + ")"
	}
	return msg
}

// aiHeader is the panel's title, w columns at most: model, scope, mode. The
// model and scope are cut first, so the mode always shows.
func (a *Model) aiHeader(w int) string {
	front, tail := a.aiHeaderParts()
	if query.DisplayWidth(front+tail) <= w || query.DisplayWidth(tail) >= w {
		return query.Truncate(front+tail, w)
	}
	return query.Truncate(front, w-query.DisplayWidth(tail)) + tail
}

// aiHeaderParts is the escaped header: what may be cut, and the mode.
func (a *Model) aiHeaderParts() (front, tail string) {
	parts := []string{"AI"}
	if a.asst != nil {
		parts = append(parts, a.asst.Model())
	}
	conn := a.aiConn()
	if conn != "" {
		s := a.aiScope(conn)
		where := conn
		if s.Collection != "" {
			if s.Namespace != "" {
				where += "/" + s.Namespace
			}
			where += "/" + s.Collection
		}
		parts = append(parts, where)
	}
	tail = " · " + a.ai.mode.String()
	if c := a.ai.conv[conn]; c != nil && c.exec && !a.mgr.State(conn).ReadOnly {
		tail += " · execute"
	}
	return query.EscapeControls(strings.Join(parts, " · ")), tail
}

// aiPending is the progress line of a turn in flight on conn, "" for none.
func (a *Model) aiPending(conn string) string {
	if t := a.ai.turn; t != nil && t.conn == conn {
		return "thinking… · ctrl+c cancels"
	}
	return ""
}
