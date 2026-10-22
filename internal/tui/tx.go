package tui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// :tx begin|commit|rollback. One transaction at a time, on the editor's
// connection, which stays pinned to it while the transaction is open. Editor
// runs and :apply go through it; the browse tab keeps reading through the
// connection, so it does not see uncommitted changes until the commit.
// Begin, Commit and Rollback each run in a command.

const (
	// txTimeout bounds Begin and Commit.
	txTimeout = 30 * time.Second
	// abortNote is added to an error inside the transaction: the driver may
	// have ended it (postgres does on any error), and only a rollback is
	// sure to work then.
	abortNote = "the transaction may be aborted; :tx rollback to continue"
)

var (
	// errNoTransactor is returned by a begin on a driver without Transactor.
	errNoTransactor = errors.New("transactions are not supported")
	// errTxStuck is returned when a cancelled call still inside the Tx did
	// not return within the wait.
	errTxStuck = errors.New("a cancelled statement is still running in the transaction")
)

// stuckText refuses everything but a rollback on a TX! transaction.
const stuckText = "a cancelled statement is still running in the transaction (TX!); only :tx rollback is allowed"

// txWait is how long a call waits for a cancelled one still inside the Tx.
func (a *Model) txWait() time.Duration {
	if a.txWaitFor > 0 {
		return a.txWaitFor
	}
	return stopWait
}

// txUsers hands the calls still inside the Tx to a new call that must wait
// for them, and records that new call's done channel (nil records nothing).
func (a *Model) txUsers(done <-chan struct{}) []<-chan struct{} {
	prior := stillRunning(a.tx.users)
	a.tx.users = prior
	if done != nil {
		a.tx.users = append(slices.Clone(prior), done)
	}
	return slices.Clone(prior)
}

