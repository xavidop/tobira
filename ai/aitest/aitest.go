// Package aitest is a scripted fake Genkit model, for tests and for the
// end-to-end smoke test. It never touches the network.
//
// A Model answers each request with the next Response of its script: text,
// JSON, or tool requests. It records every request it receives, so tests
// can assert on exactly what would have been sent to a provider. Register
// it on a Genkit instance with Define, or at genkit.Init with Plugin, under
// the provider name "fake".
//
// tobira binaries built with -tags aifake accept ai.provider: fake and load
// the script from the JSON file named by $TOBIRA_AI_FAKE_SCRIPT (see
// LoadScript). A normal build has no fake provider.
package aitest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
)

// Provider is the provider name of the fake model.
const Provider = "fake"

// Response is one scripted model answer. Exactly one of Text, JSON,
// ToolRequests and Error is normally set; Text and JSON may be combined
// (Text first).
type Response struct {
	// Text is returned as a text part.
	Text string `json:"text,omitempty"`
	// JSON is returned as text holding this JSON value.
	JSON json.RawMessage `json:"json,omitempty"`
	// ToolRequests are returned as tool-request parts.
	ToolRequests []ToolRequest `json:"tool_requests,omitempty"`
	// Error makes the model call fail with this message.
	Error string `json:"error,omitempty"`
	// Usage is reported with the response; nil reports none.
	Usage *Usage `json:"usage,omitempty"`
	// Before, when set, runs before the response is returned, with the
	// request's context; an error from it is the model call's error. Tests
	// use it to block or cancel mid-turn.
	Before func(ctx context.Context, req *gai.ModelRequest) error `json:"-"`
}

// ToolRequest is one scripted tool request. An empty Ref is filled in.
type ToolRequest struct {
	Name  string `json:"name"`
	Input any    `json:"input,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

// Usage is the token usage a response reports.
type Usage struct {
	In  int `json:"input_tokens"`
	Out int `json:"output_tokens"`
}

// Text is a text response.
func Text(s string) Response { return Response{Text: s} }

// JSON is a response whose text is v encoded as JSON.
func JSON(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("aitest.JSON: %v", err))
	}
	return Response{JSON: b}
}

// Tools is a response holding tool requests.
func Tools(reqs ...ToolRequest) Response { return Response{ToolRequests: reqs} }

// Call is a tool request for name with input.
func Call(name string, input any) ToolRequest { return ToolRequest{Name: name, Input: input} }

// Model is a scripted fake model. It is safe for concurrent use.
type Model struct {
	mu       sync.Mutex
	script   []Response
	next     int
	requests []*gai.ModelRequest
	raw      [][]byte
}

// NewModel returns a model that answers with script, in order.
func NewModel(script ...Response) *Model { return &Model{script: script} }

// Define registers the model on g as name ("fake/<model>").
func (m *Model) Define(g *genkit.Genkit, name string) {
	genkit.DefineModelAction(g, name, modelOptions(name), m.generate)
}

// action returns the model as an unregistered action, for Plugin.
func (m *Model) action(name string) *gai.ModelAction {
	return gai.NewModelAction(name, modelOptions(name), m.generate)
}

func modelOptions(name string) *gai.ModelOptions {
	return &gai.ModelOptions{
		Label:    name,
		Supports: &gai.ModelSupports{Multiturn: true, SystemRole: true, Tools: true},
	}
}

// Requests returns the requests received so far, in order.
func (m *Model) Requests() []*gai.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*gai.ModelRequest(nil), m.requests...)
}

// RequestJSON returns each request received so far as the JSON a provider
// plugin would be handed: every message, tool definition and output config.
func (m *Model) RequestJSON() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.raw...)
}

// Calls returns how many requests the model received.
func (m *Model) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *Model) generate(ctx context.Context, req *gai.ModelRequest, _ any, _ gai.ModelStreamCallback) (*gai.ModelResponse, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("aitest: marshal request: %w", err)
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.raw = append(m.raw, raw)
	n := m.next
	if n < len(m.script) {
		m.next++
	}
	m.mu.Unlock()
	if n >= len(m.script) {
		return nil, fmt.Errorf("aitest: script exhausted after %d responses", len(m.script))
	}
	r := m.script[n]
	if r.Before != nil {
		if err := r.Before(ctx, req); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Error != "" {
		return nil, errors.New(r.Error)
	}
	var parts []*gai.Part
	if r.Text != "" {
		parts = append(parts, gai.NewTextPart(r.Text))
	}
	if len(r.JSON) > 0 {
		parts = append(parts, gai.NewTextPart(string(r.JSON)))
	}
	for i, tr := range r.ToolRequests {
		ref := tr.Ref
		if ref == "" {
			ref = "call-" + strconv.Itoa(n) + "-" + strconv.Itoa(i)
		}
		parts = append(parts, gai.NewToolRequestPart(&gai.ToolRequest{Name: tr.Name, Input: tr.Input, Ref: ref}))
	}
	if len(parts) == 0 {
		parts = append(parts, gai.NewTextPart(""))
	}
	resp := &gai.ModelResponse{Message: gai.NewModelMessage(parts...), FinishReason: gai.FinishReasonStop, Request: req}
	if r.Usage != nil {
		resp.Usage = &gai.GenerationUsage{InputTokens: r.Usage.In, OutputTokens: r.Usage.Out}
	}
	return resp, nil
}

// LoadScript reads a script: a JSON array of Response objects, for example
//
//	[{"tool_requests":[{"name":"describe","input":{"connection":"dev","collection":"orders"}}]},
//	 {"text":"There are **3** orders.","usage":{"input_tokens":120,"output_tokens":12}}]
func LoadScript(path string) ([]Response, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("aitest: read script: %w", err)
	}
	var rs []Response
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("aitest: parse script %s: %w", path, err)
	}
	return rs, nil
}

// Plugin registers Fake at genkit.Init as "fake/<Model>".
type Plugin struct {
	Model string
	Fake  *Model
}

// Name is the provider name, "fake".
func (p *Plugin) Name() string { return Provider }

// Init returns the fake model's action.
func (p *Plugin) Init(context.Context) []api.Action {
	return []api.Action{p.Fake.action(Provider + "/" + p.Model)}
}
