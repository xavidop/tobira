package ai

import (
	"context"
	"github.com/xavidop/tobira/ai/schemactx"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/ai/aitest"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"

	gai "github.com/firebase/genkit/go/ai"
)

// fixedSchema stands in for a schemactx text in the goldens.
const fixedSchema = "dialect: sqlite\nkind: sql\nnamespace: main\ncollections:\n  - name: orders\n    role: in scope\n    fields:\n      - id INTEGER pk\n      - total REAL not null"

func goldenInputs() map[string]map[string]any {
	base := func(extra map[string]any) map[string]any {
		in := map[string]any{"dialect": "sqlite", "kind": "sql", "schema": fixedSchema}
		for k, v := range extra {
			in[k] = v
		}
		return in
	}
	history := []map[string]any{{"from": "user", "text": "show orders"}, {"from": "assistant", "text": "Use the orders table."}}
	return map[string]map[string]any{
		promptAsk:            base(map[string]any{"connection": "dev", "question": "total revenue per day?", "history": history, "tools": true}),
		promptAnswer:         base(map[string]any{"connection": "dev", "question": "what is the biggest order?", "history": history, "execute": false}),
		promptExplainResults: base(map[string]any{"query": "select id, total from orders", "columns": `[{"name":"id","type":"INTEGER"},{"name":"total","type":"REAL"}]`, "rows": "[[1,9.5],[2,20]]", "truncated": true}),
		promptFixError:       base(map[string]any{"query": "select totl from orders", "error": "no such column: totl", "tools": false}),
	}
}

// Each prompt renders as its golden for a fixed schema context.
func TestPromptGoldens(t *testing.T) {
	h := newHarness(t, nil)
	g, err := h.a.genkit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := &flows{a: h.a, g: g, prompts: map[string]gai.Prompt{}, nonce: func() string { return "0123456789abcdef" }}
	for _, name := range promptNames() {
		f.prompts[name] = genkit.LookupPrompt(g, name)
	}
	for name, in := range goldenInputs() {
		t.Run(name, func(t *testing.T) {
			msgs, req, err := f.render(context.Background(), name, in, nil)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for _, m := range msgs {
				b.WriteString("=== " + string(m.Role) + "\n" + m.Text() + "\n")
			}
			if wantJSON := name == promptAsk || name == promptFixError; (req.output != nil) != wantJSON {
				t.Fatalf("output schema = %v, want one: %v", req.output, wantJSON)
			}
			if req.output != nil {
				b.WriteString("=== output schema\n" + compactJSON(req.output) + "\n")
			}
			golden(t, "prompts/"+name, b.String())
		})
	}
	// Withheld rows render the note instead.
	in := goldenInputs()[promptExplainResults]
	delete(in, "rows")
	in["note"] = "rows withheld by policy (ai.send_rows: never)"
	msgs, _, err := f.render(context.Background(), promptExplainResults, in, nil)
	if err != nil || !strings.Contains(msgs[len(msgs)-1].Text(), "The rows are withheld: rows withheld by policy") {
		t.Fatalf("withheld render: %v %v", msgs, err)
	}
}

// An override file wins over the built-in prompt of the same name; other
// files are ignored.
func TestPromptOverride(t *testing.T) {
	dir := t.TempDir()
	src, err := promptFS.ReadFile("prompts/ask.prompt")
	if err != nil {
		t.Fatal(err)
	}
	custom := strings.Replace(string(src), "You are tobira's database assistant.", "You are OVERRIDDEN-ASSISTANT.", 1)
	write(t, filepath.Join(dir, "ask.prompt"), custom)
	write(t, filepath.Join(dir, "notes.txt"), "not a prompt")
	write(t, filepath.Join(dir, "unknown.prompt"), "---\n: : broken\n---\n{{")
	h := newHarness(t, []aitest.Response{aitest.JSON(map[string]string{"query": "select 1", "explanation": "x"})}, withPromptDir(dir))
	if _, err := h.a.Ask(context.Background(), assistant.AskRequest{Scope: scopeColl, Question: "q"}, assistant.Events{}); err != nil {
		t.Fatal(err)
	}
	if req := h.lastRequest(t); !strings.Contains(req, "OVERRIDDEN-ASSISTANT") || strings.Contains(req, "You are tobira's database assistant.") {
		t.Fatalf("override not used:\n%s", req)
	}
}

