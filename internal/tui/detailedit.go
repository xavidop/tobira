package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/detail"
	"github.com/xavidop/tobira/internal/tui/keymap"
)

// detailEditKey handles e and d in the row detail as the grid does: e edits
// the field under the detail's cursor, d marks or unmarks the row for
// deletion. It reports whether k was one of them. The detail then shows
// the pending value, and its title says when the row is marked.
func (a *Model) detailEditKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case a.km.Matches(k, keymap.Edit):
		if a.activeTab() == nil {
			if col, ok := a.det.Column(); ok {
				row, _ := a.gr.Cursor()
				a.gr.SetCursor(row, col)
			}
		}
	case a.km.Matches(k, keymap.DeleteRow):
	default:
		return nil, false
	}
	cmd, _ := a.editKeyPress(k)
	return tea.Batch(cmd, a.refreshDetail()), true
}

// pendingRow is the browse grid's current row as it shows: with its pending
// edits applied, and whether it is marked for deletion.
func (a *Model) pendingRow() (row driver.Row, deleted, ok bool) {
	r, ok := a.gr.CurrentRow()
	if !ok {
		return nil, false, false
	}
	if a.pend != nil && !a.pend.empty() {
		mk := pendingMarks{a.pend}.Row(a.gr.Columns(), r)
		if mk.Values != nil {
			r = mk.Values
		}
		deleted = mk.Deleted
	}
	return r, deleted, true
}

// detailDeleted reports whether the row the detail shows is marked for
// deletion.
func (a *Model) detailDeleted() bool {
	if a.det == nil || a.activeTab() != nil {
		return false
	}
	_, deleted, ok := a.pendingRow()
	return ok && deleted
}

// refreshDetail rebuilds the open detail from the row as it now shows in
// the browse grid, pending edits included, keeping the cursor where it was.
// It returns the commands that build any huge value.
func (a *Model) refreshDetail() tea.Cmd {
	if a.det == nil || a.activeTab() != nil {
		return nil
	}
	row, _, ok := a.pendingRow()
	if !ok {
		return nil
	}
	cur := a.det.Cursor()
	a.det.Cancel()
	var d detail.Model
	if a.kv {
		d = detail.NewKeyValue(a.gr.Columns(), row, a.th, a.km)
	} else {
		d = detail.New(a.gr.Columns(), row, a.th, a.km)
	}
	d = d.WithAbsent(a.gr.CurrentAbsent()).Resize(a.lay.grid.w, a.lay.grid.h).WithCursor(cur)
	a.det = &d
	return d.Init()
}
