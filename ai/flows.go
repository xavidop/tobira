package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/ai/schemactx"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/federate"

	gai "github.com/firebase/genkit/go/ai"
)

// dialectFederated is the Dialect of a proposal that is a cross-source
// statement: @source directives, then SQLite SQL over the alias tables.
const dialectFederated = "federated"

// The flow names, as the Genkit Dev UI lists them.
const (
	flowAsk            = "askFlow"
	flowAnswer         = "answerFlow"
	flowExplainResults = "explainResultsFlow"
	flowFixError       = "fixErrorFlow"
)

// Defaults for settings a zero config.AI leaves empty (config.Load fills
// them in; tests may not).
const (
	defaultMaxToolCalls  = 8
	defaultContextTokens = 8000
)

// flowSet is the flows of one Genkit instance. Each takes its request,
// scope included, and returns its result; the callbacks and the Toolset
// travel on the context (see withTurn), so a flow run from the Dev UI works
// too, with no callbacks and a Toolset of its own.
type flowSet struct {
	ask     *core.Flow[askInput, proposalOutput, struct{}]
	answer  *core.Flow[answerInput, answerOutput, struct{}]
	explain *core.Flow[explainInput, answerOutput, struct{}]
	fix     *core.Flow[fixErrorInput, proposalOutput, struct{}]
}

// setup registers the prompts and the flows on g. Genkit reports some
// definition problems by panicking; they become errors.
func (a *genkitAssistant) setup(g *genkit.Genkit) (fs *flowSet, err error) {
	defer func() {
		if r := recover(); r != nil {
			fs, err = nil, fmt.Errorf("%w: register the flows: %v", assistant.ErrProvider, r)
		}
	}()
	prompts, err := registerPrompts(g, a.prompts)
	if err != nil {
		return nil, err
	}
	f := &flows{a: a, g: g, prompts: prompts}
	return &flowSet{
		ask:     genkit.DefineFlow(g, flowAsk, f.ask),
		answer:  genkit.DefineFlow(g, flowAnswer, f.answer),
		explain: genkit.DefineFlow(g, flowExplainResults, f.explainResults),
		fix:     genkit.DefineFlow(g, flowFixError, f.fixError),
	}, nil
}

// flows holds what the flow functions share.
type flows struct {
	a       *genkitAssistant
	g       *genkit.Genkit
	prompts map[string]gai.Prompt
	nonce   func() string // draws a data-fence nonce; nil means newNonce
}

// schemaInfo is the schema context of a scope, ready for a prompt.
type schemaInfo struct {
	dialect, kind, syntax, text string
	// federated says the conversation spans several connections: the
	// prompts then describe cross-source statements, and conns lists the
	// connections with what a query is for each.
	federated bool
	conns     []schemactx.ConnectionInfo
}

// schema builds the scope's schema context, redacted for its connections
// and against the provider key. A scope it cannot use (no connections, an
// unknown connection, one that fails to open) is assistant.ErrScope.
func (f *flows) schema(ctx context.Context, scope assistant.Scope) (schemaInfo, error) {
	if f.a.deps.Manager == nil {
		return schemaInfo{}, fmt.Errorf("%w: no connections", assistant.ErrScope)
	}
	budget := f.a.cfg.ContextTokens
	if budget <= 0 {
		budget = defaultContextTokens
	}
	sc, conns, err := schemactx.BuildAll(ctx, f.a.deps.Manager, scope, budget)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return schemaInfo{}, cerr
		}
		return schemaInfo{}, fmt.Errorf("%w: %s", assistant.ErrScope, f.scrubAll(scope, err.Error()))
	}
	return schemaInfo{
		dialect: sc.Dialect, kind: sc.Kind, syntax: sc.Syntax, text: f.scrubAll(scope, sc.Text),
		federated: len(scope.Connections()) > 1, conns: conns,
	}, nil
}

// scrubAll redacts s for every connection of scope and against the key.
func (f *flows) scrubAll(scope assistant.Scope, s string) string {
	for _, c := range scope.Connections() {
		s = f.a.scrub(c, s)
	}
	return f.a.scrub(scope.Conn, s)
}

// vars are the prompt variables every flow shares: the schema context and,
// for a conversation that spans several connections, the cross-source
// facts (federated, connections, max_source_rows). federated is also set
// for a single connection when the statement under discussion is already a
// cross-source statement (fix_error).
func (f *flows) vars(sc schemaInfo, federated bool) map[string]any {
	vars := map[string]any{"dialect": sc.dialect, "kind": sc.kind, "syntax": sc.syntax, "schema": sc.text}
	if !sc.federated && !federated {
		return vars
	}
	conns := make([]map[string]any, 0, len(sc.conns))
	for _, c := range sc.conns {
		conns = append(conns, map[string]any{"name": c.Name, "dialect": c.Dialect, "kind": c.Kind, "syntax": c.Syntax})
	}
	maxRows := f.a.deps.MaxSourceRows
	if maxRows <= 0 {
		maxRows = federate.DefaultMaxSourceRows
	}
	vars["federated"], vars["connections"], vars["max_source_rows"] = true, conns, maxRows
	return vars
}

