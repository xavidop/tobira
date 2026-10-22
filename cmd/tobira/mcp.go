package main

import (
	"context"

	"github.com/xavidop/tobira/ai/mcpserver"
	"github.com/xavidop/tobira/internal/cli"
)

// serveMCP runs the MCP server for `tobira mcp`: over stdio, or over
// Streamable HTTP with --http.
func serveMCP(ctx context.Context, o cli.MCPOptions) error {
	opts := mcpserver.Options{
		Manager: o.Manager, History: o.History, Policy: o.Policy, Version: o.Version,
		Stdin: o.Stdin, Stdout: o.Stdout, Logger: o.Logger, Unavailable: o.Unavailable,
	}
	if o.HTTP == nil {
		return mcpserver.Serve(ctx, opts)
	}
	return mcpserver.ServeHTTP(ctx, opts, mcpserver.HTTP{
		Addr: o.HTTP.Addr, InsecureBind: o.HTTP.InsecureBind, AllowedHosts: o.HTTP.AllowedHosts,
		Token: o.HTTP.Token, Stderr: o.Stderr,
	})
}
