package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/internal/session"
)

// writesInFlightText refuses :readonly on while a write may still land.
const writesInFlightText = "wait for the running statement or apply to finish (ctrl+c cancels it)"

// setReadOnly is :readonly on|off on the editor's connection. It is a
// runtime override on top of the config and --read-only: every run,
// explain, browse page, edit and transaction begin reads the session's
// state when it starts, so the switch applies at once.
func (a *Model) setReadOnly(on bool) tea.Cmd {
	conn := a.editorConn()
	if conn == "" || !hasConn(a.mgr, conn) {
		return a.flash("select a connection in the sidebar first", false)
	}
	if on && a.tx != nil && a.tx.conn == conn && !a.tx.ro {
		// Writes already made in it would stay writable behind the lock.
		return a.flash("commit or roll back the open transaction first", false)
	}
	if on && (a.apply != nil && a.apply.conn == conn || a.running != nil && a.running.conn == conn) {
		// Each write was allowed when it started; the lock must not show
		// while one is still landing. Cancelling it is the user's call.
		return a.flash(writesInFlightText, false)
	}
	if err := a.mgr.SetReadOnly(conn, on); err != nil {
		if errors.Is(err, session.ErrReadOnlyEnforced) {
			return a.flash(err.Error(), false)
		}
		return a.flash(err.Error(), true)
	}
	if on {
		return a.flash("🔒 "+conn+" is read-only", false)
	}
	return a.flash(conn+" is writable", false)
}
