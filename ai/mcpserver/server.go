// Package mcpserver serves tobira's database tools to MCP clients: the
// server behind `tobira mcp`.
//
// It registers one ai/tools Toolset, limited to the exposed connections, on
// a private Genkit instance with no model plugin (ai.NewToolHost, pinned to
// prod); that is the only instance that registers tools. The MCP server
// serves exactly the tools registered there, plus a schema resource
// template, over mcp-go.
//
// Genkit's plugins/mcp server is not used, for four reasons (Genkit
// v1.13.1): its GetServer returns nil until its unexported setup runs, which
// only ServeStdio and Serve do, and both are hard-wired to os.Stdin and
// os.Stdout with their own signal handling, so the streams cannot be
// injected and ctx cannot stop them; its tool handler renders a structured
// result with fmt's %v (Go syntax, not JSON); it rebuilds each input schema
// from property types only, dropping descriptions and required fields; and
// it logs through slog's default. So the mcp-go server is built here, and
// each registered Genkit tool is served through a thin adapter that runs it
// with the MCP request's context (which carries the client's name) and
// returns its JSON output.
//
// The policy (config.MCP):
//   - Only exposed connections are visible; any other name is answered as
//     an unknown connection, in tools and in resource URIs alike.
//   - similar is registered only when some exposed connection's driver
//     declares the vector search capability. `tobira mcp` loads each
//     exposed driver plugin's Info before calling Serve, so a plugin's
//     capabilities count; a connection whose plugin did not answer is in
//     Options.Unavailable and is refused by every tool.
//   - execute is registered only when some exposed connection is in
//     allow_writes. On any other connection, or a read-only one, it is
//     refused by policy; on an allow-listed one the policy is the
//     confirmation. The read tools stay read-only everywhere.
//   - max_rows (up to 1000) bounds every result; send_rows never withholds
//     row data as the assistant's never mode does.
//   - Every tool call is recorded in history once, with source mcp and the
//     client name from initialize.
//
// Serve speaks the protocol over stdio, where stdout carries protocol
// frames only: logs go to Options.Logger, and a panic in a handler becomes
// an error result. ServeHTTP speaks it over Streamable HTTP (mcp-go's
// server mounted on an http.Server of our own), behind internal/mcphttp's
// guard: loopback unless allowed, a bearer token, and Host and Origin
// checks against DNS rebinding.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/mark3labs/mcp-go/server"
	"github.com/xavidop/tobira/ai"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/session"
)

// Options configure Serve.
type Options struct {
	// Manager opens the connections.
	Manager *session.Manager
	// History records every tool call; nil records nothing.
	History *history.Store
	// Policy is the resolved mcp: section, flags applied. Expose must not be
	// empty; AllowWrites should be a subset of it (names outside Expose are
	// never writable).
	Policy config.MCP
	// Version is reported to clients in initialize.
	Version string
	// Stdin and Stdout carry the protocol (Serve only).
	Stdin  io.Reader
	Stdout io.Writer
	// Logger receives debug logs; nil discards them.
	Logger *slog.Logger
	// Unavailable maps exposed connections whose driver could not be
	// loaded (a driver plugin whose Info failed) to a redacted reason: the
	// tools refuse them and list_connections reports them unavailable (see
	// tools.ToolDeps.Unavailable).
	Unavailable map[string]string
}

// errNothingExposed is the refusal to serve no connection at all.
var errNothingExposed = errors.New("nothing is exposed: pass --expose or set mcp.expose")

// Serve serves the MCP protocol on o.Stdin and o.Stdout until the client
// closes stdin (nil) or ctx is cancelled (nil). Other errors are failures of
// the streams or of the setup.
func Serve(ctx context.Context, o Options) error {
	s, err := newServer(ctx, o)
	if err != nil {
		return err
	}
	return s.listen(ctx)
}

