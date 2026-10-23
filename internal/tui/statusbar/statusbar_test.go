package statusbar

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
	"github.com/xavidop/tobira/internal/tui/tuitest"
)

func newBar(plain bool) Model {
	return New(theme.Dark(), keymap.Default(), Options{Plain: plain})
}

func plainView(m Model, w int) string { return ansi.Strip(m.View(w)) }

func TestStatusBarContent(t *testing.T) {
	tests := []struct {
		name  string
		build func() Model
		want  []string
		not   []string
	}{
		{"breadcrumb and stats", func() Model {
			m := newBar(false).SetContext("pg", "public", "people")
			return m.SetStats(12, 1204, 0, 23*time.Millisecond)
		}, []string{"pg › public › people", "row 12 of 1,204", "23ms"}, []string{"🔒"}},
		{"estimate", func() Model {
			return newBar(false).SetStats(1, 200, 1204, 1200*time.Millisecond).SetPartial(true)
		}, []string{"row 1 of ~1,204", "1.2s"}, nil},
		{"sub-millisecond", func() Model {
			return newBar(false).SetStats(1, 5, 0, 300*time.Microsecond)
		}, []string{"<1ms"}, nil},
		{"read-only emoji", func() Model { return newBar(false).SetReadOnly(true) }, []string{"🔒"}, []string{"RO"}},
		{"read-only plain", func() Model { return newBar(true).SetReadOnly(true) }, []string{"RO", "*"}, []string{"🔒", "●"}},
		{"dot", func() Model { return newBar(false).SetStatus(session.Ready) }, []string{"●"}, nil},
		{"hints", func() Model { return newBar(false) }, []string{"help", "filter", "quit"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plainView(tt.build(), 100)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, w := range tt.not {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in %q", w, got)
				}
			}
		})
	}
}

func TestBreadcrumbTruncatesFromLeft(t *testing.T) {
	m := newBar(false).SetContext("connection-name", "namespace", "collection")
	got := plainView(m, 30)
	if !strings.Contains(got, "…") || !strings.Contains(got, "collection") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "connection-name") {
		t.Fatalf("left should be cut: %q", got)
	}
}

func TestNeverWiderThanWidth(t *testing.T) {
	m := newBar(false).SetContext("日本語接続", "ns", "コレクション").SetStats(12, 1204, 0, time.Second).SetReadOnly(true)
	m, _ = m.Flash("something quite long happened here", true)
	for w := -1; w < 150; w++ {
		v := m.View(w)
		if got := query.DisplayWidth(ansi.Strip(v)); got > max(w, 0) {
			t.Fatalf("width %d: view is %d wide: %q", w, got, ansi.Strip(v))
		}
		if strings.Contains(v, "\n") {
			t.Fatalf("width %d: multi-line", w)
		}
	}
}

func TestFlashExpires(t *testing.T) {
	m := newBar(false).SetStats(1, 5, 0, 5*time.Millisecond)
	m, cmd := m.Flash("copied cell", false)
	if cmd == nil {
		t.Fatal("Flash returned no tick cmd")
	}
	got := plainView(m, 100)
	if !strings.Contains(got, "copied cell") || strings.Contains(got, "row 1 of") {
		t.Fatalf("flash should replace the middle: %q", got)
	}
	// An older flash's expiry must not clear a newer flash.
	m1, _ := m.Flash("first", false)
	oldID := m1.flashID
	m2, _ := m1.Flash("second", true)
	m2, _ = m2.Update(FlashExpiredMsg{ID: oldID})
	if got := plainView(m2, 100); !strings.Contains(got, "second") {
		t.Fatalf("newer flash cleared by stale tick: %q", got)
	}
	m2, _ = m2.Update(FlashExpiredMsg{ID: m2.flashID})
	got = plainView(m2, 100)
	if strings.Contains(got, "second") || !strings.Contains(got, "row 1 of") {
		t.Fatalf("flash should expire: %q", got)
	}
}

func TestFlashEscapesControls(t *testing.T) {
	m, _ := newBar(false).Flash("bad\x1b]0;x\x07", true)
	tuitest.NoInjectedControls(t, m.View(100))
	if v := plainView(m, 100); !strings.Contains(v, `bad\x1b]0;x\x07`) {
		t.Fatalf("want escaped flash, got %q", v)
	}
}

func TestBreadcrumbEscaped(t *testing.T) {
	m := newBar(false).SetContext("a\x1b]0;x\x07b", "n\ns", "")
	tuitest.NoInjectedControls(t, m.View(100))
	if got := plainView(m, 100); !strings.Contains(got, `a\x1b]0;x\x07b › n s`) {
		t.Fatalf("want escaped breadcrumb, got %q", got)
	}
}

