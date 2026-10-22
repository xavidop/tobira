package palette

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

func TestPaletteParse(t *testing.T) {
	tests := []struct {
		in      string
		want    tea.Msg
		errPart string
	}{
		{"q", QuitCmd{}, ""},
		{":quit", QuitCmd{}, ""},
		{"  quit  ", QuitCmd{}, ""},
		{"connect pg", ConnectCmd{Name: "pg"}, ""},
		{"connect", nil, "usage: connect <name>"},
		{"connect a b", nil, "usage: connect <name>"},
		{"theme tobira-light", ThemeCmd{Name: "tobira-light"}, ""},
		{"theme auto", ThemeCmd{Name: "auto"}, ""},
		{"theme neon", nil, "unknown theme"},
		{"theme", nil, "usage: theme"},
		{"keymap vim", KeymapCmd{Name: "vim"}, ""},
		{"keymap emacs", nil, "unknown keymap"},
		{"keymap", nil, "usage: keymap"},
		{"help", HelpCmd{}, ""},
		{"reload", ReloadCmd{}, ""},
		{"connection add", ConnAddCmd{}, ""},
		{"connection edit pg", ConnEditCmd{Name: "pg"}, ""},
		{"connection edit", nil, "usage: connection edit <name>"},
		{"connection rm pg", ConnRmCmd{Name: "pg"}, ""},
		{"connection rm", nil, "usage: connection rm <name>"},
		{"connection", nil, "usage: connection"},
		{"connection nope", nil, "usage: connection"},
		{"export csv out.csv", ExportCmd{Format: "csv", Path: "out.csv"}, ""},
		{"export json ~/a b.json", ExportCmd{Format: "json", Path: "~/a b.json"}, ""},
		{"export  ndjson   x.ndjson ", ExportCmd{Format: "ndjson", Path: "x.ndjson"}, ""},
		{"export csv", nil, "usage: export csv|json|ndjson <path>"},
		{"export", nil, "usage: export csv|json|ndjson <path>"},
		{"export table out.txt", nil, "usage: export csv|json|ndjson <path>"},
		{"explain", ExplainCmd{}, ""},
		{"explain now", nil, "usage: explain"},
		{"editor", EditorCmd{}, ""},
		{"apply", ApplyCmd{}, ""},
		{"apply now", nil, "usage: apply"},
		{"discard", DiscardCmd{}, ""},
		{"discard all", nil, "usage: discard"},
		{"editor x", nil, "usage: editor"},
		{"set limit 50", SetLimitCmd{N: 50}, ""},
		{"set limit 1", SetLimitCmd{N: 1}, ""},
		{"set limit 100000", SetLimitCmd{N: 100000}, ""},
		{"set limit 0", nil, "limit must be a whole number from 1 to 100000"},
		{"set limit 100001", nil, "limit must be a whole number from 1 to 100000"},
		{"set limit -3", nil, "limit must be a whole number from 1 to 100000"},
		{"set limit lots", nil, "limit must be a whole number from 1 to 100000"},
		{"set limit", nil, "usage: set limit <n>"},
		{"set color red", nil, "usage: set limit <n>"},
		{"q now", nil, "usage: q"},
		{"help me", nil, "usage: help"},
		{"reload x", nil, "usage: reload"},
		{"tx begin", TxCmd{Op: "begin"}, ""},
		{"tx commit", TxCmd{Op: "commit"}, ""},
		{"tx rollback", TxCmd{Op: "rollback"}, ""},
		{"tx", nil, "usage: tx begin|commit|rollback"},
		{"tx start", nil, "usage: tx begin|commit|rollback"},
		{"tx begin now", nil, "usage: tx begin|commit|rollback"},
		{"readonly on", ReadOnlyCmd{On: true}, ""},
		{"readonly off", ReadOnlyCmd{On: false}, ""},
		{"readonly", nil, "usage: readonly on|off"},
		{"readonly yes", nil, "usage: readonly on|off"},
		{"ask why", AskCmd{Question: "why"}, ""},
		{"ask   why  not?  ", AskCmd{Question: "why  not?"}, ""},
		{":ask", AskCmd{}, ""},
		{"ask! how many?", AskCmd{Question: "how many?", Answer: true}, ""},
		{"ask!", AskCmd{Answer: true}, ""},
		{"explain-results", ExplainResultsCmd{}, ""},
		{"explain-results now", nil, "usage: explain-results"},
		{"ai execute on", AIExecuteCmd{On: true}, ""},
		{"ai execute off", AIExecuteCmd{On: false}, ""},
		{"ai execute", nil, "usage: ai execute on|off"},
		{"ai", nil, "usage: ai execute on|off"},
		{"frobnicate", nil, "unknown command"},
		{"", nil, "empty command"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Parse(tt.in)
			if tt.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errPart) {
					t.Fatalf("err = %v, want containing %q", err, tt.errPart)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestNoPlaceholders: every command is real; none answers with a
// milestone.
func TestNoPlaceholders(t *testing.T) {
	for _, c := range commands {
		msg, _ := Parse(c)
		if u, ok := msg.(Unavailable); ok {
			t.Errorf("%s is still a placeholder (%s)", c, u.Milestone)
		}
	}
	for _, c := range Commands() {
		if strings.Contains(c.Desc, "(M4)") {
			t.Errorf("help still says %q", c.Desc)
		}
	}
}

// TestCommandHelpCoversEveryCommand: help lists every command with a usage.
func TestCommandHelpCoversEveryCommand(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Commands() {
		if c.Usage == "" || c.Desc == "" {
			t.Errorf("command help %+v is incomplete", c)
		}
		seen[strings.Fields(c.Usage)[0]] = true
	}
	for _, c := range commands {
		if c == "quit" {
			continue // listed with q
		}
		if !seen[":"+c] {
			t.Errorf("no help for :%s", c)
		}
	}
	for _, want := range []string{":tx begin|commit|rollback", ":readonly on|off", ":export csv|json|ndjson <path>",
		":ask <question>", ":ask! <question>", ":explain-results", ":ai execute on|off"} {
		found := false
		for _, c := range Commands() {
			found = found || c.Usage == want
		}
		if !found {
			t.Errorf("no help entry %q", want)
		}
	}
}

func TestUnknownListsValidCommands(t *testing.T) {
	_, err := Parse("zzz")
	for _, c := range []string{"quit", "connect", "theme", "keymap", "help", "reload", "connection"} {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("error %q does not list %q", err, c)
		}
	}
}

func typ(m Model, keys ...string) (Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		default:
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
		m, cmd = m.Update(msg)
	}
	return m, cmd
}

func typeText(m Model, s string) Model {
	for _, r := range s {
		m, _ = typ(m, string(r))
	}
	return m
}

func newModel() Model { return New(theme.Dark(), keymap.Default()).Resize(60).Open() }

func TestEnterEmitsCommand(t *testing.T) {
	m := typeText(newModel(), "connect pg")
	m, cmd := typ(m, "enter")
	if m.Focused() || cmd == nil {
		t.Fatalf("focused=%v cmd=%v", m.Focused(), cmd)
	}
	if got := cmd(); got != (ConnectCmd{Name: "pg"}) {
		t.Fatalf("got %#v", got)
	}
}

func TestEnterErrorStaysOpen(t *testing.T) {
	m := typeText(newModel(), "nope")
	m, cmd := typ(m, "enter")
	if !m.Focused() || cmd != nil {
		t.Fatalf("should stay open without cmd")
	}
	if v := m.View(60); !strings.Contains(v, "unknown command") {
		t.Fatalf("no inline error: %q", v)
	}
	m = typeText(m, "x")
	if v := m.View(60); strings.Contains(v, "unknown command") {
		t.Fatalf("error should clear on edit: %q", v)
	}
}

func TestEscCancels(t *testing.T) {
	m, cmd := typ(typeText(newModel(), "he"), "esc")
	if m.Focused() || cmd == nil {
		t.Fatal("esc should close")
	}
	if _, ok := cmd().(CancelledMsg); !ok {
		t.Fatalf("got %#v", cmd())
	}
}

func TestTabCompletes(t *testing.T) {
	tests := []struct{ in, want string }{
		{"con", "connect"}, // connect and connection share the prefix "connect"
		{"qu", "quit "},
		{"th", "theme "},
		{"key", "keymap "},
		{"rel", "reload "},
		{"zz", "zz"},
		{"theme au", "theme au"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			m, _ := typ(typeText(newModel(), tt.in), "tab")
			if m.Value() != tt.want {
				t.Fatalf("got %q, want %q", m.Value(), tt.want)
			}
		})
	}
}

