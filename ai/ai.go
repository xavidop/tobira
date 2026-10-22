// Package ai is tobira's Genkit implementation of assistant.Assistant.
//
// It is a separate module because Genkit's go.mod requires database SDKs
// for its optional plugins, which the root module must never depend on.
// cmd/tobira builds an Assistant here from the ai: config and injects it
// into the TUI and the CLI, which only know internal/assistant.
//
// Each Assistant method runs a Genkit flow (askFlow, answerFlow,
// explainResultsFlow, fixErrorFlow) over a Dotprompt prompt from prompts/,
// which a file of the same name in Deps.PromptDir overrides. A flow's
// request and result are plain values; the turn's assistant.Events and the
// session's Toolset travel on the context, since functions cannot cross the
// flow boundary as data. A flow run from the Genkit Dev UI (`genkit start --
// tobira ai dev`) therefore has no callbacks: row consent is no and execute
// is refused.
//
// answerFlow runs its tool loop itself (Genkit returns the tool requests),
// so the ai.max_tool_calls cap, the refusal of tools that were not offered
// and the ToolCall events stay under tobira's control. Every text that goes
// into a prompt is redacted for its connection and against the provider
// key. The prompts wrap data in fences named by a random per-render nonce,
// and Dotprompt's message markers are broken in every input, so data can
// neither close its fence nor open a message (see fence). Row data, and
// the driver error that fix_error sends, obey ai.send_rows.
//
// Errors: an unusable scope is assistant.ErrScope; a turn that fails after
// calling the model returns an assistant.TurnError with the usage it spent
// (also reported through Events.Usage); a cancelled turn returns the
// context's error.
package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// Deps are the collaborators of the assistant.
type Deps struct {
	// Manager opens the connections the tools query.
	Manager *session.Manager
	// History records tool-run queries; nil records nothing.
	History *history.Store
	// Secrets resolves the ai.api_key ref.
	Secrets secrets.Resolver
	// Logger receives debug logs; nil discards them.
	Logger *slog.Logger
	// Getenv reads the provider key variables; nil means os.Getenv.
	Getenv func(string) string
	// DevMode leaves Genkit's environment (GENKIT_ENV, telemetry, the
	// reflection server) as the user set it, and initialises Genkit at once
	// so the flows are registered for the Dev UI. Only the hidden
	// `tobira ai dev` command sets it; everything else runs Genkit pinned to
	// prod.
	DevMode bool
	// PromptDir holds prompt overrides: a <name>.prompt file there
	// replaces the built-in prompt of that name. "" means none.
	PromptDir string
}

// genkitAssistant implements assistant.Assistant on Genkit.
type genkitAssistant struct {
	cfg   config.AI
	deps  Deps
	model string        // provider-qualified, "googleai/gemini-3.8-flash"
	key   driver.Secret // the resolved API key; zero for vertexai and ollama

	prompts map[string]promptSource // the prompt texts, overrides applied

	initFn  func(context.Context) (*genkit.Genkit, error) // nil means initGenkit; tests replace it
	mu      sync.Mutex                                    // guards g, flows and initErr; held during init
	g       *genkit.Genkit
	flows   *flowSet
	initErr error
	// inited is set once init has finished (successfully or not), so
	// initialised can read it without waiting on mu while an init runs.
	inited atomic.Bool
}

