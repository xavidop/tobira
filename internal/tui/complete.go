package tui

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/async"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

const (
	// maxCompletions caps the popup's list.
	maxCompletions = 200
	// popupRows is how many completions the popup shows at once.
	popupRows = 8
	// popupMaxW caps the popup's width.
	popupMaxW = 60
	// completeDebounce is how long typing must pause before a lookup.
	completeDebounce = 80 * time.Millisecond
	// completeTimeout bounds one lookup.
	completeTimeout = 5 * time.Second
)

// token is the name being typed at the cursor, within its line.
type token struct {
	start, end int    // the bytes a completion replaces, quotes included
	prefix     string // what is typed before the cursor, without the quote
	dot        bool   // the name follows a "."
	qual       string // the name before that ".", unquoted
	quote      byte   // the quote the name opened with (", ` or [), or 0
	ok         bool   // a name can be typed here: not in a string or comment
}

func isIdentRune(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

func closingQuote(q byte) byte {
	if q == '[' {
		return ']'
	}
	return q
}

// identBack returns where the run of identifier runes ending at i starts.
func identBack(s string, i int) int {
	for i > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:i])
		if !isIdentRune(r) {
			break
		}
		i -= n
	}
	return i
}

// identFwd returns where the run of identifier runes starting at i ends.
func identFwd(s string, i int) int {
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !isIdentRune(r) {
			break
		}
		i += n
	}
	return i
}

// squareQuotes reports whether dialect quotes names with [ ] (sqlite and
// mssql style). In postgres and mysql "[" is an array subscript or nothing.
func squareQuotes(dialect string) bool {
	switch strings.ToLower(dialect) {
	case "postgresql", "postgres", "mysql", "mariadb":
		return false
	}
	return true
}

// doubleQuoteString reports whether dialect reads "..." as a string, not a
// quoted name: mysql and mariadb do unless sql_mode has ANSI_QUOTES, which
// the TUI does not know, so it takes the default. GoogleSQL always does.
func doubleQuoteString(dialect string) bool {
	switch strings.ToLower(dialect) {
	case "mysql", "mariadb", "googlesql":
		return true
	}
	return false
}

// quoteLabel quotes label as a name opened with q, doubling any closing
// quote inside it.
func quoteLabel(label string, q byte) string {
	cl := string(closingQuote(q))
	return string(q) + strings.ReplaceAll(label, cl, cl+cl) + cl
}

// tokenAt finds the name being typed at byte col of line. It looks at this
// line only: strings, comments and quoted names are recognised when they
// open on it, with the rules every SQL dialect shares ([ ] only when square
// is set, " as a string when dqString is). The driver's own Completer sees the whole buffer and applies its
// dialect's rules; the highlighter's tokens (Model.CursorInLiteral) cover
// what spans lines.
func tokenAt(line string, col int, square, dqString bool) token {
	col = max(0, min(col, len(line)))
	quoteAt := -1
	for i := 0; i < col; {
		c := line[i]
		switch {
		case c == '\'' || c == '"' && dqString:
			j := strings.IndexByte(line[i+1:col], c)
			if j < 0 {
				return token{} // inside a string
			}
			i += j + 2
		case c == '-' && i+1 < col && line[i+1] == '-':
			return token{} // a line comment
		case c == '/' && i+1 < col && line[i+1] == '*':
			j := strings.Index(line[i+2:col], "*/")
			if j < 0 {
				return token{} // inside a block comment
			}
			i += j + 4
		case c == '"' || c == '`' || c == '[' && square:
			j := strings.IndexByte(line[i+1:col], closingQuote(c))
			if j < 0 {
				quoteAt, i = i, col // the cursor is inside this quoted name
				continue
			}
			i += j + 2
		default:
			i++
		}
	}
	t := token{ok: true}
	if quoteAt >= 0 {
		t.start, t.prefix, t.quote = quoteAt, line[quoteAt+1:col], line[quoteAt]
		if j := strings.IndexByte(line[col:], closingQuote(line[quoteAt])); j >= 0 {
			t.end = col + j + 1
		} else {
			t.end = identFwd(line, col)
		}
	} else {
		t.start, t.end = identBack(line, col), identFwd(line, col)
		t.prefix = line[t.start:col]
	}
	if d := t.start - 1; d >= 0 && line[d] == '.' {
		t.dot = true
		if d > 0 && (line[d-1] == '"' && !dqString || line[d-1] == '`' || line[d-1] == ']' && square) {
			open := line[d-1]
			if open == ']' {
				open = '['
			}
			if q := strings.LastIndexByte(line[:d-1], open); q >= 0 {
				t.qual = line[q+1 : d-1]
			}
		} else {
			t.qual = line[identBack(line, d):d]
		}
	}
	return t
}

