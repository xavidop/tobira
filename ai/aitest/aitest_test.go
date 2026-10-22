package aitest_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/ai/aitest"
)

var empty = fstest.MapFS{} // no prompts

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler)) // Genkit logs through slog's default
	os.Exit(m.Run())
}

func newG(t *testing.T) *genkit.Genkit {
	t.Helper()
	return genkit.Init(context.Background(), genkit.WithPromptFS(empty), genkit.WithPromptDir("."))
}

// The model answers from its script in order, records every request, and
// fails once the script is spent.
func TestScriptedModel(t *testing.T) {
	g := newG(t)
	m := aitest.NewModel(
		aitest.Text("hello"),
		aitest.JSON(map[string]string{"query": "select 1"}),
		aitest.Tools(aitest.Call("describe", map[string]any{"collection": "orders"})),
		aitest.Response{Text: "costly", Usage: &aitest.Usage{In: 7, Out: 3}},
	)
	m.Define(g, "fake/model")
	ctx := context.Background()
	gen := func(prompt string) (*gai.ModelResponse, error) {
		return genkit.Generate(ctx, g, gai.WithModelName("fake/model"), gai.WithPrompt(prompt), gai.WithReturnToolRequests(true))
	}
	if r, err := gen("one"); err != nil || r.Text() != "hello" {
		t.Fatalf("1: %v %v", r.Text(), err)
	}
	if r, err := gen("two"); err != nil || !strings.Contains(r.Text(), `"select 1"`) {
		t.Fatalf("2: %v %v", r.Text(), err)
	}
	r, err := gen("three")
	if err != nil || len(r.ToolRequests()) != 1 || r.ToolRequests()[0].ToolRequest.Name != "describe" || r.ToolRequests()[0].ToolRequest.Ref == "" {
		t.Fatalf("3: %+v %v", r.Message, err)
	}
	if r, err := gen("four"); err != nil || r.Usage == nil || r.Usage.InputTokens != 7 || r.Usage.OutputTokens != 3 {
		t.Fatalf("4: %+v %v", r, err)
	}
	if _, err := gen("five"); err == nil || !strings.Contains(err.Error(), "script exhausted") {
		t.Fatalf("5: err = %v", err)
	}
	reqs := m.Requests()
	if len(reqs) != 5 || reqs[0].Messages[0].Text() != "one" {
		t.Fatalf("requests = %d", len(reqs))
	}
	if !strings.Contains(string(m.RequestJSON()[3]), "four") {
		t.Fatalf("RequestJSON = %s", m.RequestJSON()[3])
	}
}

// A Before hook runs with the request's context before the response, for
// tests that block or cancel mid-turn; its error is the model's error.
func TestBeforeHook(t *testing.T) {
	g := newG(t)
	boom := errors.New("boom")
	m := aitest.NewModel(aitest.Response{Before: func(context.Context, *gai.ModelRequest) error { return boom }})
	m.Define(g, "fake/model")
	_, err := genkit.Generate(context.Background(), g, gai.WithModelName("fake/model"), gai.WithPrompt("x"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

// LoadScript reads the JSON script the aifake build uses
// ($TOBIRA_AI_FAKE_SCRIPT).
func TestLoadScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	script := `[{"text":"hi"},{"json":{"query":"select 1","explanation":"x"}},{"tool_requests":[{"name":"query","input":{"connection":"c"}}]},{"error":"provider down"}]`
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := aitest.LoadScript(path)
	if err != nil || len(rs) != 4 || rs[0].Text != "hi" || rs[2].ToolRequests[0].Name != "query" || rs[3].Error != "provider down" {
		t.Fatalf("LoadScript = %+v, %v", rs, err)
	}
	var probe map[string]any
	if json.Unmarshal(rs[1].JSON, &probe) != nil || probe["query"] != "select 1" {
		t.Fatalf("json response = %s", rs[1].JSON)
	}
	if _, err := aitest.LoadScript(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing script must be an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if _, err := aitest.LoadScript(bad); err == nil {
		t.Fatal("invalid script must be an error")
	}
}

// Plugin registers the model under the fake provider at genkit.Init.
func TestPlugin(t *testing.T) {
	m := aitest.NewModel(aitest.Text("from plugin"))
	g := genkit.Init(context.Background(), genkit.WithPromptFS(empty), genkit.WithPromptDir("."),
		genkit.WithPlugins(&aitest.Plugin{Model: "scripted", Fake: m}))
	r, err := genkit.Generate(context.Background(), g, gai.WithModelName("fake/scripted"), gai.WithPrompt("x"))
	if err != nil || r.Text() != "from plugin" {
		t.Fatalf("%v %v", r, err)
	}
}
