// Package async runs driver calls off the Bubble Tea update goroutine. Each
// call is a tea.Cmd with its own context and a request id, so a component can
// cancel a load it no longer wants and drop results that arrive late.
package async

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Tracker issues request ids for one component (or one tree node) and keeps
// only the latest request live. The zero value is ready to use.
//
// A Tracker is not safe for concurrent use: it lives inside a Bubble Tea
// model and is only touched from the Update goroutine. The tea.Cmd that Do
// returns never touches it.
type Tracker struct {
	last   uint64 // last id issued
	cur    uint64 // id of the live request; 0 when none
	cancel context.CancelFunc
}

// Start cancels the previous request, if any, and begins a new one. The
// returned context derives from parent and is cancelled by the next Start or
// by Cancel.
func (t *Tracker) Start(parent context.Context) (id uint64, ctx context.Context) {
	if t.cancel != nil {
		t.cancel()
	}
	t.last++
	t.cur = t.last
	ctx, t.cancel = context.WithCancel(parent)
	return t.cur, ctx
}

// Current reports whether id is the live request. Results for any other id
// are stale and must be dropped.
func (t *Tracker) Current(id uint64) bool { return id != 0 && id == t.cur }

// Finish is called when the result of request id is delivered. It reports
// whether id was the live request (so the result should be applied) and, if
// it was, releases the request's context: the call is over, so nothing keeps
// the context alive until the next Start. A stale id changes nothing.
func (t *Tracker) Finish(id uint64) bool {
	if !t.Current(id) {
		return false
	}
	t.Cancel()
	return true
}

// Cancel cancels the live request and makes its result stale. It is safe to
// call when nothing is running.
func (t *Tracker) Cancel() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.cur = 0
}

// Result is the message a Do command produces.
type Result[T any] struct {
	ID   uint64
	Val  T
	Err  error
	Took time.Duration
}

// Do returns a command that runs fn with ctx and reports a Result carrying
// id. Took is measured around fn inside the command. fn must honour ctx.
func Do[T any](id uint64, ctx context.Context, fn func(context.Context) (T, error)) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		v, err := fn(ctx)
		return Result[T]{ID: id, Val: v, Err: err, Took: time.Since(start)}
	}
}