func TestPartialTotals(t *testing.T) {
	tests := []struct {
		name string
		m    Model
		want string
	}{
		{"unknown total, more rows may exist", newBar(false).SetStats(1, 200, 0, time.Millisecond).SetPartial(true), "row 1 of 200+"},
		{"estimate", newBar(false).SetStats(1, 200, 5000, time.Millisecond).SetPartial(true), "row 1 of ~5,000"},
		{"exact count beyond the loaded rows", newBar(false).SetStats(1, 200, 5000, time.Millisecond), "row 1 of 5,000"},
		{"exact and fully loaded", newBar(false).SetStats(1, 5, 5, time.Millisecond), "row 1 of 5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plainView(tt.m, 100); !strings.Contains(got, tt.want) {
				t.Errorf("%q does not contain %q", got, tt.want)
			}
		})
	}
}

func TestErrorFlashUsesWholeBar(t *testing.T) {
	long := "dial tcp 10.0.0.5:5432: connect: connection refused after 30 seconds of trying"
	m, _ := newBar(false).SetReadOnly(true).Flash(long, true)
	if got := plainView(m, 100); !strings.Contains(got, long) {
		t.Fatalf("error flash truncated at 100 cols: %q", got)
	}
	if got := plainView(m, 100); !strings.Contains(got, "🔒") || !strings.Contains(got, "●") {
		t.Fatalf("badge area lost: %q", got)
	}
	// A plain flash keeps the half-bar limit.
	n, _ := newBar(false).Flash(long, false)
	if got := plainView(n, 100); strings.Contains(got, long) {
		t.Fatalf("plain flash should stay truncated: %q", got)
	}
	// Never wider than the bar, however narrow.
	for _, w := range []int{1, 5, 12, 30, 60} {
		if got := m.View(w); query.DisplayWidth(ansi.Strip(got)) > w {
			t.Errorf("width %d: bar is %d wide", w, query.DisplayWidth(ansi.Strip(got)))
		}
	}
}

func TestDismissErrorOnlyClearsErrors(t *testing.T) {
	m, _ := newBar(false).Flash("boom", true)
	if got := plainView(m.DismissError(), 100); strings.Contains(got, "boom") {
		t.Fatalf("error flash survived DismissError: %q", got)
	}
	n, _ := newBar(false).Flash("copied cell", false)
	if got := plainView(n.DismissError(), 100); !strings.Contains(got, "copied cell") {
		t.Fatalf("plain flash was dismissed by a key: %q", got)
	}
}

func TestTruncLeftKeepsClustersWhole(t *testing.T) {
	family := "\U0001F468\u200D\U0001F469\u200D\U0001F467"
	// Width 3 holds the ellipsis plus one family (2 columns).
	if got, want := truncLeft("abc"+family, 3), "…"+family; got != want {
		t.Errorf("truncLeft = %q, want %q", got, want)
	}
	// An accented tail letter must keep its combining mark.
	if got, want := truncLeft("abcé", 2), "…é"; got != want {
		t.Errorf("truncLeft = %q, want %q", got, want)
	}
	if got := truncLeft("abc"+family, 2); got != "…" {
		t.Errorf("truncLeft = %q, want only the ellipsis when the cluster does not fit", got)
	}
}

func TestTruncLeftAgreesWithLayoutWidth(t *testing.T) {
	for _, in := range []string{"\U0001F600︎", "1️⃣", "กำ", "नमस्ते", "क्ष"} {
		for w := 1; w <= 8; w++ {
			got := truncLeft("abc"+in+in+in, w)
			if ansi.StringWidth(got) > w {
				t.Errorf("truncLeft(%q, %d) = %q is %d wide", in, w, got, ansi.StringWidth(got))
			}
		}
	}
}

