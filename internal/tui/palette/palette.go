// Package palette is the ":" command line. It parses commands into typed
// messages; the root model decides what each one does.
package palette

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

const prompt = ":"

// Typed command messages emitted when the user presses enter.
type (
	// QuitCmd is :q / :quit.
	QuitCmd struct{}
	// ConnectCmd is :connect <name>.
	ConnectCmd struct{ Name string }
	// ThemeCmd is :theme <auto|tobira-dark|tobira-light>.
	ThemeCmd struct{ Name string }
	// KeymapCmd is :keymap <default|vim>.
	KeymapCmd struct{ Name string }
	// HelpCmd is :help.
	HelpCmd struct{}
	// ReloadCmd is :reload.
	ReloadCmd struct{}
	// ConnAddCmd is :connection add.
	ConnAddCmd struct{}
	// ConnEditCmd is :connection edit <name>.
	ConnEditCmd struct{ Name string }
	// ConnRmCmd is :connection rm <name>.
	ConnRmCmd struct{ Name string }
	// EditorCmd is :editor: open (or focus) the query editor.
	EditorCmd struct{}
	// ExplainCmd is :explain: explain the editor's current statement.
	ExplainCmd struct{}
	// ApplyCmd is :apply: write the pending inline edits.
	ApplyCmd struct{}
	// DiscardCmd is :discard: drop the pending inline edits.
	DiscardCmd struct{}
	// SetLimitCmd is :set limit <n>: the row limit of editor runs.
	SetLimitCmd struct{ N int }
	// TxCmd is :tx begin|commit|rollback.
	TxCmd struct{ Op string }
	// ReadOnlyCmd is :readonly on|off.
	ReadOnlyCmd struct{ On bool }
	// ExportCmd is :export csv|json|ndjson <path>. Path is as typed, "~"
	// included; spaces inside it are kept.
	ExportCmd struct {
		Format query.Format
		Path   string
	}
	// AskCmd is :ask [question] (ask mode) or :ask! [question] (answer
	// mode). An empty Question only opens the panel.
	AskCmd struct {
		Question string
		Answer   bool
	}
	// ExplainResultsCmd is :explain-results: explain the result tab.
	ExplainResultsCmd struct{}
	// AIExecuteCmd is :ai execute on|off.
	AIExecuteCmd struct{ On bool }
	// ChartCmd is :chart <field> [window] (Window 0: chosen from the range
	// and the pane width) or :chart off.
	ChartCmd struct {
		Field  string
		Window time.Duration
		Off    bool
	}
	// RangeCmd is :range <span> (the last Last, up to now) or :range
	// <start>..<end> (End exclusive).
	RangeCmd struct {
		Last       time.Duration
		Start, End time.Time
	}
	// SimilarCmd is :similar <text>: the rows nearest to text, embedded by
	// the host's embedder.
	SimilarCmd struct{ Text string }
	// WatchCmd is :watch [target]: the live tail of the open collection,
	// or of a driver-specific target (a PostgreSQL LISTEN channel, a
	// MongoDB database, a Redis key pattern).
	WatchCmd struct{ Target string }
	// UnwatchCmd is :unwatch: close the live tail.
	UnwatchCmd struct{}
	// Unavailable is a command that exists but arrives in a later milestone.
	Unavailable struct{ Feature, Milestone string }
	// CancelledMsg is emitted on esc.
	CancelledMsg struct{}
)

// commands lists every command name, for completion and the unknown-command
// error.
var commands = []string{
	"ai", "apply", "ask", "ask!", "chart", "connect", "connection", "discard", "editor", "explain", "explain-results", "export",
	"help", "keymap", "q", "quit", "range", "readonly", "reload", "set", "similar", "theme", "tx", "unwatch", "watch",
}

// unavailable maps a command that exists but has not shipped yet to the
// milestone it arrives in. Every command has shipped.
var unavailable = map[string]string{}

// Command is one command's help: its usage and what it does.
type Command struct{ Usage, Desc string }