// An invalid override is an error from New naming the file, not a panic.
func TestInvalidPromptOverride(t *testing.T) {
	cases := map[string]string{
		"frontmatter": "---\ninput: [unclosed\n---\nhello",
		"template":    "---\ninput:\n  schema:\n    question: string\n---\n{{#if question}}never closed",
		"schema":      "---\ninput:\n  schema:\n    question(badtype): string\n---\n{{question}}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "answer.prompt")
			write(t, path, body)
			_, err := New(context.Background(), config.AI{Provider: "ollama"}, Deps{Getenv: env(nil), PromptDir: dir})
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("err = %v, want one naming %s", err, path)
			}
		})
	}
}

// A missing override directory is no overrides.
func TestMissingPromptDir(t *testing.T) {
	a, err := New(context.Background(), config.AI{Provider: "ollama"}, Deps{Getenv: env(nil), PromptDir: filepath.Join(t.TempDir(), "absent")})
	if err != nil || a == nil {
		t.Fatalf("New = %v, %v", a, err)
	}
	if _, ok := a.(assistant.Sessioner); !ok {
		t.Fatal("the assistant does not keep sessions")
	}
}

// New registers every flow, under the names the Dev UI shows.
func TestFlowsAreRegistered(t *testing.T) {
	a, err := New(context.Background(), config.AI{Provider: "ollama"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	g, err := a.(*genkitAssistant).genkit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{flowAsk, flowAnswer, flowExplainResults, flowFixError} {
		if genkit.LookupAction(g, "/flow/"+name) == nil {
			t.Errorf("flow %s is not registered", name)
		}
	}
}

// In dev mode the flows are registered by New, even for a provider whose
// init is otherwise deferred to the first request, so the Dev UI lists them.
func TestDevModeRegistersFlowsAtOnce(t *testing.T) {
	t.Setenv("GENKIT_ENV", "")
	providers["deferred"] = provider{
		defaultModel: "m",
		deferInit:    true,
		plugin:       func(config.AI, driver.Secret, func(string) string) api.Plugin { return &noopPlugin{} },
	}
	t.Cleanup(func() { delete(providers, "deferred") })
	for _, dev := range []bool{false, true} {
		a, err := New(context.Background(), config.AI{Provider: "deferred"}, Deps{Getenv: env(nil), DevMode: dev})
		if err != nil {
			t.Fatal(err)
		}
		ga := a.(*genkitAssistant)
		if ga.initialised() != dev {
			t.Fatalf("dev=%v: initialised after New = %v", dev, ga.initialised())
		}
		if dev && genkit.LookupAction(ga.g, "/flow/"+flowAnswer) == nil {
			t.Fatal("dev mode: answerFlow is not registered")
		}
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The prompts that write queries tell the model what a query is for a
// driver whose dialect name alone would mislead it, verbatim, quotes and
// all; a SQL dialect adds nothing.
func TestPromptsStateTheQueryForm(t *testing.T) {
	h := newHarness(t, nil)
	g, err := h.a.genkit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := &flows{a: h.a, g: g, prompts: map[string]gai.Prompt{}, nonce: func() string { return "0123456789abcdef" }}
	for _, name := range promptNames() {
		f.prompts[name] = genkit.LookupPrompt(g, name)
	}
	note := schemactx.QuerySyntax("mongodb")
	for _, name := range []string{promptAsk, promptAnswer, promptFixError} {
		in := goldenInputs()[name]
		in["dialect"], in["kind"], in["syntax"] = "mongodb", "document", note
		msgs, _, err := f.render(context.Background(), name, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sys := msgs[0].Text(); !strings.Contains(sys, "- A query for this database is "+note) {
			t.Errorf("%s: the query form is missing or altered:\n%s", name, sys)
		}
		delete(in, "syntax")
		msgs, _, err = f.render(context.Background(), name, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sys := msgs[0].Text(); strings.Contains(sys, "A query for this database") {
			t.Errorf("%s: a query form rendered with none given:\n%s", name, sys)
		}
	}
}
