package tui

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/async"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

const (
	// walkLimit is how many history entries ↑/↓ walk through.
	walkLimit = 200
	// searchLimit is how many matches one ctrl+r search reads.
	searchLimit = 50
	// listTimeout bounds one history read.
	listTimeout = 2 * time.Second
)

// histWalk is ↑/↓ through the connection's history, newest first.
type histWalk struct {
	tr      async.Tracker
	active  bool
	loaded  bool
	entries []string
	idx     int    // the entry shown; -1 is the stash
	stash   buffer // the buffer as it was when the walk started
}

// histSearch is ctrl+r: an incremental search of the connection's history.
type histSearch struct {
	tr      async.Tracker
	active  bool
	term    string
	matches []string
	idx     int
	failed  bool   // the term matched nothing
	readErr bool   // the last read failed
	orig    buffer // the buffer as it was when the search started
}

type walkLoaded struct{ entries []string }

type walkDoneMsg = async.Result[walkLoaded]

type searchLoaded struct {
	term    string
	entries []string
}

type searchDoneMsg = async.Result[searchLoaded]

// queries returns the entries' query texts with consecutive duplicates
// shown once.
func queries(es []history.Entry) []string {
	var out []string
	for _, e := range es {
		if len(out) > 0 && out[len(out)-1] == e.Query {
			continue
		}
		out = append(out, e.Query)
	}
	return out
}

// listHistory reads the history in a command with its own context, tracked
// by tr so a newer read cancels it and its late answer is dropped.
func listHistory[T any](h HistoryStore, tr *async.Tracker, f history.Filter, wrap func([]string) T) tea.Cmd {
	id, ctx := tr.Start(context.Background())
	return async.Do(id, ctx, func(ctx context.Context) (T, error) {
		ctx, cancel := context.WithTimeout(ctx, listTimeout)
		defer cancel()
		es, err := h.List(ctx, f)
		return wrap(queries(es)), err
	})
}

// historyErr is the flash for a failed history read.
func (a *Model) historyErr(err error) tea.Cmd {
	return a.flash("history: "+a.mgr.Redact(a.edConn, err.Error()), true)
}

// ---- ↑ / ↓ -------------------------------------------------------------------

// recallExcluded are the history sources the editor never recalls: the
// tool calls MCP clients and the AI assistant ran (schema calls, resource
// reads, refusals), which are not the user's queries.
var recallExcluded = []history.Source{history.SourceMCP, history.SourceAI}

// walkKey is ↑ on the first line and ↓ on the last: they walk the history.
// The first ↑ keeps the buffer aside, and ↓ past the newest entry brings it
// back. ok is false for keys the editor handles as usual.
func (a *Model) walkKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	if a.hist == nil || a.edConn == "" {
		return nil, false
	}
	switch k.String() {
	case "up":
		if !a.ed.FirstLine() {
			return nil, false
		}
		if !a.walk.active {
			a.walk.active, a.walk.loaded, a.walk.entries, a.walk.idx = true, false, nil, -1
			a.walk.stash = buffer{a.ed.Value(), a.ed.Cursor()}
			return listHistory(a.hist, &a.walk.tr, history.Filter{Conn: a.edConn, ExcludeSources: recallExcluded, Limit: walkLimit},
				func(qs []string) walkLoaded { return walkLoaded{qs} }), true
		}
		if a.walk.loaded && a.walk.idx+1 < len(a.walk.entries) {
			a.walk.idx++
			a.showWalk()
		}
		return nil, true
	case "down":
		if !a.walk.active || !a.ed.LastLine() {
			return nil, false
		}
		if !a.walk.loaded {
			return nil, true
		}
		a.walk.idx--
		if a.walk.idx < 0 {
			a.ed.SetValue(a.walk.stash.text)
			a.ed.SetCursor(a.walk.stash.cursor)
			a.endWalk()
			return nil, true
		}
		a.showWalk()
		return nil, true
	}
	return nil, false
}

// showWalk puts the walk's current entry in the buffer.
func (a *Model) showWalk() { a.ed.SetValue(a.walk.entries[a.walk.idx]) }

// endWalk leaves walk mode; the buffer keeps what it shows.
func (a *Model) endWalk() {
	if a.walk.active {
		a.walk.tr.Cancel()
		a.walk.active, a.walk.entries = false, nil
	}
}

// walkDone shows the newest entry once the walk's history has loaded.
func (a *Model) walkDone(r walkDoneMsg) tea.Cmd {
	if !a.walk.tr.Finish(r.ID) || !a.walk.active {
		return nil
	}
	if r.Err != nil {
		a.endWalk()
		return a.historyErr(r.Err)
	}
	if len(r.Val.entries) == 0 {
		a.endWalk()
		return a.flash("no history for "+a.edConn+" yet", false)
	}
	// The newest entry is often the buffer itself (just run): skip what
	// equals the draft, as readline does, so every ↑ shows something new.
	var entries []string
	for _, q := range r.Val.entries {
		if q != a.walk.stash.text && (len(entries) == 0 || entries[len(entries)-1] != q) {
			entries = append(entries, q)
		}
	}
	if len(entries) == 0 {
		a.endWalk()
		return a.flash("no other queries in the history of "+a.edConn, false)
	}
	a.walk.entries, a.walk.loaded, a.walk.idx = entries, true, 0
	a.showWalk()
	return nil
}