// New builds the assistant for cfg. It resolves the API key (ai.api_key
// through deps.Secrets, else the provider's environment variable) and
// initialises Genkit with the provider plugin. Initialisation is local: no
// plugin call here touches the network, except Vertex AI's credential
// detection, which may probe the GCE metadata server and so is deferred to
// the first request (unless deps.DevMode). Errors wrap assistant.ErrAuth or
// ErrProvider and are redacted against the key. An invalid prompt override
// is an error naming the file.
//
// The Assistant it returns also implements assistant.Sessioner.
func New(ctx context.Context, cfg config.AI, deps Deps) (assistant.Assistant, error) {
	if deps.Getenv == nil {
		deps.Getenv = os.Getenv
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	p, ok := lookupProvider(cfg.Provider)
	if !ok {
		return nil, fmt.Errorf("%w: unknown provider", assistant.ErrProvider)
	}
	if cfg.Model == "" {
		cfg.Model = p.defaultModel
	}
	key, err := resolveKey(ctx, cfg, deps)
	if err != nil {
		return nil, err
	}
	prompts, err := loadPromptSources(deps.PromptDir, deps.Logger)
	if err != nil {
		return nil, err
	}
	a := &genkitAssistant{cfg: cfg, deps: deps, model: cfg.Provider + "/" + cfg.Model, key: key, prompts: prompts}
	if !p.deferInit || deps.DevMode {
		if _, err := a.genkit(ctx); err != nil {
			return nil, err
		}
	}
	deps.Logger.Debug("ai configured", "model", a.model, "deferred_init", p.deferInit)
	return a, nil
}

// genkit returns the Genkit instance, initialising it on first use and
// registering the prompts and flows on it. Init runs detached from ctx's
// cancellation (Genkit keeps the context for the life of the instance, and
// a deferred init runs on the first request), and concurrent callers wait
// for it. A failed init is remembered and returned again, except one caused
// by a context error, which is retried.
func (a *genkitAssistant) genkit(ctx context.Context) (*genkit.Genkit, error) {
	g, _, err := a.ready(ctx)
	return g, err
}

// ready is genkit, with the flows.
func (a *genkitAssistant) ready(ctx context.Context) (*genkit.Genkit, *flowSet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.g != nil || a.initErr != nil {
		return a.g, a.flows, a.initErr
	}
	initFn := a.initFn
	if initFn == nil {
		initFn = a.initGenkit
	}
	g, err := initFn(context.WithoutCancel(ctx))
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil, nil, err
	}
	var fs *flowSet
	if err == nil {
		fs, err = a.setup(g)
	}
	if err != nil {
		g = nil
	}
	a.g, a.flows, a.initErr = g, fs, err
	a.inited.Store(true)
	return g, fs, err
}

// initialised reports whether Genkit has been initialised (successfully or
// not). It never takes mu, so it does not block behind an init in progress.
func (a *genkitAssistant) initialised() bool { return a.inited.Load() }

// genkitEnvMu serialises the environment pinning around genkit.Init.
var genkitEnvMu sync.Mutex

// pinGenkitProd makes Genkit run in prod mode whatever the user's shell
// says. Genkit reads its mode from the process environment only: with an
// inherited GENKIT_ENV=dev, genkit.Init starts an unauthenticated
// reflection server (or dials GENKIT_REFLECTION_V2_SERVER) that can invoke
// every registered flow and tool, and GENKIT_TELEMETRY_SERVER exports
// traces, which carry prompts and row data, whatever the mode. The
// variables are changed for the whole process and not restored, because
// Genkit reads them again after Init (tracing, on the first request).
func pinGenkitProd() {
	_ = os.Setenv("GENKIT_ENV", "prod")
	_ = os.Unsetenv("GENKIT_TELEMETRY_SERVER")
	_ = os.Unsetenv("GENKIT_REFLECTION_V2_SERVER")
}

// initGenkit runs genkit.Init with the provider plugin. Genkit plugins
// report setup problems by panicking; any panic while building the plugin,
// initialising it or defining models becomes a redacted ErrProvider.
func (a *genkitAssistant) initGenkit(ctx context.Context) (g *genkit.Genkit, err error) {
	defer func() {
		if r := recover(); r != nil {
			g, err = nil, wrapProviderError(fmt.Errorf("initialise %s: %v", a.cfg.Provider, r), a.key)
		}
	}()
	p, _ := lookupProvider(a.cfg.Provider)
	plugin := p.plugin(a.cfg, a.key, a.deps.Getenv)
	genkitEnvMu.Lock()
	defer genkitEnvMu.Unlock()
	if !a.deps.DevMode {
		pinGenkitProd()
	}
	g = genkit.Init(ctx,
		genkit.WithPlugins(plugin),
		genkit.WithPromptFS(noPromptFS),
		genkit.WithPromptDir("."),
		genkit.WithDefaultModel(a.model),
	)
	if p.afterInit != nil {
		p.afterInit(g, plugin, a.cfg)
	}
	return g, nil
}

// NewToolHost initialises a Genkit instance with no model plugin and no
// prompts, pinned to prod like the assistant's: an instance that only hosts
// tools (the MCP server's). It makes no provider call and needs no ai:
// section. Init runs detached from ctx's cancellation, as for the assistant.
func NewToolHost(ctx context.Context) (g *genkit.Genkit, err error) {
	defer func() {
		if r := recover(); r != nil {
			g, err = nil, fmt.Errorf("initialise genkit: %v", r)
		}
	}()
	genkitEnvMu.Lock()
	defer genkitEnvMu.Unlock()
	pinGenkitProd()
	return genkit.Init(context.WithoutCancel(ctx),
		genkit.WithPromptFS(noPromptFS),
		genkit.WithPromptDir("."),
	), nil
}

