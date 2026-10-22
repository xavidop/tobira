package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/session"
)

// The tool names.
const (
	NameListConnections = "list_connections"
	NameListNamespaces  = "list_namespaces"
	NameListCollections = "list_collections"
	NameDescribe        = "describe"
	NameSample          = "sample"
	NameQuery           = "query"
	NameExplain         = "explain"
	NameExecute         = "execute"
)

// The limits of every tool call.
const (
	// MaxRowsCap is the most rows a tool result carries, whatever MaxRows says.
	MaxRowsCap = config.AIMaxRowsCap
	// DefaultMaxRows is the row limit when ToolDeps.MaxRows is not positive.
	DefaultMaxRows = 50
	// CallTimeout bounds one tool call.
	CallTimeout = 30 * time.Second
	// MaxValueRunes is the length a string value is cut to (plus "…").
	MaxValueRunes = 256
	// maxPlanRunes bounds an explain plan.
	maxPlanRunes = 4000
	// maxErrorRunes bounds an error message.
	maxErrorRunes = 1000
)

// RowPolicy is ai.send_rows: whether row data may be sent to the model.
type RowPolicy string

// The row policies.
const (
	// RowsAsk asks through assistant.Events.ConsentRows; no answer is no.
	RowsAsk RowPolicy = "ask"
	// RowsAlways sends rows without asking.
	RowsAlways RowPolicy = "always"
	// RowsNever withholds rows: tools return the columns and a count.
	RowsNever RowPolicy = "never"
)

// The notes a tool result carries when it withholds rows.
const (
	noteNever    = "rows withheld by policy (ai.send_rows: never)"
	noteDeclined = "rows withheld: the user declined to send them (ai.send_rows: ask)"
	noteNoAnswer = "rows withheld: the consent request failed (ai.send_rows: ask)"
)

// HistoryRecorder records tool-run queries. *history.Store satisfies it.
type HistoryRecorder interface {
	Record(ctx context.Context, e history.Entry, cc driver.ConnConfig) error
}

// ToolDeps are a Toolset's collaborators and settings.
type ToolDeps struct {
	// Manager opens the connections.
	Manager *session.Manager
	// History records query, sample, explain and execute runs, best effort;
	// nil records nothing.
	History HistoryRecorder
	// Policy is ai.send_rows; "" means RowsAsk.
	Policy RowPolicy
	// MaxRows is ai.max_rows; not positive means DefaultMaxRows, and it is
	// capped at RowCap.
	MaxRows int
	// RowCap is the most rows a result may carry; not positive means
	// MaxRowsCap. The MCP server raises it to mcp.max_rows' cap.
	RowCap int
	// Redact, when set, masks more after the connection's own redaction,
	// for example the provider's API key.
	Redact func(string) string
	// Connections is the set of connections the tools may name; nil means
	// every configured one. A turn narrows it further with WithConnections
	// (the assistant binds each turn to its scope's connection). Any other
	// name is refused exactly like a name that is not configured, and
	// list_connections shows only these.
	Connections []string
	// Unavailable maps connections whose driver could not be loaded (a
	// driver plugin whose Info failed) to a redacted one-line reason. The
	// tools refuse them with it, without opening anything, and
	// list_connections reports them with status unavailable.
	Unavailable map[string]string
}

// Toolset is the tools of one assistant session. Row consent given for the
// session (assistant.ConsentSession) lasts as long as the Toolset. It is
// safe for concurrent use.
type Toolset struct {
	deps    ToolDeps
	policy  RowPolicy
	maxRows int
	timeout time.Duration

	mu      sync.Mutex
	consent map[string]bool              // consentKey -> session consent given
	flights map[flightKey]*consentFlight // per turn and connection: prompt in flight

	read    []ai.ToolRef
	similar ai.ToolRef
	execute ai.ToolRef

	// fallbackEvents, when set, gives the callbacks of a call whose context
	// carries none (see WithEvents). Only tests set it: production turns
	// always put their callbacks on the context.
	fallbackEvents func() assistant.Events
}

