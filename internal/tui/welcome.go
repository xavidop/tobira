package tui

import (
	"strings"

	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

// welcomeRow is one line of the welcome block: an action and what it does
// from the first screen.
type welcomeRow struct {
	a    keymap.Action
	desc string
}

var welcomeRows = []welcomeRow{
	{keymap.Open, "open the selected connection or collection"},
	{keymap.ConnAdd, "add a connection"},
	{keymap.ConnEdit, "edit the selected connection"},
	{keymap.SidebarFilter, "filter the sidebar"},
	{keymap.EditorToggle, "open the query editor"},
	{keymap.Ask, "ask the AI about the selected connection"},
	{keymap.Palette, "command palette: export, theme, keymap, ..."},
	{keymap.Help, "every key and command"},
}

// welcome fills the main panel while no collection is open: what to do
// next, then the keys a newcomer needs, so the first screen teaches the app
// instead of staying blank. The keys come from the active keymap.
func (a *Model) welcome() string {
	dim, key := a.th.Styles.Dim, a.th.Styles.Title
	lead := "select a collection in the sidebar and press enter"
	if len(a.mgr.Names()) == 0 {
		lead = "no connections yet: press " + a.km.Binding(keymap.ConnAdd).Help().Key + " to add one"
	}
	var keys, descs []string
	keyW := 0
	for _, r := range welcomeRows {
		if r.a == keymap.Ask && a.asst == nil {
			continue
		}
		k := a.km.Binding(r.a).Help().Key
		if k == "" {
			continue
		}
		keys = append(keys, k)
		descs = append(descs, r.desc)
		keyW = max(keyW, query.DisplayWidth(k))
	}
	lines := []string{dim.Render(lead), ""}
	for i := range keys {
		pad := strings.Repeat(" ", keyW-query.DisplayWidth(keys[i])+2)
		lines = append(lines, key.Render(keys[i])+pad+dim.Render(descs[i]))
	}
	return strings.Join(lines, "\n")
}
