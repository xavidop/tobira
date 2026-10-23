package keymap

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// press builds the key press event for a key string such as "j", "ctrl+d" or "G".
func press(s string) tea.KeyPressMsg {
	special := map[string]rune{
		"left": tea.KeyLeft, "right": tea.KeyRight, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
		"home": tea.KeyHome, "end": tea.KeyEnd, "tab": tea.KeyTab, "enter": tea.KeyEnter,
		"esc": tea.KeyEscape, "f1": tea.KeyF1,
	}
	if c, ok := special[s]; ok {
		return tea.KeyPressMsg{Code: c}
	}
	switch s {
	case "ctrl+home":
		return tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl}
	case "ctrl+end":
		return tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "G", "Y":
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func TestDefaultKeymapCoversAllActions(t *testing.T) {
	for _, m := range []Map{Default(), Vim()} {
		for _, a := range AllActions() {
			if len(m.Binding(a).Keys()) == 0 {
				t.Errorf("%s: action %q has no key", m.Name, a)
			}
		}
	}
}

func TestVimAddsHJKL(t *testing.T) {
	v := Vim()
	tests := []struct {
		key string
		a   Action
	}{
		{"j", Down}, {"down", Down}, {"k", Up}, {"up", Up},
		{"G", Bottom}, {"ctrl+d", PageDown},
	}
	for _, tt := range tests {
		if !v.Matches(press(tt.key), tt.a) {
			t.Errorf("vim: %q should match %s", tt.key, tt.a)
		}
	}
	if Default().Matches(press("j"), Down) {
		t.Error("default keymap must not bind j")
	}
}

func TestNoConflicts(t *testing.T) {
	// Context-scoped pairs allowed to share a key: f filters the browse tab
	// and fixes a failed run on its error tab; [ and ] switch result tabs on
	// the grid and collapse / expand every node in the row detail.
	allowed := map[string]bool{"f": true, "[": true, "]": true}
	for _, m := range []Map{Default(), Vim()} {
		seen := map[string]Action{}
		for _, a := range AllActions() {
			for _, k := range m.Binding(a).Keys() {
				if prev, ok := seen[k]; ok && !allowed[k] {
					t.Errorf("%s: key %q bound to both %s and %s", m.Name, k, prev, a)
				}
				seen[k] = a
			}
		}
		// The live tail keys: W opens it from the grid; p and c act only in
		// its tab, and no other action may take them.
		for k, want := range map[string]Action{"W": Watch, "p": WatchPause, "c": WatchClear} {
			if got := seen[k]; got != want {
				t.Errorf("%s: key %q bound to %q, want %s", m.Name, k, got, want)
			}
		}
	}
}

func TestHelpGroups(t *testing.T) {
	for _, m := range []Map{Default(), Vim()} {
		groups := m.Help()
		if len(groups) != 5 {
			t.Fatalf("%s: %d help groups, want 5", m.Name, len(groups))
		}
		count := 0
		for _, g := range groups {
			count += len(g)
		}
		if count != len(AllActions()) {
			t.Errorf("%s: help lists %d bindings, want %d", m.Name, count, len(AllActions()))
		}
		seen := map[string]int{}
		for _, g := range groups {
			for _, b := range g {
				seen[b.Help().Desc]++
				if b.Help().Key == "" {
					t.Errorf("%s: binding %q has empty help key", m.Name, b.Help().Desc)
				}
			}
		}
		for d, n := range seen {
			if n != 1 {
				t.Errorf("%s: help desc %q appears %d times", m.Name, d, n)
			}
		}
	}
}

func TestByName(t *testing.T) {
	for _, n := range []string{"default", "vim"} {
		m, err := ByName(n)
		if err != nil || m.Name != n {
			t.Errorf("ByName(%q) = %q, %v", n, m.Name, err)
		}
	}
	if _, err := ByName("emacs"); err == nil || !strings.Contains(err.Error(), "emacs") {
		t.Errorf("unknown keymap err = %v", err)
	}
}

func TestDefaultBindingsMatch(t *testing.T) {
	tests := []struct {
		key string
		a   Action
	}{
		{"up", Up}, {"down", Down}, {"left", Left}, {"right", Right},
		{"pgup", PageUp}, {"pgdown", PageDown}, {"ctrl+home", Top}, {"ctrl+end", Bottom},
		{"home", Home}, {"end", End}, {"tab", FocusNext}, {"shift+tab", FocusPrev},
		{"enter", Open}, {"esc", Back}, {"f", Filter}, {"/", SidebarFilter}, {"s", Sort},
		{">", WidenCol}, {"<", NarrowCol}, {"y", CopyCell}, {"Y", CopyRow},
		{":", Palette}, {"f1", Help}, {"?", Help}, {"q", Quit}, {"ctrl+c", Cancel},
		{"e", Edit}, {"a", Ask}, {"n", ConnAdd}, {"E", ConnEdit}, {"D", ConnRm},
	}
	m := Default()
	for _, tt := range tests {
		if !m.Matches(press(tt.key), tt.a) {
			t.Errorf("default: %q should match %s", tt.key, tt.a)
		}
	}
}

func TestVimSingleGDoesNothing(t *testing.T) {
	v := Vim()
	if v.Matches(press("g"), Top) {
		t.Error("a single g must not be Top in vim")
	}
	if _, act, consumed := v.Step(Seq{}, press("g"), time.Unix(0, 0)); act != "" || !consumed {
		t.Errorf("first g: act=%q consumed=%v, want pending (consumed, no action)", act, consumed)
	}
	if got := v.Binding(Top).Help().Key; !strings.Contains(got, "gg") {
		t.Errorf("help key %q should show gg", got)
	}
}

func TestSeqWindowIs500ms(t *testing.T) {
	if SeqWindow != 500*time.Millisecond {
		t.Fatalf("SeqWindow = %v, want 500ms", SeqWindow)
	}
}

func TestStepGG(t *testing.T) {
	v := Vim()
	t0 := time.Unix(1000, 0)
	s, act, consumed := v.Step(Seq{}, press("g"), t0)
	if !consumed || act != "" {
		t.Fatalf("first g: consumed=%v act=%q", consumed, act)
	}
	if _, act, consumed = v.Step(s, press("g"), t0.Add(SeqWindow)); !consumed || act != Top {
		t.Errorf("gg inside the window: consumed=%v act=%q, want Top", consumed, act)
	}
	// Past the window the second g starts over instead of completing.
	s2, act, consumed := v.Step(s, press("g"), t0.Add(SeqWindow+time.Millisecond))
	if !consumed || act != "" || s2.first != "g" {
		t.Errorf("late g: consumed=%v act=%q, want a fresh pending g", consumed, act)
	}
	// Another key breaks the sequence and is left for the caller.
	s3, act, consumed := v.Step(s, press("j"), t0)
	if consumed || act != "" || s3 != (Seq{}) {
		t.Errorf("g j: consumed=%v act=%q seq=%v, want j left alone and nothing pending", consumed, act, s3)
	}
	// The default keymap has no sequences.
	if _, _, consumed := Default().Step(Seq{}, press("g"), t0); consumed {
		t.Error("default keymap must not consume g")
	}
}

func TestEditorActions(t *testing.T) {
	keys := map[Action][]tea.KeyPressMsg{
		EditorToggle: {{Code: 'e', Mod: tea.ModCtrl}},
		RunStatement: {{Code: tea.KeyEnter, Mod: tea.ModCtrl}, {Code: tea.KeyF5}},
		RunBuffer:    {{Code: tea.KeyEnter, Mod: tea.ModAlt}, {Code: tea.KeyF5, Mod: tea.ModShift}},
		PrevTab:      {press("[")},
		NextTab:      {press("]")},
		CloseTab:     {press("x")},
		Complete:     {{Code: tea.KeySpace, Mod: tea.ModCtrl}},
		HistSearch:   {{Code: 'r', Mod: tea.ModCtrl}},
	}
	for _, m := range []Map{Default(), Vim()} {
		for a, ks := range keys {
			for _, k := range ks {
				if !m.Matches(k, a) {
					t.Errorf("%s: %q should match %s", m.Name, k.String(), a)
				}
			}
		}
	}
	titles := GroupTitles()
	if titles[len(titles)-1] != "Editor" {
		t.Fatalf("group titles = %v, want an Editor group", titles)
	}
}

func TestInlineEditingActions(t *testing.T) {
	keys := map[Action]tea.KeyPressMsg{
		Edit:      press("e"),
		InsertRow: press("i"),
		DeleteRow: press("d"),
		SetNull:   {Code: 'n', Mod: tea.ModCtrl},
		SaveEdit:  {Code: 's', Mod: tea.ModCtrl},
	}
	for _, m := range []Map{Default(), Vim()} {
		for a, k := range keys {
			if !m.Matches(k, a) {
				t.Errorf("%s: %q should match %s", m.Name, k.String(), a)
			}
		}
		if d := m.Binding(Edit).Help().Desc; d != "edit cell (grid or row detail)" {
			t.Errorf("%s: Edit help = %q, want %q", m.Name, d, "edit cell (grid or row detail)")
		}
	}
}

func TestAIActions(t *testing.T) {
	for _, m := range []Map{Default(), Vim()} {
		if !m.Matches(press("a"), Ask) || !m.Matches(press("f"), FixError) {
			t.Errorf("%s: a / f not bound to the AI actions", m.Name)
		}
		if !m.Matches(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, LoadProposal) || !m.Matches(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}, CopyProposal) {
			t.Errorf("%s: ctrl+o / ctrl+y not bound to the proposal actions", m.Name)
		}
		if m.Matches(press("y"), CopyProposal) || m.Matches(tea.KeyPressMsg{Code: tea.KeyEnter}, LoadProposal) {
			t.Errorf("%s: a plain key acts on proposals", m.Name)
		}
		if d := m.Binding(Ask).Help().Desc; strings.Contains(d, "M4") {
			t.Errorf("%s: Ask help still says %q", m.Name, d)
		}
	}
	if strings.Contains(Markdown(), "planned, not available yet") {
		t.Error("keymaps.md still marks an action as planned")
	}
}

