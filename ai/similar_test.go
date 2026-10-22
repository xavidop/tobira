package ai

import (
	"strings"
	"testing"

	"github.com/xavidop/tobira/ai/aitest"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
)

// Answer offers similar only when the turn's connection is a
// VectorSearcher.
func TestAnswerOffersSimilarOnlyForVectorSearchers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fopts []drivertest.FakeOption
		want  bool
	}{
		{"vector", nil, true},
		{"plain", []drivertest.FakeOption{drivertest.WithoutCapabilities(driver.CapVectorSearcher)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessFake(t, []aitest.Response{aitest.Text("ok")}, tc.fopts, nil)
			if _, err := h.a.Answer(ctx, assistant.AnswerRequest{Scope: scopeDev, Question: "q"}, assistant.Events{}); err != nil {
				t.Fatal(err)
			}
			if got := contains(h.toolNames(0), tools.NameSimilar); got != tc.want {
				t.Fatalf("similar offered = %v, want %v (tools %v)", got, tc.want, h.toolNames(0))
			}
		})
	}
}

// The model can call similar; the rows go back to it and the call is
// reported.
func TestAnswerRunsSimilar(t *testing.T) {
	h := newHarnessFake(t, []aitest.Response{
		aitest.Tools(aitest.Call(tools.NameSimilar, map[string]any{"connection": "dev", "collection": drivertest.VectorCollection, "vector": []float64{1, 0, 0, 0}, "k": 2, "metric": "l2"})),
		aitest.Text("The nearest is near."),
	}, []drivertest.FakeOption{drivertest.WithVectors()}, nil)
	var ev events
	if _, err := h.a.Answer(ctx, assistant.AnswerRequest{Scope: scopeDev, Question: "nearest?"}, ev.Events()); err != nil {
		t.Fatal(err)
	}
	calls := ev.toolCalls()
	if len(calls) != 1 || calls[0].Name != tools.NameSimilar || calls[0].Rows != 2 || calls[0].Err != "" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if req := h.lastRequest(t); !strings.Contains(req, "near") || !strings.Contains(req, "_distance") {
		t.Fatalf("similar rows not sent back:\n%s", req)
	}
	h.noSentinels(t)
}
