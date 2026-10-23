// Package keymap maps named actions to key bindings. The keymap is the single
// source of truth for the help overlay and the status bar hints.
package keymap

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Action names something the user can do.
type Action string

// Movement.
const (
	Up       Action = "up"
	Down     Action = "down"
	Left     Action = "left"
	Right    Action = "right"
	PageUp   Action = "page-up"
	PageDown Action = "page-down"
	Top      Action = "top"
	Bottom   Action = "bottom"
	Home     Action = "home"
	End      Action = "end"
)

// Panes and overlays.
const (
	FocusNext     Action = "focus-next"
	FocusPrev     Action = "focus-prev"
	Open          Action = "open"
	Back          Action = "back"
	Filter        Action = "filter"
	SidebarFilter Action = "sidebar-filter"
	Sort          Action = "sort"
)

// Grid.
const (
	WidenCol  Action = "widen-column"
	NarrowCol Action = "narrow-column"
	CopyCell  Action = "copy-cell"
	CopyRow   Action = "copy-row"
	// Inline editing of the browse tab.
	InsertRow Action = "insert-row"
	DeleteRow Action = "delete-row"
	// SetNull and SaveEdit act inside the cell editor only.
	SetNull  Action = "set-null"
	SaveEdit Action = "save-edit"
	// FindSimilar runs "find similar to this row" on a vector collection.
	// It is S, apart from sort's s.
	FindSimilar Action = "find-similar"
	// CollapseAll and ExpandAll act inside the row detail only, where the
	// result tab keys they share ([ and ]) do not apply.
	CollapseAll Action = "collapse-all"
	ExpandAll   Action = "expand-all"
	// Watch opens the live tail of the open collection in a new tab.
	// WatchPause and WatchClear act inside a live tail tab only.
	Watch      Action = "watch"
	WatchPause Action = "watch-pause"
	WatchClear Action = "watch-clear"
)

// App.
const (
	Palette Action = "palette"
	Help    Action = "help"
	Quit    Action = "quit"
	Cancel  Action = "cancel"
	Edit    Action = "edit"
	Ask     Action = "ask"
	// FixError sends a failed run to the AI panel, from its error tab.
	// It shares f with Filter, which acts on the browse tab only.
	FixError Action = "fix-error"
	// LoadProposal and CopyProposal act on the AI panel's latest proposed
	// query (RunStatement there loads and runs it). They take a modifier,
	// so typing a question never triggers them.
	LoadProposal Action = "load-proposal"
	CopyProposal Action = "copy-proposal"
	// Connection management, from the sidebar.
	ConnAdd  Action = "connection-add"
	ConnEdit Action = "connection-edit"
	ConnRm   Action = "connection-remove"
)

// Query editor and result tabs.
const (
	EditorToggle Action = "editor"
	RunStatement Action = "run-statement"
	RunBuffer    Action = "run-buffer"
	PrevTab      Action = "previous-tab"
	NextTab      Action = "next-tab"
	CloseTab     Action = "close-tab"
	// Complete opens the completion popup; it also opens on its own.
	Complete Action = "complete"
	// HistSearch searches the query history incrementally.
	HistSearch Action = "history-search"
)

// groups lists every action once, in help-overlay order.
var groups = []struct {
	title   string
	actions []Action
}{
	{"Navigation", []Action{Up, Down, Left, Right, PageUp, PageDown, Top, Bottom, Home, End}},
	{"Panes", []Action{FocusNext, FocusPrev, Open, Back, Filter, SidebarFilter, Sort}},
	{"Grid", []Action{WidenCol, NarrowCol, CopyCell, CopyRow, Edit, InsertRow, DeleteRow, SetNull, SaveEdit, FindSimilar, CollapseAll, ExpandAll, Watch, WatchPause, WatchClear}},
	{"App", []Action{Palette, Help, Quit, Cancel, Ask, FixError, LoadProposal, CopyProposal, ConnAdd, ConnEdit, ConnRm}},
	{"Editor", []Action{EditorToggle, RunStatement, RunBuffer, PrevTab, NextTab, CloseTab, Complete, HistSearch}},
}

