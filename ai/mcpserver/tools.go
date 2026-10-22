package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
)

// genkitTool is a registered tool that runs on raw JSON, which keeps row
// values (large integers included) exactly as the Toolset encoded them.
type genkitTool interface {
	gai.Tool
	RunJSON(ctx context.Context, input json.RawMessage, cb core.StreamCallback[json.RawMessage]) (json.RawMessage, error)
}

// executeDescription replaces the execute tool's description, which speaks
// of the TUI's per-call confirmation: over MCP the policy confirms.
const executeDescription = "Run a statement that may write, on a connection listed in mcp.allow_writes. " +
	"Refused by policy on any other connection and on read-only ones."

// mcpTool describes t for MCP with its own JSON input schema.
func mcpTool(t genkitTool) (mcp.Tool, error) {
	def := t.Definition()
	schema := def.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return mcp.Tool{}, fmt.Errorf("mcp: tool %s: input schema: %w", def.Name, err)
	}
	desc := def.Description
	if def.Name == tools.NameExecute {
		desc = executeDescription
	}
	return mcp.NewToolWithRawSchema(def.Name, desc, b), nil
}

// toolHandler runs t for an MCP call. The call's context carries the
// client's name (for history), callbacks that apply the policy, and a flag
// the history recorder sets, so a call the Toolset did not record is
// recorded here: every call lands in history exactly once. A tool result
// with an "error" field becomes an error result with that text. A panic
// becomes an error result too, and the server goes on.
func (s *srv) toolHandler(t genkitTool) server.ToolHandlerFunc {
	name := t.Name()
	return func(ctx context.Context, req mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
		start := time.Now()
		args, conn, stmt := callArgs(req.Params.Arguments)
		recorded := new(atomic.Bool)
		ctx = s.callContext(ctx, recorded)
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("mcp tool panicked", "tool", name, "panic", fmt.Sprint(r))
				res, err = mcp.NewToolResultError("internal error: the "+name+" tool failed"), nil
			}
			switch {
			case res == nil || recorded.Load():
			case name == tools.NameExecute && res.IsError && strings.HasPrefix(resultText(res), refusedPrefix):
				s.record(ctx, conn, refusedPrefix+stmt, false, time.Since(start))
			default:
				s.recordCall(ctx, name, conn, args, !res.IsError, time.Since(start))
			}
		}()
		if name == tools.NameExecute {
			if msg := s.refuseWrite(conn); msg != "" {
				return mcp.NewToolResultError(msg), nil
			}
		}
		out, err := t.RunJSON(ctx, args, nil)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return mcp.NewToolResultError(s.redact(conn, err.Error())), nil
		}
		var env struct {
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(out, &env); err != nil {
			return mcp.NewToolResultError("internal error: the " + name + " tool returned an invalid result"), nil
		}
		var failed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(env.Output, &failed) == nil && failed.Error != "" {
			return mcp.NewToolResultError(failed.Error), nil
		}
		return mcp.NewToolResultText(string(env.Output)), nil
	}
}

// callArgs returns a call's arguments as JSON ({} when absent), the
// connection they name and their statement (execute's).
func callArgs(a any) (json.RawMessage, string, string) {
	b, err := json.Marshal(a)
	if err != nil || string(b) == "null" {
		b = []byte("{}")
	}
	var named struct {
		Connection string `json:"connection"`
		Statement  string `json:"statement"`
	}
	_ = json.Unmarshal(b, &named)
	return b, named.Connection, named.Statement
}

// resultText is the text of a tool result.
func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// recoverTool turns a panic in any tool handler into an error result that
// does not carry the panic value; the server goes on. tobira's own handlers
// recover first (to record the call); this covers every other handler.
func (s *srv) recoverTool(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("mcp tool handler panicked", "tool", req.Params.Name, "panic", fmt.Sprint(r))
				res, err = mcp.NewToolResultError("internal error: the tool failed"), nil
			}
		}()
		return next(ctx, req)
	}
}

// callContext is ctx for one tool call: the client's name, the policy
// callbacks, execute enabled when writes are allowed anywhere, and the
// recorded flag.
func (s *srv) callContext(ctx context.Context, recorded *atomic.Bool) context.Context {
	ctx = context.WithValue(ctx, clientKey{}, s.clientName(ctx))
	ctx = context.WithValue(ctx, recordedKey{}, recorded)
	ctx = tools.WithEvents(ctx, assistant.Events{
		ToolCall: func(tc assistant.ToolCall) {
			s.log.Debug("mcp tool call", "tool", tc.Name, "conn", tc.Conn, "rows", tc.Rows, "error", tc.Err)
		},
		ConsentRows:    s.consentRows,
		ConfirmExecute: s.confirmExecute,
	})
	if len(s.o.Policy.AllowWrites) > 0 {
		ctx = tools.WithExecute(ctx)
	}
	return ctx
}

// consentRows answers the Toolset's consent question. MCP has no prompt:
// exposing a connection is the opt-in, so the answer is the send_rows
// policy (the Toolset only asks under ask, which the server never sets).
func (s *srv) consentRows(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
	if s.o.Policy.SendRows == "never" {
		return assistant.ConsentNo, nil
	}
	return assistant.ConsentSession, nil
}

// confirmExecute is the policy as execute's confirmation: yes only for an
// allow-listed connection that is not read-only. The Toolset re-checks
// read-only after it, at call time.
func (s *srv) confirmExecute(_ context.Context, conn, _ string) (bool, error) {
	if msg := s.refuseWrite(conn); msg != "" {
		return false, &assistant.ExecuteRefusal{Reason: msg}
	}
	return true, nil
}

// refusedPrefix starts every policy refusal, and the history entry of one.
const refusedPrefix = "refused by policy: "

// refuseWrite returns why execute may not run on conn, or "". A name that
// is not an exposed connection gets "": the Toolset answers it as unknown,
// exactly like a name that does not exist.
func (s *srv) refuseWrite(conn string) string {
	if !s.visible(conn) {
		return ""
	}
	if !slices.Contains(s.o.Policy.AllowWrites, conn) {
		return fmt.Sprintf(refusedPrefix+"connection %q is not in mcp.allow_writes", conn)
	}
	if s.o.Manager.State(conn).ReadOnly {
		return fmt.Sprintf(refusedPrefix+"connection %q is read-only", conn)
	}
	return ""
}

// visible reports whether conn is an exposed, configured connection.
func (s *srv) visible(conn string) bool {
	return conn != "" && slices.Contains(s.o.Policy.Expose, conn) && slices.Contains(s.o.Manager.Names(), conn)
}

// redact masks conn's secrets in msg when conn is visible, and DSN-shaped
// credentials in any case.
func (s *srv) redact(conn, msg string) string {
	if s.visible(conn) {
		msg = s.o.Manager.Redact(conn, msg)
	}
	return driver.RedactMessage(msg, driver.ConnConfig{})
}
