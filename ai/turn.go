package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/firebase/genkit/go/core/status"
	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/internal/assistant"

	gai "github.com/firebase/genkit/go/ai"
)

// Limits on text sent to the model: the panel's transcript sent with a
// question, and the error of a refused tool call.
const (
	maxHistoryTurns = 20
	maxTurnRunes    = 4000
	// maxToolErrorRunes cuts the text of a refused tool call (tools cut
	// their own errors at the same length).
	maxToolErrorRunes = 1000
)

// The messages a turn adds to steer the model.
const (
	nudgeJSON = `Your last reply was not valid JSON. Reply again with JSON only, exactly {"query": "...", "explanation": "..."}, and nothing else.`
	nudgeStop = "You have reached the limit of %d tool calls for this question. Do not call more tools: answer now with what you have."
)

// errInvalidAnswer is a structured answer the model failed to give twice.
var errInvalidAnswer = fmt.Errorf("%w: the model returned an invalid answer", assistant.ErrProvider)

// errInvalidOutput marks a response Genkit could not parse against the
// output schema; the turn may retry.
var errInvalidOutput = errors.New("output does not match the schema")

// turnKey carries a turn's callbacks and Toolset through the flow, whose
// input and output are plain values: Events (functions) cannot cross the
// flow boundary as data, so the Assistant methods put them on the context.
type turnKey struct{}

type turnValues struct {
	ev assistant.Events
	ts *tools.Toolset
}

// withTurn puts the turn's callbacks and Toolset on ctx.
func withTurn(ctx context.Context, ev assistant.Events, ts *tools.Toolset) context.Context {
	return context.WithValue(ctx, turnKey{}, turnValues{ev: ev, ts: ts})
}

// turn is one call of a flow: its callbacks, tools and summed usage.
type turn struct {
	a     *genkitAssistant
	g     *genkit.Genkit
	ev    assistant.Events
	ts    *tools.Toolset
	start time.Time

	mu         sync.Mutex
	usage      assistant.Usage
	modelCalls int
	calls      []assistant.ToolCall // reported by the tools, in order
}

// newTurn starts a turn on scope's connection from the values withTurn put
// on ctx. A flow run from the Dev UI has none: it gets no callbacks (so row
// consent is no, and execute is refused) and a Toolset of its own. The
// returned context makes the tools report to the turn and binds them to
// the scope's connection: the conversation is about that connection, so a
// tool naming any other is refused as unknown.
func (a *genkitAssistant) newTurn(ctx context.Context, g *genkit.Genkit, scope flowScope) (context.Context, *turn) {
	v, _ := ctx.Value(turnKey{}).(turnValues)
	if v.ts == nil {
		v.ts = a.newToolset()
	}
	t := &turn{a: a, g: g, ev: v.ev, ts: v.ts, start: time.Now()}
	toolEvents := v.ev
	toolEvents.ToolCall = t.recordCall
	ctx = tools.WithConnections(ctx, scope.Conn)
	return tools.WithEvents(ctx, toolEvents), t
}

// recordCall keeps a tool's report for Ran and forwards it.
func (t *turn) recordCall(tc assistant.ToolCall) {
	t.mu.Lock()
	t.calls = append(t.calls, tc)
	t.mu.Unlock()
	t.ev.OnToolCall(tc)
}

// refused reports a tool request that did not run (not offered, over the
// cap, or with an input that does not fit the tool) like any tool call:
// redacted, the input cut like a tool's, no rows.
func (t *turn) refused(conn string, tr *gai.ToolRequest, msg string) {
	in := cut(t.a.scrub(conn, compactJSON(tr.Input)), tools.MaxValueRunes)
	t.recordCall(assistant.ToolCall{Name: t.a.scrub(conn, tr.Name), Input: in, Rows: -1, Err: msg})
}

func (t *turn) lastCall(name string, after int) (assistant.ToolCall, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.calls) - 1; i >= after; i-- {
		if t.calls[i].Name == name {
			return t.calls[i], true
		}
	}
	return assistant.ToolCall{}, false
}

func (t *turn) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

// finish reports the turn's usage: the tokens of every model call and the
// turn's latency.
func (t *turn) finish() assistant.Usage {
	t.mu.Lock()
	u := t.usage
	t.mu.Unlock()
	u.Model = t.a.model
	u.Latency = time.Since(t.start)
	t.ev.OnUsage(u)
	return u
}

// fail returns err for a turn that failed. Once the turn has called the
// model, the usage it spent is reported through Events.Usage and err comes
// back as an assistant.TurnError carrying it; a cancelled turn reports
// nothing and returns err as is.
func (t *turn) fail(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	t.mu.Lock()
	called := t.modelCalls > 0
	t.mu.Unlock()
	if !called {
		return err
	}
	return &assistant.TurnError{Err: err, Usage: t.finish()}
}

// request is what one generate call sends besides the conversation.
type request struct {
	tools  []gai.Tool
	output map[string]any // JSON schema; nil for text
	config any
}