func TestSmallWidthsDoNotPanic(t *testing.T) {
	m := typeText(newModel(), "connect")
	for w := -1; w < 12; w++ {
		_ = m.Resize(w).View(w)
	}
}

func TestPaletteParseKindCommands(t *testing.T) {
	day := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	tests := []struct {
		in      string
		want    tea.Msg
		errPart string
	}{
		{"chart usage_user", ChartCmd{Field: "usage_user"}, ""},
		{"chart usage_user 5m", ChartCmd{Field: "usage_user", Window: 5 * time.Minute}, ""},
		{"chart cpu 1d", ChartCmd{Field: "cpu", Window: 24 * time.Hour}, ""},
		{"chart off", ChartCmd{Off: true}, ""},
		{"chart", nil, "usage: chart <field> [window] | chart off"},
		{"chart a 5m x", nil, "usage: chart <field> [window] | chart off"},
		{"chart a soon", nil, "window"},
		{"chart a 0s", nil, "window"},
		{"chart a -5m", nil, "window"},
		{"range 6h", RangeCmd{Last: 6 * time.Hour}, ""},
		{"range 7d", RangeCmd{Last: 7 * 24 * time.Hour}, ""},
		{"range 2026-10-01..2026-10-02", RangeCmd{Start: day("2026-10-01"), End: day("2026-10-02")}, ""},
		{"range 2026-10-01T10:00:00Z..2026-10-01T12:30:00Z", RangeCmd{
			Start: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)}, ""},
		{"range 2026-10-02..2026-10-01", nil, "before"},
		{"range 2026-10-01..", nil, "usage: range"},
		{"range never", nil, "usage: range"},
		{"range 0h", nil, "usage: range"},
		{"range", nil, "usage: range"},
		{"similar a red bicycle", SimilarCmd{Text: "a red bicycle"}, ""},
		{"similar   two  spaces ", SimilarCmd{Text: "two  spaces"}, ""},
		{"similar", nil, "usage: similar <text>"},
		{"watch", WatchCmd{}, ""},
		{"watch orders", WatchCmd{Target: "orders"}, ""},
		{"watch user:*", WatchCmd{Target: "user:*"}, ""},
		{"watch a b", nil, "usage: watch [target]"},
		{"unwatch", UnwatchCmd{}, ""},
		{"unwatch x", nil, "usage: unwatch"},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if tt.errPart != "" {
			if err == nil || !strings.Contains(err.Error(), tt.errPart) {
				t.Errorf("Parse(%q) err = %v, want containing %q", tt.in, err, tt.errPart)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Parse(%q) = %#v, %v; want %#v", tt.in, got, err, tt.want)
		}
	}
	help := map[string]bool{}
	for _, c := range Commands() {
		help[strings.Fields(c.Usage)[0]] = true
	}
	for _, c := range []string{":chart", ":range", ":similar", ":watch", ":unwatch"} {
		if !help[c] {
			t.Errorf("help does not list %s", c)
		}
	}
}
