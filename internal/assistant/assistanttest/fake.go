// Package assistanttest provides a scriptable assistant.Assistant for the
// CLI and TUI tests.
package assistanttest

import (
	"context"
	"errors"
	"sync"

	"github.com/xavidop/tobira/internal/assistant"
)

// ErrUnscripted is returned by a Fake method that has no script.
var ErrUnscripted = errors.New("assistanttest: method not scripted")

// Call is one recorded call: the method name, its request value and the
// session it was made through (0 for the Fake itself, then 1, 2, ... in
// NewSession order).
type Call struct {
	Method  string
	Req     any
	Session int
}

// Fake is an assistant.Assistant whose methods run the scripts set on it. A
// method without a script returns ErrUnscripted. Every call is recorded. A
// Fake is safe for concurrent use; set the scripts before sharing it.
type Fake struct {
	ModelName        string
	AskFn            func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error)
	AnswerFn         func(context.Context, assistant.AnswerRequest, assistant.Events) (assistant.AnswerResult, error)
	ExplainResultsFn func(context.Context, assistant.ExplainResultsRequest, assistant.Events) (assistant.AnswerResult, error)
	FixErrorFn       func(context.Context, assistant.FixErrorRequest, assistant.Events) (assistant.AskResult, error)

	mu       sync.Mutex
	calls    []Call
	sessions int
}

func (f *Fake) recordIn(session int, method string, req any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: method, Req: req, Session: session})
}

// Calls returns a copy of the calls made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Ask runs AskFn.
func (f *Fake) Ask(ctx context.Context, req assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
	return f.ask(ctx, 0, req, ev)
}

func (f *Fake) ask(ctx context.Context, session int, req assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
	f.recordIn(session, "Ask", req)
	if f.AskFn == nil {
		return assistant.AskResult{}, ErrUnscripted
	}
	return f.AskFn(ctx, req, ev)
}

// Answer runs AnswerFn.
func (f *Fake) Answer(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return f.answer(ctx, 0, req, ev)
}

func (f *Fake) answer(ctx context.Context, session int, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	f.recordIn(session, "Answer", req)
	if f.AnswerFn == nil {
		return assistant.AnswerResult{}, ErrUnscripted
	}
	return f.AnswerFn(ctx, req, ev)
}

// ExplainResults runs ExplainResultsFn.
func (f *Fake) ExplainResults(ctx context.Context, req assistant.ExplainResultsRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return f.explainResults(ctx, 0, req, ev)
}

func (f *Fake) explainResults(ctx context.Context, session int, req assistant.ExplainResultsRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	f.recordIn(session, "ExplainResults", req)
	if f.ExplainResultsFn == nil {
		return assistant.AnswerResult{}, ErrUnscripted
	}
	return f.ExplainResultsFn(ctx, req, ev)
}

// FixError runs FixErrorFn.
func (f *Fake) FixError(ctx context.Context, req assistant.FixErrorRequest, ev assistant.Events) (assistant.AskResult, error) {
	return f.fixError(ctx, 0, req, ev)
}

func (f *Fake) fixError(ctx context.Context, session int, req assistant.FixErrorRequest, ev assistant.Events) (assistant.AskResult, error) {
	f.recordIn(session, "FixError", req)
	if f.FixErrorFn == nil {
		return assistant.AskResult{}, ErrUnscripted
	}
	return f.FixErrorFn(ctx, req, ev)
}

// Model returns ModelName.
func (f *Fake) Model() string { return f.ModelName }

// NewSession returns a session that runs the Fake's scripts; its calls are
// recorded with the session's number.
func (f *Fake) NewSession() assistant.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions++
	return &fakeSession{f: f, n: f.sessions}
}

type fakeSession struct {
	f *Fake
	n int
}

func (s *fakeSession) Ask(ctx context.Context, req assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
	return s.f.ask(ctx, s.n, req, ev)
}

func (s *fakeSession) Answer(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return s.f.answer(ctx, s.n, req, ev)
}

func (s *fakeSession) ExplainResults(ctx context.Context, req assistant.ExplainResultsRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return s.f.explainResults(ctx, s.n, req, ev)
}

func (s *fakeSession) FixError(ctx context.Context, req assistant.FixErrorRequest, ev assistant.Events) (assistant.AskResult, error) {
	return s.f.fixError(ctx, s.n, req, ev)
}