// autoOpens reports whether typing this token opens the popup by itself:
// right after a "." or once two characters of a name are typed.
func (t token) autoOpens() bool {
	if !t.ok {
		return false
	}
	if t.dot {
		return true
	}
	r, _ := utf8.DecodeRuneInString(t.prefix)
	return utf8.RuneCountInString(t.prefix) >= 2 && (unicode.IsLetter(r) || r == '_')
}

// schemaCache is what the TUI learned of a connection's fields: the columns
// of the collections opened in the grid. Namespaces and collections come from
// the sidebar's loaded tree.
type schemaCache struct {
	fields map[driver.CollectionRef][]string
}

// schemaSnap is a copy of the known names of one connection, safe to read off
// the Update goroutine.
type schemaSnap struct {
	namespaces  []string
	collections []driver.CollectionRef
	fields      map[driver.CollectionRef][]string
}

func hasPrefixFold(s, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
}

// complete matches the snapshot against tok: after "x." the collections of
// namespace x or the fields of collection x, otherwise every namespace,
// collection and field whose name starts with the prefix (any case).
func (s schemaSnap) complete(tok token) []driver.Completion {
	var out []driver.Completion
	add := func(label, kind, detail string) {
		if hasPrefixFold(label, tok.prefix) {
			out = append(out, driver.Completion{Label: label, Kind: kind, Detail: detail})
		}
	}
	refs := make([]driver.CollectionRef, 0, len(s.fields))
	for r := range s.fields {
		refs = append(refs, r)
	}
	slices.SortFunc(refs, func(a, b driver.CollectionRef) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	if tok.dot {
		for _, c := range s.collections {
			if strings.EqualFold(c.Namespace, tok.qual) {
				add(c.Name, "collection", c.Namespace)
			}
		}
		for _, r := range refs {
			if strings.EqualFold(r.Name, tok.qual) {
				for _, f := range s.fields[r] {
					add(f, "field", r.Name)
				}
			}
		}
		return out
	}
	for _, ns := range s.namespaces {
		if ns != "" {
			add(ns, "namespace", "")
		}
	}
	for _, c := range s.collections {
		add(c.Name, "collection", c.Namespace)
	}
	for _, r := range refs {
		for _, f := range s.fields[r] {
			add(f, "field", r.Name)
		}
	}
	return out
}

// mergeCompletions puts the Completer's entries first, then the local ones,
// keeping the first entry of each label and at most maxCompletions.
func mergeCompletions(lists ...[]driver.Completion) []driver.Completion {
	seen := map[string]bool{}
	var out []driver.Completion
	for _, l := range lists {
		for _, c := range l {
			if seen[c.Label] {
				continue
			}
			seen[c.Label] = true
			out = append(out, c)
			if len(out) == maxCompletions {
				return out
			}
		}
	}
	return out
}

// ---- popup state -------------------------------------------------------------

// completion is the editor's completion popup.
type completion struct {
	tr     async.Tracker // the lookup in flight
	seq    uint64        // debounce generation; a tick of an older one is ignored
	open   bool
	manual bool // opened with ctrl+space: an empty prefix still completes
	start  int  // buffer offset of the token the popup completes
	// lookups are the lookups whose command may not have returned yet: a
	// cancelled one can still be unwinding inside the driver.
	lookups []lookupRun
	items   []driver.Completion
	sel     int
	off     int
}

// lookupRun is a lookup's connection and the channel closed when its
// command returned.
type lookupRun struct {
	conn string
	done <-chan struct{}
}

// running drops the lookups that have returned.
func running(ls []lookupRun) []lookupRun {
	out := ls[:0]
	for _, l := range ls {
		select {
		case <-l.done:
		default:
			out = append(out, l)
		}
	}
	return out
}

// completeTickMsg fires when typing paused long enough for a lookup.
type completeTickMsg struct{ seq uint64 }

// completeOutcome is a lookup's answer and what it was computed for.
type completeOutcome struct {
	version uint64
	cursor  int
	items   []driver.Completion
}

type completeDoneMsg = async.Result[completeOutcome]

// closePopup closes the popup and forgets its lookups, pending or in flight.
func (a *Model) closePopup() {
	a.cp.tr.Cancel()
	a.cp.seq++
	a.cp.open, a.cp.manual, a.cp.items = false, false, nil
}

// popupShown reports whether the popup is drawn and owns its keys.
func (a *Model) popupShown() bool {
	return a.cp.open && a.edOpen && a.focus == focusEditor && !a.hs.active
}

// scheduleComplete starts the debounce: a lookup runs when no key arrives
// for completeDebounce. It never blocks.
func (a *Model) scheduleComplete() tea.Cmd {
	a.cp.seq++
	seq := a.cp.seq
	return tea.Tick(completeDebounce, func(time.Time) tea.Msg { return completeTickMsg{seq} })
}

// completeTick runs the lookup of the latest debounce.
func (a *Model) completeTick(msg completeTickMsg) tea.Cmd {
	if msg.seq != a.cp.seq || !a.edOpen || a.focus != focusEditor {
		return nil
	}
	// Opening by itself inside a string or comment is wrong. The frame
	// drawn since the key lexed it, so this costs no extra lexing; when
	// that pass does not know, the line scan in lookup decides.
	if in, known := a.ed.CursorInLiteral(); known && in && !a.cp.manual {
		a.closePopup()
		return nil
	}
	return a.lookup()
}

// manualComplete is ctrl+space: look up now, whatever was typed.
func (a *Model) manualComplete() tea.Cmd {
	a.cp.seq++
	a.cp.manual = true
	return a.lookup()
}

// editorToken is the token at the cursor and its line's buffer offset.
func (a *Model) editorToken() (token, int) {
	line, col, start := a.ed.CursorLine()
	d := a.ed.Dialect()
	return tokenAt(line, col, squareQuotes(d), doubleQuoteString(d)), start
}

// lookup asks the Completer (when the connection is open and has one) and
// the schema cache for the names at the cursor, in a command with its own
// context. A newer lookup cancels it; its answer carries the buffer version
// and cursor it was computed for, so a stale one is dropped.
func (a *Model) lookup() tea.Cmd {
	tok, ls := a.editorToken()
	conn := a.edConn
	if !tok.ok || conn == "" {
		a.closePopup()
		return nil
	}
	a.cp.start = ls + tok.start
	snap := a.schemaSnapshot(conn)
	c, _ := a.mgr.Conn(conn) // only an open connection; completion never opens one
	ns := ""
	if a.cur.set && a.cur.conn == conn {
		ns = a.cur.ref.Namespace
	}
	ver, cursor := a.ed.Version(), a.ed.Cursor()
	req := driver.CompletionRequest{Text: a.ed.Value(), Cursor: cursor, Namespace: ns}
	id, ctx := a.cp.tr.Start(context.Background())
	done := make(chan struct{})
	a.cp.lookups = append(running(a.cp.lookups), lookupRun{conn: conn, done: done})
	return async.Do(id, ctx, func(ctx context.Context) (completeOutcome, error) {
		defer close(done)
		ctx, cancel := context.WithTimeout(ctx, completeTimeout)
		defer cancel()
		var items []driver.Completion
		var err error
		if comp, ok := driver.As[driver.Completer](c); ok {
			items, err = comp.Complete(ctx, req)
		}
		items = mergeCompletions(items, snap.complete(tok))
		return completeOutcome{version: ver, cursor: cursor, items: items}, err
	})
}

// completeDone shows a lookup's answer, unless the buffer or the cursor
// moved on since it was asked for. A Completer error leaves the schema
// cache's names; it is only logged.
func (a *Model) completeDone(r completeDoneMsg) tea.Cmd {
	if !a.cp.tr.Finish(r.ID) {
		return nil
	}
	if r.Err != nil {
		a.log.Debug("completion failed", "error", a.mgr.Redact(a.edConn, r.Err.Error()))
	}
	v := r.Val
	if v.version != a.ed.Version() || v.cursor != a.ed.Cursor() || !a.edOpen || a.focus != focusEditor {
		return nil
	}
	if len(v.items) == 0 {
		a.closePopup()
		return nil
	}
	a.cp.open, a.cp.items, a.cp.sel, a.cp.off = true, v.items, 0, 0
	return nil
}

// schemaSnapshot copies what is known of conn's names: the sidebar's loaded
// namespaces and collections, and the fields of the collections opened in
// the grid (the open one is noted first).
func (a *Model) schemaSnapshot(conn string) schemaSnap {
	sc := a.schema[conn]
	if sc == nil {
		sc = &schemaCache{fields: map[driver.CollectionRef][]string{}}
		a.schema[conn] = sc
	}
	if a.cur.set && a.cur.conn == conn {
		if cols := a.gr.Columns(); len(cols) > 0 {
			names := make([]string, len(cols))
			for i, c := range cols {
				names[i] = c.Name
			}
			sc.fields[a.cur.ref] = names
		}
	}
	nss, colls := a.sb.Loaded(conn)
	fields := make(map[driver.CollectionRef][]string, len(sc.fields))
	for r, f := range sc.fields {
		fields[r] = f
	}
	return schemaSnap{namespaces: nss, collections: colls, fields: fields}
}

// afterEditorKey keeps the popup in step with an editor key that changed
// the buffer or moved the cursor: it opens on its own after "." or two
// typed characters, re-queries while typing, and closes when the cursor
// leaves the token.
func (a *Model) afterEditorKey(typed bool, oldVer uint64, oldCur int) tea.Cmd {
	edited, moved := a.ed.Version() != oldVer, a.ed.Cursor() != oldCur
	if edited {
		a.endWalk()
	}
	if !edited && !moved {
		return nil
	}
	tok, ls := a.editorToken()
	if !tok.ok {
		a.closePopup()
		return nil
	}
	if a.cp.open {
		sameToken := ls+tok.start == a.cp.start
		empty := tok.prefix == "" && !tok.dot
		// A move off the token closes it; so does emptying the name, unless
		// ctrl+space opened it on this very token.
		movedOff := !edited && !sameToken
		emptied := empty && (!a.cp.manual || !sameToken)
		if movedOff || emptied {
			a.closePopup()
			return nil
		}
		if !sameToken {
			// A new name (after a "."): the items were for the old one.
			a.closePopup()
			if tok.autoOpens() {
				return a.scheduleComplete()
			}
			return nil
		}
		// Keep only what still matches, at once: the re-query takes a
		// while, and a stale item must never be shown or accepted.
		if !a.filterPopup(tok.prefix) {
			a.closePopup()
			return nil
		}
		return a.scheduleComplete()
	}
	if edited && typed && tok.autoOpens() {
		return a.scheduleComplete()
	}
	return nil
}

// filterPopup keeps the items whose label starts with prefix, reporting
// whether any is left. The selection stays on its item when it survives.
func (a *Model) filterPopup(prefix string) bool {
	var sel string
	if a.cp.sel < len(a.cp.items) {
		sel = a.cp.items[a.cp.sel].Label
	}
	kept := a.cp.items[:0:0]
	a.cp.sel, a.cp.off = 0, 0
	for _, it := range a.cp.items {
		if hasPrefixFold(it.Label, prefix) {
			if it.Label == sel {
				a.cp.sel = len(kept)
			}
			kept = append(kept, it)
		}
	}
	a.cp.items = kept
	a.movePopup(0)
	return len(kept) > 0
}

// selectedMatches reports whether the selected item completes the name at
// the cursor.
func (a *Model) selectedMatches() bool {
	if a.cp.sel >= len(a.cp.items) {
		return false
	}
	tok, _ := a.editorToken()
	return tok.ok && hasPrefixFold(a.cp.items[a.cp.sel].Label, tok.prefix)
}

// popupKey handles a key while the popup is shown; ok is false for keys it
// leaves to the editor. tab and enter accept only an item that matches
// what is typed; otherwise the popup closes and they act as usual.
func (a *Model) popupKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case a.km.Matches(k, keymap.Back):
		a.closePopup()
		return nil, true
	case k.String() == "tab", k.String() == "enter":
		if !a.selectedMatches() {
			a.closePopup()
			return nil, false
		}
		return a.acceptCompletion(), true
	}
	switch k.String() {
	case "up", "ctrl+p":
		a.movePopup(-1)
		return nil, true
	case "down", "ctrl+n":
		a.movePopup(1)
		return nil, true
	}
	return nil, false
}