// generate is one model call. Tool requests come back to the caller (the
// loop runs them). A response that fails the output schema returns with
// errInvalidOutput; any other failure is a redacted provider error, or the
// turn's cancellation.
func (t *turn) generate(ctx context.Context, msgs []*gai.Message, req request) (*gai.ModelResponse, error) {
	opts := []gai.GenerateOption{gai.WithModelName(t.a.model), gai.WithMessages(msgs...), gai.WithReturnToolRequests(true)}
	if len(req.tools) > 0 {
		refs := make([]gai.ToolRef, len(req.tools))
		for i, tool := range req.tools {
			refs[i] = tool
		}
		opts = append(opts, gai.WithTools(refs...))
	}
	if req.output != nil {
		opts = append(opts, gai.WithOutputSchema(req.output))
	}
	if req.config != nil {
		opts = append(opts, gai.WithConfig(req.config))
	}
	resp, err := genkit.Generate(ctx, t.g, opts...)
	t.mu.Lock()
	t.modelCalls++
	if resp != nil && resp.Usage != nil {
		t.usage.InputTokens += resp.Usage.InputTokens
		t.usage.OutputTokens += resp.Usage.OutputTokens
	}
	t.mu.Unlock()
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		if errors.Is(err, status.ErrInvalidOutput) && resp != nil && resp.Message != nil {
			return resp, errInvalidOutput
		}
		return nil, wrapProviderError(err, t.a.key)
	}
	return resp, nil
}

// loopResult is how a tool loop ended.
type loopResult struct {
	resp   *gai.ModelResponse   // the final response
	msgs   []*gai.Message       // the conversation before resp
	ran    []assistant.RanQuery // the statements the tools ran, or refused to
	capped bool                 // the loop hit the tool-call cap
}

// loop calls the model until it stops requesting tools. It runs each tool
// request itself through the offered tools, at most maxCalls per turn;
// once the cap is reached it asks once more with no tools offered, telling
// the model to answer with what it has. A request for a tool that was not
// offered gets a tool error back, as does a failing tool; only the turn's
// cancellation ends the loop early. An execute request that is refused (not
// offered, or over the cap) is listed in ran with Err "refused".
func (t *turn) loop(ctx context.Context, msgs []*gai.Message, req request, maxCalls int) (loopResult, error) {
	offered := map[string]gai.Tool{}
	for _, tool := range req.tools {
		offered[tool.Name()] = tool
	}
	res := loopResult{msgs: msgs}
	calls := 0
	for {
		resp, err := t.generate(ctx, res.msgs, req)
		res.resp = resp
		if err != nil {
			return res, err
		}
		reqs := resp.ToolRequests()
		if len(reqs) == 0 {
			return res, nil
		}
		res.msgs = append(res.msgs, resp.Message)
		var parts []*gai.Part
		for _, p := range reqs {
			tr := p.ToolRequest
			var out any
			var q *assistant.RanQuery
			if calls >= maxCalls {
				msg := fmt.Sprintf("tool call limit (%d) reached: not run", maxCalls)
				t.refused("", tr, msg)
				out, q = toolError(msg), t.refusedExecute(tr)
			} else {
				calls++
				out, q, err = t.runTool(ctx, offered, tr)
				if err != nil {
					res.resp = nil
					return res, err
				}
			}
			if q != nil {
				res.ran = append(res.ran, *q)
			}
			parts = append(parts, gai.NewToolResponsePart(&gai.ToolResponse{Name: tr.Name, Ref: tr.Ref, Output: out}))
		}
		res.msgs = append(res.msgs, gai.NewMessage(gai.RoleTool, nil, parts...))
		if calls >= maxCalls {
			res.msgs = append(res.msgs, gai.NewUserTextMessage(fmt.Sprintf(nudgeStop, maxCalls)))
			final := req
			final.tools = nil
			res.capped = true
			res.resp, err = t.generate(ctx, res.msgs, final)
			return res, err
		}
	}
}

// refusedExecute is the Ran entry of an execute request that did not run
// because it was refused; nil for any other tool.
func (t *turn) refusedExecute(tr *gai.ToolRequest) *assistant.RanQuery {
	if tr.Name != tools.NameExecute {
		return nil
	}
	args := parseToolArgs(tr)
	return &assistant.RanQuery{Conn: args.Connection, Query: t.a.scrub(args.Connection, args.Statement), Rows: -1, Err: "refused"}
}