// waitUsers waits, off the Update goroutine, until every call in users has
// returned. It returns errTxStuck when d passed first, and ctx's error when
// the waiting call was cancelled meanwhile: nothing reached the Tx then.
// hook, when set, is called as the wait begins (tests synchronise on it).
func waitUsers(ctx context.Context, users []<-chan struct{}, d time.Duration, hook func()) error {
	if len(users) == 0 {
		return nil
	}
	if hook != nil {
		hook()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for _, done := range users {
		select {
		case <-done:
		case <-timer.C:
			return errTxStuck
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// txState is the open transaction, or the begin that will open it.
type txState struct {
	conn   string
	tx     driver.Tx // nil while begin is in flight
	ro     bool      // begun read-only (the connection was read-only then)
	busy   string    // "begin", "commit" or "rollback" while that call runs
	busyAt time.Time
	id     uint64
	cancel context.CancelFunc // cancels a begin in flight
	// users are the done channels of the runs and applies that went into the
	// Tx. A cancelled one may still be inside the driver: nothing else may
	// reach the Tx until it has returned (a driver Tx is not safe for two
	// calls at once).
	users []<-chan struct{}
	// stuck (TX!) is set when a cancelled call did not return within the
	// wait: only a rollback is allowed then.
	stuck bool
}

// txDoneMsg is the result of a Begin, Commit or Rollback. then is the
// guarded action a commit or rollback from a prompt goes on with.
type txDoneMsg struct {
	id   uint64
	op   string
	tx   driver.Tx
	err  error
	then tea.Cmd
}

// open reports a transaction ready for statements.
func (t *txState) open() bool { return t != nil && t.tx != nil && t.busy == "" }

// txOn returns the open transaction on conn, if any.
func (a *Model) txOn(conn string) (driver.Tx, bool) {
	if a.tx.open() && a.tx.conn == conn {
		return a.tx.tx, true
	}
	return nil, false
}

// txBusyText refuses work while a transaction call is in flight.
func (a *Model) txBusyText() string {
	switch a.tx.busy {
	case "begin":
		return "the transaction is still starting"
	case "commit":
		return "the transaction is still committing"
	}
	return "the transaction is still rolling back"
}

func (a *Model) txCmd(msg palette.TxCmd) tea.Cmd {
	if a.tx != nil && a.tx.busy != "" {
		return a.flash(a.txBusyText(), false)
	}
	switch msg.Op {
	case "begin":
		return a.beginTx()
	case "commit", "rollback":
		if a.tx == nil {
			return a.flash("no transaction is open", false)
		}
		if a.tx.stuck && msg.Op == "commit" {
			return a.flash(stuckText, false)
		}
		if why := a.txInUse(); why != "" {
			return a.flash(why, false)
		}
		return a.endTx(msg.Op, nil)
	}
	return nil
}

// txInUse says why the transaction cannot be ended or used for something
// else right now: a run or an apply is using it. A driver transaction is
// not safe for concurrent use.
func (a *Model) txInUse() string {
	switch {
	case a.apply != nil && a.apply.mode == applyOpenTx:
		return applyingText
	case a.running != nil && a.running.inTx:
		return "a statement is still running in the transaction; ctrl+c cancels it"
	}
	return ""
}

func (a *Model) beginTx() tea.Cmd {
	if a.tx != nil {
		return a.flash("a transaction is already open", false)
	}
	name := a.editorConn()
	if name == "" || !hasConn(a.mgr, name) {
		return a.flash("select a connection in the sidebar first", false)
	}
	st := a.mgr.State(name)
	if st.Broken {
		return a.flash("connection "+name+" is misconfigured", true)
	}
	if a.apply != nil {
		return a.flash(applyingText, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), txTimeout)
	a.txSeq++
	a.tx = &txState{conn: name, ro: st.ReadOnly, busy: "begin", busyAt: a.clock(), id: a.txSeq, cancel: cancel}
	a.wantTick = true // the connection may be opening
	mgr, id, ro := a.mgr, a.txSeq, st.ReadOnly
	return func() tea.Msg {
		defer cancel()
		conn, err := mgr.Open(ctx, name)
		if err != nil {
			return txDoneMsg{id: id, op: "begin", err: err}
		}
		tr, ok := driver.As[driver.Transactor](conn)
		if !ok {
			return txDoneMsg{id: id, op: "begin", err: errNoTransactor}
		}
		tx, err := tr.Begin(ctx, driver.TxOptions{ReadOnly: ro})
		return txDoneMsg{id: id, op: "begin", tx: tx, err: err}
	}
}

// endTx commits or rolls back the open transaction in a command; then runs
// after a success.
func (a *Model) endTx(op string, then tea.Cmd) tea.Cmd {
	t := a.tx
	t.busy, t.busyAt = op, a.clock()
	tx, id := t.tx, t.id
	users, wait, hook := a.txUsers(nil), a.txWait(), a.txWaitHook
	return func() tea.Msg {
		// A commit or rollback is never cancelled: only the bound ends the wait.
		if err := waitUsers(context.Background(), users, wait, hook); err != nil {
			return txDoneMsg{id: id, op: op, err: err, then: then}
		}
		timeout := txTimeout
		if op == "rollback" {
			timeout = rollbackTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var err error
		if op == "commit" {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		return txDoneMsg{id: id, op: op, err: err, then: then}
	}
}

func (a *Model) txDone(msg txDoneMsg) tea.Cmd {
	t := a.tx
	if t == nil || msg.id != t.id {
		if msg.tx != nil { // a begin that lost its state (shutdown): end it
			return func() tea.Msg { _ = rollback(msg.tx); return nil }
		}
		return nil
	}
	conn := t.conn
	errText := func() string { return a.mgr.Redact(conn, msg.err.Error()) }
	if errors.Is(msg.err, errTxStuck) {
		// Nothing reached the Tx: it stays open, TX!, for a rollback.
		t.busy, t.stuck = "", true
		return a.flash(stuckText, true)
	}
	switch msg.op {
	case "begin":
		if msg.err != nil {
			a.tx = nil
			if errors.Is(msg.err, errNoTransactor) {
				return a.flash("transactions are not supported by "+a.mgr.State(conn).Info.Name, false)
			}
			return a.flash("begin failed: "+errText(), true)
		}
		t.tx, t.busy, t.cancel = msg.tx, "", nil
		text := "transaction open on " + conn
		if t.ro {
			text += " (read-only)"
		}
		return a.flash(text, false)
	case "commit":
		// A failed commit ends the transaction too: drivers roll back.
		a.tx = nil
		if msg.err != nil {
			return a.flash("commit failed: "+errText()+"; the transaction was rolled back", true)
		}
		cmds := []tea.Cmd{a.flash("committed", false), a.invalidateCmd(conn), msg.then}
		if a.cur.set && a.cur.conn == conn && a.src != nil && !a.gr.Static() {
			cmds = append(cmds, a.gr.Refresh()) // the browse tab sees the commit now
		}
		return tea.Batch(cmds...)
	}
	a.tx = nil
	if msg.err != nil {
		return a.flash("rollback failed: "+errText()+"; the transaction is closed", true)
	}
	return tea.Batch(a.flash("rolled back", false), msg.then)
}

// txStuck reports a commit or rollback that has not returned within
// stuckAfter.
func (a *Model) txStuck() bool {
	return a.tx != nil && (a.tx.busy == "commit" || a.tx.busy == "rollback") &&
		a.clock().Sub(a.tx.busyAt) >= stuckAfter
}

// txEnding reports a commit or rollback in flight.
func (a *Model) txEnding() bool {
	return a.tx != nil && (a.tx.busy == "commit" || a.tx.busy == "rollback")
}

// quitStuckTx quits past a commit or rollback that will not return; closing
// the connections at exit ends the transaction, and what was not committed
// is rolled back by the database.
func (a *Model) quitStuckTx() tea.Cmd {
	text := "quitting with a stuck " + a.tx.busy + "; whatever was not committed is rolled back when the connection closes"
	if n := a.pend.count(); n > 0 {
		text += "; " + strconv.Itoa(n) + plural(n, " pending edit", " pending edits") + " dropped"
	}
	a.exitNotice = text
	flash := a.flash(text, true)
	a.tx = nil
	a.discardPending()
	return tea.Batch(flash, a.leave())
}

// waitTxText answers a quit while a commit or rollback runs.
func (a *Model) waitTxText() string {
	secs := strconv.Itoa(int(stuckAfter / time.Second))
	return "waiting for the " + a.tx.busy + " to finish; after " + secs + "s quitting again leaves anyway"
}

// txGuard holds an action that would end or leave the transaction (switching
// the editor's connection, closing it, :reload, quitting): it asks to
// commit, roll back or cancel, and goes on with proceed after a successful
// commit or rollback. A begin in flight is refused for these, except when
// quitting, which just abandons it.
func (a *Model) txGuard(reason string, proceed tea.Cmd, quitting bool) (tea.Cmd, bool) {
	t := a.tx
	switch {
	case t == nil:
		return nil, false
	case t.busy == "begin" && quitting:
		return nil, false
	case t.stuck && quitting && t.busy == "":
		// Its rollback could not reach it either: the connection close at
		// exit ends it.
		a.exitNotice = "quitting with a statement still running in the transaction; it is rolled back when the connection closes"
		return nil, false
	case t.busy != "":
		return a.flash(a.txBusyText(), false), true
	}
	if why := a.txInUse(); why != "" {
		return a.flash(why, false), true
	}
	title := "transaction open on " + t.conn + ": [c]ommit · [r]ollback · cancel"
	if t.stuck {
		title = "transaction on " + t.conn + " is stuck (TX!): [r]ollback · cancel"
	}
	a.yieldAIAsk()
	a.ask = &askState{kind: askTx, title: title, reason: reason, yes: proceed}
	a.cell, a.help = nil, false
	a.dropDetail()
	return nil, true
}

// closeTxAtExit rolls back a transaction left open when the program stopped
// (a cancelled context, say). It runs after the program has exited, never
// on the Update goroutine; a commit or rollback still in flight is left to
// the connection close.
func (a *Model) closeTxAtExit() {
	if a.tx.open() && waitUsers(context.Background(), stillRunning(a.tx.users), a.txWait(), nil) == nil {
		_ = rollback(a.tx.tx) // past the wait, the connection close ends it
	}
	if a.tx != nil && a.tx.cancel != nil {
		a.tx.cancel()
	}
	a.tx = nil
}