func (a *Model) movePopup(d int) {
	n := len(a.cp.items)
	if n == 0 {
		return
	}
	a.cp.sel = ((a.cp.sel+d)%n + n) % n
	if a.cp.sel < a.cp.off {
		a.cp.off = a.cp.sel
	}
	if a.cp.sel >= a.cp.off+popupRows {
		a.cp.off = a.cp.sel - popupRows + 1
	}
}

// acceptCompletion replaces the token at the cursor with the selected
// completion's Insert (or Label). A namespace inserted with its "." looks up
// what follows.
func (a *Model) acceptCompletion() tea.Cmd {
	if a.cp.sel >= len(a.cp.items) {
		a.closePopup()
		return nil
	}
	it := a.cp.items[a.cp.sel]
	tok, ls := a.editorToken()
	a.closePopup()
	if !tok.ok {
		return nil
	}
	text := it.Insert
	switch {
	case text != "":
	case tok.quote != 0:
		// The replaced range holds the user's quotes: keep them.
		text = quoteLabel(it.Label, tok.quote)
	default:
		text = it.Label
	}
	if !a.ed.ReplaceRange(ls+tok.start, ls+tok.end, text) {
		return a.flash("refused: the editor holds at most 1 MiB", true)
	}
	a.endWalk()
	if strings.HasSuffix(text, ".") {
		return a.scheduleComplete()
	}
	return nil
}