// proposalDialect is the Dialect of a proposal: federated when the
// conversation spans several connections and the model wrote a cross-source
// statement, else the primary connection's.
func proposalDialect(sc schemaInfo, query string) string {
	if federate.IsFederated(query) {
		return dialectFederated
	}
	return sc.dialect
}

// render renders a prompt into the conversation and the request settings
// it declares (its output schema and config; its model is ignored). The
// input gets a fresh data-fence nonce and its Dotprompt markers broken
// (see fence).
func (f *flows) render(ctx context.Context, name string, input map[string]any, offered []gai.Tool) ([]*gai.Message, request, error) {
	next := f.nonce
	if next == nil {
		next = newNonce
	}
	opts, err := f.prompts[name].Render(ctx, fence(input, next))
	if err != nil {
		return nil, request{}, fmt.Errorf("%w: render prompt %s: %s", assistant.ErrProvider, name, f.a.scrub("", err.Error()))
	}
	req := request{tools: offered, config: opts.Config}
	if opts.Output != nil && opts.Output.JsonSchema != nil {
		req.output = opts.Output.JsonSchema
	}
	return opts.Messages, req, nil
}

func (f *flows) maxToolCalls() int {
	if n := f.a.cfg.MaxToolCalls; n > 0 {
		return n
	}
	return defaultMaxToolCalls
}

// schemaTools are the tools that look up schema: offered to ask and fix
// only when no collection is in scope.
func schemaTools(ts *tools.Toolset, scope assistant.Scope) []gai.Tool {
	if scope.Collection != "" {
		return nil
	}
	var out []gai.Tool
	for _, ref := range ts.Read() {
		switch ref.Name() {
		case tools.NameListNamespaces, tools.NameListCollections, tools.NameDescribe:
			out = append(out, ref.(gai.Tool))
		}
	}
	return out
}

func asTools(refs []gai.ToolRef) []gai.Tool {
	out := make([]gai.Tool, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.(gai.Tool))
	}
	return out
}

// proposal is the structured answer of ask and fix_error.
type proposal struct {
	Query       string `json:"query"`
	Explanation string `json:"explanation"`
}

func parseProposal(resp *gai.ModelResponse) (proposal, bool) {
	var p proposal
	if resp == nil || resp.Output(&p) != nil || strings.TrimSpace(p.Query) == "" {
		return proposal{}, false
	}
	return p, true
}

// propose runs a structured turn (ask, fix_error): the tool loop, then the
// {query, explanation} answer. An answer that does not parse is retried
// once, with no tools, with a reminder to reply with JSON only; a second
// failure is errInvalidAnswer.
func (f *flows) propose(ctx context.Context, t *turn, msgs []*gai.Message, req request) (proposal, error) {
	res, err := t.loop(ctx, msgs, req, f.maxToolCalls())
	if err != nil && !errors.Is(err, errInvalidOutput) {
		return proposal{}, err
	}
	if p, ok := parseProposal(res.resp); ok && err == nil {
		return p, nil
	}
	// The retry offers no tools and sends no tool request it leaves
	// unanswered (the tool-less call after the cap may still ask for one).
	msgs = res.msgs
	if res.resp != nil {
		if m := withoutToolRequests(res.resp.Message); m != nil {
			msgs = append(msgs, m)
		}
	}
	msgs = append(msgs, gai.NewUserTextMessage(nudgeJSON))
	retry := req
	retry.tools = nil
	resp, err := t.generate(ctx, msgs, retry)
	if err != nil && !errors.Is(err, errInvalidOutput) {
		return proposal{}, err
	}
	p, ok := parseProposal(resp)
	if !ok || err != nil {
		return proposal{}, errInvalidAnswer
	}
	return p, nil
}

func (f *flows) ask(ctx context.Context, in askInput) (proposalOutput, error) {
	ctx, t := f.a.newTurn(ctx, f.g, in.Scope)
	out, err := f.askTurn(ctx, t, in)
	return out, t.fail(ctx, err)
}

func (f *flows) askTurn(ctx context.Context, t *turn, in askInput) (proposalOutput, error) {
	scope := in.Scope.scope()
	sc, err := f.schema(ctx, scope)
	if err != nil {
		return proposalOutput{}, err
	}
	offered := schemaTools(t.ts, scope)
	vars := f.vars(sc, false)
	vars["connection"] = scope.Conn
	vars["question"] = f.a.scrub(scope.Conn, in.Question)
	vars["history"] = f.a.history(scope.Conn, turns(in.History))
	vars["tools"] = len(offered) > 0
	msgs, r, err := f.render(ctx, promptAsk, vars, offered)
	if err != nil {
		return proposalOutput{}, err
	}
	p, err := f.propose(ctx, t, msgs, r)
	if err != nil {
		return proposalOutput{}, err
	}
	return proposalOutput{Query: p.Query, Explanation: p.Explanation, Dialect: proposalDialect(sc, p.Query), Usage: toFlowUsage(t.finish())}, nil
}

