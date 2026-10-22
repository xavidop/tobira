package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/detail"
	"github.com/xavidop/tobira/internal/tui/grid"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// The live tail (:watch, W): a tab that streams a driver.Watcher's events,
// newest first, into a bounded ring. Each live tail tab holds exactly one
// watch; the watch's context is cancelled when the tab closes, is evicted,
// when the open collection moves to another connection, before a config
// write or reload that may close its connection, and on quit.

const (
	// watchKeep is how many events a live tail keeps; older ones drop.
	watchKeep = 1000
	// watchBatch is the most events one message delivers, so a busy stream
	// redraws once per batch rather than once per event.
	watchBatch = 256
	// timeLayout is how an event's time is shown.
	timeLayout = "15:04:05.000"
)

// noWatchText refuses :watch on a connection that cannot watch.
const noWatchText = "live tail needs a driver with watch support"

// maxEventPayload is the most bytes of a payload a live tail keeps; a
// longer one is cut and ends in truncatedMark.
const maxEventPayload = 64 << 10

// truncatedMark ends a payload cut to maxEventPayload.
const truncatedMark = "…(truncated)"

// tailEvent is an event as the live tail keeps it: redacted, its payload
// cut to maxEventPayload (truncated says so).
type tailEvent struct {
	driver.Event
	truncated bool
}

// eventRing keeps the last watchKeep events. The zero value is empty.
type eventRing struct {
	buf  []tailEvent
	head int // index of the oldest event
	n    int
}

func (r *eventRing) push(ev tailEvent) {
	if r.buf == nil {
		r.buf = make([]tailEvent, watchKeep)
	}
	if r.n < len(r.buf) {
		r.buf[(r.head+r.n)%len(r.buf)] = ev
		r.n++
		return
	}
	r.buf[r.head] = ev
	r.head = (r.head + 1) % len(r.buf)
}

func (r *eventRing) len() int { return r.n }

// newest returns the i-th newest event (0 is the newest).
func (r *eventRing) newest(i int) tailEvent {
	return r.buf[(r.head+r.n-1-i)%len(r.buf)]
}

func (r *eventRing) clear() {
	clear(r.buf)
	r.head, r.n = 0, 0
}

// watchState is a live tail tab's watch.
type watchState struct {
	id     uint64 // 0 once the watch has stopped: its messages are stale
	conn   string
	req    driver.WatchRequest
	cancel context.CancelFunc
	// starting is set until Watch returns; live while events may arrive.
	starting, live bool
	paused         bool
	ring           eventRing
	total          int // events received, cleared ones included
	// held is how many of the newest events a pause keeps off the screen;
	// cursor and off index the events shown, newest first.
	held, cursor, off int
	err               string // redacted: why the watch failed
	note              string // why it stopped, when something else stopped it
}

// shown is how many events the tail shows: all but the held ones.
func (w *watchState) shown() int { return max(w.ring.len()-w.held, 0) }

// event returns the i-th event shown, newest first.
func (w *watchState) event(i int) tailEvent { return w.ring.newest(w.held + i) }

// watchStartedMsg is Watch's return: the stream, or why there is none.
type watchStartedMsg struct {
	id  uint64
	ch  <-chan driver.Event
	err error
}

// watchEventsMsg delivers a batch of events and carries the channel to
// listen on next; closed reports the stream ended after evs.
type watchEventsMsg struct {
	id     uint64
	evs    []driver.Event
	closed bool
	ch     <-chan driver.Event
}

// openWatch calls Watch off the Update goroutine.
func openWatch(id uint64, ctx context.Context, w driver.Watcher, req driver.WatchRequest) tea.Cmd {
	return func() tea.Msg {
		ch, err := w.Watch(ctx, req)
		return watchStartedMsg{id: id, ch: ch, err: err}
	}
}