// GroupTitles returns the help group titles, parallel to Map.Help().
func GroupTitles() []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = g.title
	}
	return out
}

// AllActions returns every action, in help order.
func AllActions() []Action {
	var out []Action
	for _, g := range groups {
		out = append(out, g.actions...)
	}
	return out
}

var descriptions = map[Action]string{
	Up: "move up", Down: "move down", Left: "move left", Right: "move right",
	PageUp: "page up", PageDown: "page down", Top: "go to top", Bottom: "go to bottom",
	Home: "first column", End: "last column",
	FocusNext: "next pane", FocusPrev: "previous pane", Open: "open / row detail",
	Back: "back / close; in the grid, focus the sidebar", Filter: "filter rows", SidebarFilter: "filter sidebar", Sort: "cycle sort",
	WidenCol: "widen column", NarrowCol: "narrow column", CopyCell: "copy cell", CopyRow: "copy row as JSON",
	Palette: "command palette", Help: "help", Quit: "quit", Cancel: "cancel a run, :apply, export or load (waits on a commit or rollback) / quit", Edit: "edit cell",
	Ask: "open the AI panel / close it", FixError: "fix a failed run with AI (on its error tab)",
	LoadProposal: "AI panel: load the proposed query into the editor (ctrl+enter / f5 also runs it)", CopyProposal: "AI panel: copy the proposed query",
	InsertRow: "insert a row", DeleteRow: "mark / unmark the row for deletion",
	SetNull: "set the edited cell to NULL", SaveEdit: "save a multi-line cell edit",
	FindSimilar: "find rows similar to this one (vector collections)",
	CollapseAll: "row detail: collapse every node", ExpandAll: "row detail: expand every node",
	Watch: "live tail the open collection (drivers with watch support)", WatchPause: "live tail: pause / resume",
	WatchClear: "live tail: clear the events",
	ConnAdd:    "add connection", ConnEdit: "edit connection", ConnRm: "delete connection",
	EditorToggle: "open / close the query editor", RunStatement: "run the statement or selection",
	RunBuffer: "run the whole buffer", PrevTab: "previous result tab", NextTab: "next result tab",
	CloseTab: "close result tab", Complete: "complete the name at the cursor",
	HistSearch: "search the query history",
}

// SeqWindow is how long the first key of a two-key sequence waits for its
// second.
const SeqWindow = 500 * time.Millisecond

// Map binds actions to keys. A key written "g g" is a two-key sequence: two
// presses in a row, the second within SeqWindow of the first.
type Map struct {
	Name     string
	bindings map[Action]key.Binding
	seqs     map[Action][][2]string
}

// Seq is the first key of a two-key sequence that is waiting for its second.
// The zero value is nothing pending. A component keeps one next to its
// state and feeds every key it handles through Map.Step.
type Seq struct {
	first string
	at    time.Time
}

// Step feeds key press k, at time now, through the map's two-key sequences.
// It returns the new pending state and whether the key was consumed by a
// sequence: as its first key (act is empty, wait for the next press) or as
// the second one (act is the completed action). A key that is not part of a
// sequence, or whose first half has expired, is not consumed and clears the
// pending state, so a stray first key never changes what the next one does.
func (m Map) Step(s Seq, k tea.KeyPressMsg, now time.Time) (next Seq, act Action, consumed bool) {
	ks := k.String()
	if s.first != "" && now.Sub(s.at) <= SeqWindow {
		for a, pairs := range m.seqs {
			for _, p := range pairs {
				if p[0] == s.first && p[1] == ks {
					return Seq{}, a, true
				}
			}
		}
	}
	for _, pairs := range m.seqs {
		for _, p := range pairs {
			if p[0] == ks {
				return Seq{first: ks, at: now}, "", true
			}
		}
	}
	return Seq{}, "", false
}

// Binding returns the binding for an action (zero Binding if unknown).
func (m Map) Binding(a Action) key.Binding { return m.bindings[a] }

// Matches reports whether msg triggers action a.
func (m Map) Matches(msg tea.KeyPressMsg, a Action) bool {
	b, ok := m.bindings[a]
	return ok && key.Matches(msg, b)
}

