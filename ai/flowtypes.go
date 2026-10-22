package ai

import (
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
)

// The flows' inputs and outputs. Genkit validates both against JSON
// schemas inferred from these types, and the Dev UI builds its forms from
// them, so they are plain JSON shapes; the Assistant methods convert from
// and to the internal/assistant types. What cannot travel as JSON stays in
// unexported fields: a flow run from the Dev UI lacks it and falls back to
// the JSON fields.

// flowScope is assistant.Scope.
type flowScope struct {
	Conn       string `json:"conn"`
	Namespace  string `json:"namespace,omitempty"`
	Collection string `json:"collection,omitempty"`
}

func (s flowScope) scope() assistant.Scope {
	return assistant.Scope{Conn: s.Conn, Namespace: s.Namespace, Collection: s.Collection}
}

func toFlowScope(s assistant.Scope) flowScope {
	return flowScope{Conn: s.Conn, Namespace: s.Namespace, Collection: s.Collection}
}

// flowTurn is assistant.Turn.
type flowTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func toFlowTurns(ts []assistant.Turn) []flowTurn {
	out := make([]flowTurn, 0, len(ts))
	for _, t := range ts {
		out = append(out, flowTurn{Role: t.Role, Text: t.Text})
	}
	return out
}

func turns(ts []flowTurn) []assistant.Turn {
	out := make([]assistant.Turn, 0, len(ts))
	for _, t := range ts {
		out = append(out, assistant.Turn{Role: t.Role, Text: t.Text})
	}
	return out
}

type askInput struct {
	Scope    flowScope  `json:"scope"`
	Question string     `json:"question"`
	History  []flowTurn `json:"history,omitempty"`
}

type answerInput struct {
	Scope    flowScope  `json:"scope"`
	Question string     `json:"question"`
	History  []flowTurn `json:"history,omitempty"`
	// AllowExecute is never JSON: only the TUI, which confirms every call,
	// offers execute, never the Dev UI.
	AllowExecute bool `json:"-"`
}

type fixErrorInput struct {
	Scope flowScope `json:"scope"`
	Query string    `json:"query"`
	Error string    `json:"error"`

	// The original driver error, when the Assistant has it; nil from the
	// Dev UI, where Error is all there is.
	err error
}

// flowColumn is a result column: its name and native type.
type flowColumn struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

type explainInput struct {
	Scope     flowScope    `json:"scope"`
	Query     string       `json:"query"`
	Columns   []flowColumn `json:"columns,omitempty"`
	Rows      [][]any      `json:"rows,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`

	// The result as the TUI holds it; nil from the Dev UI.
	cols []driver.Column
	rows []driver.Row
}

// result returns the result tab's columns and rows: the driver values when
// the Assistant passed them, else the JSON fields.
func (in explainInput) result() ([]driver.Column, []driver.Row) {
	if in.cols != nil {
		return in.cols, in.rows
	}
	cols := make([]driver.Column, 0, len(in.Columns))
	for _, c := range in.Columns {
		cols = append(cols, driver.Column{Name: c.Name, NativeType: c.Type})
	}
	rows := make([]driver.Row, 0, len(in.Rows))
	for _, r := range in.Rows {
		row := make(driver.Row, 0, len(r))
		for _, x := range r {
			v, err := driver.FromAny(x)
			if err != nil {
				v = driver.Null()
			}
			row = append(row, v)
		}
		rows = append(rows, row)
	}
	return cols, rows
}

type flowUsage struct {
	Model        string `json:"model"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	LatencyMs    int64  `json:"latencyMs"`
	latency      time.Duration
}

func toFlowUsage(u assistant.Usage) flowUsage {
	return flowUsage{Model: u.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, LatencyMs: u.Latency.Milliseconds(), latency: u.Latency}
}

func (u flowUsage) usage() assistant.Usage {
	return assistant.Usage{Model: u.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, Latency: u.latency}
}

type proposalOutput struct {
	Query       string    `json:"query"`
	Explanation string    `json:"explanation"`
	Dialect     string    `json:"dialect"`
	Usage       flowUsage `json:"usage"`
}

func (o proposalOutput) result() assistant.AskResult {
	return assistant.AskResult{Query: o.Query, Explanation: o.Explanation, Dialect: o.Dialect, Usage: o.Usage.usage()}
}

// flowRan is assistant.RanQuery.
type flowRan struct {
	Conn  string `json:"conn"`
	Query string `json:"query"`
	Rows  int    `json:"rows"`
	Error string `json:"error,omitempty"`
}

type answerOutput struct {
	Markdown string    `json:"markdown"`
	Ran      []flowRan `json:"ran,omitempty"`
	Usage    flowUsage `json:"usage"`
}

func (o answerOutput) result() assistant.AnswerResult {
	var ran []assistant.RanQuery
	for _, r := range o.Ran {
		ran = append(ran, assistant.RanQuery{Conn: r.Conn, Query: r.Query, Rows: r.Rows, Err: r.Error})
	}
	return assistant.AnswerResult{Markdown: o.Markdown, Ran: ran, Usage: o.Usage.usage()}
}