// listenWatch waits for the next event, then takes whatever else is ready
// (up to watchBatch) without waiting. The driver closes the channel when
// the watch's ctx ends, which ends the chain.
func listenWatch(id uint64, ch <-chan driver.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return watchEventsMsg{id: id, closed: true}
		}
		evs := []driver.Event{ev}
		for len(evs) < watchBatch && ev.Err == nil {
			select {
			case ev, ok = <-ch:
				if !ok {
					return watchEventsMsg{id: id, evs: evs, closed: true}
				}
				evs = append(evs, ev)
				continue
			default:
			}
			break
		}
		return watchEventsMsg{id: id, evs: evs, ch: ch}
	}
}

// watchCmd starts a live tail: of the open collection, or of c.Target on
// its connection (the selected one when no collection is open). On a live
// tail tab it replaces that tab's watch; elsewhere it opens a new tab.
func (a *Model) watchCmd(c palette.WatchCmd) tea.Cmd {
	connName := a.cur.conn
	if !a.cur.set {
		connName = ""
		if c.Target != "" {
			connName, _ = a.sb.SelectedConn()
		}
	}
	if connName == "" {
		return a.flash("open a collection to watch it, or name a target: :watch <target>", false)
	}
	conn, ok := a.mgr.Conn(connName)
	if !ok {
		return a.flash("connection "+strconv.Quote(query.EscapeControls(connName))+" is not open", false)
	}
	w, ok := driver.As[driver.Watcher](conn)
	if !ok {
		return a.flash(noWatchText, false)
	}
	req, name := driver.WatchRequest{Channel: c.Target}, c.Target
	if c.Target == "" {
		ref := a.cur.ref
		req, name = driver.WatchRequest{Collection: &ref}, ref.Name
	}
	t := a.activeTab()
	if t == nil || t.watch == nil {
		t = a.addTab(connName, "", false)
		t.isText, t.state, t.noHistory = true, tabDone, true
		t.text = nil
	} else {
		a.stopWatch(t, "")
	}
	t.conn, t.stmt, t.label = connName, "watch "+name, tabLabel("watch "+name)
	a.watchSeq++
	ctx, cancel := context.WithCancel(context.Background())
	t.watch = &watchState{id: a.watchSeq, conn: connName, req: req, cancel: cancel, starting: true}
	t.state = tabDone
	a.dropDetail()
	a.focus = focusGrid
	return openWatch(a.watchSeq, ctx, w, req)
}

// watchTabOf returns the live tail tab whose watch has id.
func (a *Model) watchTabOf(id uint64) *resultTab {
	if id == 0 {
		return nil
	}
	for _, t := range a.tabs {
		if t.watch != nil && t.watch.id == id {
			return t
		}
	}
	return nil
}

func (a *Model) watchStarted(msg watchStartedMsg) tea.Cmd {
	t := a.watchTabOf(msg.id)
	if t == nil {
		return nil // stopped meanwhile: its ctx is cancelled, so the stream closes
	}
	w := t.watch
	w.starting = false
	if msg.err != nil {
		a.failWatch(t, msg.err)
		return nil
	}
	w.live = true
	return listenWatch(msg.id, msg.ch)
}

func (a *Model) watchEvents(msg watchEventsMsg) tea.Cmd {
	t := a.watchTabOf(msg.id)
	if t == nil {
		return nil
	}
	w := t.watch
	for _, ev := range msg.evs {
		if ev.Err != nil {
			a.failWatch(t, ev.Err)
			return nil
		}
		if ev.Time.IsZero() {
			ev.Time = a.clock()
		}
		w.ring.push(a.keepEvent(w.conn, ev))
		w.total++
		switch {
		case w.paused:
			w.held = min(w.held+1, w.ring.len())
		case w.cursor > 0: // keep the selected event selected
			w.cursor++
		}
	}
	w.cursor = clampInt(w.cursor, 0, max(w.shown()-1, 0))
	if msg.closed {
		a.endWatch(t, "the stream ended")
		return nil
	}
	return listenWatch(msg.id, msg.ch)
}