func (a *genkitAssistant) Model() string { return a.model }

// newToolset builds the tools of one session over the assistant's
// connections, with the provider key masked in everything they return.
func (a *genkitAssistant) newToolset() *tools.Toolset {
	d := tools.ToolDeps{
		Manager: a.deps.Manager,
		Policy:  tools.RowPolicy(a.cfg.SendRows),
		MaxRows: a.cfg.MaxRows,
		Redact:  func(s string) string { return redact(s, a.key) },
	}
	if a.deps.History != nil {
		d.History = a.deps.History // never a typed nil in the interface
	}
	return tools.NewToolset(d)
}

// NewSession returns a session whose calls share one Toolset, so row
// consent given for the session spans its turns.
func (a *genkitAssistant) NewSession() assistant.Session {
	return &assistantSession{a: a, ts: a.newToolset()}
}

// Each Assistant method is a session of its own: row consent does not
// outlive the call.
func (a *genkitAssistant) Ask(ctx context.Context, req assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
	return a.NewSession().Ask(ctx, req, ev)
}

func (a *genkitAssistant) Answer(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return a.NewSession().Answer(ctx, req, ev)
}

func (a *genkitAssistant) ExplainResults(ctx context.Context, req assistant.ExplainResultsRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	return a.NewSession().ExplainResults(ctx, req, ev)
}

func (a *genkitAssistant) FixError(ctx context.Context, req assistant.FixErrorRequest, ev assistant.Events) (assistant.AskResult, error) {
	return a.NewSession().FixError(ctx, req, ev)
}

// assistantSession runs the flows with one Toolset.
type assistantSession struct {
	a  *genkitAssistant
	ts *tools.Toolset
}

// run runs a flow for the session: Genkit is initialised if needed, the
// callbacks and the Toolset go on the context, and a cancelled ctx returns
// its error (wrapped) with no partial result.
func run[In, Out any](s *assistantSession, ctx context.Context, ev assistant.Events, flow func(*flowSet) func(context.Context, In) (Out, error), in In) (Out, error) {
	var zero Out
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("ai: %w", err)
	}
	_, fs, err := s.a.ready(ctx)
	if err != nil {
		return zero, err
	}
	out, err := flow(fs)(withTurn(ctx, ev, s.ts), in)
	if cerr := ctx.Err(); cerr != nil {
		return zero, fmt.Errorf("ai: %w", cerr)
	}
	if err != nil {
		return zero, err
	}
	return out, nil
}

func (s *assistantSession) Ask(ctx context.Context, req assistant.AskRequest, ev assistant.Events) (assistant.AskResult, error) {
	in := askInput{Scope: toFlowScope(req.Scope), Question: req.Question, History: toFlowTurns(req.History)}
	out, err := run(s, ctx, ev, func(fs *flowSet) func(context.Context, askInput) (proposalOutput, error) { return fs.ask.Run }, in)
	return out.result(), err
}

func (s *assistantSession) Answer(ctx context.Context, req assistant.AnswerRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	in := answerInput{Scope: toFlowScope(req.Scope), Question: req.Question, History: toFlowTurns(req.History), AllowExecute: req.AllowExecute}
	out, err := run(s, ctx, ev, func(fs *flowSet) func(context.Context, answerInput) (answerOutput, error) { return fs.answer.Run }, in)
	return out.result(), err
}

func (s *assistantSession) ExplainResults(ctx context.Context, req assistant.ExplainResultsRequest, ev assistant.Events) (assistant.AnswerResult, error) {
	cols := req.Columns
	if cols == nil {
		cols = []driver.Column{}
	}
	in := explainInput{Scope: toFlowScope(req.Scope), Query: req.Query, Truncated: req.Truncated, cols: cols, rows: req.Rows}
	out, err := run(s, ctx, ev, func(fs *flowSet) func(context.Context, explainInput) (answerOutput, error) { return fs.explain.Run }, in)
	return out.result(), err
}

func (s *assistantSession) FixError(ctx context.Context, req assistant.FixErrorRequest, ev assistant.Events) (assistant.AskResult, error) {
	in := fixErrorInput{Scope: toFlowScope(req.Scope), Query: req.Query, Error: req.Error, err: req.Err}
	out, err := run(s, ctx, ev, func(fs *flowSet) func(context.Context, fixErrorInput) (proposalOutput, error) { return fs.fix.Run }, in)
	return out.result(), err
}