// stopLookups cancels the completion lookups on conn ("" for any) and the
// history reads in flight, because conn is about to be closed (an edit, a
// removal or a reload). Like a cancelled run, every lookup on conn that has
// not returned yet, an earlier one still unwinding included, goes on
// a.stopping, so the config write waits until they have returned.
func (a *Model) stopLookups(conn string) {
	var kept []lookupRun
	stopped := false
	for _, l := range running(a.cp.lookups) {
		if conn == "" || l.conn == conn {
			a.stopping = append(stillRunning(a.stopping), l.done)
			stopped = true
			continue
		}
		kept = append(kept, l)
	}
	a.cp.lookups = kept
	if stopped {
		a.closePopup()
	}
	a.hs.tr.Cancel()
	if a.walk.active && !a.walk.loaded {
		a.endWalk()
	}
}

// invalidateCmd drops the completion caches of conns: the TUI's at once,
// the drivers' (Invalidate on an open connection that has one) in a
// command, off the Update goroutine.
func (a *Model) invalidateCmd(conns ...string) tea.Cmd {
	var invs []driver.CatalogInvalidator
	for _, name := range conns {
		delete(a.schema, name)
		if c, ok := a.mgr.Conn(name); ok {
			if inv, ok := driver.As[driver.CatalogInvalidator](c); ok {
				invs = append(invs, inv)
			}
		}
	}
	if len(invs) == 0 {
		return nil
	}
	return func() tea.Msg {
		for _, inv := range invs {
			inv.Invalidate()
		}
		return nil
	}
}