// failWatch ends t's watch with err, shown redacted in the pane.
func (a *Model) failWatch(t *resultTab, err error) {
	w := t.watch
	text := err.Error()
	if errors.Is(err, context.Canceled) {
		text = "cancelled"
	}
	a.endWatch(t, "")
	w.err = a.mgr.Redact(w.conn, text)
	t.state = tabFailed
}

// endWatch cancels t's watch and marks it stopped, keeping its events.
func (a *Model) endWatch(t *resultTab, note string) {
	w := t.watch
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	w.id, w.starting, w.live = 0, false, false
	if note != "" {
		w.note = note
	}
}

// stopWatch stops t's watch, if it runs, for the reason note ("" when the
// tab is going away or is about to watch something else).
func (a *Model) stopWatch(t *resultTab, note string) {
	if t == nil || t.watch == nil || (!t.watch.live && !t.watch.starting) {
		return
	}
	a.endWatch(t, note)
	if note != "" {
		t.state = tabCancelled
	}
}

// stopWatchesOn stops every watch on conn ("" for any) except on keep.
func (a *Model) stopWatchesOn(conn, keep, note string) {
	for _, t := range a.tabs {
		if t.watch != nil && (conn == "" || t.watch.conn == conn) && (keep == "" || t.watch.conn != keep) {
			a.stopWatch(t, note)
		}
	}
}

// unwatch closes the active live tail tab, or else the newest one.
func (a *Model) unwatch() tea.Cmd {
	idx := -1
	if t := a.activeTab(); t != nil && t.watch != nil {
		idx = a.active
	} else {
		for i := len(a.tabs) - 1; i >= 0; i-- {
			if a.tabs[i].watch != nil {
				idx = i + 1
				break
			}
		}
	}
	if idx < 0 {
		return a.flash("no live tail is open", false)
	}
	a.active = idx
	return a.closeTab()
}

// watchKey handles a key on a live tail tab; it reports whether it did.
func (a *Model) watchKey(t *resultTab, k tea.KeyPressMsg) (tea.Cmd, bool) {
	w := t.watch
	page := max(a.lay.grid.h-2, 1)
	switch {
	case a.km.Matches(k, keymap.Back):
		return a.closeTab(), true
	case a.km.Matches(k, keymap.WatchPause):
		if w.paused {
			w.cursor += w.held
			w.held, w.paused = 0, false
		} else {
			w.paused = true
		}
	case a.km.Matches(k, keymap.WatchClear):
		w.ring.clear()
		w.held, w.cursor, w.off = 0, 0, 0
	case a.km.Matches(k, keymap.Open):
		return a.openWatchDetail(t), true
	case a.km.Matches(k, keymap.Up):
		w.cursor--
	case a.km.Matches(k, keymap.Down):
		w.cursor++
	case a.km.Matches(k, keymap.PageUp):
		w.cursor -= page
	case a.km.Matches(k, keymap.PageDown):
		w.cursor += page
	case a.km.Matches(k, keymap.Top):
		w.cursor = 0
	case a.km.Matches(k, keymap.Bottom):
		w.cursor = w.shown()
	default:
		return nil, false
	}
	w.cursor = clampInt(w.cursor, 0, max(w.shown()-1, 0))
	return nil, true
}

// eventColumns are the fields of an event's detail.
var eventColumns = []driver.Column{
	{Name: "time", Type: driver.TypeTime}, {Name: "op", Type: driver.TypeString},
	{Name: "channel", Type: driver.TypeString}, {Name: "key", Type: driver.TypeObject},
	{Name: "payload"},
}