// NewToolset builds the tools over deps. It needs no Genkit instance: the
// tools are dynamic and are registered by each Generate call that uses them.
func NewToolset(deps ToolDeps) *Toolset {
	t := &Toolset{deps: deps, policy: deps.Policy, maxRows: deps.MaxRows, timeout: CallTimeout, consent: map[string]bool{}, flights: map[flightKey]*consentFlight{}}
	if t.policy == "" {
		t.policy = RowsAsk
	}
	if t.maxRows <= 0 {
		t.maxRows = DefaultMaxRows
	}
	rowCap := deps.RowCap
	if rowCap <= 0 {
		rowCap = MaxRowsCap
	}
	t.maxRows = min(t.maxRows, rowCap)
	t.read = []ai.ToolRef{
		newTool(NameListConnections, "List the configured database connections with their driver, kind, dialect, read-only flag and status.",
			func(ctx context.Context, _ struct{}) ([]Connection, error) { return t.ListConnections(ctx) }),
		newTool(NameListNamespaces, "List the namespaces (schemas or databases) of a connection.", t.ListNamespaces),
		newTool(NameListCollections, "List the collections (tables, views, ...) of a namespace with row estimates.", t.ListCollections),
		newTool(NameDescribe, "Describe a collection: fields and types, primary key, indexes, foreign keys, row estimate and comment.", t.Describe),
		newTool(NameSample, "Read the first rows of a collection, read-only.", t.Sample),
		newTool(NameQuery, "Run a read-only query in the connection's own dialect and return its rows. Writes are refused.", t.Query),
		newTool(NameExplain, "Show the query plan of a read-only query without running it.", t.Explain),
	}
	t.similar = newTool(NameSimilar, "Find the rows of a vector collection nearest to a vector or to a text (embedded with the configured embedder), read-only. Results carry a _distance column, nearest first.", t.Similar)
	t.execute = newTool(NameExecute, "Run a statement that may write. The user must confirm every call; refused on read-only connections.", t.Exec)
	return t
}

func newTool[In, Out any](name, desc string, fn func(context.Context, In) (Out, error)) *ai.ToolAction[In, Out] {
	return ai.NewTool(name, desc, func(tc *ai.ToolContext, in In) (Out, error) { return fn(tc, in) })
}

// Read returns the schema and read tools, for the answer flow.
func (t *Toolset) Read() []ai.ToolRef { return slices.Clone(t.read) }

// Execute returns the execute tool. It is offered only where every call is
// confirmed (the TUI), and it refuses unless the turn's context came from
// WithExecute.
func (t *Toolset) Execute() ai.ToolRef { return t.execute }

type executeKey struct{}

// WithExecute marks ctx as a turn whose caller allowed execute
// (assistant.AnswerRequest.AllowExecute).
func WithExecute(ctx context.Context) context.Context {
	return context.WithValue(ctx, executeKey{}, true)
}

type eventsKey struct{}

// turnEvents is what WithEvents puts on a context: the callbacks and the
// turn they belong to.
type turnEvents struct {
	ev   assistant.Events
	turn *turnToken
}

// turnToken identifies one turn. It has a size so that every token is a
// distinct pointer.
type turnToken struct{ _ byte }

// WithEvents starts a turn on ctx with callbacks ev, so one Toolset (a panel
// session, whose row consent spans its turns) can serve turns that each
// report to their own callbacks. Each call is a new turn: a consent prompt
// is shared only by the calls of the turn that opened it. A call whose
// context has no callbacks gets none: row consent is no and execute is
// refused.
func WithEvents(ctx context.Context, ev assistant.Events) context.Context {
	return context.WithValue(ctx, eventsKey{}, turnEvents{ev: ev, turn: &turnToken{}})
}

type connsKey struct{}

// WithConnections limits the tools of the turn ctx belongs to to names:
// any other connection is refused as unknown. It narrows
// ToolDeps.Connections and any earlier WithConnections; it never widens
// them.
func WithConnections(ctx context.Context, names ...string) context.Context {
	allow := map[string]bool{}
	prev, scoped := ctx.Value(connsKey{}).(map[string]bool)
	for _, n := range names {
		if !scoped || prev[n] {
			allow[n] = true
		}
	}
	return context.WithValue(ctx, connsKey{}, allow)
}