// Commands lists every command for the help overlay, in a stable order.
func Commands() []Command {
	return []Command{
		{":editor", "open the query editor"},
		{":explain", "explain the statement under the cursor"},
		{":set limit <n>", "row limit of editor runs (1–100000)"},
		{":apply", "write the pending inline edits"},
		{":discard", "drop the pending inline edits"},
		{":tx begin|commit|rollback", "a transaction for editor runs and :apply"},
		{":readonly on|off", "turn read-only on or off for this connection"},
		{":export csv|json|ndjson <path>", "export the browse collection or the result tab"},
		{":chart <field> [window] | off", "chart a time-series field below the grid, or close the chart"},
		{":range <6h> | <start>..<end>", "the time range of the chart (default the last 1h)"},
		{":similar <text>", "rows similar to text, in a new tab (needs ai.embedder)"},
		{":watch [target]", "live tail the open collection, or a channel, database or key pattern, in a new tab"},
		{":unwatch", "close the live tail and stop watching"},
		{":connect <name>", "expand a connection"},
		{":connection add | edit <name> | rm <name>", "manage connections"},
		{":reload", "reload the config and the collection"},
		{":theme <auto|tobira-dark|tobira-light>", "switch theme"},
		{":keymap <default|vim>", "switch keymap"},
		{":ask <question>", "ask the AI for a query (opens the AI panel)"},
		{":ask! <question>", "ask the AI for an answer; it may run read-only queries"},
		{":explain-results", "ask the AI to explain the result tab"},
		{":ai execute on|off", "let AI answers run writes, each one confirmed (off by default)"},
		{":help", "this help"},
		{":q", "quit (also :quit)"},
	}
}

const (
	txUsage     = "tx begin|commit|rollback"
	roUsage     = "readonly on|off"
	aiUsage     = "ai execute on|off"
	exportUsage = "export csv|json|ndjson <path>"
	chartUsage  = "chart <field> [window] | chart off"
	rangeUsage  = "range <span like 6h or 7d> | range <start>..<end>"
)

// MaxLimit is the highest row limit :set limit accepts. There is no
// unlimited mode in the TUI, which keeps the memory a result holds bounded.
const MaxLimit = 100000

func usage(s string) error { return errors.New("usage: " + s) }

// Parse turns a command line (with or without the leading ":") into its
// typed message.
func Parse(line string) (tea.Msg, error) {
	f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), prompt))
	if len(f) == 0 {
		return nil, errors.New("empty command")
	}
	cmd, args := f[0], f[1:]
	one := func(u string) (string, error) {
		if len(args) != 1 {
			return "", usage(u)
		}
		return args[0], nil
	}
	switch cmd {
	case "q", "quit", "help", "reload", "editor", "explain", "apply", "discard":
		if len(args) != 0 {
			return nil, usage(cmd)
		}
		switch cmd {
		case "help":
			return HelpCmd{}, nil
		case "reload":
			return ReloadCmd{}, nil
		case "editor":
			return EditorCmd{}, nil
		case "explain":
			return ExplainCmd{}, nil
		case "apply":
			return ApplyCmd{}, nil
		case "discard":
			return DiscardCmd{}, nil
		}
		return QuitCmd{}, nil
	case "set":
		return parseSet(args)
	case "connect":
		n, err := one("connect <name>")
		if err != nil {
			return nil, err
		}
		return ConnectCmd{Name: n}, nil
	case "theme":
		n, err := one("theme <auto|tobira-dark|tobira-light>")
		if err != nil {
			return nil, err
		}
		if _, err := theme.ByName(n, true); err != nil {
			return nil, err
		}
		return ThemeCmd{Name: n}, nil
	case "keymap":
		n, err := one("keymap <default|vim>")
		if err != nil {
			return nil, err
		}
		if _, err := keymap.ByName(n); err != nil {
			return nil, err
		}
		return KeymapCmd{Name: n}, nil
	case "connection":
		return parseConnection(args)
	case "tx":
		if len(args) != 1 || (args[0] != "begin" && args[0] != "commit" && args[0] != "rollback") {
			return nil, usage(txUsage)
		}
		return TxCmd{Op: args[0]}, nil
	case "readonly":
		if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
			return nil, usage(roUsage)
		}
		return ReadOnlyCmd{On: args[0] == "on"}, nil
	case "export":
		return parseExport(line)
	case "ask", "ask!":
		return AskCmd{Question: rawArgs(line, cmd), Answer: cmd == "ask!"}, nil
	case "explain-results":
		if len(args) != 0 {
			return nil, usage(cmd)
		}
		return ExplainResultsCmd{}, nil
	case "chart":
		return parseChart(args)
	case "range":
		return parseRange(args)
	case "similar":
		text := rawArgs(line, cmd)
		if text == "" {
			return nil, usage("similar <text>")
		}
		return SimilarCmd{Text: text}, nil
	case "watch":
		if len(args) > 1 {
			return nil, usage("watch [target]")
		}
		if len(args) == 1 {
			return WatchCmd{Target: args[0]}, nil
		}
		return WatchCmd{}, nil
	case "unwatch":
		if len(args) != 0 {
			return nil, usage(cmd)
		}
		return UnwatchCmd{}, nil
	case "ai":
		if len(args) != 2 || args[0] != "execute" || (args[1] != "on" && args[1] != "off") {
			return nil, usage(aiUsage)
		}
		return AIExecuteCmd{On: args[1] == "on"}, nil
	}
	if ms, ok := unavailable[cmd]; ok {
		return Unavailable{Feature: cmd, Milestone: ms}, nil
	}
	return nil, fmt.Errorf("unknown command %q (valid: %s)", cmd, strings.Join(commands, ", "))
}

