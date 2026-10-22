// Package assistant defines the AI assistant the TUI and the CLI program
// against. It is stdlib-only: the Genkit implementation lives in the
// separate github.com/xavidop/tobira/ai module, so the root module never
// depends on Genkit or the database SDKs its go.mod requires. cmd/tobira
// builds the implementation and injects it.
//
// How "not configured" surfaces: the front ends receive an Assistant and an
// error (tui.Options.Assistant and AIError). A nil Assistant with a nil
// error means there is no ai: section, and the TUI and CLI show the
// "configure ai:" message for ErrNotConfigured. A nil Assistant with an
// error means ai: is configured but setting it up failed; the error (already
// redacted, wrapping ErrAuth, ErrProvider, ...) is shown instead.
package assistant

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/xavidop/tobira/driver"
)

// The assistant's errors. Implementations wrap them with %w and always
// redact the message: it never holds an API key, a password or a secret ref.
var (
	// ErrNotConfigured means there is no ai: section.
	ErrNotConfigured = errors.New("ai is not configured")
	// ErrAuth means the provider credentials are missing or were rejected.
	ErrAuth = errors.New("ai provider authentication failed")
	// ErrRateLimit means the provider refused the request for quota.
	ErrRateLimit = errors.New("ai provider rate limit reached")
	// ErrUnavailable means the provider could not be reached.
	ErrUnavailable = errors.New("ai provider unreachable")
	// ErrProvider is any other provider or model failure.
	ErrProvider = errors.New("ai provider error")
	// ErrScope means the request's scope cannot be used: the connection is
	// unknown or fails to open, or its schema cannot be read. It is the
	// user's input or config, not the provider: `tobira ask` exits 1 for
	// it (usage or config error).
	ErrScope = errors.New("ai scope unavailable")
)

// TurnError is the error of a turn that failed after it called the model:
// Usage is what the turn spent (also reported through Events.Usage). It
// unwraps to the cause, so the sentinels above still match. A cancelled
// turn is never a TurnError.
type TurnError struct {
	Err   error
	Usage Usage
}

func (e *TurnError) Error() string { return e.Err.Error() }
func (e *TurnError) Unwrap() error { return e.Err }

// Scope is what the user is looking at: a connection and, optionally, a
// namespace and a collection in it.
type Scope struct{ Conn, Namespace, Collection string }

// Usage describes one model turn.
type Usage struct {
	// Model is the provider-qualified model name ("googleai/gemini-3.8-flash").
	Model        string
	InputTokens  int
	OutputTokens int
	Latency      time.Duration
}

// Consent is the answer to a request to send row data to the model.
type Consent int

// The row-consent answers.
const (
	ConsentNo Consent = iota
	ConsentOnce
	ConsentSession
)

// MaxConsentWhatRunes caps ConsentRequest.What.
const MaxConsentWhatRunes = 120

// ConsentRequest asks whether row data may be sent to the model.
type ConsentRequest struct {
	// Conn is the connection the rows come from.
	Conn string
	// Rows is how many rows would be sent.
	Rows int
	// What says what produced them, for the prompt: the tool name and its
	// statement ("query: select ..."), or "the error text of your failed
	// query" for fix_error. It is redacted, on one line, with control
	// characters escaped, and at most MaxConsentWhatRunes runes.
	What string
}

// ConsentWhat builds ConsentRequest.What from a tool (or flow) name and the
// statement it ran, already redacted: "tool: statement" on one line, with
// control and format characters (bidi overrides included) escaped as
// \u{XXXX}, cut to MaxConsentWhatRunes runes ending in "…".
func ConsentWhat(tool, stmt string) string {
	s := tool
	if stmt = strings.Join(strings.Fields(stmt), " "); stmt != "" {
		s += ": " + stmt
	}
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "\uFFFD") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u{%04X}", r)
			continue
		}
		b.WriteRune(r)
	}
	out := []rune(b.String())
	if len(out) <= MaxConsentWhatRunes {
		return string(out)
	}
	return string(out[:MaxConsentWhatRunes-1]) + "…"
}

// RowsText is "1 row" or "N rows".
func RowsText(n int) string {
	if n == 1 {
		return "1 row"
	}
	return strconv.Itoa(n) + " rows"
}

// ToolCall reports one tool call of an answer turn, for display.
type ToolCall struct {
	// Name is the tool ("query", "describe", ...).
	Name string
	// Conn is the connection the call used, "" for none (list_connections,
	// or a call refused before it reached one).
	Conn string
	// Input is a compact, redacted rendering of the call's input.
	Input string
	// Rows is the number of rows the call returned, -1 when not applicable.
	Rows int
	// Err is the redacted error, "" on success.
	Err string
}

