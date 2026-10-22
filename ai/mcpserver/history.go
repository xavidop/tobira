package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/query"
)

// maxClientRunes is the most of a client's name that is kept, as history
// stores it.
const maxClientRunes = 64

type (
	clientKey   struct{} // the calling client's name
	recordedKey struct{} // *atomic.Bool: the call was recorded
)

// recorder stores the Toolset's history entries with source mcp and the
// calling client's name, and marks the call as recorded.
type recorder struct{ store *history.Store }

func (r recorder) Record(ctx context.Context, e history.Entry, cc driver.ConnConfig) error {
	if f, ok := ctx.Value(recordedKey{}).(*atomic.Bool); ok && f != nil {
		f.Store(true)
	}
	e.Source = history.SourceMCP
	e.Client, _ = ctx.Value(clientKey{}).(string)
	return r.store.Record(ctx, e, cc)
}

// rememberClient keeps the clientInfo a session sent in initialize as
// "name/version" ("name" without a version), the whole value cut to its
// first 64 runes (history escapes it when storing). A panic here is
// recovered: a hook must never end the session.
func (s *srv) rememberClient(ctx context.Context, _ any, req *mcp.InitializeRequest, _ *mcp.InitializeResult) {
	defer s.recoverHook("initialize")
	cs := server.ClientSessionFromContext(ctx)
	if cs == nil || req == nil {
		return
	}
	name := clientLabel(req.Params.ClientInfo)
	s.clients.Store(cs.SessionID(), name)
	s.log.Debug("mcp client initialized", "client", query.EscapeControls(name))
}

// recoverHook recovers a panic in a hook, logging it when it can.
func (s *srv) recoverHook(hook string) {
	if r := recover(); r != nil && s.log != nil {
		s.log.Error("mcp hook panicked", "hook", hook, "panic", fmt.Sprint(r))
	}
}

// forgetClient drops a closed session's client name.
func (s *srv) forgetClient(_ context.Context, cs server.ClientSession) {
	defer s.recoverHook("unregister")
	s.clients.Delete(cs.SessionID())
}

// clientLabel is "name/version" from a client's declared implementation, cut
// to maxClientRunes.
func clientLabel(info mcp.Implementation) string {
	name := info.Name
	if info.Version != "" {
		name += "/" + info.Version
	}
	return cut(name, maxClientRunes)
}

// clientName is the name of the calling client: the one this request
// declares in its _meta (protocol revisions without initialize), else the
// one its session gave in initialize; "" when it gave none.
func (s *srv) clientName(ctx context.Context) string {
	if info := server.RequestProtocolInfoFromContext(ctx); info != nil && info.ClientInfo != nil {
		return clientLabel(*info.ClientInfo)
	}
	cs := server.ClientSessionFromContext(ctx)
	if cs == nil {
		return ""
	}
	name, _ := s.clients.Load(cs.SessionID())
	str, _ := name.(string)
	return str
}

// cut returns the first n runes of s, counting an invalid byte as one rune
// as history does.
func cut(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// maxToolNameRunes is the most of an unknown tool's name history keeps.
const maxToolNameRunes = 64

// recordUnknownTool records a call to a tool the server does not offer
// (mcp-go answers it before any handler runs) as failed, its query
// "unknown tool <name>" with the name cut to 64 runes and escaped.
func (s *srv) recordUnknownTool(ctx context.Context, _ any, method mcp.MCPMethod, message any, err error) {
	defer s.recoverHook("error")
	req, ok := message.(*mcp.CallToolRequest)
	if method != mcp.MethodToolsCall || !ok || req == nil || !errors.Is(err, server.ErrToolNotFound) {
		return
	}
	ctx = context.WithValue(ctx, clientKey{}, s.clientName(ctx))
	s.record(ctx, "", "unknown tool "+query.EscapeControls(cut(req.Params.Name, maxToolNameRunes)), false, 0)
}

// recordCall records a call the Toolset did not record itself: schema
// tools, refusals and failures before a statement ran. The query is the
// tool and its arguments as JSON (which escapes control characters), with
// similar's query vector as "<vector dim=N>"; an execute refused by policy
// is "refused by policy: <statement>" instead.
func (s *srv) recordCall(ctx context.Context, tool, conn string, args json.RawMessage, ok bool, took time.Duration) {
	if tool == tools.NameSimilar {
		args = summariseVector(args)
	}
	text := tool
	if string(args) != "{}" {
		text += " " + string(args)
	}
	s.record(ctx, conn, text, ok, took)
}

// record stores one history entry with source mcp and the calling client,
// best effort: detached from the call's cancellation, bounded to two
// seconds, as the Toolset records. The text is redacted, and the
// connection is set only when it is visible, so a refused name is never
// stored as a connection.
func (s *srv) record(ctx context.Context, conn, text string, ok bool, took time.Duration) {
	if s.o.History == nil {
		return
	}
	e := history.Entry{Source: history.SourceMCP, Query: s.redact(conn, text), OK: ok, Took: took}
	e.Client, _ = ctx.Value(clientKey{}).(string)
	if s.visible(conn) {
		e.Conn = conn
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := s.o.History.Record(ctx, e, driver.ConnConfig{}); err != nil {
		s.log.Debug("mcp history record failed", "error", driver.RedactMessage(err.Error(), driver.ConnConfig{}))
	}
}

// recordTimeout bounds one history write.
const recordTimeout = 2 * time.Second

// summariseVector replaces the "vector" argument of a similar call with
// "<vector dim=N>" when it is a list; anything else is kept as it is.
func summariseVector(args json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return args
	}
	var vec []json.RawMessage
	if raw, ok := m["vector"]; !ok || json.Unmarshal(raw, &vec) != nil {
		return args
	}
	m["vector"] = json.RawMessage(fmt.Sprintf(`"<vector dim=%d>"`, len(vec)))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(m) != nil {
		return args
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