// rawArgs is the line after its command word, trimmed, with its inner
// spacing kept: a question is passed on as typed.
func rawArgs(line, cmd string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), prompt))
	return strings.TrimSpace(strings.TrimPrefix(rest, cmd))
}

// parseExport reads :export <format> <path> from the raw line, so a path
// keeps its inner spaces.
func parseExport(line string) (tea.Msg, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), prompt))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "export"))
	format, path, _ := strings.Cut(rest, " ")
	path = strings.TrimSpace(path)
	switch query.Format(format) {
	case query.FormatCSV, query.FormatJSON, query.FormatNDJSON:
	default:
		return nil, usage(exportUsage)
	}
	if path == "" {
		return nil, usage(exportUsage)
	}
	return ExportCmd{Format: query.Format(format), Path: path}, nil
}

func parseChart(args []string) (tea.Msg, error) {
	switch {
	case len(args) == 1 && args[0] == "off":
		return ChartCmd{Off: true}, nil
	case len(args) == 1:
		return ChartCmd{Field: args[0]}, nil
	case len(args) == 2:
		w, err := ParseSpan(args[1])
		if err != nil {
			return nil, fmt.Errorf("window %q: want a positive span like 30s, 5m, 1h or 1d", args[1])
		}
		return ChartCmd{Field: args[0], Window: w}, nil
	}
	return nil, usage(chartUsage)
}

// rangeLayouts are the times :range accepts, read as UTC unless they carry
// an offset.
var rangeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04", "2006-01-02"}

func parseRange(args []string) (tea.Msg, error) {
	if len(args) != 1 {
		return nil, usage(rangeUsage)
	}
	if from, to, ok := strings.Cut(args[0], ".."); ok {
		start, err1 := parseTime(from)
		end, err2 := parseTime(to)
		if err1 != nil || err2 != nil {
			return nil, usage(rangeUsage)
		}
		if !start.Before(end) {
			return nil, errors.New("the range start must be before its end")
		}
		return RangeCmd{Start: start, End: end}, nil
	}
	d, err := ParseSpan(args[0])
	if err != nil {
		return nil, usage(rangeUsage)
	}
	return RangeCmd{Last: d}, nil
}

func parseTime(s string) (time.Time, error) {
	for _, l := range rangeLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("not a time")
}

// ParseSpan reads a positive duration: Go's form (90s, 5m, 1h30m) or a
// whole number of days (7d).
func ParseSpan(s string) (time.Duration, error) {
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days > 100000 {
			return 0, errors.New("not a span")
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, err
		}
	}
	if d <= 0 {
		return 0, errors.New("not a positive span")
	}
	return d, nil
}