// openWatchDetail shows the selected event in the row detail.
func (a *Model) openWatchDetail(t *resultTab) tea.Cmd {
	w := t.watch
	if a.det != nil || w.shown() == 0 {
		return nil
	}
	ev := w.event(w.cursor)
	cols := append([]driver.Column(nil), eventColumns...)
	cols[4].Type = ev.Payload.Type()
	row := driver.Row{driver.Time(ev.Time), driver.String(ev.Op), driver.String(ev.Channel),
		driver.ObjectValue(ev.Key...), ev.Payload}
	if ev.truncated { // the detail says the payload is not whole
		cols = append(cols, driver.Column{Name: truncatedCol, Type: driver.TypeBool})
		row = append(row, driver.Bool(true))
	}
	d := detail.New(cols, row, a.th, a.km).Resize(a.lay.grid.w, a.lay.grid.h)
	a.det = &d
	return d.Init()
}

// keepEvent is ev as the live tail stores it: every text in it redacted
// with conn's secrets (as `tobira watch` redacts its lines), then its
// payload cut to maxEventPayload.
func (a *Model) keepEvent(conn string, ev driver.Event) tailEvent {
	red := func(s string) string { return a.mgr.Redact(conn, s) }
	ev.Op, ev.Channel = red(ev.Op), red(ev.Channel)
	if len(ev.Key) > 0 {
		key := make(driver.Object, len(ev.Key))
		for i, m := range ev.Key {
			key[i] = driver.Member{Key: red(m.Key), Value: redactValue(m.Value, red)}
		}
		ev.Key = key
	}
	ev.Payload = redactValue(ev.Payload, red)
	te := tailEvent{Event: ev}
	te.Payload, te.truncated = capPayload(ev.Payload)
	return te
}

// redactValue returns v with every string, object key and JSON text passed
// through red.
func redactValue(v driver.Value, red func(string) string) driver.Value {
	switch v.Type() {
	case driver.TypeString:
		s, _ := v.AsText()
		return driver.String(red(s))
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return driver.JSON([]byte(red(string(raw))))
	case driver.TypeList:
		list, _ := v.AsList()
		out := make([]driver.Value, len(list))
		for i, e := range list {
			out[i] = redactValue(e, red)
		}
		return driver.List(out...)
	case driver.TypeObject:
		obj, _ := v.AsObject()
		out := make([]driver.Member, len(obj))
		for i, m := range obj {
			out[i] = driver.Member{Key: red(m.Key), Value: redactValue(m.Value, red)}
		}
		return driver.ObjectValue(out...)
	}
	return v
}

// capPayload cuts a payload larger than maxEventPayload: a string to its
// first maxEventPayload bytes, anything else to that much of its JSON, as
// a string ending in truncatedMark. It reports whether it cut.
func capPayload(v driver.Value) (driver.Value, bool) {
	var text string
	switch v.Type() {
	case driver.TypeString:
		s, _ := v.AsText()
		if len(s) <= maxEventPayload {
			return v, false
		}
		text = s
	case driver.TypeObject, driver.TypeList, driver.TypeJSON, driver.TypeBytes, driver.TypeUnknown:
		b, _ := v.MarshalJSON()
		if len(b) <= maxEventPayload {
			return v, false
		}
		text = string(b)
	default:
		return v, false
	}
	n := maxEventPayload
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return driver.String(text[:n] + truncatedMark), true
}

// watchSuffix is a live tail's state for its tab label.
func watchSuffix(w *watchState) string {
	switch {
	case w.err != "":
		return " ✗"
	case w.starting:
		return " …"
	case w.live && w.paused:
		return " paused"
	case w.live:
		return " live"
	}
	return " stopped"
}

// watchTitle is a live tail tab's pane title.
func (a *Model) watchTitle(t *resultTab) string {
	w := t.watch
	title := query.EscapeControls(t.conn) + " ▸ " + cutLabel(t.stmt, 40)
	switch {
	case w.starting:
		return title + " · starting…"
	case w.err != "":
		title += " · failed"
	case w.live && w.paused:
		title += " · paused"
		if w.held > 0 {
			title += " · " + commas(w.held) + " new"
		}
	case w.live:
		title += " · live"
	default:
		title += " · stopped"
	}
	title += " · " + commas(w.total) + " " + plural(w.total, "event", "events")
	if w.total > watchKeep && w.ring.len() == watchKeep {
		title += " (last " + commas(watchKeep) + " kept)"
	}
	return title
}