// Events are callbacks invoked from the assistant's goroutine. They must be
// goroutine-safe and must not block the UI (the TUI turns them into
// messages). Any of them may be nil.
type Events struct {
	ToolCall func(ToolCall)
	// ConsentRows asks whether the rows req describes may be sent to the
	// model. Nil means ConsentNo.
	ConsentRows func(ctx context.Context, req ConsentRequest) (Consent, error)
	Usage       func(Usage)
	// ConfirmExecute asks the user to confirm running stmt on conn through
	// the execute tool. Only the TUI sets it; nil means refuse. An
	// *ExecuteRefusal error refuses the statement with its reason.
	ConfirmExecute func(ctx context.Context, conn, stmt string) (bool, error)
}

// OnToolCall reports tc when a ToolCall callback is set.
func (e Events) OnToolCall(tc ToolCall) {
	if e.ToolCall != nil {
		e.ToolCall(tc)
	}
}

// OnUsage reports u when a Usage callback is set.
func (e Events) OnUsage(u Usage) {
	if e.Usage != nil {
		e.Usage(u)
	}
}

// AskConsent asks whether the rows req describes may be sent. With no
// callback the answer is ConsentNo; a cancelled ctx returns ConsentNo and
// its error without asking.
func (e Events) AskConsent(ctx context.Context, req ConsentRequest) (Consent, error) {
	if err := ctx.Err(); err != nil {
		return ConsentNo, err
	}
	if e.ConsentRows == nil {
		return ConsentNo, nil
	}
	return e.ConsentRows(ctx, req)
}

// ConfirmExec asks whether stmt may run on conn. With no callback the answer
// is no; a cancelled ctx returns false and its error without asking.
func (e Events) ConfirmExec(ctx context.Context, conn, stmt string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if e.ConfirmExecute == nil {
		return false, nil
	}
	return e.ConfirmExecute(ctx, conn, stmt)
}

// ExecuteRefusal is what ConfirmExecute returns to refuse a statement that
// cannot run now, without asking: for example, the TUI refuses while a
// transaction is open on the connection. Reason is the error the model
// gets for the call.
type ExecuteRefusal struct{ Reason string }

func (e *ExecuteRefusal) Error() string { return e.Reason }

// Turn is one message of a panel conversation.
type Turn struct {
	// Role is "user" or "model".
	Role string
	Text string
}

// AskRequest asks for a proposed query. Nothing is run.
type AskRequest struct {
	Scope    Scope
	Question string
	History  []Turn
}

// AskResult is a proposed query with its explanation.
type AskResult struct {
	Query, Explanation, Dialect string
	Usage                       Usage
}

// AnswerRequest asks for an answer, computed with the read-only tools.
type AnswerRequest struct {
	Scope    Scope
	Question string
	History  []Turn
	// AllowExecute offers the execute tool. Only the TUI sets it, and every
	// call is confirmed there; ask and answer flows from the CLI never do.
	AllowExecute bool
}

// AnswerResult is a markdown answer and the queries run to reach it.
type AnswerResult struct {
	Markdown string
	Ran      []RanQuery
	Usage    Usage
}

// RanQuery is one query a tool ran.
type RanQuery struct {
	Conn, Query string
	Rows        int
	Err         string
}

// ExplainResultsRequest asks for a summary of a result tab.
type ExplainResultsRequest struct {
	Scope     Scope
	Query     string
	Columns   []driver.Column
	Rows      []driver.Row
	Truncated bool
}

// FixErrorRequest asks for a corrected query. Error is the message the user
// saw, already redacted. Err, when set, is the original driver error: its
// text may quote row values, so the assistant sends it only as the row
// policy (ai.send_rows) allows, and otherwise a scrubbed form built from
// its kind and code. Without Err, Error is treated the same way.
type FixErrorRequest struct {
	Scope        Scope
	Query, Error string
	Err          error
}

// Session is a conversation with the assistant: the four turns, without
// the model name. Row consent given for the session
// (ConsentSession) lasts as long as the Session, across its calls.
type Session interface {
	Ask(ctx context.Context, req AskRequest, ev Events) (AskResult, error)
	Answer(ctx context.Context, req AnswerRequest, ev Events) (AnswerResult, error)
	ExplainResults(ctx context.Context, req ExplainResultsRequest, ev Events) (AnswerResult, error)
	FixError(ctx context.Context, req FixErrorRequest, ev Events) (AskResult, error)
}

// Sessioner is implemented by an Assistant that can keep sessions. The TUI
// keeps one Session per panel so that row consent spans the panel's turns;
// an Assistant's own methods each run as a session of their own.
type Sessioner interface {
	NewSession() Session
}

// Assistant is the AI assistant.
type Assistant interface {
	Session
	// Model is the display name, e.g. "googleai/gemini-3.8-flash".
	Model() string
}