func parseSet(args []string) (tea.Msg, error) {
	if len(args) != 2 || args[0] != "limit" {
		return nil, usage("set limit <n>")
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 || n > MaxLimit {
		return nil, fmt.Errorf("limit must be a whole number from 1 to %d", MaxLimit)
	}
	return SetLimitCmd{N: n}, nil
}

func parseConnection(args []string) (tea.Msg, error) {
	const u = "connection add | edit <name> | rm <name>"
	if len(args) == 0 {
		return nil, usage(u)
	}
	switch args[0] {
	case "add":
		if len(args) != 1 {
			return nil, usage("connection add")
		}
		return ConnAddCmd{}, nil
	case "edit":
		if len(args) != 2 {
			return nil, usage("connection edit <name>")
		}
		return ConnEditCmd{Name: args[1]}, nil
	case "rm":
		if len(args) != 2 {
			return nil, usage("connection rm <name>")
		}
		return ConnRmCmd{Name: args[1]}, nil
	}
	return nil, usage(u)
}

// Model is the command line. While focused it swallows every key.
type Model struct {
	th  theme.Theme
	km  keymap.Map
	in  textinput.Model
	err string
	w   int
}

// New returns a closed palette.
func New(th theme.Theme, km keymap.Map) Model {
	in := textinput.New()
	in.Prompt = prompt
	// A steady cursor: blinking would keep a timer command alive.
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	return Model{th: th, km: km, in: in}
}

// Resize sets the bar's width.
func (m Model) Resize(w int) Model {
	m.w = w
	m.in.SetWidth(max(w-len(prompt)-1, 1))
	return m
}

// SetTheme restyles the bar in place, keeping its text and focus.
func (m Model) SetTheme(th theme.Theme) Model { m.th = th; return m }

// SetKeymap swaps the key bindings in place.
func (m Model) SetKeymap(km keymap.Map) Model { m.km = km; return m }

// Open focuses an empty palette.
func (m Model) Open() Model {
	m.err = ""
	m.in.SetValue("")
	m.in.CursorEnd()
	m.in.Focus()
	return m
}

// Focused reports whether the palette is open.
func (m Model) Focused() bool { return m.in.Focused() }

// Value is the text in the input.
func (m Model) Value() string { return m.in.Value() }

// complete extends the first word to the longest common prefix of the
// commands it prefixes, adding a space once it is a whole command.
func complete(s string) string {
	if s == "" || strings.ContainsAny(s, " \t") {
		return s
	}
	var matches []string
	for _, c := range commands {
		if strings.HasPrefix(c, s) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return s
	}
	sort.Strings(matches)
	pre := matches[0]
	for _, c := range matches[1:] {
		for !strings.HasPrefix(c, pre) {
			pre = pre[:len(pre)-1]
		}
	}
	if len(matches) == 1 {
		return pre + " "
	}
	return pre
}

// Update handles a key while the palette is open.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if !m.in.Focused() {
		return m, nil
	}
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		return m, cmd
	}
	switch {
	case m.km.Matches(k, keymap.Back):
		m.in.Blur()
		m.err = ""
		return m, func() tea.Msg { return CancelledMsg{} }
	case m.km.Matches(k, keymap.Open):
		out, err := Parse(m.in.Value())
		if err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.in.Blur()
		m.err = ""
		return m, func() tea.Msg { return out }
	case m.km.Matches(k, keymap.FocusNext):
		m.in.SetValue(complete(m.in.Value()))
		m.in.CursorEnd()
		return m, nil
	}
	before := m.in.Value()
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(k)
	if m.in.Value() != before {
		m.err = ""
	}
	return m, cmd
}

// View renders the input line and, after a failed parse, a second line with
// the error. It is never wider than width.
func (m Model) View(width int) string {
	if width < 1 {
		return ""
	}
	line := ansi.Truncate(m.in.View(), width, "")
	if m.err == "" {
		return line
	}
	return line + "\n" + m.th.Styles.Error.Render(query.Truncate(query.EscapeControls(m.err), width))
}