// watchView draws a live tail tab's content in a w x h box: a header line,
// then the events newest first, under any error or note.
func (a *Model) watchView(t *resultTab, width, h int) string {
	st := a.th.Styles
	w := t.watch
	var lines []string
	if w.err != "" {
		for _, l := range strings.Split(ansi.Wrap(query.EscapeControls(w.err), max(width, 1), ""), "\n") {
			lines = append(lines, st.Error.Render(l))
		}
	} else if w.note != "" && !w.live && !w.starting {
		lines = append(lines, st.Dim.Render(query.Truncate("stopped: "+query.EscapeControls(w.note), width)))
	}
	tw, ow := len(timeLayout), 8
	kw := clampInt(width/5, 8, 24)
	pw := max(width-tw-ow-kw-3, 1)
	row := func(tm, op, key, payload string) string {
		return padTo(query.Truncate(tm, tw), tw) + " " + padTo(query.Truncate(op, ow), ow) + " " +
			padTo(query.Truncate(key, kw), kw) + " " + query.Truncate(payload, pw)
	}
	lines = append(lines, st.Header.Render(query.Truncate(row("TIME", "OP", "KEY", "PAYLOAD"), width)))
	room := h - len(lines)
	n := w.shown()
	if n == 0 {
		switch {
		case w.starting:
			lines = append(lines, st.Dim.Render("starting the watch…"))
		case w.live && w.held > 0:
			lines = append(lines, st.Dim.Render("paused: press "+a.km.Binding(keymap.WatchPause).Help().Key+" to show the new events"))
		case w.live:
			lines = append(lines, st.Dim.Render("waiting for events…"))
		default:
			lines = append(lines, st.Dim.Render("no events"))
		}
		return strings.Join(lines, "\n")
	}
	if room < 1 {
		return strings.Join(lines, "\n")
	}
	// Keep the cursor in view.
	w.off = clampInt(w.off, max(w.cursor-room+1, 0), w.cursor)
	w.off = clampInt(w.off, 0, max(n-room, 0))
	loc := a.loc
	if loc == nil {
		loc = time.Local
	}
	for i := w.off; i < min(n, w.off+room); i++ {
		ev := w.event(i)
		text := row(ev.Time.In(loc).Format(timeLayout), query.EscapeControls(ev.Op),
			query.EscapeControls(eventKey(ev.Event)), query.EscapeControls(previewOf(ev.Payload, pw*4)))
		style := st.Cell
		if i == w.cursor && a.focus == focusGrid {
			style = st.CellCursor
		}
		lines = append(lines, style.Render(padTo(query.Truncate(text, width), width)))
	}
	return strings.Join(lines, "\n")
}

// eventKey is an event's key: its single key value, a preview of a
// compound key, or its channel when it has none.
func eventKey(ev driver.Event) string {
	switch len(ev.Key) {
	case 0:
		return ev.Channel
	case 1:
		return previewOf(ev.Key[0].Value, 256)
	}
	return grid.Preview(driver.ObjectValue(ev.Key...), 256)
}

// previewOf is a compact one-line preview of v from about budget bytes:
// an object or a list as {a: 1, …}, a scalar as its text. It is not
// escaped.
func previewOf(v driver.Value, budget int) string {
	switch v.Type() {
	case driver.TypeObject, driver.TypeList:
		return grid.Preview(v, budget)
	}
	s := detail.CellText(v)
	if len(s) > budget {
		for budget > 0 && !utf8.RuneStart(s[budget]) {
			budget--
		}
		s = s[:budget] + "…"
	}
	return s
}

// padTo pads s with spaces to n columns.
func padTo(s string, n int) string {
	return s + strings.Repeat(" ", max(n-query.DisplayWidth(s), 0))
}