// scopedConn fills an empty connection name with the one connection the
// turn is bound to (WithConnections with a single name): the conversation
// is about that connection, so a tool that names none means it. Any other
// turn keeps the empty name, which open refuses.
func (t *Toolset) scopedConn(ctx context.Context, name string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	allow, scoped := ctx.Value(connsKey{}).(map[string]bool)
	if !scoped || len(allow) != 1 {
		return name
	}
	for only := range allow {
		return only
	}
	return name
}

// allowed reports whether the tools may name conn in ctx's turn.
func (t *Toolset) allowed(ctx context.Context, conn string) bool {
	if t.deps.Connections != nil && !slices.Contains(t.deps.Connections, conn) {
		return false
	}
	if allow, scoped := ctx.Value(connsKey{}).(map[string]bool); scoped && !allow[conn] {
		return false
	}
	return true
}

// errUnknownConn is the error for a name that is not configured, or that the
// turn may not use: the two read the same, so the model learns nothing
// about connections outside its scope.
func errUnknownConn(name string) error {
	return fmt.Errorf("unknown connection %q: call list_connections for the names", name)
}

func executeAllowed(ctx context.Context) bool {
	ok, _ := ctx.Value(executeKey{}).(bool)
	return ok
}

// outcome is embedded in every result: a failed call carries only Error.
type outcome struct {
	Error string `json:"error,omitempty"`
}

func (o *outcome) fail(msg string) { o.Error = msg }
func (o *outcome) errText() string { return o.Error }
func (o *outcome) rowCount() int   { return -1 }

type result interface {
	fail(msg string)
	errText() string
	rowCount() int
}

// run is one tool call. fn gets a context bounded by the call timeout for
// the driver, and the turn's context for asking the user. An error from fn
// becomes the result's redacted Error, unless the turn itself was cancelled,
// which ends the turn. Every call is reported as an assistant.ToolCall.
func run[In, Out any, P interface {
	*Out
	result
}](t *Toolset, ctx context.Context, name, conn string, in In, fn func(call, turn context.Context) (Out, error)) (Out, error) {
	var zero Out
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	call, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, err := fn(call, ctx)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return zero, cerr
		}
		out = zero
		P(&out).fail(t.errorText(conn, call, err))
	}
	t.report(ctx, name, conn, in, P(&out).rowCount(), P(&out).errText())
	return out, nil
}

func (t *Toolset) errorText(conn string, call context.Context, err error) string {
	msg := err.Error()
	if errors.Is(err, context.DeadlineExceeded) && call.Err() != nil {
		msg = fmt.Sprintf("timed out after %s", t.timeout)
	}
	var rd *rowDataError
	timedOut := errors.Is(err, context.DeadlineExceeded) && call.Err() != nil
	if errors.As(err, &rd) && !timedOut && !t.rowsAllowedNow(conn) {
		return truncate(scrubDriverError(err, t.redactor(conn)), maxErrorRunes)
	}
	return truncate(t.redact(conn, msg), maxErrorRunes)
}

// rowDataError marks an error from running a statement (query, browse,
// explain): its text may quote row values, so it obeys the row policy.
type rowDataError struct{ err error }

func (e *rowDataError) Error() string { return e.err.Error() }
func (e *rowDataError) Unwrap() error { return e.err }

// rowData marks err as a statement error; nil stays nil.
func rowData(err error) error {
	if err == nil {
		return nil
	}
	return &rowDataError{err}
}