// listen serves the protocol on the options' streams (see Serve).
func (s *srv) listen(ctx context.Context) error {
	stdio := server.NewStdioServer(s.mcp)
	stdio.SetErrorLogger(slog.NewLogLogger(s.log.Handler(), slog.LevelDebug))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops mcp-go's notification writer
	err := stdio.Listen(ctx, s.o.Stdin, s.o.Stdout)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// srv is one MCP server: the policy, the Toolset and the mcp-go server.
type srv struct {
	o   Options
	log *slog.Logger
	ts  *tools.Toolset
	mcp *server.MCPServer

	clients sync.Map // session ID -> client name from initialize

	// HTTP only: the clock, the shutdown grace and the drain bound after
	// it, and the session table (see http.go).
	now      func() time.Time
	grace    time.Duration
	drain    time.Duration
	sessions *sessionTable
}

func newServer(ctx context.Context, o Options) (*srv, error) {
	if len(o.Policy.Expose) == 0 {
		return nil, errNothingExposed
	}
	if o.Manager == nil {
		return nil, errors.New("mcp: no session manager")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Policy.MaxRows <= 0 {
		o.Policy.MaxRows = (*config.Config)(nil).MCPSettings().MaxRows // the mcp default, not the assistant's
	}
	s := &srv{o: o, log: o.Logger, now: time.Now, grace: shutdownGrace, drain: shutdownDrain}
	s.o.Policy.AllowWrites = slices.DeleteFunc(slices.Clone(o.Policy.AllowWrites), func(n string) bool { return !slices.Contains(o.Policy.Expose, n) })
	deps := tools.ToolDeps{
		Manager:     o.Manager,
		Policy:      tools.RowsAlways,
		MaxRows:     o.Policy.MaxRows,
		RowCap:      config.MCPMaxRowsCap,
		Connections: slices.Clone(o.Policy.Expose),
		Unavailable: o.Unavailable,
	}
	if o.Policy.SendRows == "never" {
		deps.Policy = tools.RowsNever
	}
	if o.History != nil {
		deps.History = recorder{o.History} // never a typed nil in the interface
	}
	s.ts = tools.NewToolset(deps)

	g, err := ai.NewToolHost(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	offered := s.ts.Read()
	if s.ts.VectorSearchable(ctx) {
		offered = append(offered, s.ts.SimilarTool())
	}
	if len(s.o.Policy.AllowWrites) > 0 {
		offered = append(offered, s.ts.Execute())
	}
	for _, ref := range offered {
		t, ok := ref.(gai.Tool)
		if !ok {
			return nil, fmt.Errorf("mcp: tool %s cannot be registered", ref.Name())
		}
		genkit.RegisterAction(g, t)
	}

	hooks := &server.Hooks{}
	hooks.AddAfterInitialize(s.rememberClient)
	hooks.AddOnUnregisterSession(s.forgetClient)
	hooks.AddBeforeReadResource(s.beforeReadResource)
	hooks.AddOnError(s.recordUnknownTool)
	// mcp-go applies middlewares in order, the first outermost:
	// WithRecovery is the last backstop, recoverTool answers a panic with a
	// tool error before it.
	s.mcp = server.NewMCPServer("tobira", o.Version,
		server.WithRecovery(),
		server.WithToolHandlerMiddleware(s.recoverTool),
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithHooks(hooks),
	)
	for _, gt := range genkit.ListTools(g) {
		t, ok := gt.(genkitTool)
		if !ok {
			return nil, fmt.Errorf("mcp: tool %s cannot run on JSON", gt.Name())
		}
		mt, err := mcpTool(t)
		if err != nil {
			return nil, err
		}
		s.mcp.AddTool(mt, s.toolHandler(t))
	}
	s.addSchemaResource()
	s.log.Debug("mcp server ready", "tools", len(offered), "expose", o.Policy.Expose, "allow_writes", s.o.Policy.AllowWrites)
	return s, nil
}