func (f *flows) fixError(ctx context.Context, in fixErrorInput) (proposalOutput, error) {
	ctx, t := f.a.newTurn(ctx, f.g, in.Scope)
	out, err := f.fixErrorTurn(ctx, t, in)
	return out, t.fail(ctx, err)
}

func (f *flows) fixErrorTurn(ctx context.Context, t *turn, in fixErrorInput) (proposalOutput, error) {
	scope := in.Scope.scope()
	sc, err := f.schema(ctx, scope)
	if err != nil {
		return proposalOutput{}, err
	}
	// The error may quote row values: it is sent as the row policy allows
	// (asking under ask), else in the scrubbed form a tool error gets.
	errText, err := t.ts.ErrorText(ctx, scope.Conn, in.err, in.Error)
	if err != nil {
		return proposalOutput{}, err
	}
	offered := schemaTools(t.ts, scope)
	// A failed cross-source statement is fixed as one, whatever the scope.
	vars := f.vars(sc, federate.IsFederated(in.Query))
	vars["query"] = f.scrubAll(scope, in.Query)
	vars["error"] = f.scrubAll(scope, errText)
	vars["tools"] = len(offered) > 0
	msgs, r, err := f.render(ctx, promptFixError, vars, offered)
	if err != nil {
		return proposalOutput{}, err
	}
	p, err := f.propose(ctx, t, msgs, r)
	if err != nil {
		return proposalOutput{}, err
	}
	return proposalOutput{Query: p.Query, Explanation: p.Explanation, Dialect: proposalDialect(sc, p.Query), Usage: toFlowUsage(t.finish())}, nil
}

func (f *flows) answer(ctx context.Context, in answerInput) (answerOutput, error) {
	ctx, t := f.a.newTurn(ctx, f.g, in.Scope)
	out, err := f.answerTurn(ctx, t, in)
	return out, t.fail(ctx, err)
}

func (f *flows) answerTurn(ctx context.Context, t *turn, in answerInput) (answerOutput, error) {
	scope := in.Scope.scope()
	sc, err := f.schema(ctx, scope)
	if err != nil {
		return answerOutput{}, err
	}
	offered := asTools(t.ts.Read())
	// similar only where it can run: the turn's connection searches vectors.
	if t.ts.VectorSearchable(ctx) {
		offered = append(offered, t.ts.SimilarTool().(gai.Tool))
	}
	// federate only where it has something to join: several connections.
	if t.ts.Federatable(ctx) {
		offered = append(offered, t.ts.FederateTool().(gai.Tool))
	}
	if in.AllowExecute {
		offered = append(offered, t.ts.Execute().(gai.Tool))
		ctx = tools.WithExecute(ctx)
	}
	vars := f.vars(sc, false)
	vars["connection"] = scope.Conn
	vars["question"] = f.a.scrub(scope.Conn, in.Question)
	vars["history"] = f.a.history(scope.Conn, turns(in.History))
	vars["execute"] = in.AllowExecute
	msgs, r, err := f.render(ctx, promptAnswer, vars, offered)
	if err != nil {
		return answerOutput{}, err
	}
	res, err := t.loop(ctx, msgs, r, f.maxToolCalls())
	if err != nil {
		return answerOutput{}, err
	}
	md := res.resp.Text()
	if res.capped && strings.TrimSpace(md) == "" {
		md = capFallback(f.maxToolCalls(), res.ran)
	}
	out := answerOutput{Markdown: md, Usage: toFlowUsage(t.finish())}
	for _, q := range res.ran {
		out.Ran = append(out.Ran, flowRan{Conn: q.Conn, Query: q.Query, Rows: q.Rows, Error: q.Err})
	}
	return out, nil
}

func (f *flows) explainResults(ctx context.Context, in explainInput) (answerOutput, error) {
	ctx, t := f.a.newTurn(ctx, f.g, in.Scope)
	out, err := f.explainResultsTurn(ctx, t, in)
	return out, t.fail(ctx, err)
}

func (f *flows) explainResultsTurn(ctx context.Context, t *turn, in explainInput) (answerOutput, error) {
	scope := in.Scope.scope()
	sc, err := f.schema(ctx, scope)
	if err != nil {
		return answerOutput{}, err
	}
	cols, rows := in.result()
	res, err := t.ts.ResultRows(ctx, scope.Conn, in.Query, cols, rows, in.Truncated)
	if err != nil {
		return answerOutput{}, err
	}
	colJSON, _ := json.Marshal(res.Columns) // plain strings: cannot fail
	vars := map[string]any{
		"dialect": sc.dialect, "kind": sc.kind, "schema": sc.text,
		"query":     f.a.scrub(scope.Conn, in.Query),
		"columns":   string(colJSON),
		"truncated": res.Truncated,
		"note":      res.Note,
	}
	if res.Rows != nil {
		vars["rows"] = string(res.Rows)
	}
	msgs, r, err := f.render(ctx, promptExplainResults, vars, nil)
	if err != nil {
		return answerOutput{}, err
	}
	resp, err := t.generate(ctx, msgs, r)
	if err != nil {
		return answerOutput{}, err
	}
	return answerOutput{Markdown: resp.Text(), Usage: toFlowUsage(t.finish())}, nil
}