// ---- drawing ---------------------------------------------------------------------

// overlayPopup draws the popup over the editor's lines (each exactly w
// wide): below the cursor line, or above it when there is no room, clamped
// to the editor.
func (a *Model) overlayPopup(lines []string, w, h int) {
	if !a.popupShown() || len(a.cp.items) == 0 || w < 4 || h < 2 {
		return
	}
	cx, cy := a.ed.CursorPos(w, h)
	rows := min(popupRows, len(a.cp.items))
	below, above := h-cy-1, cy
	y := cy + 1
	switch {
	case below >= rows:
	case above >= rows:
		y = cy - rows
	case below >= above:
		rows = below
	default:
		rows, y = above, 0
	}
	if rows < 1 {
		return
	}
	off := min(a.cp.off, max(0, len(a.cp.items)-rows))
	if a.cp.sel < off {
		off = a.cp.sel
	}
	if a.cp.sel >= off+rows {
		off = a.cp.sel - rows + 1
	}
	shown := a.cp.items[off : off+rows]

	labels := make([]string, len(shown))
	kinds := make([]string, len(shown))
	lw, kw := 0, 0
	for i, it := range shown {
		labels[i] = query.EscapeControls(it.Label)
		kinds[i] = query.EscapeControls(cmp.Or(it.Detail, it.Kind))
		if it.Detail != "" && it.Kind != "" {
			kinds[i] = query.EscapeControls(it.Kind + " · " + it.Detail)
		}
		lw = max(lw, ansi.StringWidth(labels[i]))
		kw = max(kw, ansi.StringWidth(kinds[i]))
	}
	bw := min(w, popupMaxW, lw+kw+4)
	x := max(0, min(cx, w-bw))
	base := a.th.Styles.StatusBar
	dim := base.Foreground(a.th.Muted)
	for i := range shown {
		label := query.Truncate(labels[i], max(1, bw-2))
		room := bw - 2 - query.DisplayWidth(label) - 2
		kind := ""
		if room > 0 {
			kind = query.Truncate(kinds[i], room)
		}
		gap := bw - 2 - query.DisplayWidth(label) - query.DisplayWidth(kind)
		var row string
		if off+i == a.cp.sel {
			row = a.th.Styles.CellCursor.Render(" " + label + strings.Repeat(" ", max(0, gap)) + kind + " ")
		} else {
			row = base.Render(" "+label+strings.Repeat(" ", max(0, gap))) + dim.Render(kind+" ")
		}
		lines[y+i] = splice(lines[y+i], row, x, bw, w)
	}
}

// splice draws box (bw wide) over line (w wide) at column x.
func splice(line, box string, x, bw, w int) string {
	left := ansi.Truncate(line, x, "")
	if lw := ansi.StringWidth(left); lw < x {
		left += strings.Repeat(" ", x-lw)
	}
	right := ansi.TruncateLeft(line, x+bw, "")
	out := left + "\x1b[m" + box + right
	if ow := ansi.StringWidth(out); ow < w {
		out += strings.Repeat(" ", w-ow)
	}
	return out
}