// ---- ctrl+r ------------------------------------------------------------------

// startSearch opens the reverse-i-search prompt.
func (a *Model) startSearch() tea.Cmd {
	switch {
	case a.hist == nil:
		return a.flash("the query history is not available", false)
	case a.edConn == "":
		return a.flash("select a connection in the sidebar first", false)
	case a.lay.editor.h < 2:
		// The prompt takes the editor's last line and needs one left over.
		return a.flash("enlarge the editor to search history", false)
	}
	a.closePopup()
	a.endWalk()
	a.hs.tr.Cancel()
	a.hs.active, a.hs.term, a.hs.matches, a.hs.idx, a.hs.failed, a.hs.readErr = true, "", nil, 0, false, false
	a.hs.orig = buffer{a.ed.Value(), a.ed.Cursor()}
	return nil
}

// searchKey handles a key while searching. Typing extends the term, ctrl+r
// steps to the next older match, enter keeps the match and esc or ctrl+g
// put the buffer back. Any other key keeps the match and then acts as usual
// (ok is false).
func (a *Model) searchKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case a.km.Matches(k, keymap.Back), k.String() == "ctrl+g":
		a.endSearch(false)
		return nil, true
	case k.String() == "enter":
		a.endSearch(true)
		return nil, true
	case a.km.Matches(k, keymap.HistSearch):
		if a.hs.idx+1 < len(a.hs.matches) {
			a.hs.idx++
			a.ed.SetValue(a.hs.matches[a.hs.idx])
		}
		return nil, true
	case k.String() == "backspace":
		if a.hs.term != "" {
			_, n := utf8.DecodeLastRuneInString(a.hs.term)
			a.hs.term = a.hs.term[:len(a.hs.term)-n]
			return a.searchQuery(), true
		}
		return nil, true
	case k.Text != "" && k.Mod&^tea.ModShift == 0:
		a.hs.term += k.Text
		return a.searchQuery(), true
	}
	a.endSearch(true)
	return nil, false
}

// searchQuery reads the matches of the current term; an empty term shows
// the original buffer again.
func (a *Model) searchQuery() tea.Cmd {
	if a.hs.term == "" {
		a.hs.tr.Cancel()
		a.hs.matches, a.hs.idx, a.hs.failed, a.hs.readErr = nil, 0, false, false
		a.ed.SetValue(a.hs.orig.text)
		a.ed.SetCursor(a.hs.orig.cursor)
		return nil
	}
	term := a.hs.term
	return listHistory(a.hist, &a.hs.tr, history.Filter{Conn: a.edConn, ExcludeSources: recallExcluded, Search: term, Limit: searchLimit},
		func(qs []string) searchLoaded { return searchLoaded{term, qs} })
}

// searchDone shows the newest match of the term it was read for.
func (a *Model) searchDone(r searchDoneMsg) tea.Cmd {
	if !a.hs.tr.Finish(r.ID) || !a.hs.active || r.Val.term != a.hs.term {
		return nil
	}
	if r.Err != nil {
		// Never leave the previous term's match under the new term.
		a.hs.matches, a.hs.idx, a.hs.failed, a.hs.readErr = nil, 0, false, true
		return a.historyErr(r.Err)
	}
	a.hs.readErr = false
	a.hs.matches, a.hs.idx, a.hs.failed = r.Val.entries, 0, len(r.Val.entries) == 0
	if !a.hs.failed {
		a.ed.SetValue(a.hs.matches[0])
	}
	return nil
}

// endSearch closes the prompt, keeping the match in the buffer or putting
// the original buffer back.
func (a *Model) endSearch(accept bool) {
	if !a.hs.active {
		return
	}
	a.hs.tr.Cancel()
	if !accept {
		a.ed.SetValue(a.hs.orig.text)
		a.ed.SetCursor(a.hs.orig.cursor)
	}
	a.hs.active, a.hs.matches = false, nil
}

// oneLine shows a query on one line: escaped, newlines as ⏎.
func oneLine(q string) string {
	q = strings.ReplaceAll(strings.ReplaceAll(q, "\r\n", "\n"), "\r", "\n")
	parts := strings.Split(q, "\n")
	for i, p := range parts {
		parts[i] = query.EscapeControls(p)
	}
	return strings.Join(parts, "⏎")
}

// searchPrompt is the editor's footer line while searching.
func (a *Model) searchPrompt(w int) string {
	label := "(reverse-i-search)"
	if a.hs.failed {
		label = "(failed reverse-i-search)"
	}
	match := ""
	switch {
	case a.hs.readErr:
		match = "search failed"
	case a.hs.idx < len(a.hs.matches):
		match = oneLine(a.hs.matches[a.hs.idx])
	}
	t := query.Truncate(label+" '"+oneLine(a.hs.term)+"': "+match, w)
	return a.th.Styles.Title.Render(t) + strings.Repeat(" ", max(0, w-query.DisplayWidth(t)))
}
