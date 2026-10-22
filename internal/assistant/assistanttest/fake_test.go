package assistanttest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/assistant/assistanttest"
)

var _ assistant.Assistant = (*assistanttest.Fake)(nil)

func TestFakeRunsScriptsAndRecordsCalls(t *testing.T) {
	f := &assistanttest.Fake{
		ModelName: "fake/model",
		AskFn: func(_ context.Context, req assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
			ev.OnToolCall(assistant.ToolCall{Name: "describe", Rows: -1})
			return assistant.AskResult{Query: "select 1", Dialect: "sqlite"}, nil
		},
	}
	var tools []string
	ev := assistant.Events{ToolCall: func(tc assistant.ToolCall) { tools = append(tools, tc.Name) }}
	res, err := f.Ask(context.Background(), assistant.AskRequest{Question: "q?", Scope: assistant.Scope{Conn: "c"}}, ev)
	if err != nil || res.Query != "select 1" {
		t.Fatalf("Ask = %+v, %v", res, err)
	}
	if len(tools) != 1 || tools[0] != "describe" {
		t.Fatalf("events = %v", tools)
	}
	if f.Model() != "fake/model" {
		t.Fatalf("Model = %q", f.Model())
	}
	calls := f.Calls()
	if len(calls) != 1 || calls[0].Method != "Ask" || calls[0].Req.(assistant.AskRequest).Question != "q?" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestFakeUnscriptedMethodFails(t *testing.T) {
	f := &assistanttest.Fake{}
	ctx := context.Background()
	if _, err := f.Answer(ctx, assistant.AnswerRequest{}, assistant.Events{}); !errors.Is(err, assistanttest.ErrUnscripted) {
		t.Fatalf("Answer err = %v", err)
	}
	if _, err := f.ExplainResults(ctx, assistant.ExplainResultsRequest{}, assistant.Events{}); !errors.Is(err, assistanttest.ErrUnscripted) {
		t.Fatalf("ExplainResults err = %v", err)
	}
	if _, err := f.FixError(ctx, assistant.FixErrorRequest{}, assistant.Events{}); !errors.Is(err, assistanttest.ErrUnscripted) {
		t.Fatalf("FixError err = %v", err)
	}
	if got := len(f.Calls()); got != 3 {
		t.Fatalf("recorded %d calls, want 3", got)
	}
}

var _ assistant.Sessioner = (*assistanttest.Fake)(nil)

// A session runs the Fake's scripts and its calls are recorded with the
// session's number; calls on the Fake itself have number 0.
func TestFakeSessions(t *testing.T) {
	f := &assistanttest.Fake{
		AskFn: func(context.Context, assistant.AskRequest, assistant.Events) (assistant.AskResult, error) {
			return assistant.AskResult{Query: "select 1"}, nil
		},
	}
	ctx := context.Background()
	s1, s2 := f.NewSession(), f.NewSession()
	if res, err := s1.Ask(ctx, assistant.AskRequest{Question: "a"}, assistant.Events{}); err != nil || res.Query != "select 1" {
		t.Fatalf("session Ask = %+v, %v", res, err)
	}
	if _, err := s2.Answer(ctx, assistant.AnswerRequest{}, assistant.Events{}); !errors.Is(err, assistanttest.ErrUnscripted) {
		t.Fatalf("session Answer err = %v", err)
	}
	if _, err := f.Ask(ctx, assistant.AskRequest{}, assistant.Events{}); err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, c := range f.Calls() {
		got = append(got, c.Session)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 0 {
		t.Fatalf("call sessions = %v, want [1 2 0]", got)
	}
}