// rowsAllowedNow reports whether rows from conn may be sent without asking:
// the policy is always, or the session already consented. It never prompts,
// so an error alone never opens a consent dialog.
func (t *Toolset) rowsAllowedNow(conn string) bool {
	if t.policy == RowsAlways {
		return true
	}
	if t.policy == RowsNever {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.consent[t.consentKey(conn)]
}

// consentKey is conn plus its configuration generation, so a connection a
// reload repoints (or a new one reusing the name) asks again.
func (t *Toolset) consentKey(conn string) string {
	var gen uint64
	if t.deps.Manager != nil {
		gen = t.deps.Manager.ConfigGeneration(conn)
	}
	return conn + "\x00" + strconv.FormatUint(gen, 10)
}

// flightKey is the turn and the connection (consentKey) of a consent
// prompt.
type flightKey struct {
	consent string
	turn    *turnToken
}

// consentFlight is one consent prompt; concurrent calls of the same turn on
// the same connection wait for it and share its answer.
type consentFlight struct {
	done    chan struct{}
	consent assistant.Consent
	err     error
	waiters int // guarded by Toolset.mu
}

// consentWaiters is how many calls wait on conn's prompts (for tests).
func (t *Toolset) consentWaiters(conn string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, key := 0, t.consentKey(conn)
	for k, f := range t.flights {
		if k.consent == key {
			n += f.waiters
		}
	}
	return n
}

// events returns the turn's callbacks, set with WithEvents; none when the
// context has none.
func (t *Toolset) events(ctx context.Context) assistant.Events {
	if te, ok := ctx.Value(eventsKey{}).(turnEvents); ok {
		return te.ev
	}
	if t.fallbackEvents != nil {
		return t.fallbackEvents()
	}
	return assistant.Events{}
}

// report reports one call. Conn is set only for a connection the turn may
// use (a refused name never counts as touched).
func (t *Toolset) report(ctx context.Context, name, conn string, in any, rows int, errText string) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // shown to people, not embedded in HTML
	_ = enc.Encode(in)       // the inputs are plain structs
	b := strings.TrimSuffix(buf.String(), "\n")
	tc := assistant.ToolCall{Name: name, Input: truncate(t.redact(conn, b), MaxValueRunes), Rows: rows, Err: errText}
	if conn != "" && t.deps.Manager != nil && t.allowed(ctx, conn) && slices.Contains(t.deps.Manager.Names(), conn) {
		tc.Conn = conn
	}
	t.events(ctx).OnToolCall(tc)
}

// redact masks conn's secrets in s, then applies ToolDeps.Redact.
func (t *Toolset) redact(conn, s string) string {
	if t.deps.Manager != nil {
		s = t.deps.Manager.Redact(conn, s)
	} else {
		s = driver.RedactMessage(s, driver.ConnConfig{})
	}
	if t.deps.Redact != nil {
		s = t.deps.Redact(s)
	}
	return s
}

// redactor returns redact bound to conn.
func (t *Toolset) redactor(conn string) func(string) string {
	return func(s string) string { return t.redact(conn, s) }
}

// limit clamps a requested row limit to MaxRows; not positive means MaxRows.
func (t *Toolset) limit(n int) int {
	if n <= 0 || n > t.maxRows {
		return t.maxRows
	}
	return n
}

// open resolves name to an open connection. Unknown, missing and broken
// connections are errors the model can act on; a connection outside the
// turn's scope is unknown.
func (t *Toolset) open(ctx context.Context, name string) (driver.Conn, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("connection is required: call list_connections for the names")
	}
	if !t.allowed(ctx, name) || !slices.Contains(t.deps.Manager.Names(), name) {
		return nil, errUnknownConn(name)
	}
	if why, ok := t.deps.Unavailable[name]; ok {
		return nil, errUnavailable(name, why)
	}
	if st := t.deps.Manager.State(name); st.Broken {
		return nil, fmt.Errorf("connection %q is broken: %s", name, firstLine(st.Err))
	}
	return t.deps.Manager.Open(ctx, name)
}

// errUnavailable is the refusal of a connection in ToolDeps.Unavailable.
func errUnavailable(name, why string) error {
	return fmt.Errorf("connection %q is unavailable: %s", name, why)
}