// withoutToolRequests returns m without its tool-request parts, or nil when
// nothing else is left: a request the conversation never answers must not
// be sent back (providers reject a tool call with no result).
func withoutToolRequests(m *gai.Message) *gai.Message {
	if m == nil {
		return nil
	}
	var keep []*gai.Part
	for _, p := range m.Content {
		if !p.IsToolRequest() {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return gai.NewMessage(m.Role, m.Metadata, keep...)
}

// capFallback is the answer of a turn that hit the tool-call cap and then
// gave no text: the statements it ran, or that were refused, each as an
// inline code span so markdown does not render * or _ in them.
func capFallback(maxCalls int, ran []assistant.RanQuery) string {
	var b strings.Builder
	fmt.Fprintf(&b, "I reached the tool-call limit (%d) before finishing. Tool calls I made:\n", maxCalls)
	if len(ran) == 0 {
		b.WriteString("\n- none\n")
	}
	for _, q := range ran {
		switch {
		case q.Err != "":
			fmt.Fprintf(&b, "\n- %s: %s (error: %s)", q.Conn, codeSpan(q.Query), q.Err)
		case q.Rows == 1:
			fmt.Fprintf(&b, "\n- %s: %s (1 row)", q.Conn, codeSpan(q.Query))
		default:
			fmt.Fprintf(&b, "\n- %s: %s (%d rows)", q.Conn, codeSpan(q.Query), q.Rows)
		}
	}
	return b.String()
}

// codeSpan renders s as a markdown inline code span: fenced by one more
// backtick than its longest backtick run, padded with spaces when s starts
// or ends with a backtick. Newlines become spaces.
func codeSpan(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// toolError is the result the model gets for a call that did not run.
func toolError(msg string) map[string]any { return map[string]any{"error": msg} }

// toolArgs are the inputs of the tools that run statements.
type toolArgs struct {
	Connection string `json:"connection"`
	Namespace  string `json:"namespace"`
	Collection string `json:"collection"`
	Query      string `json:"query"`
	Statement  string `json:"statement"`
}

// parseToolArgs reads the inputs of a statement tool from tr, best effort:
// they are only for Ran and reports.
func parseToolArgs(tr *gai.ToolRequest) toolArgs {
	var args toolArgs
	if b, err := json.Marshal(tr.Input); err == nil {
		_ = json.Unmarshal(b, &args)
	}
	return args
}

// runTool runs one tool request. A tool that was not offered is refused
// with a tool error, reported like any call; execute is refused unless the
// request allowed it. It returns the output for the model and, for the
// tools that run statements, the query for Ran (a refused execute too,
// with Err "refused"). The only error is the turn's cancellation.
func (t *turn) runTool(ctx context.Context, offered map[string]gai.Tool, tr *gai.ToolRequest) (any, *assistant.RanQuery, error) {
	tool, ok := offered[tr.Name]
	if !ok {
		msg := fmt.Sprintf("unknown tool %q", cut(t.a.scrub("", tr.Name), 64))
		if tr.Name == tools.NameExecute {
			msg = "execute is not available: this request may not change data"
		}
		t.refused("", tr, msg)
		return toolError(msg), t.refusedExecute(tr), nil
	}
	before := t.callCount()
	out, err := tool.RunRaw(ctx, tr.Input)
	if cerr := ctx.Err(); cerr != nil {
		return nil, nil, cerr
	}
	args := parseToolArgs(tr)
	if err != nil {
		// The input did not fit the tool's schema: the tool never ran.
		msg := cut(t.a.scrub(args.Connection, "invalid input: "+err.Error()), maxToolErrorRunes)
		t.refused(args.Connection, tr, msg)
		return toolError(msg), nil, nil
	}
	var text string
	switch tr.Name {
	case tools.NameQuery:
		text = args.Query
	case tools.NameExecute:
		text = args.Statement
	case tools.NameExplain:
		text = "explain " + args.Query
	case tools.NameSample:
		text = "sample " + args.Collection
		if args.Namespace != "" {
			text = "sample " + args.Namespace + "." + args.Collection
		}
	default:
		return out, nil, nil
	}
	q := &assistant.RanQuery{Conn: args.Connection, Query: t.a.scrub(args.Connection, text), Rows: -1}
	if tc, ok := t.lastCall(tr.Name, before); ok {
		q.Rows, q.Err = tc.Rows, tc.Err
	}
	return out, q, nil
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// history converts the panel's transcript for a prompt: the last
// maxHistoryTurns turns, each redacted, then cut to maxTurnRunes (in that
// order, so a secret across the cut is masked whole).
func (a *genkitAssistant) history(conn string, turns []assistant.Turn) []map[string]any {
	if len(turns) > maxHistoryTurns {
		turns = turns[len(turns)-maxHistoryTurns:]
	}
	out := make([]map[string]any, 0, len(turns))
	for _, tn := range turns {
		from := "user"
		if tn.Role == "model" {
			from = "assistant"
		}
		out = append(out, map[string]any{"from": from, "text": cut(a.scrub(conn, tn.Text), maxTurnRunes)})
	}
	return out
}

// cut keeps the first n runes of s, marking a cut with "…".
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// scrub masks conn's credentials and the provider key in s, for a prompt,
// an event or an error. (render fences prompt data and breaks Dotprompt
// markers in every prompt input; see fence.)
func (a *genkitAssistant) scrub(conn, s string) string {
	if a.deps.Manager != nil {
		s = a.deps.Manager.Redact(conn, s)
	}
	return redact(s, a.key)
}