func TestEditingHintsShowF5(t *testing.T) {
	m := newBar(false).SetPane(PaneEditor)
	got := plainView(m, 140)
	for _, want := range []string{"f5 run", "shift+f5 buffer", "ctrl+space complete", "ctrl+r history", "esc grid", "ctrl+e close"} {
		if !strings.Contains(got, want) {
			t.Fatalf("editing hints missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "f filter") {
		t.Fatalf("grid hint shown while editing: %q", got)
	}
	if got := plainView(m.SetPane(PaneGrid), 140); !strings.Contains(got, "f filter") {
		t.Fatalf("grid hints not restored: %q", got)
	}
}

// The hints follow the focused pane, so a newcomer sees the keys that act
// where they are without opening the help.
func TestHintsFollowThePane(t *testing.T) {
	tests := []struct {
		name string
		pane Pane
		want []string
		not  []string
	}{
		{"sidebar", PaneSidebar, []string{"f1 help", "enter open", "/ filter", "n add", ": cmd", "q quit"}, []string{"f filter", "s sort"}},
		{"grid", PaneGrid, []string{"f1 help", "enter detail", "esc sidebar", "f filter", "s sort", "e edit", "y copy", "ctrl+e query", ": cmd", "q quit"}, []string{"n add", "a ask"}},
		{"ai", PaneAI, []string{"enter send", "ctrl+t mode", "ctrl+o load", "ctrl+y copy", "esc grid"}, []string{"f filter", "q quit"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plainView(newBar(false).SetPane(tt.pane), 160)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, w := range tt.not {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in %q", w, got)
				}
			}
		})
	}
}

// The ask hint only advertises the assistant when one is configured.
func TestAskHintNeedsAnAssistant(t *testing.T) {
	m := newBar(false).SetPane(PaneGrid)
	if got := plainView(m, 160); strings.Contains(got, "a ask") {
		t.Fatalf("ask hint shown without an assistant: %q", got)
	}
	if got := plainView(m.SetAI(true), 160); !strings.Contains(got, "a ask") {
		t.Fatalf("ask hint missing with an assistant: %q", got)
	}
}

// A narrow bar drops the hints from the end, so the ones a newcomer needs
// most stay: help, then what enter does here. Only those two may cut the
// breadcrumb; the rest never do.
func TestNarrowBarKeepsTheFirstHints(t *testing.T) {
	m := newBar(false).SetPane(PaneGrid).SetContext("local", "main", "tobira_people").
		SetStats(12, 1204, 0, 23*time.Millisecond)
	got := plainView(m, 80)
	for _, want := range []string{"local › main › tobira_people", "f1 help", "enter detail", "row 12 of 1,204"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "q quit") {
		t.Errorf("the last hint survived a narrow bar: %q", got)
	}
	if ansi.StringWidth(got) > 80 {
		t.Errorf("bar is %d wide: %q", ansi.StringWidth(got), got)
	}
	got = plainView(m, 50)
	for _, want := range []string{"…", "f1 help", "row 12 of 1,204"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "f filter") {
		t.Errorf("a third hint cut the breadcrumb: %q", got)
	}
}

func TestPendingCount(t *testing.T) {
	m := newBar(false).SetContext("local", "main", "people").SetReadOnly(true).SetPending(3)
	v := plainView(m, 100)
	if !strings.Contains(v, "3 pending") {
		t.Fatalf("no pending count: %q", v)
	}
	// It survives a flash: the count is not part of the middle section.
	m, _ = m.Flash("saved", false)
	if v := plainView(m, 100); !strings.Contains(v, "3 pending") || !strings.Contains(v, "saved") {
		t.Fatalf("pending hidden by a flash: %q", v)
	}
	if v := plainView(m.SetPending(0), 100); strings.Contains(v, "pending") {
		t.Fatalf("zero pending shown: %q", v)
	}
	for w := 1; w < 40; w++ {
		if got := ansi.StringWidth(m.View(w)); got > w {
			t.Fatalf("width %d: view is %d wide", w, got)
		}
	}
}

func TestTxBadge(t *testing.T) {
	m := newBar(false).SetContext("local", "", "").SetPending(2).SetReadOnly(true).SetTx(true)
	v := plainView(m, 100)
	if !strings.Contains(v, "2 pending  TX 🔒") {
		t.Fatalf("TX not between pending and the badge: %q", v)
	}
	// TX is drawn in the theme's accent colour (#5EEAD4).
	if raw := m.View(100); !regexp.MustCompile(`38;2;94;234;212[;0-9]*mTX`).MatchString(raw) {
		t.Fatalf("TX not in the accent colour: %q", raw)
	}
	m, _ = m.Flash("committed", false)
	if v := plainView(m, 100); !strings.Contains(v, "TX") {
		t.Fatalf("TX hidden by a flash: %q", v)
	}
	if v := plainView(m.SetTx(false), 100); strings.Contains(v, "TX") {
		t.Fatalf("TX shown with no transaction: %q", v)
	}
	for w := 1; w < 40; w++ {
		if got := ansi.StringWidth(m.View(w)); got > w {
			t.Fatalf("width %d: view is %d wide", w, got)
		}
	}
}

func TestActivityReplacesStats(t *testing.T) {
	m := newBar(false).SetContext("local", "main", "people").SetStats(3, 200, 0, time.Millisecond*20)
	m = m.SetActivity("exporting… 12,345 rows")
	v := plainView(m, 100)
	if !strings.Contains(v, "exporting… 12,345 rows") || strings.Contains(v, "row 3 of") {
		t.Fatalf("activity not shown in place of the stats: %q", v)
	}
	m, _ = m.Flash("saved", false)
	if v := plainView(m, 100); !strings.Contains(v, "saved") {
		t.Fatalf("a flash should show over the activity: %q", v)
	}
	if v := plainView(m.SetActivity(""), 100); strings.Contains(v, "exporting") {
		t.Fatalf("cleared activity still shown: %q", v)
	}
}

func TestTxStuckBadge(t *testing.T) {
	m := newBar(false).SetTx(true).SetTxStuck(true)
	if v := plainView(m, 100); !strings.Contains(v, "TX!") {
		t.Fatalf("no TX!: %q", v)
	}
	// TX! is drawn in the theme's danger colour.
	if raw := m.View(100); !regexp.MustCompile(`38;2;248;113;113[;0-9]*mTX!`).MatchString(raw) {
		t.Fatalf("TX! not in the danger colour: %q", raw)
	}
}