// openReadable is open for the read tools: the driver must declare
// driver.CapReadOnly, checked before anything is opened.
func (t *Toolset) openReadable(ctx context.Context, name string) (driver.Conn, error) {
	_, unavailable := t.deps.Unavailable[name]
	if !unavailable && t.allowed(ctx, name) && slices.Contains(t.deps.Manager.Names(), name) {
		st := t.deps.Manager.State(name)
		if !st.Broken && !driver.HasCapability(st.Info, driver.CapReadOnly) {
			return nil, fmt.Errorf("connection %q: its driver does not guarantee read-only queries, so only the schema tools (list_namespaces, list_collections, describe) are available", name)
		}
	}
	return t.open(ctx, name)
}

// allowRows applies the row policy to n rows from conn; what says what
// produced them (see consentWhat). It returns whether they may be sent and,
// when not, the note saying why. The only error is the turn's cancellation.
func (t *Toolset) allowRows(turn context.Context, conn string, n int, what string) (bool, string, error) {
	if n == 0 {
		return true, "", nil
	}
	switch t.policy {
	case RowsAlways:
		return true, "", nil
	case RowsNever:
		return false, noteNever, nil
	}
	c, err := t.askConsent(turn, assistant.ConsentRequest{Conn: conn, Rows: n, What: what})
	if err != nil {
		if cerr := turn.Err(); cerr != nil {
			return false, "", cerr
		}
		return false, noteNoAnswer, nil
	}
	if c == assistant.ConsentSession || c == assistant.ConsentOnce {
		return true, "", nil
	}
	return false, noteDeclined, nil
}

// askConsent returns the remembered session consent, or the answer of the
// prompt in flight for req.Conn, or asks. One prompt runs per connection at
// a time; calls that arrive meanwhile share its answer.
func (t *Toolset) askConsent(turn context.Context, req assistant.ConsentRequest) (assistant.Consent, error) {
	key := t.consentKey(req.Conn)
	var tok *turnToken
	if te, ok := turn.Value(eventsKey{}).(turnEvents); ok {
		tok = te.turn
	}
	fk := flightKey{consent: key, turn: tok}
	t.mu.Lock()
	if t.consent[key] {
		t.mu.Unlock()
		return assistant.ConsentSession, nil
	}
	if f := t.flights[fk]; f != nil {
		f.waiters++
		t.mu.Unlock()
		select {
		case <-f.done:
			return f.consent, f.err
		case <-turn.Done():
			return assistant.ConsentNo, turn.Err()
		}
	}
	f := &consentFlight{done: make(chan struct{})}
	t.flights[fk] = f
	t.mu.Unlock()

	// Release the flight however the prompt ends, a panic included, so no
	// later call waits on it forever. A panic leaves errConsentFailed.
	c, err := assistant.ConsentNo, errConsentFailed
	defer func() {
		t.mu.Lock()
		if err == nil && c == assistant.ConsentSession {
			t.consent[key] = true
		}
		delete(t.flights, fk)
		f.consent, f.err = c, err
		t.mu.Unlock()
		close(f.done)
	}()
	c, err = t.events(turn).AskConsent(turn, req)
	return c, err
}

// whatErrorText is ConsentRequest.What for the error of a statement the
// user ran (fix_error).
const whatErrorText = "the error text of your failed query"

// consentWhat is ConsentRequest.What for rows a tool (or a flow) produced:
// the tool name and its statement, redacted for conn (see
// assistant.ConsentWhat).
func (t *Toolset) consentWhat(conn, tool, text string) string {
	return assistant.ConsentWhat(tool, t.redact(conn, text))
}

// errConsentFailed is the answer a consent prompt that never returned
// (it panicked) leaves to the calls waiting on it.
var errConsentFailed = errors.New("the consent prompt failed")

// record stores a run in the history, best effort. The text is redacted
// with the connection's settings first, as the TUI does. The recorder gets
// ctx's values (the MCP server's client name) but not its cancellation.
func (t *Toolset) record(ctx context.Context, conn, text string, ok bool, rows int64, took time.Duration) {
	if t.deps.History == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	e := history.Entry{Conn: conn, Source: history.SourceAI, Query: t.redact(conn, text), OK: ok, Rows: rows, Took: took}
	_ = t.deps.History.Record(ctx, e, driver.ConnConfig{}) // history must never fail a tool call
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return truncate(strings.TrimSpace(s), MaxValueRunes)
}