// Help returns the bindings grouped for the help overlay (see GroupTitles).
func (m Map) Help() [][]key.Binding {
	out := make([][]key.Binding, len(groups))
	for i, g := range groups {
		for _, a := range g.actions {
			out[i] = append(out[i], m.bindings[a])
		}
	}
	return out
}

func newMap(name string, keys map[Action][]string) Map {
	m := Map{Name: name, bindings: make(map[Action]key.Binding, len(keys)), seqs: map[Action][][2]string{}}
	for a, ks := range keys {
		shown := make([]string, len(ks))
		for i, k := range ks {
			shown[i] = k
			if first, second, ok := strings.Cut(k, " "); ok {
				m.seqs[a] = append(m.seqs[a], [2]string{first, second})
				shown[i] = first + second
			}
		}
		m.bindings[a] = key.NewBinding(
			key.WithKeys(ks...),
			key.WithHelp(strings.Join(shown, "/"), descriptions[a]),
		)
	}
	return m
}

func defaultKeys() map[Action][]string {
	return map[Action][]string{
		Up: {"up"}, Down: {"down"}, Left: {"left"}, Right: {"right"},
		PageUp: {"pgup"}, PageDown: {"pgdown"},
		Top: {"ctrl+home"}, Bottom: {"ctrl+end"}, Home: {"home"}, End: {"end"},
		FocusNext: {"tab"}, FocusPrev: {"shift+tab"}, Open: {"enter"}, Back: {"esc"},
		Filter: {"f"}, SidebarFilter: {"/"}, Sort: {"s"},
		WidenCol: {">"}, NarrowCol: {"<"}, CopyCell: {"y"}, CopyRow: {"Y"},
		Palette: {":"}, Help: {"f1", "?"}, Quit: {"q"}, Cancel: {"ctrl+c"},
		Edit: {"e"}, InsertRow: {"i"}, DeleteRow: {"d"}, SetNull: {"ctrl+n"}, SaveEdit: {"ctrl+s"},
		FindSimilar: {"S"}, CollapseAll: {"["}, ExpandAll: {"]"},
		Watch: {"W"}, WatchPause: {"p"}, WatchClear: {"c"},
		Ask: {"a"}, FixError: {"f"}, ConnAdd: {"n"}, ConnEdit: {"E"}, ConnRm: {"D"},
		LoadProposal: {"ctrl+o"}, CopyProposal: {"ctrl+y"},
		// ctrl+enter needs a terminal with the kitty keyboard protocol (which
		// tobira requests); f5 and shift+f5 work everywhere.
		EditorToggle: {"ctrl+e"}, RunStatement: {"ctrl+enter", "f5"}, RunBuffer: {"alt+enter", "shift+f5"},
		PrevTab: {"["}, NextTab: {"]"}, CloseTab: {"x"},
		// Both only act inside the editor, where ctrl+r and ctrl+space are
		// otherwise unbound.
		Complete: {"ctrl+space"}, HistSearch: {"ctrl+r"},
	}
}

// Default uses arrows and PgUp/PgDn only.
func Default() Map { return newMap("default", defaultKeys()) }

// Vim is Default plus hjkl, gg/G, ctrl+d and ctrl+u. gg is a two-key
// sequence; a single g does nothing.
func Vim() Map {
	k := defaultKeys()
	add := func(a Action, extra string) { k[a] = append(k[a], extra) }
	add(Up, "k")
	add(Down, "j")
	add(Left, "h")
	add(Right, "l")
	add(Top, "g g")
	add(Bottom, "G")
	add(PageDown, "ctrl+d")
	add(PageUp, "ctrl+u")
	return newMap("vim", k)
}

// Names lists the keymap names ByName accepts. internal/config keeps its own
// copy (it must not import the TUI); a test keeps the two in step.
func Names() []string { return []string{"default", "vim"} }

// ByName resolves a keymap name: default or vim.
func ByName(name string) (Map, error) {
	switch name {
	case "default":
		return Default(), nil
	case "vim":
		return Vim(), nil
	}
	return Map{}, fmt.Errorf("unknown keymap %q (want default or vim)", name)
}