func TestCancelHelpCoversEverything(t *testing.T) {
	d := Default().Binding(Cancel).Help().Desc
	for _, w := range []string{"run", ":apply", "export", "commit"} {
		if !strings.Contains(d, w) {
			t.Errorf("Cancel help %q does not mention %q", d, w)
		}
	}
}

func TestKindViewActions(t *testing.T) {
	keys := map[Action]tea.KeyPressMsg{
		FindSimilar: {Code: 'S', Text: "S"},
		CollapseAll: press("["),
		ExpandAll:   press("]"),
	}
	for _, m := range []Map{Default(), Vim()} {
		for a, k := range keys {
			if !m.Matches(k, a) {
				t.Errorf("%s: %q should match %s", m.Name, k.String(), a)
			}
		}
		// S must not clash with sort, which is s.
		if m.Matches(press("s"), FindSimilar) || m.Matches(tea.KeyPressMsg{Code: 'S', Text: "S"}, Sort) {
			t.Errorf("%s: S and s are mixed up", m.Name)
		}
	}
	// The keys shared are only these context-scoped pairs.
	m := Default()
	for _, k := range []string{"[", "]"} {
		var acts []Action
		for _, a := range AllActions() {
			for _, b := range m.Binding(a).Keys() {
				if b == k {
					acts = append(acts, a)
				}
			}
		}
		if len(acts) != 2 {
			t.Errorf("%q is bound to %v, want a tab action and a detail action", k, acts)
		}
	}
}
